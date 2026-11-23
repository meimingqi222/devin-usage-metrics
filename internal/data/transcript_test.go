package data_test

import (
	"context"
	"strings"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func summary(parts []data.Part) string {
	var b []string
	for _, p := range parts {
		b = append(b, map[data.PartKind]string{data.PartUser: "U", data.PartAssistant: "A", data.PartThinking: "T", data.PartToolCall: "C:" + p.Name, data.PartToolResult: "R"}[p.Kind]+"="+p.Text)
	}
	return strings.Join(b, " | ")
}

func sessionOf(t *testing.T, d model.Data, id string) model.Session {
	t.Helper()
	for _, s := range d.Sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no session %s in %+v", id, d.Sessions)
	return model.Session{}
}

func TestTranscriptDevinFollowsLatestBranchOnce(t *testing.T) {
	h := env(t)
	conn := db(t, h, ".local/share/devin/cli/sessions.db")
	exec(t, conn, "CREATE TABLE sessions(id TEXT PRIMARY KEY, working_directory TEXT, model TEXT, agent_mode TEXT, created_at INTEGER, last_activity_at INTEGER, title TEXT, hidden INTEGER DEFAULT 0)")
	exec(t, conn, "CREATE TABLE message_nodes(row_id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, node_id INTEGER, parent_node_id INTEGER, chat_message TEXT, created_at INTEGER)")
	exec(t, conn, "INSERT INTO sessions VALUES ('s','/p','swe-2','smart',100,100,'t',0)")
	sys := `{"message_id":"sys","role":"system","content":"prompt"}`
	user := `{"message_id":"u1","role":"user","content":"read note","metadata":{"is_user_input":true}}`
	call := `{"message_id":"a1","role":"assistant","content":"","thinking":{"thinking":"plan"},"tool_calls":[{"name":"read","arguments":{"file_path":"note.txt"}}],"metadata":{"request_id":"r1","metrics":{"ttft_ms":10,"total_time_ms":20,"input_tokens":5,"output_tokens":1}}}`
	result := `{"message_id":"t1","role":"tool","content":"hello"}`
	reply := `{"message_id":"a2","role":"assistant","content":"hello","metadata":{"request_id":"r2","metrics":{"ttft_ms":10,"total_time_ms":20,"input_tokens":5,"output_tokens":1}}}`
	// node, parent, message: the context prefix is written again for the
	// second request, each reply twice as siblings, and an abandoned branch
	// (node 9) hangs off node 2.
	for _, n := range []struct {
		node   int
		parent any
		msg    string
	}{{1, nil, sys}, {2, 1, user}, {9, 2, `{"message_id":"x","role":"assistant","content":"abandoned"}`}, {3, 2, sys}, {4, 3, user}, {5, 4, call}, {6, 4, call}, {7, 6, result}, {8, 7, reply}, {10, 7, reply}} {
		exec(t, conn, "INSERT INTO message_nodes(session_id,node_id,parent_node_id,chat_message,created_at) VALUES ('s',?,?,?,100)", n.node, n.parent, n.msg)
	}
	s := sessionOf(t, data.Load(model.Devin, 0, 200, true), "s")
	parts, cut, err := data.Transcript(context.Background(), s, nil)
	if err != nil || cut {
		t.Fatal(err, cut)
	}
	want := `U=read note | T=plan | C:read=file_path: note.txt | R=hello | A=hello`
	if got := summary(parts); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestTranscriptClaudeAndCodex(t *testing.T) {
	h := env(t)
	write(t, h, ".claude/projects/p/c1.jsonl", `{"type":"user","sessionId":"c1","timestamp":100,"message":{"role":"user","content":"fix the bug"}}
{"type":"user","sessionId":"c1","isMeta":true,"timestamp":100,"message":{"role":"user","content":"caveat"}}
{"type":"assistant","sessionId":"c1","timestamp":101,"message":{"id":"m","model":"claude","content":[{"type":"thinking","thinking":"look first"}],"usage":{"input_tokens":1,"output_tokens":1}}}
{"type":"assistant","sessionId":"c1","timestamp":101,"message":{"id":"m","model":"claude","content":[{"type":"tool_use","name":"Read","input":{"path":"a.go"}}],"usage":{"input_tokens":1,"output_tokens":2}}}
{"type":"user","sessionId":"c1","timestamp":102,"message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"package a"}]}]}}
{"type":"assistant","sessionId":"c1","timestamp":103,"message":{"id":"n","model":"claude","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}}
`)
	write(t, h, ".claude/projects/p/c1/subagents/w.jsonl", `{"type":"assistant","sessionId":"c1","timestamp":102,"message":{"id":"w","model":"claude","content":[{"type":"text","text":"subagent text"}],"usage":{"input_tokens":1,"output_tokens":1}}}
`)
	d := data.Load(model.Claude, 0, 200, true)
	parts, _, err := data.Transcript(context.Background(), sessionOf(t, d, "c1"), d.FileSessions)
	if want := `U=fix the bug | T=look first | C:Read=path: a.go | R=package a | A=done`; err != nil || summary(parts) != want {
		t.Fatalf("claude: %v\ngot  %s\nwant %s", err, summary(parts), want)
	}

	write(t, h, ".codex/sessions/x.jsonl", `{"type":"session_meta","timestamp":90,"payload":{"id":"x1","cwd":"/p"}}
{"type":"response_item","timestamp":91,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd</environment_context>"}]}}
{"type":"response_item","timestamp":92,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}}
{"type":"response_item","timestamp":93,"payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"**Listing files**"},{"type":"summary_text","text":"use ls"}]}}
{"type":"response_item","timestamp":94,"payload":{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls\",\"script\":\"first line\\nsecond line\",\"paths\":[\"a b.go\",\"x.go\"],\"opts\":{\"id\":9007199254740993,\"dry_run\":true}}"}}
{"type":"response_item","timestamp":95,"payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"}}
{"type":"response_item","timestamp":95,"payload":{"type":"function_call_output","output":"a.go"}}
{"type":"response_item","timestamp":96,"payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"one file"}]}}
{"type":"event_msg","timestamp":97,"payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":2,"output_tokens":1}}}}
`)
	d = data.Load(model.Codex, 0, 200, true)
	parts, _, err = data.Transcript(context.Background(), sessionOf(t, d, "x1"), d.FileSessions)
	want := `U=list files | T=**Listing files**

use ls | C:shell=cmd: ls
script:
first line
second line
paths:
[
  "a b.go",
  "x.go"
]
opts:
{
  "id": 9007199254740993,
  "dry_run": true
} | C:apply_patch=*** Begin Patch
*** End Patch | R=a.go | A=one file`
	if err != nil || summary(parts) != want {
		t.Fatalf("codex: %v\ngot  %s\nwant %s", err, summary(parts), want)
	}
	// Each argument names its value in the text, for the panel's rows.
	var args []string
	for _, a := range parts[2].Args {
		args = append(args, a.Name+"="+parts[2].Text[a.At:a.End])
	}
	if got, want := strings.Join(args, " | "), "cmd=ls | script=first line\nsecond line | paths=[\n  \"a b.go\",\n  \"x.go\"\n] | opts={\n  \"id\": 9007199254740993,\n  \"dry_run\": true\n}"; got != want || parts[3].Args != nil {
		t.Fatalf("args:\ngot  %q\nwant %q (patch args %v)", got, want, parts[3].Args)
	}
	if _, _, err := data.Transcript(context.Background(), model.Session{Agent: model.Codex, Key: "codex/gone", ID: "gone"}, d.FileSessions); err != data.ErrNoTranscript {
		t.Fatalf("missing file: %v", err)
	}
}
