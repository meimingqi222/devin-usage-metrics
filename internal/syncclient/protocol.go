package syncclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type head struct {
	Protocol           int     `json:"protocol"`
	Sequence           uint64  `json:"sequence"`
	Manifest           string  `json:"manifest,omitempty"`
	Previous           *string `json:"previous_manifest"`
	Generation         string  `json:"generation,omitempty"`
	PreviousGeneration *string `json:"previous_generation,omitempty"`
	Updated            int64   `json:"updated_at"`
	DeviceID           string  `json:"device_id"`
}
type reference struct {
	File        string      `json:"file"`
	Hash        string      `json:"hash"`
	ContentHash string      `json:"content_hash,omitempty"`
	Agent       model.Agent `json:"agent,omitempty"`
	Day         string      `json:"utc_day,omitempty"`
	Month       string      `json:"utc_month,omitempty"`
	Bucket      string      `json:"bucket,omitempty"`
	Part        int         `json:"part"`
	Compressed  int         `json:"compressed_size"`
	Raw         int         `json:"uncompressed_size"`
	Sessions    int         `json:"session_count,omitempty"`
	Turns       int         `json:"turn_count"`
}

// SessionRef and BlockRef deliberately have different canonical field sets.
func (r reference) MarshalJSON() ([]byte, error) {
	if strings.HasPrefix(r.File, "v3-session-") {
		return json.Marshal(struct {
			File        string `json:"file"`
			Hash        string `json:"hash"`
			ContentHash string `json:"content_hash"`
			Day         string `json:"utc_day"`
			Compressed  int    `json:"compressed_size"`
			Raw         int    `json:"uncompressed_size"`
			Sessions    int    `json:"session_count"`
		}{r.File, r.Hash, r.ContentHash, r.Day, r.Compressed, r.Raw, r.Sessions})
	}
	type plain reference
	return json.Marshal(plain(r))
}

type manifest struct {
	Protocol     int         `json:"protocol"`
	Schema       int         `json:"data_schema"`
	DeviceID     string      `json:"device_id"`
	DeviceName   string      `json:"device_name"`
	Created      int64       `json:"created_at"`
	Parent       *string     `json:"parent"`
	Retained     int64       `json:"retained_since"`
	Start        int64       `json:"turns_start"`
	End          int64       `json:"turns_end"`
	Blocks       []reference `json:"blocks"`
	Sessions     []reference `json:"sessions"`
	Shards       []reference `json:"shards,omitempty"`
	SessionCount int         `json:"session_count"`
	TurnCount    int         `json:"turn_count"`
	Content      string      `json:"content_hash,omitempty"`
	Source       string      `json:"source_hash"`
	Input        string      `json:"source_input_hash"`
}
type devicePackage struct {
	Schema     int             `json:"schema_version"`
	DeviceID   string          `json:"device_id"`
	DeviceName string          `json:"device_name"`
	Exported   int64           `json:"exported_at"`
	Sessions   []model.Session `json:"sessions"`
	Turns      []model.Turn    `json:"turns"`
	Start      int64           `json:"turns_start"`
	End        int64           `json:"turns_end"`
}

func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}
func day(ts int64) string { return time.Unix(ts, 0).UTC().Format("2006-01-02") }
func (t transport) load(ctx context.Context, id string, h head) (devicePackage, error) {
	attempt := func(digest string) (devicePackage, error) { return t.loadManifest(ctx, id, h.Protocol, digest) }
	digest := h.Manifest
	previous := h.Previous
	if h.Protocol == 2 {
		digest = h.Generation
		previous = h.PreviousGeneration
	}
	p, e := attempt(digest)
	if e != nil && previous != nil {
		return attempt(*previous)
	}
	return p, e
}
func (t transport) loadManifest(ctx context.Context, id string, protocol int, digest string) (devicePackage, error) {
	var p devicePackage
	if !validHash(digest) || (protocol != 2 && protocol != 3) {
		return p, errors.New("invalid head")
	}
	prefix := "v3-manifest-"
	if protocol == 2 {
		prefix = "v2-generation-"
	}
	b, e := t.get(ctx, prefix+digest+".json", 8<<20)
	if e != nil {
		return p, e
	}
	if hash(b) != digest {
		return p, errors.New("manifest hash mismatch")
	}
	var m manifest
	if e = json.Unmarshal(b, &m); e != nil {
		return p, e
	}
	if m.Protocol != protocol || m.Schema != 1 || m.DeviceID != id {
		return p, errors.New("manifest identity/schema mismatch")
	}
	if protocol == 2 && (!validHash(m.Content) || (m.Source != "" && !validHash(m.Source))) {
		return p, errors.New("invalid generation content hash")
	}
	p = devicePackage{Schema: 1, DeviceID: id, DeviceName: m.DeviceName, Start: m.Start, End: m.End, Exported: m.Created}
	refs := append(append([]reference{}, m.Blocks...), m.Sessions...)
	if protocol == 2 {
		refs = m.Shards
	}
	if len(refs) > 16384 || (protocol == 2 && len(refs) > 4096) {
		return p, errors.New("too many shards")
	}
	expectedSessions, expectedTurns := 0, 0
	for index, r := range refs {
		maxCompressed, maxRaw := 512<<10, 8<<20
		objectPrefix := "v3-block-"
		if index >= len(m.Blocks) {
			objectPrefix = "v3-session-"
		}
		suffix := ".zst"
		if protocol == 2 {
			maxCompressed = 8 << 20
			maxRaw = 64 << 20
			objectPrefix = "v2-object-"
			suffix = ".json.zst"
		}
		if !validHash(r.Hash) || r.File != objectPrefix+r.Hash+suffix || r.Compressed < 0 || r.Compressed > maxCompressed || r.Raw < 0 || r.Raw > maxRaw {
			return p, errors.New("invalid shard reference")
		}
		if r.Sessions < 0 || r.Turns < 0 || r.Part < 0 || r.Part > 65535 {
			return p, errors.New("negative record count")
		}
		if protocol == 3 && r.Day != "" && r.Day < day(time.Now().Unix()-366*86400) {
			continue
		}
		expectedSessions += r.Sessions
		expectedTurns += r.Turns
		z, e := t.get(ctx, r.File, maxCompressed)
		if e != nil {
			return p, e
		}
		if len(z) != r.Compressed || hash(z) != r.Hash {
			return p, errors.New("shard hash/size mismatch")
		}
		raw, e := decompress(z, r.Raw)
		if e != nil {
			return p, e
		}
		var block struct {
			Protocol int             `json:"protocol"`
			Schema   int             `json:"data_schema"`
			Sessions []model.Session `json:"sessions"`
			Turns    []model.Turn    `json:"turns"`
			Records  []struct {
				Kind  string          `json:"kind"`
				ID    string          `json:"record_id"`
				Value json.RawMessage `json:"value"`
			} `json:"records"`
		}
		if e = json.Unmarshal(raw, &block); e != nil {
			return p, e
		}
		if block.Protocol != protocol || block.Schema != 1 {
			return p, errors.New("shard schema mismatch")
		}
		if protocol == 2 {
			for _, record := range block.Records {
				switch record.Kind {
				case "session":
					var v model.Session
					if e = json.Unmarshal(record.Value, &v); e != nil {
						return p, e
					}
					if record.ID != sessionID(v) || v.Agent != r.Agent || !strings.HasPrefix(day(v.CreatedAt), r.Month) || !strings.HasPrefix(record.ID, r.Bucket) {
						return p, errors.New("invalid session partition")
					}
					block.Sessions = append(block.Sessions, v)
				case "turn":
					var v model.Turn
					if e = json.Unmarshal(record.Value, &v); e != nil {
						return p, e
					}
					if record.ID != turnID(v) || v.Agent != r.Agent || !strings.HasPrefix(day(v.CreatedAt), r.Month) || !strings.HasPrefix(record.ID, r.Bucket) {
						return p, errors.New("invalid turn partition")
					}
					block.Turns = append(block.Turns, v)
				default:
					return p, errors.New("unknown record kind")
				}
			}
		}
		if len(block.Sessions) != r.Sessions || len(block.Turns) != r.Turns {
			return p, errors.New("shard record count mismatch")
		}
		for _, v := range block.Turns {
			if v.DeviceID != id || (protocol == 3 && (v.Agent != r.Agent || day(v.CreatedAt) != r.Day)) {
				return p, errors.New("turn partition mismatch")
			}
		}
		for _, v := range block.Sessions {
			if v.DeviceID != id {
				return p, errors.New("session device mismatch")
			}
		}
		p.Sessions = append(p.Sessions, block.Sessions...)
		p.Turns = append(p.Turns, block.Turns...)
	}
	if len(p.Sessions) != expectedSessions || len(p.Turns) != expectedTurns || (protocol == 2 && (expectedSessions != m.SessionCount || expectedTurns != m.TurnCount)) {
		return p, errors.New("manifest record count mismatch")
	}
	return p, nil
}

var cycleMu sync.Mutex
var errConflict = errors.New("sync head CAS conflict")

// Cycle returns only other devices' records; it never writes remote data to local import files.
func Cycle(ctx context.Context, local model.Data) (model.Data, error) {
	cycleMu.Lock()
	defer cycleMu.Unlock()
	var remote model.Data
	c := ReadConfig()
	if !c.Enabled {
		return remote, nil
	}
	base, e := baseURL(c.APIURL)
	if e != nil {
		return remote, e
	}
	s, e := loadSession()
	if e != nil {
		return remote, e
	}
	if s.Expires <= time.Now().Unix() {
		return remote, errors.New("sync login expired")
	}
	id, e := device()
	if e != nil {
		return remote, e
	}
	t := transport{base, s.Token}
	b, _, status, e := t.request(ctx, "GET", "/v1/objects", nil, nil)
	if e != nil {
		return remote, e
	}
	if status != 200 {
		return remote, statusError(status)
	}
	var files []string
	if e = json.Unmarshal(b, &files); e != nil {
		return remote, e
	}
	chosen := map[string]string{}
	for _, prefix := range []string{"device-", "v2-head-", "v3-head-"} {
		for _, name := range files {
			if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".json") {
				d := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
				chosen[d] = name
			}
		}
	}
	var own *devicePackage
	var first error
	for d, name := range chosen {
		var p devicePackage
		raw, e := t.get(ctx, name, 8<<20)
		if e == nil {
			if strings.HasPrefix(name, "device-") {
				e = json.Unmarshal(raw, &p)
				if e == nil && (p.Schema != 1 || p.DeviceID != d) {
					e = errors.New("v1 identity/schema mismatch")
				}
			} else {
				var h head
				e = json.Unmarshal(raw, &h)
				if e == nil {
					if h.DeviceID != d || (strings.HasPrefix(name, "v3-head-") && h.Protocol != 3) || (strings.HasPrefix(name, "v2-head-") && h.Protocol != 2) {
						e = errors.New("head identity mismatch")
					} else {
						p, e = t.load(ctx, d, h)
					}
				}
			}
		}
		if e != nil {
			if first == nil {
				first = fmt.Errorf("device %s: %w", d, e)
			}
			continue
		}
		if d == id {
			own = &p
			continue
		}
		remote.Sessions = append(remote.Sessions, p.Sessions...)
		remote.Turns = append(remote.Turns, p.Turns...)
		if remote.TurnsStart == 0 || p.Start < remote.TurnsStart {
			remote.TurnsStart = p.Start
		}
		remote.TurnsEnd = max(remote.TurnsEnd, p.End)
	}
	if first != nil {
		return remote, first
	}
	for retry := 0; retry < 3; retry++ {
		e = t.publish(ctx, id, local, own)
		if !errors.Is(e, errConflict) {
			break
		}
	}
	if e != nil {
		return remote, e
	}
	sort.Slice(remote.Turns, func(i, j int) bool { return remote.Turns[i].CreatedAt < remote.Turns[j].CreatedAt })
	return remote, nil
}
func (t transport) put(ctx context.Context, name string, b []byte, h map[string]string) error {
	_, _, s, e := t.request(ctx, "PUT", "/v1/objects/"+url.PathEscape(name), b, h)
	if e != nil {
		return e
	}
	if s == 200 || s == 201 || s == 204 {
		return nil
	}
	if s == 409 || s == 412 {
		return errConflict
	}
	return statusError(s)
}
