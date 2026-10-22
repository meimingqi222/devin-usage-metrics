package cli_test

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/cli"
)

func setup(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("USERPROFILE", h)
	t.Setenv("APPDATA", filepath.Join(h, "config"))
	t.Setenv("LOCALAPPDATA", filepath.Join(h, "cache"))
	for k, v := range map[string]string{"HOME": h, "XDG_CONFIG_HOME": filepath.Join(h, "config"), "XDG_CACHE_HOME": filepath.Join(h, "cache"), "XDG_DATA_HOME": filepath.Join(h, "data"), "DEVIN_USAGE_DISABLE_MODELS_FETCH": "1"} {
		t.Setenv(k, v)
	}
	return h
}

func TestRunFixtureCSVAndTable(t *testing.T) {
	h := setup(t)
	p := filepath.Join(h, ".claude/projects/p/a.jsonl")
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	date := time.Date(2026, 8, 30, 0, 0, 0, 0, time.Local)
	var fixture strings.Builder
	for i, at := range []time.Time{date.Add(-time.Second), date, date.AddDate(0, 0, 1).Add(-time.Second), date.AddDate(0, 0, 1)} {
		fmt.Fprintf(&fixture, `{"type":"assistant","timestamp":%d,"message":{"id":"m%d","model":"unknown-fixture","usage":{"input_tokens":1000,"output_tokens":2,"cache_creation_input_tokens":3,"cache_read_input_tokens":4}}}`+"\n", at.Unix(), i)
	}
	if e := os.WriteFile(p, []byte(fixture.String()), 0600); e != nil {
		t.Fatal(e)
	}
	for _, byAgent := range []bool{false, true} {
		args := []string{"--cli", "--agent", "CLAUDE_CODE", "--since", "20260830", "--until", "2026-08-30", "--format", "csv", "--refresh"}
		if byAgent {
			args = append(args, "--by-agent")
		}
		var out, warnings bytes.Buffer
		if e := cli.Run(args, &out, &warnings); e != nil {
			t.Fatal(e)
		}
		records, e := csv.NewReader(&out).ReadAll()
		if e != nil {
			t.Fatal(e)
		}
		wantHeader := "date,models,input,output,cache_creation,cache_read,total_tokens,cost_usd"
		if byAgent {
			wantHeader = "date,agent,models,input,output,cache_creation,cache_read,total_tokens,cost_usd"
		}
		if strings.Join(records[0], ",") != wantHeader {
			t.Fatal(records)
		}
		last := records[len(records)-1]
		if strings.Join(last[len(last)-6:], ",") != "2000,4,6,8,2018,0.00000000" {
			t.Fatal(records)
		}
		if !strings.Contains(warnings.String(), "unknown-fixture") {
			t.Fatal(warnings.String())
		}
		if byAgent && (len(records) != 4 || records[2][0] != "" || records[2][1] != "claude") {
			t.Fatal(records)
		}
	}
	var out bytes.Buffer
	if e := cli.Run([]string{"--since", "20260830", "--until", "20260830"}, &out, io.Discard); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "2,000") || !strings.Contains(out.String(), "$0.00") {
		t.Fatal(out.String())
	}
}

func TestRunValidationAndHelp(t *testing.T) {
	setup(t)
	for _, args := range [][]string{{"--wat"}, {"--agent"}, {"--agent", "amp"}, {"--format", "json"}, {"--since", "2026-02-30"}, {"--days", "0"}, {"--days", "x"}, {"--days", "9223372036854775807"}, {"--since", "20260901", "--until", "20260801"}} {
		var out bytes.Buffer
		if e := cli.Run(args, &out, io.Discard); e == nil || out.Len() != 0 {
			t.Fatalf("%v: %v %s", args, e, out.String())
		}
	}
	var out bytes.Buffer
	if e := cli.Run([]string{"--cli", "--help"}, &out, io.Discard); e != nil || !strings.Contains(out.String(), "--by-agent") {
		t.Fatal(e, out.String())
	}
	for _, a := range []string{"all", "open_code", "pi-agent", "codebuddy", "mimo", "grok-build", "z-code"} {
		if e := cli.Run([]string{"--agent", a, "--since", "20260830", "--until", "20260830"}, io.Discard, io.Discard); e != nil {
			t.Fatal(a, e)
		}
	}
}
