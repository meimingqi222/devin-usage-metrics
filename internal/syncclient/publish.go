package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

func (t transport) publish(ctx context.Context, id string, data model.Data, previous *devicePackage) error {
	input, e := exportFingerprint(data)
	if e != nil {
		return e
	}
	name := "v3-head-" + id + ".json"
	raw, h, status, e := t.request(ctx, "GET", "/v1/objects/"+url.PathEscape(name), nil, nil)
	if e != nil {
		return e
	}
	var old head
	var prior manifest
	var parent *string
	condition := map[string]string{"If-None-Match": "*"}
	if status == 200 {
		if e = json.Unmarshal(raw, &old); e != nil {
			return e
		}
		if old.Protocol != 3 || old.DeviceID != id || !validHash(old.Manifest) {
			return errors.New("invalid existing head")
		}
		revision := h.Get("ETag")
		if revision == "" || strings.HasPrefix(strings.TrimSpace(revision), "W/") {
			return errors.New("strong head ETag required for safe publication")
		}
		condition = map[string]string{"If-Match": revision}
		parent = &old.Manifest
		// Re-read under the CAS base, so a concurrent publish cannot lose history.
		p, e := t.loadManifest(ctx, id, 3, old.Manifest)
		if e != nil && old.Previous != nil {
			p, e = t.loadManifestFresh(ctx, id, 3, *old.Previous)
			parent = old.Previous
		}
		if e != nil {
			return e
		}
		previous = &p
		b, e := t.get(ctx, "v3-manifest-"+*parent+".json", 8<<20)
		if e != nil {
			return e
		}
		if hash(b) != *parent {
			return errors.New("manifest hash mismatch")
		}
		if e = json.Unmarshal(b, &prior); e != nil {
			return e
		}
	} else if status != 404 {
		return statusError(status)
	}
	now := time.Now().Unix()
	retained := now - 366*86400
	if parent != nil && *parent == old.Manifest && lastExport.scope == t.scope() && lastExport.device == id && lastExport.head == old.Manifest && lastExport.input == input && retained < lastExport.expires && previous.Start <= max(data.TurnsStart, retained) && previous.End >= min(data.TurnsEnd, now) {
		return nil
	}
	turns := map[string]model.Turn{}
	sessions := map[string]model.Session{}
	add := func(ss []model.Session, ts []model.Turn) {
		for _, v := range ts {
			if v.DeviceID != "" && v.DeviceID != id {
				continue
			}
			v.DeviceID = id
			if v.CreatedAt >= retained {
				turns[turnID(v)] = v
			}
		}
		for _, v := range ss {
			if v.DeviceID != "" && v.DeviceID != id {
				continue
			}
			v.DeviceID = id
			sessions[sessionID(v)] = v
		}
	}
	if previous != nil {
		add(previous.Sessions, previous.Turns)
	}
	add(data.Sessions, data.Turns)
	keys := map[string]bool{}
	for _, v := range turns {
		keys[v.SessionKey] = true
	}
	for k, v := range sessions {
		if max(v.CreatedAt, v.LastActivityAt) < retained && !keys[v.Key] {
			delete(sessions, k)
		}
	}
	if parent == nil && len(sessions)+len(turns) == 0 {
		return nil
	}
	// Retention can change the export even when the input has not changed.
	expires := int64(1<<63 - 1)
	for _, v := range turns {
		expires = min(expires, v.CreatedAt)
	}
	for _, v := range sessions {
		expires = min(expires, max(v.CreatedAt, v.LastActivityAt))
	}
	remember := func(digest string) {
		lastExport.scope, lastExport.device = t.scope(), id
		lastExport.head, lastExport.input, lastExport.expires = digest, input, expires
	}
	groups := map[string][]model.Turn{}
	sgroups := map[string][]model.Session{}
	for _, v := range turns {
		k := string(v.Agent) + "/" + day(v.CreatedAt)
		groups[k] = append(groups[k], v)
	}
	for _, v := range sessions {
		k := string(v.Agent) + "/" + day(max(v.CreatedAt, v.LastActivityAt))
		sgroups[k] = append(sgroups[k], v)
	}
	host, _ := os.Hostname()
	if previous != nil && previous.DeviceName != "" {
		host = previous.DeviceName
	}
	start := max(data.TurnsStart, retained)
	end := data.TurnsEnd
	if previous != nil {
		start = max(retained, min(start, previous.Start))
		end = max(end, previous.End)
	}
	m := manifest{Protocol: 3, Schema: 1, DeviceID: id, DeviceName: host, Created: now, Parent: parent, Retained: retained, Start: start, End: end, Blocks: []reference{}, Sessions: []reference{}, SessionCount: len(sessions), TurnCount: len(turns)}
	objects := map[string][]byte{}
	var encodeTurns func([]model.Turn, string, int) error
	encodeTurns = func(values []model.Turn, fingerprint string, part int) error {
		b, e := json.Marshal(struct {
			Protocol int          `json:"protocol"`
			Schema   int          `json:"data_schema"`
			Turns    []model.Turn `json:"turns"`
		}{3, 1, values})
		if e != nil {
			return e
		}
		z, e := compress(b)
		if e != nil {
			return e
		}
		if len(z) > 512<<10 || len(b) > 8<<20 {
			if len(values) < 2 {
				return errors.New("single turn exceeds protocol block limit")
			}
			mid := len(values) / 2
			if e = encodeTurns(values[:mid], fingerprint, part); e != nil {
				return e
			}
			return encodeTurns(values[mid:], fingerprint, part+mid)
		}
		digest := hash(z)
		r := reference{File: "v3-block-" + digest + ".zst", Hash: digest, ContentHash: fingerprint, Agent: values[0].Agent, Day: day(values[0].CreatedAt), Part: part, Compressed: len(z), Raw: len(b), Turns: len(values)}
		m.Blocks = append(m.Blocks, r)
		objects[r.File] = z
		return nil
	}
	var encodeSessions func([]model.Session, string) error
	encodeSessions = func(values []model.Session, fingerprint string) error {
		b, e := json.Marshal(struct {
			Protocol int             `json:"protocol"`
			Schema   int             `json:"data_schema"`
			Sessions []model.Session `json:"sessions"`
		}{3, 1, values})
		if e != nil {
			return e
		}
		z, e := compress(b)
		if e != nil {
			return e
		}
		if len(z) > 512<<10 || len(b) > 8<<20 {
			if len(values) < 2 {
				return errors.New("single session exceeds protocol block limit")
			}
			mid := len(values) / 2
			if e = encodeSessions(values[:mid], fingerprint); e != nil {
				return e
			}
			return encodeSessions(values[mid:], fingerprint)
		}
		digest := hash(z)
		r := reference{File: "v3-session-" + digest + ".zst", Hash: digest, ContentHash: fingerprint, Day: day(max(values[0].CreatedAt, values[0].LastActivityAt)), Compressed: len(z), Raw: len(b), Sessions: len(values)}
		m.Sessions = append(m.Sessions, r)
		objects[r.File] = z
		return nil
	}
	for _, v := range groups {
		sort.Slice(v, func(i, j int) bool {
			if v[i].CreatedAt != v[j].CreatedAt {
				return v[i].CreatedAt < v[j].CreatedAt
			}
			if v[i].SessionKey != v[j].SessionKey {
				return v[i].SessionKey < v[j].SessionKey
			}
			if v[i].Model != v[j].Model {
				return v[i].Model < v[j].Model
			}
			return turnID(v[i]) < turnID(v[j])
		})
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		reused := []reference{}
		count := 0
		for _, r := range prior.Blocks {
			if r.Agent == v[0].Agent && r.Day == day(v[0].CreatedAt) && r.ContentHash == hash(b) {
				reused = append(reused, r)
				count += r.Turns
			}
		}
		if count == len(v) {
			m.Blocks = append(m.Blocks, reused...)
			continue
		}
		first := len(m.Blocks)
		if e = encodeTurns(v, hash(b), 0); e != nil {
			return e
		}
		for index := first; index < len(m.Blocks); index++ {
			m.Blocks[index].Part = index - first
		}
	}
	for _, v := range sgroups {
		sort.Slice(v, func(i, j int) bool { return v3SessionID(v[i]) < v3SessionID(v[j]) })
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		reused := []reference{}
		count := 0
		for _, r := range prior.Sessions {
			if r.Day == day(max(v[0].CreatedAt, v[0].LastActivityAt)) && r.ContentHash == hash(b) {
				reused = append(reused, r)
				count += r.Sessions
			}
		}
		if count == len(v) {
			m.Sessions = append(m.Sessions, reused...)
			continue
		}
		if e = encodeSessions(v, hash(b)); e != nil {
			return e
		}
	}
	sort.Slice(m.Blocks, func(i, j int) bool { return m.Blocks[i].File < m.Blocks[j].File })
	sort.Slice(m.Sessions, func(i, j int) bool { return m.Sessions[i].File < m.Sessions[j].File })
	source, e := json.Marshal([]any{id, host, start, end, m.Blocks, m.Sessions})
	if e != nil {
		return e
	}
	m.Source = hash(source)
	// Existing content-addressed objects are verified, not blindly overwritten.
	for file, z := range objects {
		b, _, s, e := t.request(ctx, "GET", "/v1/objects/"+url.PathEscape(file), nil, nil)
		if e != nil {
			return e
		}
		if s == 200 && hash(b) == hash(z) {
			t.rememberBlock(file, z)
			continue
		}
		if s != 200 && s != 404 {
			return statusError(s)
		}
		if e = t.put(ctx, file, z, nil); e != nil {
			return e
		}
		t.rememberBlock(file, z)
	}
	if parent != nil {
		b, e := t.get(ctx, "v3-manifest-"+*parent+".json", 8<<20)
		if e != nil {
			return e
		}
		if hash(b) != *parent {
			return errors.New("manifest hash mismatch")
		}
		var prior manifest
		if json.Unmarshal(b, &prior) == nil && prior.Source == m.Source && *parent == old.Manifest {
			remember(old.Manifest)
			return nil
		}
	}
	if len(m.Blocks)+len(m.Sessions) > 16384 {
		return errors.New("too many blocks")
	}
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	if len(b) > 8<<20 {
		return errors.New("manifest exceeds limit")
	}
	digest := hash(b)
	if e = t.put(ctx, "v3-manifest-"+digest+".json", b, map[string]string{"If-None-Match": "*"}); e != nil {
		if !errors.Is(e, errConflict) {
			return e
		}
		existing, err := t.get(ctx, "v3-manifest-"+digest+".json", 8<<20)
		if err != nil {
			return err
		}
		if hash(existing) != digest {
			return errors.New("existing immutable manifest is corrupt")
		}
	}
	newHead := head{Protocol: 3, Sequence: old.Sequence + 1, Manifest: digest, Previous: parent, Updated: now, DeviceID: id}
	b, e = json.Marshal(newHead)
	if e != nil {
		return e
	}
	if e = t.put(ctx, name, b, condition); e != nil {
		return e
	}
	remember(digest)
	return nil
}
