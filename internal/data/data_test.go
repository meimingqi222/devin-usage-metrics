package data_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
)

func env(t testing.TB) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("USERPROFILE", h)
	t.Setenv("APPDATA", filepath.Join(h, ".local/share"))
	t.Setenv("LOCALAPPDATA", filepath.Join(h, "cache"))
	for k, v := range map[string]string{"HOME": h, "XDG_CONFIG_HOME": filepath.Join(h, "config"), "XDG_CACHE_HOME": filepath.Join(h, "cache"), "XDG_DATA_HOME": filepath.Join(h, ".local/share"), "CODEX_HOME": filepath.Join(h, ".codex"), "WORKBUDDY_CONFIG_DIR": "", "WORKBUDDY_DIR": "", "CODEBUDDY_CONFIG_DIR": ""} {
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
func TestAntigravityFullUsageSuppressesEcho(t *testing.T) {
	h := env(t)
	conn := db(t, h, ".gemini/antigravity/conversations/s.db")
	exec(t, conn, "CREATE TABLE trajectory_metadata_blob(id TEXT,data BLOB)")
	exec(t, conn, "CREATE TABLE gen_metadata(idx INTEGER,data BLOB)")
	exec(t, conn, "CREATE TABLE steps(idx INTEGER,step_type INTEGER,step_payload BLOB,metadata BLOB)")
	exec(t, conn, "INSERT INTO steps VALUES (1,0,NULL,?)", []byte{10, 2, 8, 100, 74, 10, 8, 3, 16, 4, 24, 5, 40, 6, 80, 7})
	exec(t, conn, "INSERT INTO steps VALUES (2,0,NULL,?)", []byte{10, 2, 8, 100, 88, 3})
	d := data.Load(model.Antigravity, 0, 200, true)
	check(t, d, 1)
	if d.Turns[0].InputTokens != 3 || d.Turns[0].OutputTokens != 11 || d.Turns[0].CacheReadTokens != 6 || d.Turns[0].CacheCreationTokens != 5 {
		t.Fatal(d)
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
