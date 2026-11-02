package data

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

type obj = map[string]any

func get(v any, keys ...string) any {
	for _, k := range keys {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}
func str(v any) string { s, _ := v.(string); return s }
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int64:
		return float64(x)
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return n
	}
	return 0
}
func ts(v any) int64 {
	if s, ok := v.(string); ok {
		t, e := time.Parse(time.RFC3339Nano, s)
		if e == nil {
			return t.Unix()
		}
	}
	n := int64(num(v))
	if n > 10000000000 {
		n /= 1000
	}
	return n
}
func first(v ...any) any {
	for _, x := range v {
		if x != nil {
			return x
		}
	}
	return nil
}
func text(v any) string {
	if s, ok := v.(string); ok {
		return compact(s)
	}
	a, _ := v.([]any)
	for _, x := range a {
		if s := str(get(x, "text")); strings.TrimSpace(s) != "" {
			return compact(s)
		}
	}
	return ""
}
func compact(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 100 {
		return string(r[:100])
	}
	return s
}
func prefix(a model.Agent) string { return strings.ReplaceAll(strings.ToLower(a.Label()), " ", "-") }
func session(a model.Agent, id string) *model.Session {
	return &model.Session{Agent: a, ID: id, Key: prefix(a) + "/" + id, Source: "local"}
}
func observe(s *model.Session, at int64) {
	if at <= 0 {
		return
	}
	if s.CreatedAt == 0 || at < s.CreatedAt {
		s.CreatedAt = at
	}
	s.LastActivityAt = max(s.LastActivityAt, at)
}
func add(s *model.Session, t model.Turn) {
	s.InputTokens += t.InputTokens
	s.OutputTokens += t.OutputTokens
	s.CachedTokens += t.CacheReadTokens
	s.CacheCreationTokens += t.CacheCreationTokens
	s.CacheCreation5mTokens += t.CacheCreation5mTokens
	s.CacheCreation1hTokens += t.CacheCreation1hTokens
	s.AgentMessages++
	if t.RecordedCost != nil {
		v := *t.RecordedCost
		if s.RecordedCost != nil {
			v += *s.RecordedCost
		}
		s.RecordedCost = &v
	}
}
func files(d *model.Data, a model.Agent, roots []string, ext string) []string {
	seen := map[string]bool{}
	var paths []string
	found := false
	for _, root := range roots {
		if _, e := os.Stat(root); os.IsNotExist(e) {
			continue
		}
		found = true
		e := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err != nil {
				fail(d, a, err)
				return nil
			}
			if !e.IsDir() && strings.HasSuffix(p, ext) && !seen[p] {
				paths = append(paths, p)
				seen[p] = true
			}
			return nil
		})
		if e != nil {
			fail(d, a, e)
		}
	}
	if !found {
		fail(d, a, "data source not found: "+strings.Join(roots, ", "))
	}
	forgetRemoved(roots, ext, seen)
	// Stat once per file; the comparator runs O(n log n) times.
	mtimes := make(map[string]int64, len(paths))
	for _, p := range paths {
		if info, e := os.Stat(p); e == nil {
			mtimes[p] = info.ModTime().UnixNano()
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		a, okA := mtimes[paths[i]]
		b, okB := mtimes[paths[j]]
		if okA && okB && a != b {
			return a > b
		}
		return paths[i] < paths[j]
	})
	if len(paths) > 20000 {
		fail(d, a, "session file limit exceeded (20000)")
		paths = paths[:20000]
	}
	return paths
}
func roots(a model.Agent) []string {
	switch a {
	case model.Claude:
		return []string{home(".claude/projects")}
	case model.Codex:
		b := codexHome()
		return []string{filepath.Join(b, "sessions"), filepath.Join(b, "archived_sessions")}
	case model.Pi:
		return []string{home(".pi/agent/sessions")}
	case model.Omp:
		return []string{home(".omp/agent/sessions")}
	case model.Grok:
		return []string{home(".grok/sessions")}
	case model.WorkBuddy:
		var r []string
		for _, k := range []string{"WORKBUDDY_CONFIG_DIR", "CODEBUDDY_CONFIG_DIR", "WORKBUDDY_DIR"} {
			if v := os.Getenv(k); v != "" {
				r = append(r, filepath.Join(v, "projects"))
			}
		}
		return append(r, home(".workbuddy/projects"), home(".workbuddy-ai/projects"), home(".codebuddy/projects"))
	}
	return nil
}

type lineCache struct {
	Mtime  int64
	Size   int64
	Lines  []obj
	Errors []error
	// Weight estimates the decoded lines' memory for the cache budget.
	Weight int64
	// Digest verifies the old prefix before decoding only appended bytes.
	Digest          []byte
	EndsWithNewline bool
	access          uint64
}

// appendHash verifies the entire old prefix before reusing decoded records.
// A matching tail alone cannot detect a rewrite near the start of the file.
func appendHash(f *os.File, c lineCache, size int64) hash.Hash {
	if c.Size <= 0 || size <= c.Size || len(c.Errors) != 0 || !c.EndsWithNewline {
		return nil
	}
	h := sha256.New()
	if n, err := io.CopyBuffer(h, io.NewSectionReader(f, 0, c.Size), make([]byte, 1<<20)); err != nil || n != c.Size || !bytes.Equal(h.Sum(nil), c.Digest) {
		return nil
	}
	return h
}

var decodedFiles sync.Map

// decodedBudget caps the decoded-transcript cache. Beyond it the files
// used longest ago are dropped and decoded again on the next reload:
// a large history otherwise kept over a gigabyte resident in the tray app.
// Reloads re-decode only evicted files on four workers, so a tight budget
// costs a little CPU on reload, never correctness.
const decodedBudget = 32 << 20

var decodedWeight struct {
	sync.Mutex
	total int64
	clock uint64
}

func rememberDecoded(p string, c lineCache) (evicted int) {
	decodedWeight.Lock()
	defer decodedWeight.Unlock()
	// One oversized source must not evict every useful cached source, then
	// remain over budget itself. Its prefetched result still reaches accounting.
	if c.Weight > decodedBudget {
		if old, ok := decodedFiles.LoadAndDelete(p); ok {
			decodedWeight.total -= old.(lineCache).Weight
		}
		return
	}
	decodedWeight.clock++
	c.access = decodedWeight.clock
	if old, ok := decodedFiles.Swap(p, c); ok {
		decodedWeight.total -= old.(lineCache).Weight
	}
	decodedWeight.total += c.Weight
	if decodedWeight.total <= decodedBudget {
		return
	}
	type entry struct {
		path   string
		access uint64
		bytes  int64
	}
	var all []entry
	decodedFiles.Range(func(key, value any) bool {
		if key != p {
			v := value.(lineCache)
			all = append(all, entry{key.(string), v.access, v.Weight})
		}
		return true
	})
	sort.Slice(all, func(i, j int) bool { return all[i].access < all[j].access })
	for _, e := range all {
		if decodedWeight.total <= decodedBudget {
			break
		}
		decodedFiles.Delete(e.path)
		decodedWeight.total -= e.bytes
		evicted++
	}
	return
}

// cachedDecoded updates recency on reads too: old history can be hot when the
// user revisits an agent after another agent's background collection.
func cachedDecoded(p string) (lineCache, bool) {
	decodedWeight.Lock()
	defer decodedWeight.Unlock()
	value, ok := decodedFiles.Load(p)
	if !ok {
		return lineCache{}, false
	}
	c := value.(lineCache)
	decodedWeight.clock++
	c.access = decodedWeight.clock
	decodedFiles.Store(p, c)
	return c, true
}

func forgetDecoded(p string) {
	decodedWeight.Lock()
	defer decodedWeight.Unlock()
	if old, ok := decodedFiles.LoadAndDelete(p); ok {
		decodedWeight.total -= old.(lineCache).Weight
	}
}

// slimThreshold bounds strings kept in decoded transcripts. Message bodies
// and tool output made the cache over a gigabyte, while parsers read only
// identifiers, numbers, roles, short metadata and titles that compact()
// limits to 100 runes.
const slimThreshold = 1024

// slim shortens long strings in a decoded line in place, keeping a prefix
// that leaves every parsed value unchanged (see titlePrefix). It returns the
// slimmed value and a rough estimate of its memory.
func slim(v any) (any, int64) {
	switch x := v.(type) {
	case map[string]any:
		weight := int64(48)
		for k, e := range x {
			var w int64
			x[k], w = slim(e)
			weight += int64(len(k)) + 48 + w
		}
		return x, weight
	case []any:
		weight := int64(24)
		for i, e := range x {
			var w int64
			x[i], w = slim(e)
			weight += 16 + w
		}
		return x, weight
	case string:
		if len(x) > slimThreshold {
			x = titlePrefix(x)
		}
		return x, int64(len(x)) + 16
	}
	return v, 16
}

// titlePrefix keeps at least slimThreshold bytes and at least 101
// non-space runes: compact() then yields the same first 100 runes and a
// blank check the same answer as for the whole string.
func titlePrefix(s string) string {
	visible := 0
	for i, r := range s {
		if !unicode.IsSpace(r) {
			visible++
		}
		if end := i + utf8.RuneLen(r); visible > 100 && end >= slimThreshold {
			return strings.Clone(s[:end]) // a substring would pin the whole body
		}
	}
	return s
}

// forgetRemoved drops decoded transcripts that disappeared from a walked root,
// so deleted sessions do not stay in memory for the life of the process.
func forgetRemoved(roots []string, ext string, seen map[string]bool) {
	decodedFiles.Range(func(key, _ any) bool {
		p := key.(string)
		if seen[p] || !strings.HasSuffix(p, ext) {
			return true
		}
		for _, root := range roots {
			if strings.HasPrefix(p, filepath.Clean(root)+string(filepath.Separator)) {
				if _, e := os.Stat(p); os.IsNotExist(e) {
					forgetDecoded(p)
				}
				break
			}
		}
		return true
	})
}

// Only changed transcripts are decoded again after the five-minute snapshot
// expires. Rebuild accounting globally so resumed Claude copies still dedup
// against unchanged files, and removed files cannot leave stale turns behind.
func readLinesContext(ctx context.Context, d *model.Data, a model.Agent, p string, refresh bool) []obj {
	started := time.Now()
	ctx = sourceContext(ctx, p)
	stats, _ := ctx.Value(decodeStatsKey{}).(*decodeStats)
	mode, reason := "error", "stat_failed"
	var cached lineCache
	var inputBytes, sourceBytes, decodedLines int64
	var diagnosticErr error
	defer func() {
		if stats != nil {
			switch mode {
			case "hit":
				stats.hits.Add(1)
			case "decode", "append":
				stats.misses.Add(1)
				stats.bytes.Add(inputBytes)
				stats.lines.Add(decodedLines)
			}
		}
		if syncdiag.HasRun(ctx) && (diagnosticErr != nil || time.Since(started) >= 100*time.Millisecond || mode != "hit" && inputBytes >= 1<<20) {
			syncdiag.Log(ctx, "file_read_result", "mode", mode, "reason", reason, "elapsed_ms", stageMS(started), "file_bytes", sourceBytes, "decoded_input_bytes", inputBytes, "lines", len(cached.Lines), "error", syncdiag.ErrorCode(diagnosticErr))
		}
	}()
	info, e := os.Stat(p)
	if e != nil {
		diagnosticErr = e
		fail(d, a, e)
		return nil
	}
	sourceBytes = info.Size()
	d.FileMtimes[p] = info.ModTime().UnixNano()
	cached, ok := cachedDecoded(p)
	mode, reason = "hit", "unchanged"
	if refresh || !ok || cached.Mtime != info.ModTime().UnixNano() || cached.Size != info.Size() {
		mode = "decode"
		f, e := os.Open(p)
		if e != nil {
			diagnosticErr = e
			fail(d, a, e)
			return nil
		}
		defer f.Close()
		// Verify the prefix and read the suffix through the same descriptor,
		// even if the path is replaced while a collection is running.
		info, e = f.Stat()
		if e != nil {
			diagnosticErr = e
			fail(d, a, e)
			return nil
		}
		sourceBytes = info.Size()
		d.FileMtimes[p] = info.ModTime().UnixNano()
		var offset int64
		h := sha256.New()
		var prefix hash.Hash
		if !refresh && ok {
			prefix = appendHash(f, cached, info.Size())
		}
		switch {
		case refresh:
			reason = "forced_rebuild"
		case !ok:
			reason = "decoded_cache_miss"
		case prefix != nil:
			// Transcripts are append-only while a session runs: keep the
			// decoded lines and decode only what was added.
			mode, reason, offset = "append", "source_appended", cached.Size
			h = prefix
		default:
			reason = "source_changed"
		}
		if syncdiag.HasRun(ctx) && sourceBytes-offset >= 1<<20 {
			_, finished := syncdiag.Watch(ctx, "file_decode")
			defer func() { finished(diagnosticErr) }()
		}
		next := lineCache{Mtime: info.ModTime().UnixNano(), Size: info.Size()}
		if offset > 0 {
			// Clip so appending never writes into a slice other loads share.
			next.Lines = slices.Clip(cached.Lines)
			next.Errors = slices.Clip(cached.Errors)
			next.Weight = cached.Weight
		}
		// Read exactly the stat'ed size, so Digest and Size describe the same
		// bytes even while a writer appends.
		r := io.NewSectionReader(f, offset, info.Size()-offset)
		scan := bufio.NewScanner(io.TeeReader(r, h))
		scan.Buffer(make([]byte, 65536), 64*1024*1024)
		for order := len(next.Lines) + 1; scan.Scan(); order++ {
			raw := scan.Bytes()
			decodedLines++
			inputBytes += int64(len(raw))
			var v obj
			if len(bytes.TrimSpace(raw)) == 0 {
				next.Lines = append(next.Lines, nil)
				continue
			}
			var e error
			if a == model.Claude {
				v, e = decodeClaudeLine(raw)
			} else if projection := transcriptProjections[a]; projection != nil {
				v, e = decodeProjectedJSON(raw, projection)
			} else {
				e = json.Unmarshal(trimLongStrings(raw), &v)
			}
			if e != nil {
				next.Errors = append(next.Errors, fmt.Errorf("%s:%d: %w", p, order, e))
			}
			_, weight := slim(v)
			next.Weight += weight
			next.Lines = append(next.Lines, v)
		}
		if e := scan.Err(); e != nil {
			next.Errors = append(next.Errors, fmt.Errorf("%s: %w", p, e))
		}
		var last [1]byte
		if info.Size() > 0 {
			if n, _ := f.ReadAt(last[:], info.Size()-1); n == 1 {
				next.EndsWithNewline = last[0] == '\n'
			}
		}
		next.Digest = h.Sum(nil)
		cached = next
		evicted := rememberDecoded(p, cached)
		if stats != nil {
			stats.evictions.Add(int64(evicted))
		}
	}
	for _, e := range cached.Errors {
		if diagnosticErr == nil {
			diagnosticErr = e
		}
		fail(d, a, e)
	}
	return cached.Lines
}

func loadFiles(ctx context.Context, d *model.Data, a model.Agent, refresh bool, previous *model.Data, signatures map[string]string) map[string]string {
	ctx, finish := syncdiag.Watch(ctx, "file_sources")
	defer func() { finish(sourceResultError(*d)) }()
	_, discovered := syncdiag.Watch(ctx, "discover_files")
	paths, tracked := fileSources(d, a)
	current := map[string]string{}
	for _, p := range tracked {
		if info, err := os.Stat(p); err == nil {
			current[p] = fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size())
		}
	}
	discovered(sourceResultError(*d))
	changed, removed := 0, 0
	for p, signature := range current {
		if signatures[p] != signature {
			changed++
		}
	}
	for p := range signatures {
		if _, ok := current[p]; !ok {
			removed++
		}
	}
	syncdiag.Log(ctx, "file_sources_plan", "files", len(paths), "tracked_files", len(tracked), "changed_files", changed, "removed_files", removed, "has_baseline", previous != nil)
	if previous != nil && len(d.Errors) == 0 && maps.Equal(current, signatures) {
		syncdiag.Log(ctx, "file_sources_cache", "mode", "unchanged")
		*d = filter(*previous, d.TurnsStart, d.TurnsEnd)
		return current
	}
	syncdiag.Log(ctx, "file_sources_cache", "mode", "reparse", "refresh", refresh)
	// Decode a few files ahead on worker goroutines while accounting runs
	// serially in path order, which resumed-session and cross-file dedup
	// need. Each file is decoded once per load, and only a bounded window of
	// decoded files is held beyond the cache budget.
	sources := paths
	if a == model.Grok {
		sources = make([]string, len(paths))
		for i, p := range paths {
			sources[i] = grokUpdates(p)
		}
	}
	take := decodeAhead(ctx, a, sources, d.TurnsStart, d.TurnsEnd, refresh)
	merge := func(r prefetched) []obj {
		for k, v := range r.part.FileMtimes {
			d.FileMtimes[k] = v
		}
		d.Errors = append(d.Errors, r.part.Errors...)
		return r.lines
	}
	_, accounted := syncdiag.Watch(ctx, "account_file_records")
	defer func() { accounted(sourceResultError(*d)) }()
	sessions := map[string]*model.Session{}
	seen := map[string]bool{}
	for i, p := range paths {
		if a == model.Grok {
			r := take(i)
			parseGrok(d, p, sessions, r.missing, func() []obj { return merge(r) })
			continue
		}
		f := newFileParser(a, p, sessions)
		for _, v := range merge(take(i)) {
			f.line(v)
		}
		d.FileSessions[p] = prefix(a) + "/" + f.id
		f.account(d, seen)
	}
	if a == model.Codex {
		index := filepath.Join(codexHome(), "session_index.jsonl")
		if _, e := os.Stat(index); e == nil {
			for _, v := range readLinesContext(ctx, d, a, index, refresh) {
				if s := sessions[str(v["id"])]; s != nil {
					if title := strings.TrimSpace(str(v["thread_name"])); title != "" {
						s.Title = title
					}
				}
			}
		}
	}
	for _, s := range sessions {
		if s.Title == "" {
			s.Title = s.ID
		}
		if a == model.WorkBuddy {
			if s.SelectedModel == "" {
				s.SelectedModel = "unknown"
			}
			if s.AgentMode == "" {
				s.AgentMode = "cli"
			}
		}
		d.Sessions = append(d.Sessions, *s)
	}
	return current
}

// fileSources lists an agent's transcripts in accounting order, and every
// file whose change must trigger a reparse.
func fileSources(d *model.Data, a model.Agent) (paths, tracked []string) {
	if a == model.Grok {
		paths = files(d, a, roots(a), "summary.json")
	} else {
		paths = files(d, a, roots(a), ".jsonl")
	}
	tracked = append([]string(nil), paths...)
	switch a {
	case model.Grok:
		for _, p := range paths {
			tracked = append(tracked, grokUpdates(p))
		}
	case model.Codex:
		tracked = append(tracked, filepath.Join(codexHome(), "session_index.jsonl"))
	}
	return paths, tracked
}

func grokUpdates(summary string) string { return filepath.Join(filepath.Dir(summary), "updates.jsonl") }

func codexHome() string {
	if base := os.Getenv("CODEX_HOME"); base != "" {
		return base
	}
	return home(".codex")
}

// fileParser accounts one JSONL transcript. Sessions span files, since a
// resumed session continues in a new one; id, the active model and order
// are the file's own, and records may switch id mid-file.
type fileParser struct {
	a           model.Agent
	p           string
	id, stream  string
	activeModel string
	order       uint64
	sessions    map[string]*model.Session
	// latest keeps Claude's last record of each message; ordered holds the
	// other agents' turns as they appear.
	latest  map[string]model.Turn
	ordered []model.Turn
}

func newFileParser(a model.Agent, p string, sessions map[string]*model.Session) *fileParser {
	id := fileID(p)
	if a == model.Omp {
		// omp names transcripts "<createdAt>_<session-id>.jsonl"; the UUID
		// suffix matches the session record's id, which the "title" record
		// can precede.
		if i := strings.LastIndexByte(id, '_'); i >= 0 {
			id = id[i+1:]
		}
	}
	f := &fileParser{a: a, p: p, id: id, stream: "main", sessions: sessions, latest: map[string]model.Turn{}}
	if a == model.Claude && filepath.Base(filepath.Dir(p)) == "subagents" {
		f.id = filepath.Base(filepath.Dir(filepath.Dir(p)))
		f.stream = "subagent/" + strings.TrimSuffix(filepath.Base(p), ".jsonl")
	}
	return f
}

func fileID(p string) string { return strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)) }

// line applies one decoded record: the session fields every agent shares,
// then the agent's own turn rules.
func (f *fileParser) line(v obj) {
	f.order++
	if v == nil {
		return
	}
	kind := str(v["type"])
	m := v["message"]
	payload := v["payload"]
	if f.a == model.WorkBuddy {
		// Rust resolves missing IDs per record, not from the previous line.
		f.id = fileID(f.p)
	}
	if f.a == model.Codex && kind == "session_meta" {
		if s := str(first(get(payload, "id"), get(payload, "session_id"))); s != "" {
			f.id = s
		}
	} else if (f.a == model.Pi || f.a == model.Omp) && kind == "session" {
		if s := str(v["id"]); s != "" {
			f.id = s
		}
	} else if s := str(first(v["sessionId"], v["session_id"])); s != "" {
		f.id = s
	}
	s := f.sessions[f.id]
	if s == nil {
		s = session(f.a, f.id)
		f.sessions[f.id] = s
	}
	at := ts(v["timestamp"])
	observe(s, at)
	if cwd := str(v["cwd"]); cwd != "" {
		s.WorkingDirectory = cwd
	}
	if s.Title == "" && (kind == "user" || str(get(m, "role")) == "user") {
		s.Title = text(get(m, "content"))
	}
	if kind == "ai-title" {
		s.Title = compact(str(v["aiTitle"]))
	}
	if kind == "custom-title" {
		s.Title = compact(str(v["customTitle"]))
	}
	t := model.Turn{Agent: f.a, SessionKey: s.Key, CreatedAt: at, ContextStream: f.stream, ContextOrder: f.order - 1}
	valid := false
	switch f.a {
	case model.Claude:
		f.claude(v, s, t, kind, m)
	case model.Codex:
		t, valid = f.codex(s, t, kind, payload)
	case model.Pi:
		t, valid = pi(v, s, t, kind, m)
	case model.Omp:
		t, valid = f.omp(v, s, t, kind, m)
	case model.WorkBuddy:
		t, valid = f.workBuddy(v, s, t)
	}
	if valid {
		f.ordered = append(f.ordered, t)
	}
}

// claude keeps the last record of each message: streaming writes the same
// message id several times with growing usage.
func (f *fileParser) claude(v obj, s *model.Session, t model.Turn, kind string, m any) {
	if x := str(v["entrypoint"]); x != "" {
		s.Source = x
	}
	if x := str(v["effort"]); x != "" {
		s.AgentMode = x
	}
	if kind != "assistant" {
		return
	}
	if x := str(get(m, "model")); x != "" {
		s.SelectedModel = x
	}
	u := get(m, "usage")
	if u == nil {
		return
	}
	t.Model = str(get(m, "model"))
	t.InputTokens = num(get(u, "input_tokens"))
	t.OutputTokens = num(get(u, "output_tokens"))
	t.CacheReadTokens = num(get(u, "cache_read_input_tokens"))
	t.CacheCreation5mTokens = num(get(u, "cache_creation", "ephemeral_5m_input_tokens"))
	t.CacheCreation1hTokens = num(get(u, "cache_creation", "ephemeral_1h_input_tokens"))
	t.CacheCreation5mTokens += max(0, num(get(u, "cache_creation_input_tokens"))-t.CacheCreation5mTokens-t.CacheCreation1hTokens)
	t.CacheCreationTokens = t.CacheCreation5mTokens + t.CacheCreation1hTokens
	t.DedupKey = str(first(get(m, "id"), v["requestId"]))
	if t.DedupKey == "" {
		t.DedupKey = fmt.Sprintf("anon:%s:%d", f.p, f.order)
	}
	f.latest[t.DedupKey] = t
}

func (f *fileParser) codex(s *model.Session, t model.Turn, kind string, payload any) (model.Turn, bool) {
	s.WorkingDirectory = str(first(get(payload, "cwd"), s.WorkingDirectory))
	switch kind {
	case "session_meta":
		if x := get(payload, "source"); x != nil {
			if st, ok := x.(string); ok {
				s.Source = st
			} else {
				b, _ := json.Marshal(x)
				s.Source = "subagent"
				if strings.Contains(string(b), "guardian") {
					s.Source = "guardian"
				}
			}
		}
	case "turn_context":
		f.activeModel = str(get(payload, "model"))
		s.SelectedModel = f.activeModel
		s.AgentMode = str(first(get(payload, "collaboration_mode", "kind"), get(payload, "collaboration_mode")))
	case "response_item":
		if s.Title == "" && str(get(payload, "role")) == "user" {
			s.Title = text(get(payload, "content"))
		}
	case "event_msg":
		if str(get(payload, "type")) == "user_message" && s.Title == "" {
			s.Title = compact(str(get(payload, "message")))
		}
		if str(get(payload, "type")) != "token_count" {
			return t, false
		}
		u := get(payload, "info", "last_token_usage")
		if u == nil {
			return t, false
		}
		// Codex repeats a token count when it re-emits events; the running
		// total identifies the turn.
		total := first(get(payload, "info", "total_token_usage"), u)
		t.DedupKey = fmt.Sprintf("%s#%g:%g:%g:%g", f.id, num(get(total, "input_tokens")), num(get(total, "cached_input_tokens")), num(get(total, "cache_write_input_tokens")), num(get(total, "output_tokens")))
		t.CacheReadTokens = num(get(u, "cached_input_tokens"))
		t.InputTokens = max(0, num(get(u, "input_tokens"))-t.CacheReadTokens)
		t.OutputTokens = num(get(u, "output_tokens"))
		t.CacheCreationTokens = num(get(u, "cache_write_input_tokens"))
		t.Model = f.activeModel
		return t, true
	}
	return t, false
}

func pi(v obj, s *model.Session, t model.Turn, kind string, m any) (model.Turn, bool) {
	if kind == "thinking_level_change" {
		s.AgentMode = str(v["thinkingLevel"])
	}
	if kind != "message" || str(get(m, "role")) != "assistant" || get(m, "usage") == nil {
		return t, false
	}
	u := get(m, "usage")
	t.InputTokens = num(get(u, "input"))
	t.OutputTokens = num(get(u, "output"))
	t.CacheReadTokens = num(get(u, "cacheRead"))
	t.CacheCreationTokens = num(get(u, "cacheWrite"))
	if c := get(u, "cost", "total"); c != nil && num(c) >= 0 {
		cost := num(c)
		t.RecordedCost = &cost
	}
	t.Model = modelName(str(get(m, "provider")), str(get(m, "model")))
	s.SelectedModel = t.Model
	return t, true
}

// omp transcripts share pi's record schema; omp also carries titles, a model
// change record, per-response ttft/duration and a responseId that dedups
// repeated assistant records.
func (f *fileParser) omp(v obj, s *model.Session, t model.Turn, kind string, m any) (model.Turn, bool) {
	if kind == "thinking_level_change" {
		s.AgentMode = str(v["thinkingLevel"])
		return t, false
	}
	switch kind {
	case "title", "title_change":
		if x := str(v["title"]); x != "" {
			s.Title = compact(x)
		}
		return t, false
	case "model_change":
		if x := str(v["model"]); x != "" {
			s.SelectedModel = x
		}
		return t, false
	case "session":
		if s.Title == "" {
			s.Title = compact(str(v["title"]))
		}
		return t, false
	}
	if kind == "model_usage" {
		// Side-channel calls (find, auto-thinking, judge) log usage outside
		// the message stream; purpose marks them in the turn's stream.
		u := get(v, "usage")
		if u == nil {
			return t, false
		}
		t.InputTokens = num(get(u, "input"))
		t.OutputTokens = num(get(u, "output"))
		t.CacheReadTokens = num(get(u, "cacheRead"))
		t.CacheCreationTokens = num(get(u, "cacheWrite"))
		if c := get(u, "cost", "total"); c != nil && num(c) >= 0 {
			cost := num(c)
			t.RecordedCost = &cost
		}
		if p := str(v["purpose"]); p != "" {
			t.ContextStream = "purpose/" + p
		}
		t.Model = modelName(str(v["provider"]), str(v["model"]))
		t.DedupKey = str(v["id"])
		return t, true
	}
	if kind != "message" || str(get(m, "role")) != "assistant" || get(m, "usage") == nil {
		return t, false
	}
	u := get(m, "usage")
	t.InputTokens = num(get(u, "input"))
	t.OutputTokens = num(get(u, "output"))
	t.CacheReadTokens = num(get(u, "cacheRead"))
	t.CacheCreationTokens = num(get(u, "cacheWrite"))
	if c := get(u, "cost", "total"); c != nil && num(c) >= 0 {
		cost := num(c)
		t.RecordedCost = &cost
	}
	t.TTFTMs = max(0, num(get(m, "ttft")))
	t.TotalTimeMs = max(0, num(get(m, "duration")))
	t.Model = modelName(str(get(m, "provider")), str(get(m, "model")))
	t.DedupKey = str(first(get(m, "responseId"), v["id"]))
	s.SelectedModel = t.Model
	return t, true
}

func (f *fileParser) workBuddy(v obj, s *model.Session, t model.Turn) (model.Turn, bool) {
	pr := v["providerData"]
	u := get(pr, "usage")
	raw := get(pr, "rawUsage")
	if s.AgentMode == "" {
		s.AgentMode = str(get(pr, "agent"))
	}
	if u == nil {
		return t, false
	}
	t.DedupKey = str(first(get(pr, "messageId"), v["id"]))
	if t.DedupKey == "" {
		t.DedupKey = fmt.Sprintf("anon:%s:%d", f.p, f.order)
	}
	t.InputTokens, t.OutputTokens, t.CacheReadTokens, t.CacheCreationTokens = buddyUsage(u, raw)
	if t.InputTokens+t.OutputTokens+t.CacheReadTokens+t.CacheCreationTokens == 0 {
		return t, false
	}
	for _, k := range []string{"model", "requestModelId", "requestModelName"} {
		if x := strings.TrimSpace(str(get(pr, k))); x != "" {
			t.Model = x
			break
		}
	}
	if t.Model == "" {
		t.Model = "unknown"
	}
	if s.SelectedModel == "" || s.SelectedModel == "unknown" {
		s.SelectedModel = t.Model
	}
	return t, true
}

// account adds the file's turns to d, dropping any already counted: Claude
// dedups globally by message (a resumed session copies earlier messages),
// the others per session.
func (f *fileParser) account(d *model.Data, seen map[string]bool) {
	if f.a == model.Claude {
		for _, t := range f.latest {
			f.ordered = append(f.ordered, t)
		}
		sort.Slice(f.ordered, func(i, j int) bool { return f.ordered[i].ContextOrder < f.ordered[j].ContextOrder })
	}
	for _, t := range f.ordered {
		key := t.SessionKey + "\x00" + t.DedupKey
		if f.a == model.Claude {
			key = t.DedupKey
		}
		if t.DedupKey != "" && seen[key] {
			continue
		}
		seen[key] = true
		s := f.sessions[strings.TrimPrefix(t.SessionKey, prefix(f.a)+"/")]
		if f.a == model.Claude && (t.CreatedAt < d.TurnsStart || t.CreatedAt >= d.TurnsEnd) {
			continue
		}
		add(s, t)
		if t.CreatedAt >= d.TurnsStart && t.CreatedAt < d.TurnsEnd {
			d.Turns = append(d.Turns, t)
		}
	}
}

func modelName(provider, id string) string {
	id = strings.TrimSpace(strings.SplitN(id, "(", 2)[0])
	if id == "" {
		return "unknown"
	}
	if provider == "" || strings.HasPrefix(id, provider+"/") {
		return id
	}
	return provider + "/" + id
}
func pick(v any, keys ...string) any {
	for _, k := range keys {
		if x := get(v, k); x != nil {
			return x
		}
	}
	return nil
}
func buddyUsage(u, raw any) (input, output, cached, write float64) {
	for _, v := range []any{u, raw} {
		for _, k := range []string{"cached_tokens", "cachedTokens", "cache_read_input_tokens", "cacheReadInputTokens", "prompt_cache_hit_tokens", "promptCacheHitTokens"} {
			cached = max(cached, num(get(v, k)))
		}
		for _, k := range []string{"cache_creation_input_tokens", "cacheCreationInputTokens", "prompt_cache_write_tokens", "promptCacheWriteTokens", "cache_write_input_tokens", "cacheWriteInputTokens"} {
			write = max(write, num(get(v, k)))
		}
	}
	cached = max(cached, num(get(raw, "prompt_tokens_details", "cached_tokens")))
	for _, k := range []string{"inputTokensDetails", "input_tokens_details"} {
		arr, _ := get(u, k).([]any)
		sum := 0.0
		for _, it := range arr {
			for _, key := range []string{"cached_tokens", "cachedTokens", "cache_read_input_tokens", "cacheReadInputTokens"} {
				sum += num(get(it, key))
			}
		}
		cached = max(cached, sum)
	}
	input = math.Max(0, num(first(pick(u, "inputTokens", "input_tokens", "prompt_tokens"), pick(raw, "prompt_tokens", "inputTokens", "input_tokens")))-cached)
	output = max(0, num(first(pick(u, "outputTokens", "output_tokens", "completion_tokens"), pick(raw, "completion_tokens", "outputTokens", "output_tokens"))))
	return
}

// parseGrok reads a session summary; updates yields its prefetched update
// log, which missing reports absent.
func parseGrok(d *model.Data, p string, sessions map[string]*model.Session, missing bool, updates func() []obj) {
	b, e := os.ReadFile(p)
	if e != nil {
		fail(d, model.Grok, e)
		return
	}
	var v obj
	if e = json.Unmarshal(b, &v); e != nil {
		fail(d, model.Grok, e)
		return
	}
	id := str(get(v, "info", "id"))
	if id == "" {
		id = filepath.Base(filepath.Dir(p))
	}
	s := session(model.Grok, id)
	sessions[id] = s
	s.Title = compact(str(first(v["generated_title"], v["session_summary"])))
	s.WorkingDirectory = str(get(v, "info", "cwd"))
	s.SelectedModel = str(v["current_model_id"])
	s.AgentMode = str(v["agent_name"])
	if s.SelectedModel == "" {
		s.SelectedModel = "grok"
	}
	if s.AgentMode == "" {
		s.AgentMode = "grok-build"
	}
	messages := num(first(v["num_messages"], v["num_chat_messages"]))
	defer func() { s.AgentMessages = messages }()
	observe(s, ts(v["created_at"]))
	observe(s, max(ts(v["updated_at"]), ts(v["last_active_at"])))
	if info, e := os.Stat(p); e == nil {
		d.FileMtimes[p] = info.ModTime().UnixNano()
		d.FileSessions[p] = s.Key
	}
	// A Grok summary is a valid session before its optional update log exists.
	if missing {
		return
	}
	for _, v := range updates() {
		u := get(v, "params", "update")
		if str(get(u, "sessionUpdate")) != "turn_completed" {
			continue
		}
		usage := get(u, "usage")
		if usage == nil {
			continue
		}
		t := model.Turn{Agent: model.Grok, SessionKey: s.Key, CreatedAt: ts(v["timestamp"]), Model: s.SelectedModel}
		if t.CreatedAt <= 0 {
			t.CreatedAt = ts(get(v, "_meta", "agentTimestampMs"))
			if t.CreatedAt == 0 {
				t.CreatedAt = s.CreatedAt
			}
		}
		t.CacheReadTokens = num(get(usage, "cachedReadTokens"))
		t.InputTokens = max(0, num(get(usage, "inputTokens"))-t.CacheReadTokens)
		t.OutputTokens = num(get(usage, "outputTokens"))
		t.CacheCreationTokens = num(get(usage, "cacheCreationTokens"))
		t.TotalTimeMs = num(get(usage, "apiDurationMs"))
		if c := num(get(usage, "costUsdTicks")); c > 0 {
			c /= 1e10
			t.RecordedCost = &c
		}
		if m, ok := get(usage, "modelUsage").(map[string]any); ok {
			var keys []string
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if len(keys) > 0 {
				t.Model = keys[0]
			}
		}
		add(s, t)
		observe(s, t.CreatedAt)
		if t.CreatedAt >= d.TurnsStart && t.CreatedAt < d.TurnsEnd {
			d.Turns = append(d.Turns, t)
		}
	}
}

// prefetched is one source decoded ahead of accounting: its lines, and the
// errors and mtime readLinesContext recorded for it.
type prefetched struct {
	lines   []obj
	part    model.Data
	missing bool
	done    chan struct{}
}

// decodeAhead decodes sources on four workers, at most eight ahead of the
// consumer, and returns take, which waits for source i. Every index must be
// taken once, in order.
func decodeAhead(ctx context.Context, a model.Agent, sources []string, start, end int64, refresh bool) (take func(i int) prefetched) {
	results := make([]prefetched, len(sources))
	for i := range results {
		results[i].done = make(chan struct{})
	}
	window := make(chan struct{}, 8)
	work := make(chan int)
	go func() {
		for i := range sources {
			window <- struct{}{}
			work <- i
		}
		close(work)
	}()
	for range 4 {
		go func() {
			for i := range work {
				r := &results[i]
				r.part = empty(start, end)
				if _, err := os.Stat(sources[i]); a == model.Grok && os.IsNotExist(err) {
					r.missing = true
				} else {
					r.lines = readLinesContext(ctx, &r.part, a, sources[i], refresh)
				}
				close(r.done)
			}
		}()
	}
	return func(i int) prefetched {
		<-results[i].done
		<-window
		r := results[i]
		results[i] = prefetched{} // let the decoded lines go after use
		return r
	}
}
