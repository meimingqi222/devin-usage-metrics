package data

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
)

func query(d *model.Data, a model.Agent, db *sqlite.DB, sql string, args ...any) []obj {
	r, e := db.Query(context.Background(), sql, args...)
	if e != nil {
		fail(d, a, e)
		return nil
	}
	var out []obj
	for _, row := range r.Values {
		v := obj{}
		for i, k := range r.Columns {
			v[k] = row[i]
		}
		out = append(out, v)
	}
	return out
}

// each streams a query's rows as objects; a query error is recorded like query.
func each(d *model.Data, a model.Agent, db *sqlite.DB, sql string, fn func(obj), args ...any) {
	e := db.Each(context.Background(), sql, func(columns []string, row []any) error {
		v := obj{}
		for i, k := range columns {
			v[k] = row[i]
		}
		fn(v)
		return nil
	}, args...)
	if e != nil {
		fail(d, a, e)
	}
}

type databaseCache struct {
	Signature string
	Data      model.Data
}

var parsedDatabases sync.Map

func databaseSignature(p string) string {
	var b strings.Builder
	for _, suffix := range []string{"", "-wal"} {
		if s, e := os.Stat(p + suffix); e == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", suffix, s.ModTime().UnixNano(), s.Size())
		}
	}
	return b.String()
}
func loadDBs(d *model.Data, a model.Agent, refresh bool, previous *model.Data, signatures map[string]string) map[string]string {
	var paths []string
	switch a {
	case model.Devin:
		base := home(".local/share/devin")
		if runtime.GOOS == "windows" {
			base = filepath.Join(os.Getenv("APPDATA"), "devin")
		}
		paths = []string{filepath.Join(base, "cli/sessions.db"), filepath.Join(base, "cli-next/sessions.db")}
	case model.Antigravity:
		paths = files(d, a, []string{home(".gemini/antigravity/conversations")}, ".db")
	case model.ZCode:
		paths = []string{home(".zcode/cli/db/db.sqlite"), home("Library/Application Support/zcode/cli/db/db.sqlite")}
	default:
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			base = home(".local/share")
		}
		if runtime.GOOS == "windows" {
			base = os.Getenv("APPDATA")
		} else if runtime.GOOS == "darwin" {
			base = home("Library/Application Support")
		}
		paths = []string{filepath.Join(base, string(a), string(a)+".db"), home(".local/share/" + string(a) + "/" + string(a) + ".db")}
	}
	var existing []string
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, e := os.Stat(p); os.IsNotExist(e) {
			continue
		}
		existing = append(existing, p)
		if a != model.Devin && a != model.Antigravity {
			break
		}
	}
	if len(existing) == 0 && a != model.Antigravity {
		fail(d, a, "database not found: "+strings.Join(paths, ", "))
	}
	parts := make([]model.Data, len(existing))
	current := make([]string, len(existing))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i, p := range existing {
		wg.Go(func() {
			limit <- struct{}{}
			defer func() { <-limit }()
			signature := databaseSignature(p)
			part := empty(d.TurnsStart, d.TurnsEnd)
			var cached databaseCache
			if value, ok := parsedDatabases.Load(p); ok {
				cached = value.(databaseCache)
			}
			if !refresh && previous != nil && a == model.Devin {
				// Split the persisted agent snapshot by source, including hidden-session turns.
				base := empty(previous.TurnsStart, previous.TurnsEnd)
				prefix := filepath.Base(filepath.Dir(p)) + "/"
				for _, s := range previous.Sessions {
					if strings.HasPrefix(s.Key, prefix) {
						base.Sessions = append(base.Sessions, s)
					}
				}
				for _, turn := range previous.Turns {
					if strings.HasPrefix(turn.SessionKey, prefix) {
						base.Turns = append(base.Turns, turn)
					}
				}
				cached = databaseCache{signatures[p], base}
			} else if !refresh && previous != nil && len(existing) == 1 && a != model.Antigravity {
				cached = databaseCache{signatures[p], *previous}
			}
			covered := cached.Data.TurnsStart <= d.TurnsStart && cached.Data.TurnsEnd >= min(d.TurnsEnd, time.Now().Unix())
			if !refresh && covered && cached.Signature == signature {
				part = filter(cached.Data, d.TurnsStart, d.TurnsEnd)
			} else {
				db, e := sqlite.Open(context.Background(), p, sqlite.OpenOptions{ReadOnly: true, BusyTimeout: time.Second})
				if e != nil {
					fail(&part, a, fmt.Errorf("%s: %w", p, e))
				} else {
					switch a {
					case model.Devin:
						var baseline *model.Data
						if !refresh && covered {
							baseline = &cached.Data
						}
						parseDevin(&part, db, filepath.Base(filepath.Dir(p)), baseline)
					case model.Antigravity:
						parseAG(&part, db, strings.TrimSuffix(filepath.Base(p), ".db"))
					default:
						parseSQLAgent(&part, a, db)
					}
					db.Close()
				}
			}
			// Never persist a signature for a partial parse or a concurrent write.
			if len(part.Errors) == 0 && databaseSignature(p) == signature {
				current[i] = signature
				parsedDatabases.Store(p, databaseCache{signature, part})
			}
			parts[i] = part
		})
	}
	wg.Wait()
	out := map[string]string{}
	for i, p := range existing {
		part := parts[i]
		d.Sessions = append(d.Sessions, part.Sessions...)
		d.Turns = append(d.Turns, part.Turns...)
		d.Errors = append(d.Errors, part.Errors...)
		if current[i] != "" {
			out[p] = current[i]
		}
		if info, e := os.Stat(p); e == nil {
			d.FileMtimes[p] = info.ModTime().UnixNano()
		}
	}
	return out
}
func parseDevin(d *model.Data, db *sqlite.DB, source string, previous *model.Data) {
	a := model.Devin
	rows := query(d, a, db, "SELECT * FROM sessions ORDER BY last_activity_at DESC LIMIT 20001")
	if len(rows) > 20000 {
		fail(d, a, "session limit exceeded (20000)")
		rows = rows[:20000]
	}
	ids := map[string]bool{}
	for _, r := range rows {
		id := str(r["id"])
		ids[id] = true
		if num(r["hidden"]) != 0 {
			continue
		}
		s := session(a, id)
		s.Source = source
		s.Key = source + "/" + id
		s.Title = str(r["title"])
		s.WorkingDirectory = str(r["working_directory"])
		s.SelectedModel = str(r["model"])
		s.AgentMode = str(r["agent_mode"])
		s.CreatedAt = ts(r["created_at"])
		s.LastActivityAt = ts(r["last_activity_at"])
		var meta obj
		if json.Unmarshal([]byte(str(r["metadata"])), &meta) == nil {
			dims, _ := meta["response_dimensions"].([]any)
			for _, dim := range dims {
				val := first(get(dim, "kind", "Metric", "value"), get(dim, "kind", "CumulativeMetric", "value"))
				switch str(get(dim, "uid")) {
				case "model":
					m := str(val)
					s.RealModel = &m
				case "input_tokens":
					s.InputTokens = num(val)
				case "output_tokens":
					s.OutputTokens = num(val)
				case "cached_input_tokens":
					s.CachedTokens = num(val)
				case "agent_messages":
					s.AgentMessages = num(val)
				}
			}
		}
		d.Sessions = append(d.Sessions, *s)
	}
	seen := map[string]bool{}
	latest := map[string]int64{}
	legacy := map[string]bool{}
	watermark := d.TurnsStart
	if previous != nil {
		for _, turn := range previous.Turns {
			id := strings.TrimPrefix(turn.SessionKey, source+"/")
			if !ids[id] || turn.CreatedAt < d.TurnsStart || turn.CreatedAt >= d.TurnsEnd {
				continue
			}
			if turn.DedupKey == "" {
				legacy[id] = true
				continue
			}
			if turn.DedupKey != "" && seen[turn.DedupKey] {
				continue
			}
			if turn.DedupKey != "" {
				seen[turn.DedupKey] = true
			}
			latest[id] = max(latest[id], turn.CreatedAt)
			watermark = max(watermark, turn.CreatedAt)
			d.Turns = append(d.Turns, turn)
		}
	}
	var active []any
	queryStart := d.TurnsEnd
	for _, row := range rows {
		id := str(row["id"])
		floor, cached := latest[id]
		if !cached || legacy[id] {
			floor = d.TurnsStart
		}
		if cached && !legacy[id] && ts(row["last_activity_at"]) < watermark-86400 {
			continue
		}
		active = append(active, id)
		queryStart = min(queryStart, floor)
	}
	// Chunk the IN list below SQLite's parameter limit. LIKE avoids transferring
	// large non-metric messages; metrics without ttft_ms remain valid calls.
	for offset := 0; offset < len(active); offset += 500 {
		batch := active[offset:min(offset+500, len(active))]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := append(append([]any{}, batch...), queryStart-86400, d.TurnsEnd+7*86400)
		sql := "SELECT rowid AS node_id, session_id, created_at, chat_message FROM message_nodes WHERE session_id IN (" + marks + ") AND created_at >= ? AND created_at < ? AND chat_message LIKE '%\"metrics\"%'"
		// Stream: each chat_message carries the whole message and tool output,
		// so materializing the result set peaked at gigabytes.
		each(d, a, db, sql, func(r obj) {
			id := str(r["session_id"])
			if !ids[id] {
				return
			}
			var v obj
			if e := json.Unmarshal([]byte(str(r["chat_message"])), &v); e != nil {
				fail(d, a, e)
				return
			}
			m := v["metadata"]
			u := get(m, "metrics")
			if u == nil {
				return
			}
			at := ts(first(get(m, "started_generation_at"), get(m, "created_at")))
			if at <= 0 {
				at = ts(r["created_at"])
			}
			key := str(first(v["message_id"], get(m, "request_id")))
			if key == "" {
				key = fmt.Sprint("row:", r["node_id"])
			}
			if key != "" {
				key = id + "\x00" + key
				if seen[key] {
					return
				}
				seen[key] = true
			}
			if at < d.TurnsStart || at >= d.TurnsEnd {
				return
			}
			d.Turns = append(d.Turns, model.Turn{Agent: a, SessionKey: source + "/" + id, CreatedAt: at, Model: str(get(m, "generation_model")), InputTokens: num(get(u, "input_tokens")), OutputTokens: num(get(u, "output_tokens")), CacheReadTokens: num(get(u, "cache_read_tokens")), CacheCreationTokens: num(get(u, "cache_creation_tokens")), TTFTMs: max(0, num(get(u, "ttft_ms"))), TotalTimeMs: num(get(u, "total_time_ms")), DedupKey: key})
		}, args...)
	}
}
func parseSQLAgent(d *model.Data, a model.Agent, db *sqlite.DB) {
	st, mt := "session", "message"
	if a == model.OpenCode {
		r := query(d, a, db, "SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session_v2','session_message')")
		if len(r) == 2 {
			st = "session_v2"
			mt = "session_message"
		}
	}
	sessions := map[string]*model.Session{}
	parents := map[string]string{}
	for _, r := range query(d, a, db, "SELECT * FROM "+st) {
		id := str(r["id"])
		s := session(a, id)
		s.Title = str(r["title"])
		s.WorkingDirectory = str(r["directory"])
		s.CreatedAt = ts(r["time_created"])
		s.LastActivityAt = ts(r["time_updated"])
		s.AgentMode = str(first(r["agent"], r["task_type"]))
		var m obj
		if json.Unmarshal([]byte(str(r["model"])), &m) == nil {
			s.SelectedModel = modelName(str(m["providerID"]), str(m["id"]))
		}
		sessions[id] = s
		if p := str(r["parent_id"]); p != "" {
			parents[id] = p
		}
	}
	root := func(id string) string {
		seen := map[string]bool{}
		for parents[id] != "" && !seen[id] {
			seen[id] = true
			id = parents[id]
		}
		return id
	}
	sql := "SELECT * FROM " + mt + " ORDER BY time_created, rowid"
	if a == model.ZCode {
		sql = "SELECT * FROM model_usage WHERE status='completed' OR input_tokens>0 OR output_tokens>0"
	}
	// Stream: message rows carry full message bodies.
	each(d, a, db, sql, func(r obj) {
		id := root(str(r["session_id"]))
		s := sessions[id]
		if s == nil {
			s = session(a, id)
			sessions[id] = s
		}
		t := model.Turn{Agent: a, SessionKey: s.Key}
		if a == model.ZCode {
			t.CreatedAt = ts(r["started_at"])
			t.Model = str(r["model_id"])
			t.CacheReadTokens = num(r["cache_read_input_tokens"])
			t.InputTokens = max(0, num(r["input_tokens"])-t.CacheReadTokens)
			t.OutputTokens = num(r["output_tokens"])
			t.CacheCreationTokens = num(r["cache_creation_input_tokens"])
			t.TTFTMs = num(r["time_to_first_token_ms"])
			t.TotalTimeMs = max(0, num(r["completed_at"])-num(r["started_at"]))
		} else {
			var v obj
			if e := json.Unmarshal([]byte(str(r["data"])), &v); e != nil {
				fail(d, a, e)
				return
			}
			if str(v["role"]) != "assistant" && str(r["type"]) != "assistant" {
				return
			}
			t.CreatedAt = ts(r["time_created"])
			t.Model = modelName(str(first(get(v, "model", "providerID"), v["providerID"])), str(first(get(v, "model", "id"), v["modelID"])))
			t.InputTokens = max(0, num(get(v, "tokens", "input")))
			t.OutputTokens = max(0, num(get(v, "tokens", "output")))
			t.CacheReadTokens = max(0, num(get(v, "tokens", "cache", "read")))
			t.CacheCreationTokens = max(0, num(get(v, "tokens", "cache", "write")))
			if c := num(v["cost"]); c > 0 {
				t.RecordedCost = &c
			}
			start := first(get(v, "time", "created"), r["time_created"])
			t.TotalTimeMs = max(0, num(get(v, "time", "completed"))-num(start))
			if s.AgentMode == "" {
				s.AgentMode = str(v["mode"])
			}
		}
		if s.SelectedModel == "" {
			s.SelectedModel = t.Model
		}
		observe(s, t.CreatedAt)
		add(s, t)
		if t.CreatedAt >= d.TurnsStart && t.CreatedAt < d.TurnsEnd {
			d.Turns = append(d.Turns, t)
		}
	})
	for id, s := range sessions {
		if parents[id] == "" {
			if s.Title == "" {
				s.Title = id
			}
			d.Sessions = append(d.Sessions, *s)
		}
	}
}

// Protobuf fields are decoded directly: Antigravity uses private field numbers.
type field struct {
	n uint64
	b []byte
	v uint64
}

func proto(b []byte) []field {
	var out []field
	for len(b) > 0 {
		k, n := binary.Uvarint(b)
		if n <= 0 {
			return out
		}
		b = b[n:]
		f := field{n: k >> 3}
		switch k & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return out
			}
			f.v = v
			b = b[n:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || l > uint64(len(b)-n) {
				return out
			}
			f.b = b[n : n+int(l)]
			b = b[n+int(l):]
		case 1:
			if len(b) < 8 {
				return out
			}
			b = b[8:]
		case 5:
			if len(b) < 4 {
				return out
			}
			b = b[4:]
		default:
			return out
		}
		out = append(out, f)
	}
	return out
}
func pb(b []byte, path ...uint64) field {
	for i, n := range path {
		var found field
		for _, f := range proto(b) {
			if f.n == n {
				found = f
				break
			}
		}
		if i == len(path)-1 {
			return found
		}
		b = found.b
	}
	return field{}
}
func blob(v any) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	return []byte(str(v))
}
func parseAG(d *model.Data, db *sqlite.DB, id string) {
	a := model.Antigravity
	s := session(a, id)
	for _, r := range query(d, a, db, "SELECT data FROM trajectory_metadata_blob WHERE id='main'") {
		b := blob(r["data"])
		s.WorkingDirectory = strings.TrimPrefix(string(pb(b, 1, 1).b), "file://")
		if s.WorkingDirectory == "" {
			s.WorkingDirectory = strings.TrimPrefix(string(pb(b, 7).b), "file://")
		}
		observe(s, int64(pb(b, 2, 1).v))
	}
	for _, r := range query(d, a, db, "SELECT step_payload FROM steps WHERE step_type=14 ORDER BY idx LIMIT 5") {
		b := blob(r["step_payload"])
		for _, path := range [][]uint64{{26, 3, 1}, {26, 2, 1}, {19, 3, 1}, {19, 2, 1}} {
			if txt := string(pb(b, path...).b); txt != "" && s.Title == "" {
				s.Title = compact(txt)
			}
		}
	}
	for _, r := range query(d, a, db, "SELECT data FROM gen_metadata ORDER BY idx") {
		if m := string(pb(blob(r["data"]), 1, 19).b); m != "" {
			s.SelectedModel = m
		}
	}
	var full, fallback []model.Turn
	for _, r := range query(d, a, db, "SELECT metadata FROM steps ORDER BY idx") {
		b := blob(r["metadata"])
		t := model.Turn{Agent: a, SessionKey: s.Key, CreatedAt: int64(pb(b, 1, 1).v)}
		observe(s, t.CreatedAt)
		if u := pb(b, 9).b; u != nil {
			t.InputTokens = float64(pb(u, 1).v)
			t.OutputTokens = float64(pb(u, 2).v + pb(u, 10).v)
			t.CacheReadTokens = float64(pb(u, 5).v)
			t.CacheCreationTokens = float64(pb(u, 3).v)
			full = append(full, t)
		} else if p := pb(b, 11); p.n != 0 {
			t.InputTokens = float64(p.v)
			fallback = append(fallback, t)
		}
	}
	if len(full) == 0 {
		full = fallback
	}
	for _, t := range full {
		add(s, t)
		if t.CreatedAt >= d.TurnsStart && t.CreatedAt < d.TurnsEnd {
			d.Turns = append(d.Turns, t)
		}
	}
	if s.Title == "" {
		s.Title = id
	}
	if s.AgentMessages > 0 {
		d.Sessions = append(d.Sessions, *s)
	}
}
