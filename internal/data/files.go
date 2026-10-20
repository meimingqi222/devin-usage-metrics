package data

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
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
	sort.Slice(paths, func(i, j int) bool {
		a, _ := os.Stat(paths[i])
		b, _ := os.Stat(paths[j])
		if a != nil && b != nil && !a.ModTime().Equal(b.ModTime()) {
			return a.ModTime().After(b.ModTime())
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
		b := os.Getenv("CODEX_HOME")
		if b == "" {
			b = home(".codex")
		}
		return []string{filepath.Join(b, "sessions"), filepath.Join(b, "archived_sessions")}
	case model.Pi:
		return []string{home(".pi/agent/sessions")}
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
}

var decodedFiles = map[string]lineCache{}

// Only changed transcripts are decoded again after the five-minute snapshot
// expires. Rebuild accounting globally so resumed Claude copies still dedup
// against unchanged files, and removed files cannot leave stale turns behind.
func readLines(d *model.Data, a model.Agent, p string, refresh bool) []obj {
	info, e := os.Stat(p)
	if e != nil {
		fail(d, a, e)
		return nil
	}
	d.FileMtimes[p] = info.ModTime().UnixNano()
	cached, ok := decodedFiles[p]
	if refresh || !ok || cached.Mtime != info.ModTime().UnixNano() || cached.Size != info.Size() {
		cached = lineCache{Mtime: info.ModTime().UnixNano(), Size: info.Size()}
		f, e := os.Open(p)
		if e != nil {
			fail(d, a, e)
			return nil
		}
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 65536), 64*1024*1024)
		for order := 1; scan.Scan(); order++ {
			var v obj
			if len(strings.TrimSpace(scan.Text())) == 0 {
				cached.Lines = append(cached.Lines, nil)
				continue
			}
			if e := json.Unmarshal(scan.Bytes(), &v); e != nil {
				cached.Errors = append(cached.Errors, fmt.Errorf("%s:%d: %w", p, order, e))
			}
			cached.Lines = append(cached.Lines, v)
		}
		if e := scan.Err(); e != nil {
			cached.Errors = append(cached.Errors, fmt.Errorf("%s: %w", p, e))
		}
		f.Close()
		decodedFiles[p] = cached
	}
	for _, e := range cached.Errors {
		fail(d, a, e)
	}
	return cached.Lines
}

func loadFiles(d *model.Data, a model.Agent, refresh bool) {
	paths := files(d, a, roots(a), ".jsonl")
	if a == model.Grok {
		paths = files(d, a, roots(a), "summary.json")
	}
	sessions := map[string]*model.Session{}
	seen := map[string]bool{}
	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		stream := "main"
		if a == model.Claude && filepath.Base(filepath.Dir(p)) == "subagents" {
			id = filepath.Base(filepath.Dir(filepath.Dir(p)))
			stream = "subagent/" + strings.TrimSuffix(filepath.Base(p), ".jsonl")
		}
		if a == model.Grok {
			parseGrok(d, p, sessions, refresh)
			continue
		}
		latest := map[string]model.Turn{}
		var ordered []model.Turn
		activeModel := ""
		order := uint64(0)
		for _, v := range readLines(d, a, p, refresh) {
			order++
			if v == nil {
				continue
			}
			kind := str(v["type"])
			m := v["message"]
			payload := v["payload"]
			if a == model.Codex && kind == "session_meta" {
				if s := str(first(get(payload, "id"), get(payload, "session_id"))); s != "" {
					id = s
				}
			} else if a == model.Pi && kind == "session" {
				if s := str(v["id"]); s != "" {
					id = s
				}
			} else if s := str(first(v["sessionId"], v["session_id"])); s != "" {
				id = s
			}
			s := sessions[id]
			if s == nil {
				s = session(a, id)
				sessions[id] = s
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
			t := model.Turn{Agent: a, SessionKey: s.Key, CreatedAt: at, ContextStream: stream, ContextOrder: order - 1}
			valid := false
			switch a {
			case model.Claude:
				if x := str(v["entrypoint"]); x != "" {
					s.Source = x
				}
				if x := str(v["effort"]); x != "" {
					s.AgentMode = x
				}
				if kind != "assistant" {
					continue
				}
				if x := str(get(m, "model")); x != "" {
					s.SelectedModel = x
				}
				u := get(m, "usage")
				if u == nil {
					continue
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
					t.DedupKey = fmt.Sprintf("anon:%s:%d", p, order)
				}
				latest[t.DedupKey] = t
			case model.Codex:
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
					activeModel = str(get(payload, "model"))
					s.SelectedModel = activeModel
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
						continue
					}
					u := get(payload, "info", "last_token_usage")
					if u == nil {
						continue
					}
					total := first(get(payload, "info", "total_token_usage"), u)
					t.DedupKey = fmt.Sprintf("%s#%g:%g:%g:%g", id, num(get(total, "input_tokens")), num(get(total, "cached_input_tokens")), num(get(total, "cache_write_input_tokens")), num(get(total, "output_tokens")))
					t.CacheReadTokens = num(get(u, "cached_input_tokens"))
					t.InputTokens = max(0, num(get(u, "input_tokens"))-t.CacheReadTokens)
					t.OutputTokens = num(get(u, "output_tokens"))
					t.CacheCreationTokens = num(get(u, "cache_write_input_tokens"))
					t.Model = activeModel
					valid = true
				}
			case model.Pi:
				if kind == "thinking_level_change" {
					s.AgentMode = str(v["thinkingLevel"])
				}
				if kind != "message" || str(get(m, "role")) != "assistant" || get(m, "usage") == nil {
					continue
				}
				u := get(m, "usage")
				t.InputTokens = num(get(u, "input"))
				t.OutputTokens = num(get(u, "output"))
				t.CacheReadTokens = num(get(u, "cacheRead"))
				t.CacheCreationTokens = num(get(u, "cacheWrite"))
				if c := get(u, "cost", "total"); c != nil && num(c) >= 0 {
					v := num(c)
					t.RecordedCost = &v
				}
				t.Model = modelName(str(get(m, "provider")), str(get(m, "model")))
				s.SelectedModel = t.Model
				valid = true
			case model.WorkBuddy:
				pr := v["providerData"]
				u := get(pr, "usage")
				raw := get(pr, "rawUsage")
				if s.AgentMode == "" {
					s.AgentMode = str(get(pr, "agent"))
				}
				if u == nil {
					continue
				}
				t.DedupKey = str(first(get(pr, "messageId"), v["id"]))
				if t.DedupKey == "" {
					t.DedupKey = fmt.Sprintf("anon:%s:%d", p, order)
				}
				t.InputTokens, t.OutputTokens, t.CacheReadTokens, t.CacheCreationTokens = buddyUsage(u, raw)
				if t.InputTokens+t.OutputTokens+t.CacheReadTokens+t.CacheCreationTokens == 0 {
					continue
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
				valid = true
			}
			if valid {
				ordered = append(ordered, t)
			}
		}
		if a == model.Claude {
			for _, t := range latest {
				ordered = append(ordered, t)
			}
			sort.Slice(ordered, func(i, j int) bool { return ordered[i].ContextOrder < ordered[j].ContextOrder })
		}
		d.FileSessions[p] = prefix(a) + "/" + id
		for _, t := range ordered {
			key := t.SessionKey + "\x00" + t.DedupKey
			if a == model.Claude {
				key = t.DedupKey
			}
			if t.DedupKey != "" && seen[key] {
				continue
			}
			seen[key] = true
			s := sessions[strings.TrimPrefix(t.SessionKey, prefix(a)+"/")]
			if a == model.Claude && (t.CreatedAt < d.TurnsStart || t.CreatedAt >= d.TurnsEnd) {
				continue
			}
			add(s, t)
			if t.CreatedAt >= d.TurnsStart && t.CreatedAt < d.TurnsEnd {
				d.Turns = append(d.Turns, t)
			}
		}
	}
	if a == model.Codex {
		base := os.Getenv("CODEX_HOME")
		if base == "" {
			base = home(".codex")
		}
		index := filepath.Join(base, "session_index.jsonl")
		if _, e := os.Stat(index); e == nil {
			for _, v := range readLines(d, a, index, refresh) {
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
func parseGrok(d *model.Data, p string, sessions map[string]*model.Session, refresh bool) {
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
	for _, v := range readLines(d, model.Grok, filepath.Join(filepath.Dir(p), "updates.jsonl"), refresh) {
		u := get(v, "params", "update")
		if str(get(u, "sessionUpdate")) != "turn_completed" {
			continue
		}
		usage := get(u, "usage")
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
