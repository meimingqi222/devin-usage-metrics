package data

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func checkClaudeProjection(t *testing.T, raw []byte) {
	t.Helper()
	var full obj
	wantErr := json.Unmarshal(raw, &full)
	got, gotErr := decodeClaudeLine(raw)
	if (wantErr != nil) != (gotErr != nil) {
		t.Fatalf("decode error differs: %v vs %v", wantErr, gotErr)
	}
	if wantErr != nil {
		return
	}
	slim(full)
	slim(got)
	for _, key := range []string{"type", "sessionId", "session_id", "timestamp", "cwd", "aiTitle", "customTitle", "entrypoint", "effort", "requestId"} {
		if !reflect.DeepEqual(full[key], got[key]) {
			t.Fatalf("field %s differs", key)
		}
	}
	for _, key := range []string{"role", "model", "id", "usage"} {
		if !reflect.DeepEqual(get(full, "message", key), get(got, "message", key)) {
			t.Fatalf("message.%s differs", key)
		}
	}
	if str(full["type"]) == "user" || str(get(full, "message", "role")) == "user" {
		if text(get(full, "message", "content")) != text(get(got, "message", "content")) {
			t.Fatal("title differs")
		}
	}
	// A scanner's input buffer is reused for the next line; projected values
	// must own their bytes, including Unicode and escaped object keys.
	before, _ := json.Marshal(got)
	for i := range raw {
		raw[i] = ' '
	}
	after, _ := json.Marshal(got)
	if !bytes.Equal(before, after) {
		t.Fatal("projection retained the input buffer")
	}
}

func FuzzClaudeProjection(f *testing.F) {
	for _, seed := range []string{
		`{"type":"user","message":{"content":"` + strings.Repeat("a ", 50) + `tail"}}`,
		`{}`, `null`, `[]`, `{"message":null}`, `{"ignored":[true,false,null,1.2e-3]}`,
		`{"type":"assistant","message":{"id":"a","usage":{"input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":4}}}}`,
		`{"type":"user","message":{"content":[null,42,[],{"text":" \n "},{"text":"你好 😀 \\\""}]}}`,
		`{"type":"user","message":{"content":[{"text":"first","text":"last"}]}}`,
		`{"TYPE":"user","t\u0079pe":"assistant","message":{"role":"user","content":"title"},"message":[]}`,
		`{"ignored":{"invalid":"\q"},"type":"assistant"}`,
		`{"ignored":1e999,"type":"assistant"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		checkClaudeProjection(t, raw)
	})
}

func TestClaudeProjectionMatchesFullAccounting(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	root := filepath.Join(h, ".claude/projects/p")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	var paths []string
	write := func(name string, rows ...string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(strings.Join(rows, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
		t.Cleanup(func() { forgetDecoded(p) })
	}
	write("a.jsonl",
		`{"type":"user","sessionId":"s","timestamp":100,"cwd":"/p","message":{"content":[{"type":"tool_result","content":[{"text":"ignored"}]},{"text":"  标题  one  "}]}}`,
		`{"type":"progress","sessionId":"s","timestamp":101,"entrypoint":"cli","effort":"high","data":{"nested":[{"message":{"usage":{"input_tokens":999999}}}]}}`,
		`{"type":"assistant","sessionId":"s","timestamp":102,"message":{"id":"m","model":"claude-sonnet-4-5","usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"assistant","sessionId":"s","timestamp":103,"message":{"id":"m","model":"claude-sonnet-4-5","usage":{"input_tokens":10,"output_tokens":4,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":2}}}}`,
		`{"type":"assistant","sessionId":"s","timestamp":104,"requestId":"request","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":"7","cache_read_input_tokens":20}}}`,
	)
	write("b.jsonl",
		`{"type":"assistant","session_id":"resume","timestamp":105,"message":{"id":"m","model":"claude-sonnet-4-5","usage":{"input_tokens":10,"output_tokens":4}}}`,
		`{"type":"custom-title","session_id":"resume","timestamp":106,"customTitle":" renamed  session "}`,
		`{"type":"assistant","session_id":"resume","timestamp":107,"message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":11}}}`,
	)
	load := func() model.Data {
		d := empty(0, 200)
		loadFiles(context.Background(), &d, model.Claude, false, nil, nil)
		sort.Slice(d.Sessions, func(i, j int) bool { return d.Sessions[i].Key < d.Sessions[j].Key })
		return d
	}
	// Seed the same cache using the unprojected JSON decoder, then run the
	// production accounting. Compare that result to selective decoding.
	for _, p := range paths {
		d := empty(0, 200)
		readLines(&d, model.Devin, p, true)
	}
	want := load()
	for _, p := range paths {
		forgetDecoded(p)
	}
	got := load()
	if !reflect.DeepEqual(want, got) {
		t.Fatal("selective decoding changed full accounting")
	}
}

func TestClaudeProjectionDiscardsNestedTranscripts(t *testing.T) {
	record := obj{"type": "progress", "sessionId": "s", "timestamp": 100, "data": make([]any, 20000)}
	for i := range record["data"].([]any) {
		record["data"].([]any)[i] = obj{"message": obj{"content": "irrelevant tool output", "usage": obj{"input_tokens": 999}}}
	}
	raw, _ := json.Marshal(record)
	v, err := decodeClaudeLine(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, weight := slim(v)
	if weight > 2048 {
		t.Fatalf("irrelevant nested records retained %d bytes", weight)
	}
	checkClaudeProjection(t, raw)
}
