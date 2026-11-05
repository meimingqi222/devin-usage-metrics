package data_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
)

func env(t testing.TB) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("USERPROFILE", h)
	t.Setenv("APPDATA", filepath.Join(h, ".local/share"))
	t.Setenv("LOCALAPPDATA", filepath.Join(h, "cache"))
	for k, v := range map[string]string{"HOME": h, "XDG_CONFIG_HOME": filepath.Join(h, "config"), "XDG_CACHE_HOME": filepath.Join(h, "cache"), "XDG_DATA_HOME": filepath.Join(h, ".local/share"), "CODEX_HOME": filepath.Join(h, ".codex"), "WORKBUDDY_CONFIG_DIR": "", "WORKBUDDY_DIR": "", "CODEBUDDY_CONFIG_DIR": "", "PI_CONFIG_DIR": "", "PI_CODING_AGENT_DIR": ""} {
		t.Setenv(k, v)
	}
	return h
}
func write(t testing.TB, h, p, s string) {
	t.Helper()
	p = filepath.Join(h, p)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
}
func db(t testing.TB, h, p string) *sqlite.DB {
	t.Helper()
	p = filepath.Join(h, p)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	db, e := sqlite.Open(context.Background(), p, sqlite.OpenOptions{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func exec(t testing.TB, db *sqlite.DB, s string, args ...any) {
	t.Helper()
	if _, e := db.Execute(context.Background(), s, args...); e != nil {
		t.Fatal(e)
	}
}
func check(t testing.TB, d model.Data, n int) {
	t.Helper()
	if len(d.Errors) > 0 {
		t.Fatalf("errors: %+v", d.Errors)
	}
	if len(d.Turns) != n {
		t.Fatalf("want %d turns: %+v", n, d)
	}
	for _, r := range d.Turns {
		if r.DeviceID == "" || r.DeviceName == "" {
			t.Fatal("unstamped record")
		}
	}
}

func TestClaudeResumeSubagentsAndFinalUsage(t *testing.T) {
	h := env(t)
	line := `{"type":"assistant","timestamp":100,"message":{"id":"m","model":"claude","usage":{"input_tokens":2,"output_tokens":1,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_1h_input_tokens":4}}}}` + "\n"
	final := strings.Replace(line, `"output_tokens":1`, `"output_tokens":9`, 1)
	write(t, h, ".claude/projects/p/a.jsonl", line+final)
	write(t, h, ".claude/projects/p/resume.jsonl", final)
	write(t, h, ".claude/projects/p/a/subagents/worker.jsonl", strings.Replace(final, `"id":"m"`, `"id":"worker"`, 1))
	d := data.Load(model.Claude, 0, 200, true)
	check(t, d, 2)
	for _, r := range d.Turns {
		if r.OutputTokens != 9 || r.CacheCreation5mTokens != 6 || r.CacheCreation1hTokens != 4 {
			t.Fatalf("wrong usage %+v", r)
		}
	}
	found := false
	for _, r := range d.Turns {
		if r.ContextStream == "subagent/worker" {
			found = true
			if r.SessionKey != "claude-code/a" {
				t.Fatal(r.SessionKey)
			}
		}
	}
	if !found {
		t.Fatal("missing subagent stream")
	}
}
func TestCodexCumulativeSignatureAndModels(t *testing.T) {
	h := env(t)
	write(t, h, ".codex/sessions/s.jsonl", `{"type":"session_meta","timestamp":90,"payload":{"id":"s","cwd":"/project","source":{"guardian":{}}}}
{"type":"turn_context","timestamp":91,"payload":{"model":"gpt"}}
{"type":"event_msg","timestamp":100,"payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":20,"cached_input_tokens":5,"output_tokens":3},"total_token_usage":{"input_tokens":20,"cached_input_tokens":5,"output_tokens":3}}}}
{"type":"event_msg","timestamp":101,"payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":20,"cached_input_tokens":5,"output_tokens":3},"total_token_usage":{"input_tokens":20,"cached_input_tokens":5,"output_tokens":3}}}}
{"type":"event_msg","timestamp":102,"payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":8,"cached_input_tokens":2,"output_tokens":1},"total_token_usage":{"input_tokens":28,"cached_input_tokens":7,"output_tokens":4}}}}
`)
	d := data.Load(model.Codex, 0, 200, true)
	check(t, d, 2)
	if d.Turns[0].InputTokens != 15 || d.Turns[1].InputTokens != 6 || d.Turns[0].Model != "gpt" || d.Sessions[0].Source != "guardian" {
		t.Fatalf("wrong codex %+v", d)
	}
}
func TestWorkBuddyAliasesAndZeroPrecedence(t *testing.T) {
	h := env(t)
	write(t, h, ".codebuddy/projects/p/a.jsonl", `{"sessionId":"s","timestamp":1700000000000,"providerData":{"messageId":"m","requestModelName":"glm","usage":{"inputTokens":"100","input_tokens":500,"outputTokens":0,"output_tokens":9,"cached_tokens":10,"inputTokensDetails":[{"cached_tokens":30}],"cache_creation_input_tokens":5,"cacheWriteInputTokens":"8"},"rawUsage":{"prompt_cache_hit_tokens":20}}}
{"sessionId":"s","timestamp":1700000000000,"providerData":{"messageId":"m","usage":{"inputTokens":999}}}
`)
	d := data.Load(model.WorkBuddy, 1699999999, 1700000001, true)
	check(t, d, 1)
	r := d.Turns[0]
	if r.InputTokens != 70 || r.OutputTokens != 0 || r.CacheReadTokens != 30 || r.CacheCreationTokens != 8 || r.Model != "glm" {
		t.Fatalf("wrong aliases %+v", r)
	}
}
func TestPiAndGrokRecordedCost(t *testing.T) {
	h := env(t)
	write(t, h, ".pi/agent/sessions/s.jsonl", `{"type":"session","id":"s","timestamp":90,"cwd":"/p"}
{"type":"message","timestamp":100,"message":{"role":"assistant","model":"m (high)","provider":"p","usage":{"input":3,"output":4,"cacheRead":5,"cacheWrite":6,"cost":{"total":0}}}}
`)
	d := data.Load(model.Pi, 0, 200, true)
	check(t, d, 1)
	if d.Turns[0].RecordedCost == nil || *d.Turns[0].RecordedCost != 0 || d.Turns[0].Model != "p/m" {
		t.Fatal(d)
	}
	write(t, h, ".grok/sessions/g/summary.json", `{"info":{"id":"g"},"current_model_id":"grok","created_at":90}`)
	write(t, h, ".grok/sessions/g/updates.jsonl", `{"timestamp":100,"params":{"update":{"sessionUpdate":"turn_completed","usage":{"inputTokens":30,"cachedReadTokens":10,"outputTokens":5,"costUsdTicks":2500000000}}}}
`)
	d = data.Load(model.Grok, 0, 200, true)
	check(t, d, 1)
	if d.Turns[0].InputTokens != 20 || d.Turns[0].RecordedCost == nil || *d.Turns[0].RecordedCost != 0.25 {
		t.Fatal(d)
	}
}
func TestOmpTranscript(t *testing.T) {
	h := env(t)
	write(t, h, ".omp/agent/sessions/-work-code-app/2026-11-02T00-54-47-761Z_s1.jsonl", `{"type":"title","v":1,"title":"Fix login bug","source":"auto","updatedAt":"2026-11-02T00:55:00.000Z"}
{"type":"session","version":3,"id":"s1","timestamp":"2026-11-02T00:54:47.761Z","cwd":"/app","title":"Fix login bug"}
{"type":"model_change","timestamp":"2026-11-02T00:54:48.000Z","model":"devin/swe-2"}
{"type":"thinking_level_change","timestamp":"2026-11-02T00:54:48.001Z","thinkingLevel":"high"}
{"type":"message","id":"m1","timestamp":"2026-11-02T00:54:50.000Z","message":{"role":"assistant","model":"swe-2","provider":"devin","usage":{"input":3,"output":4,"cacheRead":5,"cacheWrite":6,"cost":{"total":0.5}},"responseId":"r1","ttft":150,"duration":2000}}
{"type":"message","id":"m2","timestamp":"2026-11-02T00:54:51.000Z","message":{"role":"assistant","model":"swe-2","provider":"devin","usage":{"input":1,"output":2,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.25}},"responseId":"r1","ttft":100,"duration":500}}
`)
	d := data.Load(model.Omp, 0, 2000000000, true)
	check(t, d, 1)
	r := d.Turns[0]
	if r.Model != "devin/swe-2" || r.InputTokens != 3 || r.OutputTokens != 4 || r.CacheReadTokens != 5 || r.CacheCreationTokens != 6 || r.TTFTMs != 150 || r.TotalTimeMs != 2000 || r.RecordedCost == nil || *r.RecordedCost != 0.5 {
		t.Fatalf("wrong omp turn %+v", r)
	}
	s := d.Sessions[0]
	if s.Key != "omp/s1" || s.Title != "Fix login bug" || s.WorkingDirectory != "/app" || s.AgentMode != "high" || s.SelectedModel != "devin/swe-2" {
		t.Fatalf("wrong omp session %+v", s)
	}
}

func TestOmpSideChannelUsage(t *testing.T) {
	h := env(t)
	write(t, h, ".omp/agent/sessions/-work-code-app/2026-11-02T01-00-00-000Z_s2.jsonl", `{"type":"session","version":3,"id":"s2","timestamp":"2026-11-02T01:00:00.000Z","cwd":"/app"}
{"type":"model_usage","id":"u1","timestamp":"2026-11-02T01:01:00.000Z","purpose":"find","role":"typesafe","api":"typesafe","provider":"typesafe","model":"jev-latest","usage":{"input":6661,"output":1220,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.0003}},"stopReason":"stop"}
`)
	d := data.Load(model.Omp, 0, 2000000000, true)
	check(t, d, 1)
	r := d.Turns[0]
	if r.Model != "typesafe/jev-latest" || r.ContextStream != "purpose/find" || r.InputTokens != 6661 || r.OutputTokens != 1220 || r.RecordedCost == nil || *r.RecordedCost != 0.0003 || r.SessionKey != "omp/s2" {
		t.Fatalf("model_usage not counted: %+v", r)
	}
}

func TestOmpForkCopiesDeduped(t *testing.T) {
	h := env(t)
	// createBranchedSession/fork deep-copies the parent's root-to-leaf entries
	// into a new session file with the same entry ids, responseIds and entry
	// timestamps; that usage was billed once. The fork's own follow-up turns
	// (new ids, new timestamps) belong to the fork and must still count.
	const copied = `{"type":"message","id":"m1","timestamp":"2026-11-02T01:01:00Z","message":{"role":"assistant","provider":"devin","model":"swe-2","responseId":"r1","usage":{"input":100,"output":10,"cost":{"total":0.001}}}}`
	const fresh = `{"type":"message","id":"m2","timestamp":"2026-11-02T02:01:00Z","message":{"role":"assistant","provider":"devin","model":"swe-2","responseId":"r2","usage":{"input":7,"output":1,"cost":{"total":0.0001}}}}`
	// Transcript names start with the creation timestamp, so basename order —
	// not file mtime order — decides which file owns a copied entry.
	write(t, h, ".omp/agent/sessions/project/2026-11-02T01-00-00-000Z_parent.jsonl",
		`{"type":"session","id":"parent","timestamp":"2026-11-02T01:00:00Z"}`+"\n"+copied+"\n")
	write(t, h, ".omp/agent/sessions/project/2026-11-02T02-00-00-000Z_fork.jsonl",
		`{"type":"session","id":"fork","timestamp":"2026-11-02T02:00:00Z"}`+"\n"+copied+"\n"+fresh+"\n")
	d := data.Load(model.Omp, 0, 2000000000, true)
	check(t, d, 2)
	if d.Turns[0].SessionKey != "omp/parent" || d.Turns[0].InputTokens != 100 {
		t.Fatalf("copied entry must be owned by the parent session: %+v", d.Turns)
	}
	if d.Turns[1].SessionKey != "omp/fork" || d.Turns[1].InputTokens != 7 {
		t.Fatalf("the fork's own turn must still count: %+v", d.Turns)
	}
	if len(d.Sessions) != 2 {
		t.Fatalf("fork must remain a session: %+v", d.Sessions)
	}
}

func TestOmpResumedSessionDeduped(t *testing.T) {
	h := env(t)
	// Resuming appends the same responseId records to a continued file under
	// the same session id; each billed call must count once.
	const copied = `{"type":"message","id":"m1","timestamp":"2026-11-02T01:01:00Z","message":{"role":"assistant","provider":"devin","model":"swe-2","responseId":"r1","usage":{"input":100,"output":10,"cost":{"total":0.001}}}}`
	write(t, h, ".omp/agent/sessions/project/2026-11-02T01-00-00-000Z_a.jsonl",
		`{"type":"session","id":"s","timestamp":"2026-11-02T01:00:00Z"}`+"\n"+copied+"\n")
	write(t, h, ".omp/agent/sessions/project/2026-11-02T02-00-00-000Z_b.jsonl",
		`{"type":"session","id":"s","timestamp":"2026-11-02T02:00:00Z"}`+"\n"+copied+"\n")
	d := data.Load(model.Omp, 0, 2000000000, true)
	check(t, d, 1)
}

func TestOmpCustomDirectories(t *testing.T) {
	h := env(t)
	// PI_CODING_AGENT_DIR points at the agent dir; sessions live in <dir>/sessions.
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(h, "custom-agent"))
	// PI_CONFIG_DIR replaces the whole ~/.omp root, profiles live inside it.
	t.Setenv("PI_CONFIG_DIR", filepath.Join(h, "my-omp"))
	write(t, h, "custom-agent/sessions/p/2026-11-02T01-00-00-000Z_a.jsonl",
		`{"type":"session","id":"a","timestamp":"2026-11-02T01:00:00Z"}`+"\n")
	write(t, h, "my-omp/agent/sessions/p/2026-11-02T01-00-00-000Z_b.jsonl",
		`{"type":"session","id":"b","timestamp":"2026-11-02T01:00:00Z"}`+"\n")
	write(t, h, "my-omp/profiles/work/agent/sessions/p/2026-11-02T01-00-00-000Z_c.jsonl",
		`{"type":"session","id":"c","timestamp":"2026-11-02T01:00:00Z"}`+"\n")
	// XDG data home flattens the layout to <xdg>/omp/sessions.
	write(t, h, ".local/share/omp/sessions/p/2026-11-02T01-00-00-000Z_d.jsonl",
		`{"type":"session","id":"d","timestamp":"2026-11-02T01:00:00Z"}`+"\n")
	d := data.Load(model.Omp, 0, 2000000000, true)
	if len(d.Sessions) != 4 {
		t.Fatalf("expected sessions from all custom roots, got %+v", d.Sessions)
	}
}

func TestOmpOrchestrationAndZeroCost(t *testing.T) {
	h := env(t)
	write(t, h, ".omp/agent/sessions/p/2026-11-02T01-00-00-000Z_s.jsonl",
		`{"type":"session","id":"s","timestamp":"2026-11-02T01:00:00Z"}`+"\n"+
			`{"type":"message","id":"m1","timestamp":"2026-11-02T01:01:00Z","message":{"role":"assistant","provider":"xai","model":"grok-4-7","responseId":"r1","usage":{"input":10,"output":20,"cacheRead":30,"cacheWrite":0,"orchestration":{"input":15,"output":10,"cacheRead":5,"cacheWrite":1},"cost":{"total":0}}}}`+"\n")
	d := data.Load(model.Omp, 0, 2000000000, true)
	check(t, d, 1)
	r := d.Turns[0]
	if r.InputTokens != 25 || r.OutputTokens != 30 || r.CacheReadTokens != 35 || r.CacheCreationTokens != 1 {
		t.Fatalf("orchestration legs must fold into buckets: %+v", r)
	}
	if r.RecordedCost != nil {
		t.Fatalf("zero recorded cost must stay unknown, got %v", *r.RecordedCost)
	}
}

func TestOmpUsesMessageTimestamp(t *testing.T) {
	h := env(t)
	write(t, h, ".omp/agent/sessions/p/2026-11-02T01-00-00-000Z_ts.jsonl",
		`{"type":"session","id":"ts","timestamp":"2026-11-02T00:00:00Z"}`+"\n"+
			`{"type":"message","id":"m1","timestamp":"2026-11-02T00:00:30Z","message":{"role":"assistant","provider":"devin","model":"swe-2","timestamp":1793542378000,"usage":{"input":1}}}`+"\n")
	d := data.Load(model.Omp, 0, 2000000000, true)
	check(t, d, 1)
	if d.Turns[0].CreatedAt != 1793542378 {
		t.Fatalf("message.timestamp must win over entry time, got %d", d.Turns[0].CreatedAt)
	}
}

func TestOmpAdaptiveModels(t *testing.T) {
	h := env(t)
	path := ".omp/agent/sessions/project/2026-11-02T01-00-00-000Z_router.jsonl"
	transcript := `{"type":"session","id":"router","timestamp":"2026-11-02T01:00:00Z"}
{"type":"model_change","model":"devin/adaptive"}
{"type":"message","id":"m1","timestamp":"2026-11-02T01:01:00Z","message":{"role":"assistant","provider":"devin","model":"adaptive","upstreamModel":"swe-2-medium","usage":{"input":10,"output":2,"cost":{"total":0.1}}}}
{"type":"message","id":"m2","timestamp":"2026-11-02T01:02:00Z","message":{"role":"assistant","provider":"devin","model":"adaptive","upstreamModel":"gpt-6-1-sol-low","usage":{"input":20,"output":3,"cost":{"total":0.2}}}}
{"type":"model_usage","id":"u1","timestamp":"2026-11-02T01:03:00Z","provider":"typesafe","model":"jev-latest","purpose":"find","usage":{"input":5,"cost":{"total":0.01}}}
`
	write(t, h, path, transcript)
	// An otherwise fresh version-5 snapshot must not hide adaptive billing.
	old, err := json.Marshal(map[string]any{"Version": 5, "At": time.Now().Unix(), "Data": model.Data{TurnsStart: 0, TurnsEnd: 2000000000}})
	if err != nil {
		t.Fatal(err)
	}
	write(t, h, "cache/devin-usage-metrics/go-data-omp.json", string(old))
	d := data.Load(model.Omp, 0, 2000000000, false)
	check(t, d, 3)
	s := d.Sessions[0]
	if s.SelectedModel != "devin/adaptive" || s.DisplayModel() != "gpt-6-1-sol-low" {
		t.Fatalf("adaptive selection and upstream model lost: %+v", s)
	}
	for i, want := range []string{"devin/swe-2-medium", "devin/gpt-6-1-sol-low", "typesafe/jev-latest"} {
		if d.Turns[i].Model != want {
			t.Fatalf("turn %d model = %q, want %q", i, d.Turns[i].Model, want)
		}
		if (i < 2) != d.Turns[i].IsAdaptive() {
			t.Fatalf("wrong adaptive billing route: %+v", d.Turns[i])
		}
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	bs := analytics.Build(d, analytics.Day, 0, model.Omp, "", now)
	b := bs[len(bs)-1]
	for name, input := range map[string]float64{"devin/swe-2-medium(adaptive)": 10, "devin/gpt-6-1-sol-low(adaptive)": 20, "typesafe/jev-latest": 5} {
		if b.ByModel[name].Input != input || !b.ByModel[name].Priced {
			t.Fatalf("wrong model attribution for %q: %+v", name, b.ByModel)
		}
	}
	wantCost := (30*.5+5*2)/1e6 + .01
	if len(b.ByModel) != 3 || b.Cost < wantCost-1e-10 || b.Cost > wantCost+1e-10 {
		t.Fatalf("wrong routed model totals: %+v", b)
	}

	for _, tc := range []struct {
		name, tail, selected string
	}{
		{"old-message", `{"type":"message","id":"old","timestamp":"2026-11-02T01:04:00Z","message":{"role":"assistant","provider":"devin","model":"adaptive","usage":{"input":7}}}`, "devin/adaptive"},
		{"switch-model", `{"type":"model_change","model":"devin/swe-2"}`, "devin/swe-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(t, h, path, transcript+tc.tail+"\n")
			updated := data.Load(model.Omp, 0, 2000000000, true)
			if len(updated.Sessions) != 1 || updated.Sessions[0].SelectedModel != tc.selected || updated.Sessions[0].RealModel != nil {
				t.Fatalf("stale upstream model after %s: %+v", tc.name, updated.Sessions)
			}
			if tc.name == "old-message" {
				last := updated.Turns[len(updated.Turns)-1]
				if last.Model != "devin/adaptive" {
					t.Fatalf("old message borrowed a previous upstream model: %+v", last)
				}
			}
		})
	}
}

func TestSQLiteImportersReadOnlyAndParentRollup(t *testing.T) {
	for _, a := range []model.Agent{model.OpenCode, model.MimoCode, model.ZCode} {
		t.Run(string(a), func(t *testing.T) {
			h := env(t)
			p := ".local/share/" + string(a) + "/" + string(a) + ".db"
			if a == model.ZCode {
				p = ".zcode/cli/db/db.sqlite"
			}
			conn := db(t, h, p)
			exec(t, conn, "CREATE TABLE session(id TEXT,parent_id TEXT,directory TEXT,title TEXT,time_created INTEGER,time_updated INTEGER)")
			exec(t, conn, "INSERT INTO session VALUES ('root',NULL,'/p','Root',90,120),('child','root','/p','Child',95,110)")
			if a == model.ZCode {
				exec(t, conn, "CREATE TABLE model_usage(session_id TEXT,status TEXT,started_at INTEGER,completed_at INTEGER,model_id TEXT,input_tokens REAL,output_tokens REAL,cache_creation_input_tokens REAL,cache_read_input_tokens REAL,time_to_first_token_ms REAL)")
				exec(t, conn, "INSERT INTO model_usage VALUES ('child','completed',100,110,'m',12,3,4,5,6)")
			} else {
				exec(t, conn, "CREATE TABLE message(session_id TEXT,time_created INTEGER,data TEXT)")
				exec(t, conn, "INSERT INTO message VALUES (?,?,?)", "child", 100, `{"role":"assistant","modelID":"m","providerID":"p","tokens":{"input":12,"output":-3,"cache":{"read":5,"write":4}},"cost":0.5}`)
			}
			conn.Close()
			before, e := os.ReadFile(filepath.Join(h, p))
			if e != nil {
				t.Fatal(e)
			}
			d := data.Load(a, 0, 200, true)
			check(t, d, 1)
			if len(d.Sessions) != 1 || d.Turns[0].SessionKey != string(a)+"/root" {
				t.Fatal(d)
			}
			after, _ := os.ReadFile(filepath.Join(h, p))
			if string(before) != string(after) {
				t.Fatal("importer modified database")
			}
			if a == model.ZCode && d.Turns[0].InputTokens != 7 {
				t.Fatal(d)
			}
			if a != model.ZCode && (d.Turns[0].OutputTokens != 0 || d.Turns[0].RecordedCost == nil) {
				t.Fatal(d)
			}
		})
	}
}
func TestDevinHiddenDedupMetadataTimeAndNullTTFT(t *testing.T) {
	h := env(t)
	conn := db(t, h, ".local/share/devin/cli/sessions.db")
	exec(t, conn, "CREATE TABLE sessions(id TEXT,title TEXT,created_at INTEGER,last_activity_at INTEGER,hidden INTEGER)")
	exec(t, conn, "INSERT INTO sessions VALUES ('s','hidden',90,200,1)")
	exec(t, conn, "CREATE TABLE message_nodes(session_id TEXT,created_at INTEGER,chat_message TEXT)")
	for _, at := range []int{1000, 2000} {
		exec(t, conn, "INSERT INTO message_nodes VALUES (?,?,?)", "s", at, `{"message_id":"m","metadata":{"started_generation_at":"1970-01-01T00:01:40Z","metrics":{"ttft_ms":null,"input_tokens":3,"cache_creation_tokens":8}}}`)
	}
	d := data.Load(model.Devin, 0, 200, true)
	check(t, d, 1)
	if len(d.Sessions) != 0 || d.Turns[0].CreatedAt != 100 || d.Turns[0].CacheCreationTokens != 8 {
		t.Fatal(d)
	}
}

func TestDevinAdaptiveBilling(t *testing.T) {
	h := env(t)
	conn := db(t, h, ".local/share/devin/cli/sessions.db")
	exec(t, conn, "CREATE TABLE sessions(id TEXT,title TEXT,model TEXT,created_at INTEGER,last_activity_at INTEGER,hidden INTEGER)")
	exec(t, conn, "INSERT INTO sessions VALUES ('s','routed','adaptive',90,200,0)")
	exec(t, conn, "CREATE TABLE message_nodes(session_id TEXT,created_at INTEGER,chat_message TEXT)")
	for i, name := range []string{"gpt-6-1-sol-low", "compactor"} {
		body := fmt.Sprintf(`{"message_id":"m%d","metadata":{"generation_model":%q,"metrics":{"ttft_ms":1,"input_tokens":100,"output_tokens":20,"cache_read_tokens":300,"cache_creation_tokens":40}}}`, i, name)
		exec(t, conn, "INSERT INTO message_nodes VALUES (?,?,?)", "s", 100+i, body)
	}
	d := data.Load(model.Devin, 0, 200, true)
	check(t, d, 2)
	if d.Turns[0].Model != "gpt-6-1-sol-low" || d.Turns[0].BillingModel != "adaptive" || d.Turns[1].BillingModel != "" {
		t.Fatalf("display model and billing route were conflated: %+v", d.Turns)
	}
	cost, ok := pricing.Cost(d.Turns[0], "")
	if !ok || cost < .00014-1e-10 || cost > .00014+1e-10 {
		t.Fatalf("Devin adaptive priced using its upstream: %g, %v", cost, ok)
	}
}

// Bodies decode in parallel batches; the first occurrence must still win, as
// in a serial scan, and a malformed body must still surface as an error.
func TestDevinParallelDecodeKeepsRowOrder(t *testing.T) {
	h := env(t)
	conn := db(t, h, ".local/share/devin/cli/sessions.db")
	exec(t, conn, "CREATE TABLE sessions(id TEXT,title TEXT,created_at INTEGER,last_activity_at INTEGER,hidden INTEGER)")
	exec(t, conn, "INSERT INTO sessions VALUES ('s','s',90,200,0)")
	exec(t, conn, "CREATE TABLE message_nodes(session_id TEXT,created_at INTEGER,chat_message TEXT)")
	exec(t, conn, "BEGIN")
	for i := range 1000 {
		exec(t, conn, "INSERT INTO message_nodes VALUES ('s',100,?)", fmt.Sprintf(`{"message_id":"m%d","content":%q,"metadata":{"metrics":{"ttft_ms":1,"input_tokens":%d}}}`, i%300, strings.Repeat("x", 8<<10), i))
	}
	exec(t, conn, "COMMIT")
	d := data.Load(model.Devin, 0, 200, true)
	check(t, d, 300)
	var tokens float64
	for _, turn := range d.Turns {
		tokens += turn.InputTokens
	}
	if tokens != 299*300/2 {
		t.Fatalf("later duplicates replaced first rows: %v", tokens)
	}
	exec(t, conn, "INSERT INTO message_nodes VALUES ('s',100,?)", `{"ttft_ms":`)
	if d = data.Load(model.Devin, 0, 200, true); len(d.Errors) != 1 || len(d.Turns) != 300 {
		t.Fatalf("errors=%v turns=%d", d.Errors, len(d.Turns))
	}
}
func TestAntigravityFullUsageSuppressesEcho(t *testing.T) {
	h := env(t)
	conn := db(t, h, ".gemini/antigravity/conversations/s.db")
	exec(t, conn, "CREATE TABLE trajectory_metadata_blob(id TEXT,data BLOB)")
	exec(t, conn, "CREATE TABLE gen_metadata(idx INTEGER,data BLOB)")
	exec(t, conn, "CREATE TABLE steps(idx INTEGER,step_type INTEGER,step_payload BLOB,metadata BLOB)")
	// Usage: {1: model ID, 2: input, 3: output with thinking, 4: cache
	// write, 5: cache read, 7: message ID, 9: thinking, 10: visible output}.
	usage := []byte{8, 3, 16, 4, 24, 7, 32, 1, 40, 6, 58, 1, 'a', 72, 2, 80, 5}
	exec(t, conn, "INSERT INTO steps VALUES (1,0,NULL,?)", append([]byte{10, 2, 8, 100, 74, byte(len(usage))}, usage...))
	// The same message on a later step counts once.
	exec(t, conn, "INSERT INTO steps VALUES (2,0,NULL,?)", append([]byte{10, 2, 8, 101, 74, byte(len(usage))}, usage...))
	exec(t, conn, "INSERT INTO steps VALUES (3,0,NULL,?)", []byte{10, 2, 8, 100, 88, 3})
	// A retried attempt carries its usage in field 28's field 2.
	retry := []byte{16, 9, 24, 2, 58, 1, 'b'}
	exec(t, conn, "INSERT INTO steps VALUES (4,0,NULL,?)", append([]byte{10, 2, 8, 102, 226, 1, byte(len(retry) + 2), 18, byte(len(retry))}, retry...))
	d := data.Load(model.Antigravity, 0, 200, true)
	check(t, d, 2)
	if d.Turns[0].InputTokens != 4 || d.Turns[0].OutputTokens != 7 || d.Turns[0].CacheReadTokens != 6 || d.Turns[0].CacheCreationTokens != 1 {
		t.Fatal(d)
	}
	if d.Turns[1].InputTokens != 9 || d.Turns[1].OutputTokens != 2 || d.Turns[1].CreatedAt != 102 {
		t.Fatal(d)
	}
}
func TestAntigravityIncludesCLIStore(t *testing.T) {
	h := env(t)
	desktop := db(t, h, ".gemini/antigravity/conversations/desktop.db")
	cli := db(t, h, ".gemini/antigravity-cli/conversations/cli.db")
	for _, conn := range []*sqlite.DB{desktop, cli} {
		exec(t, conn, "CREATE TABLE trajectory_metadata_blob(id TEXT,data BLOB)")
		exec(t, conn, "CREATE TABLE gen_metadata(idx INTEGER,data BLOB)")
		exec(t, conn, "CREATE TABLE steps(idx INTEGER,step_type INTEGER,step_payload BLOB,metadata BLOB)")
	}
	usage := []byte{8, 3, 16, 4, 24, 7, 32, 1, 40, 6, 58, 1, 'a', 72, 2, 80, 5}
	step := append([]byte{10, 2, 8, 100, 74, byte(len(usage))}, usage...)
	exec(t, desktop, "INSERT INTO steps VALUES (1,0,NULL,?)", step)
	exec(t, cli, "INSERT INTO steps VALUES (1,0,NULL,?)", step)
	d := data.Load(model.Antigravity, 0, 200, true)
	check(t, d, 2)
	sessions := map[string]model.Session{}
	for _, s := range d.Sessions {
		sessions[s.ID] = s
	}
	if s, ok := sessions["desktop"]; !ok || s.Source != "app" || s.Key != "antigravity/desktop" {
		t.Fatalf("desktop session: %+v", sessions)
	}
	if s, ok := sessions["cli"]; !ok || s.Source != "cli" || s.Key != "antigravity-cli/cli" {
		t.Fatalf("cli session: %+v", sessions)
	}
	for _, turn := range d.Turns {
		if turn.SessionKey != "antigravity/desktop" && turn.SessionKey != "antigravity-cli/cli" {
			t.Fatalf("unexpected turn key %q", turn.SessionKey)
		}
	}
}
func TestCacheRefreshExpiryAndDevice(t *testing.T) {
	h := env(t)
	p := ".pi/agent/sessions/s.jsonl"
	line := `{"type":"message","timestamp":100,"message":{"role":"assistant","usage":{"input":1}}}` + "\n"
	write(t, h, p, line)
	d := data.Load(model.Pi, 0, 200, true)
	check(t, d, 1)
	id := data.DeviceID()
	setDeviceName(t, "  Laptop  ")
	if data.DeviceID() != id || data.DeviceName() != "Laptop" {
		t.Fatal("identity changed")
	}
	write(t, h, p, line+line)
	check(t, data.Load(model.Pi, 0, 200, false), 1)
	check(t, data.Load(model.Pi, 0, 200, true), 2)
	cache := filepath.Join(h, "cache/devin-usage-metrics/go-data-pi.json")
	b, e := os.ReadFile(cache)
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	v["At"] = time.Now().Unix() - 301
	b, _ = json.Marshal(v)
	if e = os.WriteFile(cache, b, 0600); e != nil {
		t.Fatal(e)
	}
	write(t, h, p, line+line+line)
	check(t, data.Load(model.Pi, 0, 200, false), 3)
	write(t, h, p, "malformed\n")
	d = data.Load(model.Pi, 0, 200, true)
	if len(d.Errors) == 0 {
		t.Fatal("malformed input silently dropped")
	}
	setDeviceName(t, "")
	if data.DeviceID() != id || data.DeviceName() == "" {
		t.Fatal("reset name broke identity")
	}
}
func TestLoadAllReportsMissingSources(t *testing.T) {
	env(t)
	d := data.LoadAll(0, 200, true)
	seen := map[model.Agent]bool{}
	for _, e := range d.Errors {
		seen[e.Agent] = true
	}
	for _, a := range model.Agents {
		if !seen[a] {
			t.Fatalf("missing source unreported: %s", a)
		}
	}
}

func TestOpenCodeV2AndDatabaseErrors(t *testing.T) {
	h := env(t)
	conn := db(t, h, ".local/share/opencode/opencode.db")
	exec(t, conn, "CREATE TABLE session_v2(id TEXT,parent_id TEXT,directory TEXT,title TEXT,agent TEXT,model TEXT,time_created INTEGER,time_updated INTEGER)")
	exec(t, conn, "INSERT INTO session_v2 VALUES ('s',NULL,'/project','v2','build','{}',90,110)")
	exec(t, conn, "CREATE TABLE session_message(session_id TEXT,time_created INTEGER,type TEXT,data TEXT)")
	exec(t, conn, "INSERT INTO session_message VALUES ('s',100,'assistant',?)", `{"model":{"id":"m","providerID":"p"},"tokens":{"input":2,"output":3,"cache":{"read":4,"write":5}},"cost":0.2}`)
	d := data.Load(model.OpenCode, 0, 200, true)
	check(t, d, 1)
	if d.Turns[0].Model != "p/m" || d.Turns[0].RecordedCost == nil || *d.Turns[0].RecordedCost != 0.2 {
		t.Fatal(d)
	}
	exec(t, conn, "DROP TABLE session_message")
	d = data.Load(model.OpenCode, 0, 200, true)
	if len(d.Errors) == 0 {
		t.Fatal("schema/query failure silently dropped")
	}
}

func TestExpiredUnchangedAndDeletedFiles(t *testing.T) {
	h := env(t)
	line := `{"type":"assistant","timestamp":100,"message":{"id":"m","usage":{"input_tokens":2}}}` + "\n"
	p := ".claude/projects/p/s.jsonl"
	write(t, h, p, line)
	check(t, data.Load(model.Claude, 0, 200, true), 1)
	expire := func() {
		t.Helper()
		cache := filepath.Join(h, "cache/devin-usage-metrics/go-data-claude.json")
		b, e := os.ReadFile(cache)
		if e != nil {
			t.Fatal(e)
		}
		var v map[string]any
		if e = json.Unmarshal(b, &v); e != nil {
			t.Fatal(e)
		}
		v["At"] = time.Now().Unix() - 301
		b, _ = json.Marshal(v)
		if e = os.WriteFile(cache, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	expire()
	check(t, data.Load(model.Claude, 0, 200, false), 1)
	write(t, h, ".claude/projects/p/resume.jsonl", line)
	expire()
	check(t, data.Load(model.Claude, 0, 200, false), 1)
	if e := os.Remove(filepath.Join(h, p)); e != nil {
		t.Fatal(e)
	}
	expire()
	d := data.Load(model.Claude, 0, 200, false)
	check(t, d, 1)
	if d.Turns[0].SessionKey != "claude-code/resume" {
		t.Fatal("deleted session retained", d)
	}
}

func TestGrokOptionalUpdatesAndUsageMatchRust(t *testing.T) {
	h := env(t)
	write(t, h, ".grok/sessions/summary-only/summary.json", `{"info":{"id":"summary-only"},"created_at":1700000000}`)
	write(t, h, ".grok/sessions/with-updates/summary.json", `{"info":{"id":"with-updates"},"created_at":1700000000}`)
	write(t, h, ".grok/sessions/with-updates/updates.jsonl", `{"timestamp":1700000001,"params":{"update":{"sessionUpdate":"turn_completed"}}}
{"timestamp":1700000002,"params":{"update":{"sessionUpdate":"turn_completed","usage":{"inputTokens":10,"outputTokens":2}}}}
`)
	d := data.Load(model.Grok, 0, 1800000000, true)
	if len(d.Errors) != 0 || len(d.Sessions) != 2 || len(d.Turns) != 1 {
		t.Fatalf("summary-only sessions and unmetered events: sessions=%d turns=%d errors=%d", len(d.Sessions), len(d.Turns), len(d.Errors))
	}
}

func TestWorkBuddyMissingSessionIDUsesFileNamePerRecord(t *testing.T) {
	h := env(t)
	write(t, h, ".workbuddy/projects/project/fallback.jsonl", `{"sessionId":"explicit","timestamp":1700000000,"providerData":{"usage":{"inputTokens":10,"outputTokens":2}}}
{"type":"ai-title","aiTitle":"Fallback title","timestamp":1700000001}
`)
	d := data.Load(model.WorkBuddy, 0, 1800000000, true)
	if len(d.Sessions) != 2 || len(d.Turns) != 1 {
		t.Fatalf("fallback metadata session lost: sessions=%d turns=%d", len(d.Sessions), len(d.Turns))
	}
	found := false
	for _, s := range d.Sessions {
		if s.ID == "fallback" && s.Title == "Fallback title" {
			found = true
		}
	}
	if !found {
		t.Fatal("record without sessionId inherited the previous record's ID")
	}
}

func TestLegacySnapshotCannotHideImporterFixes(t *testing.T) {
	h := env(t)
	write(t, h, ".workbuddy/projects/project/fallback.jsonl", `{"type":"ai-title","aiTitle":"Recovered session","timestamp":1700000000}`)
	legacy := map[string]any{"Version": 1, "At": time.Now().Unix(), "Data": model.Data{TurnsStart: 0, TurnsEnd: 1800000000}}
	b, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	write(t, h, "cache/devin-usage-metrics/go-data-workbuddy.json", string(b))
	d := data.Load(model.WorkBuddy, 0, 1800000000, false)
	if len(d.Sessions) != 1 {
		t.Fatal("legacy cache hid corrected importer output")
	}
}

// setDeviceName writes a name into device.json as the Rust app's device
// settings did; the Go app honours it but has no editor for it.
func setDeviceName(t *testing.T, name string) {
	t.Helper()
	dir, _ := os.UserConfigDir()
	p := filepath.Join(dir, "devin-usage-metrics", "device.json")
	v := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &v)
	}
	v["name"] = name
	b, _ := json.Marshal(v)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDimModelUsageTurns(t *testing.T) {
	h := env(t)
	conn := db(t, h, ".dimcode-alpha/rust/dimcode.sqlite")
	exec(t, conn, "CREATE TABLE agent_sessions (session_id TEXT PRIMARY KEY, config TEXT, created_at_ms INTEGER, origin TEXT)")
	exec(t, conn, "CREATE TABLE session_metadata (session_id TEXT PRIMARY KEY, revision INTEGER, title TEXT, title_source TEXT)")
	exec(t, conn, "CREATE TABLE session_catalog (session_id TEXT PRIMARY KEY, updated_at_ms INTEGER, preview TEXT, cwd TEXT)")
	exec(t, conn, "CREATE TABLE timeline_events (session_id TEXT, branch_id TEXT, sequence INTEGER, last_sequence INTEGER, kind TEXT, body TEXT, event_id TEXT)")
	exec(t, conn, "INSERT INTO agent_sessions VALUES ('sess-1', ?, 1700000000000, 'client')", `{"model":{"providerId":"grok-build","modelId":"grok-4.6"},"cwd":"/work"}`)
	exec(t, conn, "INSERT INTO agent_sessions VALUES ('sess-empty', '{}', 1700000000000, 'client')")
	exec(t, conn, "INSERT INTO session_metadata VALUES ('sess-1', 1, 'Ask identity', 'auto')")
	exec(t, conn, "INSERT INTO session_catalog VALUES ('sess-1', 1700000100000, 'hello', '/work')")
	call := `{"encoding":"event","record":{"event":{"id":"c1","sequence":1,"create_at":1700000050000,"type":"model","name":"call","data":{"turn_id":"t1","request_id":"req-1","request":{"providerId":"grok-build","modelId":"grok-4.6"}}}}}`
	usage := `{"encoding":"event","record":{"event":{"id":"u1","sequence":2,"create_at":1700000060000,"type":"model","name":"usage","data":{"turn_id":"t1","request_id":"req-1","usage":{"promptTokens":100,"completionTokens":20,"totalTokens":120,"cacheReadTokens":30}}}}}`
	dup := `{"encoding":"event","record":{"event":{"id":"u2","sequence":3,"create_at":1700000061000,"type":"model","name":"usage","data":{"turn_id":"t1","request_id":"req-1","usage":{"promptTokens":100,"completionTokens":20,"totalTokens":120,"cacheReadTokens":30}}}}}`
	exec(t, conn, "INSERT INTO timeline_events VALUES ('sess-1','main',1,1,'model/call',?, 'c1')", call)
	exec(t, conn, "INSERT INTO timeline_events VALUES ('sess-1','main',2,2,'model/usage',?, 'u1')", usage)
	exec(t, conn, "INSERT INTO timeline_events VALUES ('sess-1','main',3,3,'model/usage',?, 'u2')", dup)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	d := data.Load(model.Dim, 0, 2000000000, true)
	check(t, d, 1)
	if len(d.Sessions) != 1 || d.Sessions[0].Title != "Ask identity" || d.Sessions[0].WorkingDirectory != "/work" || d.Sessions[0].Key != "dimcode-alpha/sess-1" {
		t.Fatalf("session %+v", d.Sessions)
	}
	r := d.Turns[0]
	if r.InputTokens != 70 || r.OutputTokens != 20 || r.CacheReadTokens != 30 || r.Model != "grok-build/grok-4.6" || r.DedupKey != "req-1" {
		t.Fatalf("turn %+v", r)
	}
}
