package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"sort"
	"strings"
	stdatomic "sync/atomic"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

// auditInterval spaces full listings that confirm the parent manifest's
// objects still exist. Local caches can otherwise hide an object the server
// lost, and reuse would keep referencing it for every other device.
const auditInterval = 24 * time.Hour

// Protected by cycleMu.
var lastAudit = map[string]time.Time{}

// verifyManifest reports whether cached copies of this device's current
// manifest may be used. Other devices read it from the server, so a HEAD
// checks the stored copy's ETag each cycle without a download; a damaged or
// missing copy is restored from the verified cache. Servers without a usable
// HEAD answer fall back to a fresh download and hash check.
func (t transport) verifyManifest(ctx context.Context, digest string) bool {
	name := "v3-manifest-" + digest + ".json"
	_, h, s, e := t.request(ctx, "HEAD", "/v1/objects/"+url.PathEscape(name), nil, nil)
	if e != nil {
		return false
	}
	if s == 200 && h.Get("ETag") == `"`+digest+`"` {
		return true
	}
	cached := t.cachedManifest(name)
	if (s != 200 && s != 404) || (s == 200 && h.Get("ETag") == "") || cached == nil {
		return false
	}
	syncdiag.Log(ctx, "manifest_repair", "object", syncdiag.ID(name), "status", s)
	_, e = t.putImmutable(ctx, name, cached)
	return e == nil
}

// auditPrior drops references to missing objects from the parent manifest, so
// their groups are encoded and uploaded again instead of being reused.
func (t transport) auditPrior(ctx context.Context, prior *manifest) error {
	if time.Since(lastAudit[t.account()]) < auditInterval {
		return nil
	}
	names, e := t.list(ctx)
	if e != nil {
		return e
	}
	present := make(map[string]bool, len(names))
	for _, name := range names {
		present[name] = true
	}
	missing := 0
	keep := func(refs []reference) []reference {
		out := refs[:0:0]
		for _, r := range refs {
			if present[r.File] {
				out = append(out, r)
			} else {
				missing++
			}
		}
		return out
	}
	prior.Blocks, prior.Sessions = keep(prior.Blocks), keep(prior.Sessions)
	syncdiag.Log(ctx, "publish_audit", "objects", len(names), "missing", missing)
	lastAudit[t.account()] = time.Now()
	return nil
}

func (t transport) publish(ctx context.Context, id string, data model.Data, previous *devicePackage) (resultErr error) {
	ctx, finished := syncdiag.Stage(syncdiag.WithDevice(ctx, id), "publish")
	defer func() { finished(resultErr) }()
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
		load := t.loadManifestFresh
		if t.verifyManifest(ctx, old.Manifest) {
			load = t.loadManifest
		}
		p, e := load(ctx, id, 3, old.Manifest)
		if e != nil && old.Previous != nil {
			p, e = t.loadManifestFresh(ctx, id, 3, *old.Previous)
			parent = old.Previous
		}
		if e != nil {
			return e
		}
		previous = &p
		b, e := t.manifestBytes(ctx, "v3-manifest-"+*parent+".json", *parent, *parent == old.Manifest)
		if e != nil {
			return e
		}
		if e = json.Unmarshal(b, &prior); e != nil {
			return e
		}
		if e = t.auditPrior(ctx, &prior); e != nil {
			return e
		}
	} else if status != 404 {
		return statusError(status)
	}
	now := time.Now().Unix()
	retained := now - 366*86400
	if parent != nil && *parent == old.Manifest && lastExport.scope == t.scope() && lastExport.device == id && lastExport.head == old.Manifest && lastExport.input == input && retained < lastExport.expires && previous.Start <= max(data.TurnsStart, retained) && previous.End >= min(data.TurnsEnd, now) {
		syncdiag.Log(ctx, "publish_unchanged", "reason", "input_and_head")
		return nil
	}
	syncdiag.Log(ctx, "encode_start")
	encodeStarted := time.Now()
	turns := map[turnIdentity]model.Turn{}
	sessions := map[string]model.Session{}
	add := func(ss []model.Session, ts []model.Turn) {
		for _, v := range ts {
			if v.DeviceID != "" && v.DeviceID != id {
				continue
			}
			v.DeviceID = id
			if v.CreatedAt >= retained {
				turns[identityOf(v)] = v
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
	files := make([]string, 0, len(objects))
	for file := range objects {
		files = append(files, file)
	}
	syncdiag.Log(ctx, "encode_end", "elapsed_ms", time.Since(encodeStarted).Milliseconds(), "references", len(m.Blocks)+len(m.Sessions), "encoded_objects", len(files), "reused_references", len(m.Blocks)+len(m.Sessions)-len(files))
	// Objects the parent manifest references are on the server: it roots them
	// against GC, and auditPrior dropped any that went missing.
	held := map[string]bool{}
	for _, r := range append(append([]reference{}, prior.Blocks...), prior.Sessions...) {
		held[r.File] = true
	}
	var uploaded, existing stdatomic.Int64
	var pending []string
	for _, file := range files {
		if held[file] {
			existing.Add(1)
			t.rememberBlock(file, objects[file])
		} else {
			pending = append(pending, file)
		}
	}
	uploadCtx, uploadedDone := syncdiag.Stage(ctx, "upload_blocks")
	if e = parallelTransfers(uploadCtx, len(pending), func(ctx context.Context, index int) error {
		file := pending[index]
		z := objects[file]
		sent, err := t.putImmutable(ctx, file, z)
		if err != nil {
			return err
		}
		if sent {
			uploaded.Add(1)
		} else {
			existing.Add(1)
		}
		t.rememberBlock(file, z)
		return nil
	}); e != nil {
		syncdiag.Log(uploadCtx, "upload_result", "uploaded", uploaded.Load(), "existing", existing.Load())
		uploadedDone(e)
		return e
	}
	syncdiag.Log(uploadCtx, "upload_result", "uploaded", uploaded.Load(), "existing", existing.Load())
	uploadedDone(nil)
	if parent != nil && *parent == old.Manifest && prior.Source == m.Source {
		syncdiag.Log(ctx, "publish_unchanged", "reason", "source_hash")
		remember(old.Manifest)
		return nil
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
	syncdiag.Log(ctx, "manifest_publish_start")
	if _, e = t.putImmutable(ctx, "v3-manifest-"+digest+".json", b); e != nil {
		return e
	}
	t.rememberManifest("v3-manifest-"+digest+".json", b)
	newHead := head{Protocol: 3, Sequence: old.Sequence + 1, Manifest: digest, Previous: parent, Updated: now, DeviceID: id}
	b, e = json.Marshal(newHead)
	if e != nil {
		return e
	}
	syncdiag.Log(ctx, "head_cas_start", "sequence", newHead.Sequence)
	if e = t.put(ctx, name, b, condition); e != nil {
		return e
	}
	syncdiag.Log(ctx, "head_cas_committed", "sequence", newHead.Sequence)
	remember(digest)
	// The next cycle re-reads this head under its CAS base. The package is the
	// data just written, so keep it under the key a read of this manifest
	// uses instead of decoding every block again.
	published := devicePackage{Schema: 1, DeviceID: id, DeviceName: host, Start: start, End: end, Exported: now}
	cutoff := day(time.Now().Unix() - 366*86400)
	for _, v := range sessions {
		// A read skips session shards dated before the cutoff, even when a
		// recent turn kept the session in this publication.
		if day(max(v.CreatedAt, v.LastActivityAt)) >= cutoff {
			published.Sessions = append(published.Sessions, v)
		}
	}
	for _, v := range turns {
		published.Turns = append(published.Turns, v)
	}
	rememberPackage(t.packageKey(id, "v3-manifest-"+digest, cutoff), published)
	return nil
}
