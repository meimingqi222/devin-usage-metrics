package syncclient

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	stdatomic "sync/atomic"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
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
		syncdiag.Log(ctx, "previous_manifest_fallback", "error", syncdiag.ErrorCode(e))
		return t.loadManifestFresh(ctx, id, h.Protocol, *previous)
	}
	return p, e
}
func (t transport) loadManifest(ctx context.Context, id string, protocol int, digest string) (devicePackage, error) {
	return t.readManifest(ctx, id, protocol, digest, true)
}
func (t transport) loadManifestFresh(ctx context.Context, id string, protocol int, digest string) (devicePackage, error) {
	return t.readManifest(ctx, id, protocol, digest, false)
}
func (t transport) readManifest(ctx context.Context, id string, protocol int, digest string, useCache bool) (result devicePackage, resultErr error) {
	ctx, finished := syncdiag.Stage(syncdiag.WithDevice(ctx, id), "load_manifest")
	defer func() { finished(resultErr) }()
	var p devicePackage
	if !validHash(digest) || (protocol != 2 && protocol != 3) {
		return p, errors.New("invalid head")
	}
	prefix := "v3-manifest-"
	if protocol == 2 {
		prefix = "v2-generation-"
	}
	// The head names the manifest by its hash, so an unchanged head needs no
	// download: the package assembled from that exact manifest is still valid.
	cutoff := day(time.Now().Unix() - 366*86400)
	cacheKey := t.packageKey(id, prefix+digest, cutoff)
	if useCache {
		if cached, ok := cachedPackage(cacheKey); ok {
			syncdiag.Log(ctx, "package_cache_hit", "protocol", protocol, "sessions", len(cached.Sessions), "turns", len(cached.Turns))
			return cached, nil
		}
	}
	b, e := t.manifestBytes(ctx, prefix+digest+".json", digest, useCache)
	if e != nil {
		return p, e
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
	for _, r := range refs {
		if protocol == 3 && r.Day != "" && r.Day < cutoff {
			continue
		}
		expectedSessions += r.Sessions
		expectedTurns += r.Turns
	}
	syncdiag.Log(ctx, "manifest_loaded", "protocol", protocol, "references", len(refs), "cache_allowed", useCache)
	ctx, downloaded := syncdiag.Stage(ctx, "download_blocks")
	stats := new(downloadStats)
	defer func() {
		syncdiag.Log(ctx, "download_summary", "downloaded", stats.downloaded.Load(), "cache_hits", stats.cacheHits.Load(), "skipped", stats.skipped.Load(), "bytes", stats.bytes.Load())
	}()
	parts := make([]devicePackage, len(refs))
	if e = parallelTransfers(ctx, len(refs), func(ctx context.Context, index int) error {
		var err error
		parts[index], err = t.readReference(ctx, id, protocol, refs[index], index >= len(m.Blocks), useCache, cutoff, stats)
		return err
	}); e != nil {
		downloaded(e)
		return p, e
	}
	downloaded(nil)
	for _, part := range parts {
		p.Sessions = append(p.Sessions, part.Sessions...)
		p.Turns = append(p.Turns, part.Turns...)
	}
	if len(p.Sessions) != expectedSessions || len(p.Turns) != expectedTurns || (protocol == 2 && (expectedSessions != m.SessionCount || expectedTurns != m.TurnCount)) {
		return p, errors.New("manifest record count mismatch")
	}
	if useCache {
		rememberPackage(cacheKey, p)
	}
	// Parts share records with the block cache; hand the caller its own copy.
	return clonePackage(p), nil
}

// packageKey names a device package assembled from one manifest. The cutoff
// day is part of it because references older than it are skipped.
func (t transport) packageKey(id, manifest, cutoff string) string {
	return t.account() + "/" + id + "/" + manifest + "/" + cutoff
}

// manifestBytes returns an immutable manifest, verified against its name.
func (t transport) manifestBytes(ctx context.Context, name, digest string, useCache bool) ([]byte, error) {
	if useCache {
		if b := t.cachedManifest(name); b != nil {
			return b, nil
		}
	}
	b, e := t.get(ctx, name, 8<<20)
	if e != nil {
		return nil, e
	}
	if hash(b) != digest {
		return nil, errors.New("manifest hash mismatch")
	}
	t.rememberManifest(name, b)
	return b, nil
}

type downloadStats struct{ downloaded, cacheHits, skipped, bytes stdatomic.Int64 }

func (t transport) readReference(ctx context.Context, id string, protocol int, r reference, sessionBlock, useCache bool, cutoff string, stats *downloadStats) (result devicePackage, resultErr error) {
	defer func() {
		if resultErr != nil {
			syncdiag.Log(ctx, "block_read_failed", "object", syncdiag.ID(r.File), "error", syncdiag.ErrorCode(resultErr))
		}
	}()
	var blockData devicePackage
	maxCompressed, maxRaw := 512<<10, 8<<20
	objectPrefix := "v3-block-"
	if sessionBlock {
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
		return blockData, errors.New("invalid shard reference")
	}
	if r.Sessions < 0 || r.Turns < 0 || r.Part < 0 || r.Part > 65535 {
		return blockData, errors.New("negative record count")
	}
	if protocol == 3 && r.Day != "" && r.Day < cutoff {
		stats.skipped.Add(1)
		return blockData, nil
	}
	// v2 validates partitions while decoding, against this reference's claims,
	// so only v3 blocks share decoded records.
	shareDecoded := useCache && protocol == 3
	block, cached := decodedBlock{}, false
	if shareDecoded {
		block, cached = t.cachedDecoded(r.File)
	}
	if cached {
		stats.cacheHits.Add(1)
	} else {
		decoded, rawSize, e := t.decodeReference(ctx, protocol, r, useCache, maxCompressed, stats)
		if e != nil {
			return blockData, e
		}
		block = decoded
		if shareDecoded {
			t.rememberDecoded(r.File, block, rawSize)
		}
	}
	// Validate every use: references to the same object may claim differently.
	if len(block.Sessions) != r.Sessions || len(block.Turns) != r.Turns {
		return blockData, errors.New("shard record count mismatch")
	}
	for _, v := range block.Turns {
		if v.DeviceID != id || (protocol == 3 && (v.Agent != r.Agent || day(v.CreatedAt) != r.Day)) {
			return blockData, errors.New("turn partition mismatch")
		}
	}
	for _, v := range block.Sessions {
		if v.DeviceID != id {
			return blockData, errors.New("session device mismatch")
		}
	}
	blockData.Sessions, blockData.Turns = block.Sessions, block.Turns
	return blockData, nil
}

// decodeReference reads a block from the disk cache or the server and decodes
// it. A cached copy that fails its hash is discarded and downloaded again.
func (t transport) decodeReference(ctx context.Context, protocol int, r reference, useCache bool, maxCompressed int, stats *downloadStats) (decodedBlock, int, error) {
	var out decodedBlock
	var z []byte
	if useCache {
		if z = t.cachedBlock(r.File); z != nil && (len(z) != r.Compressed || hash(z) != r.Hash) {
			syncdiag.Log(ctx, "cached_block_invalid", "object", syncdiag.ID(r.File))
			t.forgetBlock(r.File)
			z = nil
		}
	}
	if z == nil {
		var e error
		if z, e = t.get(ctx, r.File, maxCompressed); e != nil {
			return out, 0, e
		}
		stats.downloaded.Add(1)
		stats.bytes.Add(int64(len(z)))
		if len(z) != r.Compressed || hash(z) != r.Hash {
			syncdiag.Log(ctx, "block_validation_failed", "object", syncdiag.ID(r.File), "reason", "hash_or_size")
			return out, 0, errors.New("shard hash/size mismatch")
		}
		if useCache {
			t.rememberBlock(r.File, z)
		}
	} else {
		stats.cacheHits.Add(1)
	}
	raw, e := decompress(z, r.Raw)
	if e != nil {
		return out, 0, e
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
		return out, 0, e
	}
	if block.Protocol != protocol || block.Schema != 1 {
		return out, 0, errors.New("shard schema mismatch")
	}
	if protocol == 2 {
		for _, record := range block.Records {
			switch record.Kind {
			case "session":
				var v model.Session
				if e = json.Unmarshal(record.Value, &v); e != nil {
					return out, 0, e
				}
				if record.ID != sessionID(v) || v.Agent != r.Agent || !strings.HasPrefix(day(v.CreatedAt), r.Month) || !strings.HasPrefix(record.ID, r.Bucket) {
					return out, 0, errors.New("invalid session partition")
				}
				block.Sessions = append(block.Sessions, v)
			case "turn":
				var v model.Turn
				if e = json.Unmarshal(record.Value, &v); e != nil {
					return out, 0, e
				}
				if record.ID != turnID(v) || v.Agent != r.Agent || !strings.HasPrefix(day(v.CreatedAt), r.Month) || !strings.HasPrefix(record.ID, r.Bucket) {
					return out, 0, errors.New("invalid turn partition")
				}
				block.Turns = append(block.Turns, v)
			default:
				return out, 0, errors.New("unknown record kind")
			}
		}
	}
	out.Sessions, out.Turns = block.Sessions, block.Turns
	return out, len(raw), nil
}

func (t transport) readDevice(ctx context.Context, d, name string) (devicePackage, error) {
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
	return p, e
}

var cycleMu sync.Mutex
var errConflict = errors.New("sync head CAS conflict")

// Cycle returns only other devices' records; it never writes remote data to local import files.
func Cycle(ctx context.Context, local model.Data) (result model.Data, resultErr error) {
	ctx = syncdiag.WithRun(ctx)
	ctx, finished := syncdiag.Stage(ctx, "cycle")
	defer func() { finished(resultErr) }()
	syncdiag.Log(ctx, "cycle_lock_wait")
	cycleMu.Lock()
	syncdiag.Log(ctx, "cycle_lock_acquired")
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
	listCtx, listed := syncdiag.Stage(ctx, "list_objects")
	files, e := t.list(listCtx, "device-", "v2-head-", "v3-head-")
	listed(e)
	if e != nil {
		return remote, e
	}
	chosen := map[string][]string{}
	for _, prefix := range []string{"device-", "v2-head-", "v3-head-"} {
		for _, name := range files {
			if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".json") {
				d := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")
				chosen[d] = append([]string{name}, chosen[d]...)
			}
		}
	}
	syncdiag.Log(ctx, "devices_discovered", "objects", len(files), "devices", len(chosen))
	// Publication re-reads this device's v3 head under its CAS base, so only a
	// device still on v1/v2 needs its own history imported here.
	if names := chosen[id]; len(names) > 0 && strings.HasPrefix(names[0], "v3-head-") {
		delete(chosen, id)
	}
	devices := make([]string, 0, len(chosen))
	for d := range chosen {
		devices = append(devices, d)
	}
	sort.Strings(devices)
	packages := make([]devicePackage, len(devices))
	failures := make([]error, len(devices))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 3)
	for i, d := range devices {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			packages[i], failures[i] = t.importDevice(ctx, d, chosen[d], d == id)
		})
	}
	wg.Wait()
	var own *devicePackage
	var problems []error
	for i, d := range devices {
		p, err := packages[i], failures[i]
		key := t.account() + "/" + d
		if d == id {
			if err != nil {
				// Publishing without this device's legacy history would drop it.
				return remote, fmt.Errorf("device %s: %w", d, err)
			}
			own = &p
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("device %s: %w", d, err))
			good, ok := lastGood[key]
			if !ok {
				continue
			}
			syncdiag.Log(syncdiag.WithDevice(ctx, d), "device_last_good_used")
			p = clonePackage(good)
		} else {
			lastGood[key] = clonePackage(p)
		}
		remote.Sessions = append(remote.Sessions, p.Sessions...)
		remote.Turns = append(remote.Turns, p.Turns...)
		if remote.TurnsStart == 0 || p.Start < remote.TurnsStart {
			remote.TurnsStart = p.Start
		}
		remote.TurnsEnd = max(remote.TurnsEnd, p.End)
	}
	sort.Slice(remote.Turns, func(i, j int) bool { return remote.Turns[i].CreatedAt < remote.Turns[j].CreatedAt })
	// Like Rust, another device's failure never blocks publishing this one.
	for retry := 0; retry < 3; retry++ {
		syncdiag.Log(ctx, "publish_attempt", "attempt", retry+1)
		e = t.publish(ctx, id, local, own)
		if !errors.Is(e, errConflict) {
			break
		}
	}
	if e != nil {
		problems = append(problems, e)
	}
	if len(problems) > 0 {
		return remote, fmt.Errorf("%w: %w", ErrPartial, errors.Join(problems...))
	}
	return remote, nil
}

// ErrPartial reports that some devices failed to import or publication
// failed, while the returned remote data is still the best available: each
// failed device keeps its last good package from this process.
var ErrPartial = errors.New("sync incomplete")

func (t transport) importDevice(ctx context.Context, d string, names []string, local bool) (p devicePackage, e error) {
	deviceCtx, deviceDone := syncdiag.Stage(syncdiag.WithDevice(ctx, d), "import_device")
	syncdiag.Log(deviceCtx, "device_import_start", "local", local, "candidates", len(names))
	for attempt, name := range names {
		p, e = t.readDevice(deviceCtx, d, name)
		if e != nil {
			syncdiag.Log(deviceCtx, "candidate_failed", "candidate", attempt+1, "object", syncdiag.ID(name), "error", syncdiag.ErrorCode(e))
		}
		if e == nil || ctx.Err() != nil {
			break
		}
	}
	syncdiag.Log(deviceCtx, "device_import_result", "sessions", len(p.Sessions), "turns", len(p.Turns), "error", syncdiag.ErrorCode(e))
	deviceDone(e)
	return p, e
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
