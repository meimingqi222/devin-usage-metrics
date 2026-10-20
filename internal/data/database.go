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
	"time"

	"github.com/egoist/mygo/plugins/sqlite"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
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

type databaseCache struct {
	Signature string
	Data      model.Data
}

var parsedDatabases = map[string]databaseCache{}

func databaseSignature(p string) string {
	var b strings.Builder
	for _, suffix := range []string{"", "-wal"} {
		if s, e := os.Stat(p + suffix); e == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", suffix, s.ModTime().UnixNano(), s.Size())
		}
	}
	return b.String()
}
func loadDBs(d *model.Data, a model.Agent, refresh bool) {
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
	found := false
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, e := os.Stat(p); os.IsNotExist(e) {
			continue
		}
		found = true
		signature := databaseSignature(p)
		part := empty(d.TurnsStart, d.TurnsEnd)
		cached, ok := parsedDatabases[p]
		if !refresh && ok && cached.Signature == signature && cached.Data.TurnsStart <= d.TurnsStart && cached.Data.TurnsEnd >= d.TurnsEnd {
			part = filter(cached.Data, d.TurnsStart, d.TurnsEnd)
		} else {
			db, e := sqlite.Open(context.Background(), p, sqlite.OpenOptions{ReadOnly: true, BusyTimeout: time.Second})
			if e != nil {
				fail(d, a, fmt.Errorf("%s: %w", p, e))
				continue
			}
			switch a {
			case model.Devin:
				parseDevin(&part, db, filepath.Base(filepath.Dir(p)))
			case model.Antigravity:
				parseAG(&part, db, strings.TrimSuffix(filepath.Base(p), ".db"))
			default:
				parseSQLAgent(&part, a, db)
			}
			db.Close()
			// A concurrent writer invalidates this parse for reuse, not for display.
			if len(part.Errors) == 0 && databaseSignature(p) == signature {
				parsedDatabases[p] = databaseCache{signature, part}
			}
		}
		d.Sessions = append(d.Sessions, part.Sessions...)
		d.Turns = append(d.Turns, part.Turns...)
		d.Errors = append(d.Errors, part.Errors...)
		if info, e := os.Stat(p); e == nil {
			d.FileMtimes[p] = info.ModTime().UnixNano()
		}
		if a != model.Devin && a != model.Antigravity {
			break
		}
	}
	if !found && a != model.Antigravity {
		fail(d, a, "database not found: "+strings.Join(paths, ", "))
	}
}
func parseDevin(d *model.Data, db *sqlite.DB, source string) {
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
	for _, r := range query(d, a, db, "SELECT session_id, created_at, chat_message FROM message_nodes WHERE created_at >= ? AND created_at < ?", d.TurnsStart-86400, d.TurnsEnd+7*86400) {
		id := str(r["session_id"])
		if !ids[id] {
			continue
		}
		var v obj
		if e := json.Unmarshal([]byte(str(r["chat_message"])), &v); e != nil {
			fail(d, a, e)
			continue
		}
		m := v["metadata"]
		u := get(m, "metrics")
		if u == nil {
			continue
		}
		at := ts(first(get(m, "started_generation_at"), get(m, "created_at")))
		if at <= 0 {
			at = ts(r["created_at"])
		}
		key := str(first(v["message_id"], get(m, "request_id")))
		if key != "" {
			key = id + "\x00" + key
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		if at < d.TurnsStart || at >= d.TurnsEnd {
			continue
		}
		d.Turns = append(d.Turns, model.Turn{Agent: a, SessionKey: source + "/" + id, CreatedAt: at, Model: str(get(m, "generation_model")), InputTokens: num(get(u, "input_tokens")), OutputTokens: num(get(u, "output_tokens")), CacheReadTokens: num(get(u, "cache_read_tokens")), CacheCreationTokens: num(get(u, "cache_creation_tokens")), TTFTMs: max(0, num(get(u, "ttft_ms"))), TotalTimeMs: num(get(u, "total_time_ms")), DedupKey: key})
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
	for _, r := range query(d, a, db, sql) {
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
				continue
			}
			if str(v["role"]) != "assistant" && str(r["type"]) != "assistant" {
				continue
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
	}
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
