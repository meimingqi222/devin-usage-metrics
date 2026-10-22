package data_test

import (
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
)

func devinDB(t testing.TB, h, source string) *sqlite.DB {
	t.Helper()
	c := db(t, h, ".local/share/devin/"+source+"/sessions.db")
	exec(t, c, "CREATE TABLE sessions(id TEXT,title TEXT,created_at INTEGER,last_activity_at INTEGER,hidden INTEGER)")
	exec(t, c, "CREATE TABLE message_nodes(session_id TEXT,created_at INTEGER,chat_message TEXT)")
	exec(t, c, "CREATE INDEX message_session_time ON message_nodes(session_id,created_at)")
	return c
}

func expireDevin(t testing.TB, h string) {
	t.Helper()
	p := filepath.Join(h, "cache/devin-usage-metrics/go-data-devin.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]json.RawMessage
	if err = json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	s["At"], _ = json.Marshal(time.Now().Unix() - 600)
	b, err = json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}

// A separate process cannot use parsedDatabases or any decoded-file memory.
func TestDevinSnapshotChild(t *testing.T) {
	result := os.Getenv("AUM_TEST_DEVIN_RESULT")
	if result == "" {
		t.Skip("subprocess entry point")
	}
	d := data.Load(model.Devin, 0, time.Now().Add(time.Hour).Unix(), false)
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(result, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func restartedDevin(t testing.TB, result string) model.Data {
	t.Helper()
	cmd := osexec.Command(os.Args[0], "-test.run=^TestDevinSnapshotChild$")
	cmd.Env = append(os.Environ(), "AUM_TEST_DEVIN_RESULT="+result)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	b, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var d model.Data
	if err = json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDevinExpiredSnapshotAcrossRestart(t *testing.T) {
	h := env(t)
	now := time.Now().Unix()
	message := func(id string, at int64, tokens int) string {
		// Unrelated nested metrics must not suppress valid inference accounting.
		return fmt.Sprintf(`{"message_id":%q,"other":{"metrics":null},"metadata":{"started_generation_at":%d,"metrics":{"ttft_ms":null,"input_tokens":%d}}}`, id, at, tokens)
	}
	var cli *sqlite.DB
	for _, source := range []string{"cli", "cli-next"} {
		c := devinDB(t, h, source)
		if source == "cli" {
			cli = c
		}
		for i, id := range []string{"active", "stale", "hidden"} {
			at := now - int64(i)*10*86400
			exec(t, c, "INSERT INTO sessions VALUES (?,?,?,?,?)", id, id, at, at, i/2)
			exec(t, c, "INSERT INTO message_nodes VALUES (?,?,?)", id, at, message(id, at, []int{1, 3, 7}[i]))
		}
	}
	check(t, data.Load(model.Devin, 0, now+3600, true), 6)
	exec(t, cli, "INSERT INTO message_nodes VALUES ('active',?,?)", now, message("new", now, 13))
	exec(t, cli, "UPDATE sessions SET last_activity_at=? WHERE id='active'", now)
	// A late persisted duplicate must not double-count an already cached inference.
	exec(t, cli, "INSERT INTO message_nodes VALUES ('active',?,?)", now, message("active", now, 1))
	exec(t, cli, "DELETE FROM sessions WHERE id='stale'")
	// New sessions may contain history older than the global watermark.
	exec(t, cli, "INSERT INTO sessions VALUES ('imported','old import',?,?,0)", now-30*86400, now-30*86400)
	exec(t, cli, "INSERT INTO message_nodes VALUES ('imported',?,?)", now-30*86400, message("imported", now-30*86400, 17))
	expireDevin(t, h)
	d := restartedDevin(t, filepath.Join(h, "result.json"))
	check(t, d, 7)
	var tokens float64
	for _, turn := range d.Turns {
		tokens += turn.InputTokens
	}
	if tokens != 49 {
		t.Fatalf("tokens=%v, want 49", tokens)
	}
	full := data.Load(model.Devin, 0, d.TurnsEnd, true)
	check(t, full, 7)
	if !reflect.DeepEqual(d.Sessions, full.Sessions) {
		t.Fatal("incremental session metadata differs from full scan")
	}
	// Compare logical records; duplicate message persistence can change row times,
	// but must not change inference accounting.
	counts := func(d model.Data) map[string]float64 {
		m := map[string]float64{}
		for _, turn := range d.Turns {
			m[turn.SessionKey+"/"+turn.DedupKey] += turn.InputTokens
		}
		return m
	}
	if !reflect.DeepEqual(counts(d), counts(full)) {
		t.Fatal("incremental accounting differs from full scan")
	}
}

func BenchmarkDevinExpiredSnapshot(b *testing.B) {
	h := env(b)
	now := time.Now().Unix()
	var cli *sqlite.DB
	for _, source := range []string{"cli", "cli-next"} {
		c := devinDB(b, h, source)
		if source == "cli" {
			cli = c
		}
		for i := 0; i < 100; i++ {
			id := fmt.Sprint(i)
			at := now - int64(i)*86400
			exec(b, c, "INSERT INTO sessions VALUES (?,?,?,?,0)", id, id, at, at)
			for j := 0; j < 10; j++ {
				msg := fmt.Sprintf(`{"message_id":"%d-%d","metadata":{"started_generation_at":%d,"metrics":{"ttft_ms":null,"input_tokens":3}}}`, i, j, at)
				exec(b, c, "INSERT INTO message_nodes VALUES (?,?,?)", id, at, msg)
			}
			exec(b, c, "INSERT INTO message_nodes VALUES (?,?,?)", id, at, `{"metadata":{"metrics":null},"content":"`+strings.Repeat("x", 256*1024)+`"}`)
		}
	}
	check(b, data.Load(model.Devin, 0, now+3600, true), 2000)
	result := filepath.Join(h, "result.json")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		at := now + int64(i) + 1
		msg := fmt.Sprintf(`{"message_id":"added-%d","metadata":{"started_generation_at":%d,"metrics":{"ttft_ms":null,"input_tokens":5}}}`, i, at)
		exec(b, cli, "INSERT INTO message_nodes VALUES ('0',?,?)", at, msg)
		exec(b, cli, "UPDATE sessions SET last_activity_at=? WHERE id='0'", at)
		expireDevin(b, h)
		b.StartTimer()
		check(b, restartedDevin(b, result), 2001+i)
	}
}

// ageDevinWindow rewrites the snapshot as if it had been written more than an
// hour ago: its cached window no longer reaches the current time.
func ageDevinWindow(t testing.TB, h string, at, turnsEnd int64) {
	t.Helper()
	p := filepath.Join(h, "cache/devin-usage-metrics/go-data-devin.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]json.RawMessage
	if err = json.Unmarshal(b, &snapshot); err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err = json.Unmarshal(snapshot["Data"], &data); err != nil {
		t.Fatal(err)
	}
	data["turns_end"], _ = json.Marshal(turnsEnd)
	if snapshot["Data"], err = json.Marshal(data); err != nil {
		t.Fatal(err)
	}
	snapshot["At"], _ = json.Marshal(at)
	if b, err = json.Marshal(snapshot); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
}

// A snapshot whose window ended in the past is still a valid incremental
// baseline: an app restarted after an hour must not fall back to a full scan.
func TestDevinStaleWindowSnapshotSeedsIncrementalParse(t *testing.T) {
	h := env(t)
	now := time.Now().Unix()
	message := func(id string, at int64, tokens int) string {
		return fmt.Sprintf(`{"message_id":%q,"metadata":{"started_generation_at":%d,"metrics":{"ttft_ms":null,"input_tokens":%d}}}`, id, at, tokens)
	}
	var cli *sqlite.DB
	for _, source := range []string{"cli", "cli-next"} {
		c := devinDB(t, h, source)
		if source == "cli" {
			cli = c
		}
		for i, id := range []string{"active", "older"} {
			at := now - int64(i+3)*3600
			exec(t, c, "INSERT INTO sessions VALUES (?,?,?,?,?)", id, id, at, at, 0)
			exec(t, c, "INSERT INTO message_nodes VALUES (?,?,?)", id, at, message(id, at, []int{1, 3}[i]))
		}
	}
	check(t, data.Load(model.Devin, 0, now+3600, true), 4)
	exec(t, cli, "INSERT INTO message_nodes VALUES ('active',?,?)", now, message("new", now, 13))
	exec(t, cli, "UPDATE sessions SET last_activity_at=? WHERE id='active'", now)
	ageDevinWindow(t, h, now-7200, now-3600)
	d := restartedDevin(t, filepath.Join(h, "result.json"))
	check(t, d, 5)
	var tokens float64
	for _, turn := range d.Turns {
		tokens += turn.InputTokens
	}
	// The fixture writes the same sessions into both databases.
	if tokens != 21 {
		t.Fatalf("tokens=%v, want 21", tokens)
	}
	full := data.Load(model.Devin, 0, d.TurnsEnd, true)
	check(t, full, 5)
	counts := func(d model.Data) map[string]float64 {
		m := map[string]float64{}
		for _, turn := range d.Turns {
			m[turn.SessionKey+"/"+turn.DedupKey] += turn.InputTokens
		}
		return m
	}
	if !reflect.DeepEqual(counts(d), counts(full)) {
		t.Fatal("incremental accounting differs from full scan")
	}
}

func TestReloadRebuildsHistoricalDevinEdits(t *testing.T) {
	h := env(t)
	now := time.Now().Unix()
	c := devinDB(t, h, "cli")
	msg := func(id string, n int) string {
		return fmt.Sprintf(`{"message_id":%q,"metadata":{"metrics":{"ttft_ms":null,"input_tokens":%d}}}`, id, n)
	}
	for _, s := range []struct {
		id string
		at int64
	}{{"old", now - 30*86400}, {"new", now}} {
		exec(t, c, "INSERT INTO sessions VALUES(?,?,?,?,0)", s.id, s.id, s.at, s.at)
		exec(t, c, "INSERT INTO message_nodes VALUES(?,?,?)", s.id, s.at, msg(s.id, 1))
	}
	check(t, data.Load(model.Devin, 0, now+3600, true), 2)
	// The old session lies outside the ordinary incremental query overlap.
	exec(t, c, "UPDATE message_nodes SET chat_message=? WHERE session_id='old'", msg("old", 99))
	d := data.Reload(model.Devin, 0, now+3600)
	check(t, d, 2)
	var tokens float64
	for _, turn := range d.Turns {
		tokens += turn.InputTokens
	}
	if tokens != 100 {
		t.Fatalf("manual reload kept old historical metrics: %v", tokens)
	}
}

// Normal background collection can keep old inferences outside the overlap;
// manual Reload must still rebuild them when the database signature changes.
func TestDevinStaleWindowKeepsIncrementalBaseline(t *testing.T) {
	h := env(t)
	now := time.Now().Unix()
	c := devinDB(t, h, "cli")
	for _, s := range []struct {
		id string
		at int64
	}{{"old", now - 30*86400}, {"new", now - 7200}} {
		exec(t, c, "INSERT INTO sessions VALUES(?,?,?,?,0)", s.id, s.id, s.at, s.at)
		msg := fmt.Sprintf(`{"message_id":%q,"metadata":{"metrics":{"ttft_ms":null,"input_tokens":1}}}`, s.id)
		exec(t, c, "INSERT INTO message_nodes VALUES(?,?,?)", s.id, s.at, msg)
	}
	check(t, data.Load(model.Devin, 0, now+3600, true), 2)
	// Removing a historical message distinguishes an incremental read from a
	// full scan: the former keeps its previously accounted inference.
	exec(t, c, "DELETE FROM message_nodes WHERE session_id='old'")
	ageDevinWindow(t, h, now-7200, now-3600)
	incremental := restartedDevin(t, filepath.Join(h, "result.json"))
	if len(incremental.Errors) != 0 || len(incremental.Turns) != 2 {
		t.Fatalf("expired window discarded baseline: turns=%d errors=%d", len(incremental.Turns), len(incremental.Errors))
	}
	reloaded := data.Reload(model.Devin, 0, now+3600)
	if len(reloaded.Errors) != 0 || len(reloaded.Turns) != 1 {
		t.Fatalf("manual reload reused incremental history: turns=%d errors=%d", len(reloaded.Turns), len(reloaded.Errors))
	}
}
