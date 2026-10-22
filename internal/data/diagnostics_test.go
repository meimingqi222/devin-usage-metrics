package data_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

func TestCollectionDiagnosticsCorrelateSourcesWithoutPayloads(t *testing.T) {
	h := env(t)
	secret := "private-session-body-and-title"
	c := devinDB(t, h, "cli")
	exec(t, c, "INSERT INTO sessions VALUES ('s',?,90,100,0)", secret)
	body := `{"message_id":"m","body":"` + strings.Repeat(secret, 50000) + `","metadata":{"started_generation_at":"1970-01-01T00:01:40Z","metrics":{"ttft_ms":null,"input_tokens":3}}}`
	exec(t, c, "INSERT INTO message_nodes VALUES ('s',100,?)", body)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, h, ".claude/projects/"+secret+"/a.jsonl", `{"type":"assistant","timestamp":100,"message":{"id":"m","content":"`+strings.Repeat(secret, 50000)+`","model":"claude","usage":{"input_tokens":2,"output_tokens":1}}}`+"\n")
	ctx := syncdiag.WithFields(syncdiag.WithRun(context.Background()), "operation", "diagnostic_test")
	d := data.LoadAllContext(ctx, 0, 200, true)
	if len(d.Turns) != 2 {
		t.Fatalf("fixture produced %d turns", len(d.Turns))
	}
	// The second collection must expose snapshot reuse under the same run.
	data.LoadContext(ctx, model.Devin, 0, 200, false)
	raw, err := os.ReadFile(syncdiag.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{h, secret, "SELECT ", "chat_message"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatal("diagnostics exposed source path, payload or SQL")
		}
	}
	var run any
	loads, queues, snapshots := map[string]bool{}, map[string]bool{}, map[string]bool{}
	query, files, cacheHit, fileDetail := false, false, false, false
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var r map[string]any
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		if r["operation"] != "diagnostic_test" {
			continue
		}
		if run == nil {
			run = r["run"]
		}
		if run == nil || r["run"] != run {
			t.Fatal("collection lost run correlation")
		}
		agent, _ := r["agent"].(string)
		switch r["event"] {
		case "agent_load_result":
			loads[agent] = true
		case "agent_queue_result":
			queues[agent] = true
		case "snapshot_result":
			snapshots[agent] = true
			cacheHit = cacheHit || r["reason"] == "snapshot_hit"
		case "query_result":
			if r["query"] == "devin_messages" {
				query = true
				if r["rows"].(float64) != 1 || r["main_file_reads"].(float64) <= 0 || r["main_file_bytes"].(float64) <= 0 || r["materialized_bytes"].(float64) < float64(len(body)) || r["consumer_ms"] == nil || r["source"] == nil {
					t.Fatal("missing query attribution", r)
				}
			}
		case "file_read_result":
			if agent == string(model.Claude) && r["mode"] == "decode" {
				fileDetail = r["file_bytes"].(float64) >= 1<<20 && r["source"] != nil && r["reason"] == "forced_rebuild"
			}
		case "decode_cache_result":
			if agent == string(model.Claude) && r["file_decodes"].(float64) > 0 && r["decoded_input_bytes"].(float64) > 0 {
				files = true
			}
		}
	}
	for _, a := range model.Agents {
		if !loads[string(a)] || !queues[string(a)] || !snapshots[string(a)] {
			t.Fatalf("missing agent diagnostics for %s", a)
		}
	}
	if !query || !files || !cacheHit || !fileDetail {
		t.Fatalf("missing query=%v file decode=%v snapshot hit=%v file detail=%v", query, files, cacheHit, fileDetail)
	}
}
