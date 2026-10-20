package compaction_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	c "github.com/meimingqi222/devin-usage-metrics/internal/compaction"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
)

func history(agent model.Agent, at uint64, n int) model.Data {
	d := model.Data{}
	ctx := uint64(30000)
	for i := 0; i < n; i++ {
		t := model.Turn{Agent: agent, SessionKey: "session", DeviceID: "desktop", DeviceName: "Desktop", ContextStream: "main", CreatedAt: 1000 + int64(i)*30, Model: "gpt-5.2", InputTokens: 5000, CacheReadTokens: float64(ctx - 5000), OutputTokens: 800}
		if agent == model.Claude {
			t.Model = "claude-opus-4-6"
			t.InputTokens = 500
			t.CacheCreationTokens = 4500
			t.CacheCreation5mTokens = 4500
		}
		d.Turns = append(d.Turns, t)
		ctx += 5000
		if ctx >= at {
			ctx = 60000
		}
	}
	return d
}
func analyze(d model.Data, a model.Agent) c.Recommendation {
	return c.Analyze(d, a, "", 0, math.MaxInt64, nil).Recommendations[0]
}
func TestAsymmetricSavingsAndCoverage(t *testing.T) {
	for _, tc := range []struct {
		at        uint64
		status    string
		sensitive bool
	}{{320000, "keep", false}, {340000, "keep", true}, {960000, "recommend", false}} {
		r := analyze(history(model.Codex, tc.at, 1000), model.Codex)
		if r.Status != tc.status || r.SensitivityFailed != tc.sensitive {
			t.Fatalf("%d: %+v", tc.at, r)
		}
		if r.Candidate >= r.Observed {
			t.Fatal("expected smaller candidate")
		}
	}
	d := history(model.Codex, 320000, 1000)
	before := analyze(d, model.Codex)
	unknown := history(model.Codex, 150000, 2000)
	for i := range unknown.Turns {
		unknown.Turns[i].Model = "unpriced-model-xyz"
		unknown.Turns[i].SessionKey = "unknown"
	}
	d.Turns = append(d.Turns, unknown.Turns...)
	after := analyze(d, model.Codex)
	if after.Unpriced != 1 || after.Calls != 3000 || before.BaselineCost != after.BaselineCost || before.Candidate != after.Candidate {
		t.Fatal("unknown stream changed calibration")
	}
	for _, a := range []model.Agent{model.Claude, model.Codex} {
		r := analyze(history(a, 960000, 20), a)
		if r.Status != "not_enough_calls" {
			t.Fatal(r.Status)
		}
		r = analyze(history(a, 960000, 100), a)
		if r.Status != "not_enough_compactions" {
			t.Fatal(r.Status)
		}
	}
}
func TestSummaryCostAndIndependentPrices(t *testing.T) {
	d := history(model.Codex, 960000, 50)
	for i := range d.Turns {
		tokens := []uint64{30000, 100000, 200000}[i%3]
		if i >= 3 {
			tokens = []uint64{60000, 130000, 200000}[(i-3)%3]
		}
		d.Turns[i].CacheReadTokens = float64(tokens - 5000)
	}
	r := analyze(d, model.Codex)
	p, _ := pricing.Lookup("gpt-5.2")
	want := 3.61*p.Input + 9*p.CacheRead + .97*p.Output
	if r.Compactions != 16 || r.Rework != 10000 || math.Abs(r.BaselineCost-want) > 1e-9 {
		t.Fatalf("got %+v want %g", r, want)
	}
	second := append([]model.Turn(nil), d.Turns...)
	for i := range second {
		second[i].SessionKey = "second"
		second[i].Model = "gpt-5.1"
	}
	d.Turns = append(d.Turns, second...)
	p, _ = pricing.Lookup("gpt-5.1")
	want += 3.61*p.Input + 9*p.CacheRead + .97*p.Output
	r = analyze(d, model.Codex)
	if math.Abs(r.BaselineCost-want) > 1e-9 || r.Streams != 2 {
		t.Fatal("pooled price mismatch")
	}
}
func TestBoundariesAndCurrent(t *testing.T) {
	d := history(model.Claude, 960000, 120)
	for i := range d.Turns {
		if i%3 == 1 {
			d.Turns[i].Model = "claude-sonnet-4-6"
		}
		if i%3 == 2 {
			d.Turns[i].CacheReadTokens = 10000
		}
	}
	for _, r := range c.Analyze(d, model.Claude, "", 0, math.MaxInt64, nil).Recommendations {
		if r.Compactions != 0 {
			t.Fatal("model switch joined")
		}
	}
	d = history(model.Codex, 390000, 1000)
	r := c.Analyze(d, model.Codex, "desktop", 0, math.MaxInt64, map[[2]string]uint64{{"desktop", c.CodexGroup}: 900000}).Recommendations[0]
	if r.Current != 400000 || r.BaselineInferred {
		t.Fatal("baseline not capped")
	}
	for i := range d.Turns {
		d.Turns[i].ContextStream = ""
	}
	report := c.Analyze(d, model.Codex, "", 0, math.MaxInt64, nil)
	if report.Skipped != 1000 || len(report.Recommendations) != 0 {
		t.Fatal(report)
	}
}
func TestApplyUndoAndExternalEdits(t *testing.T) {
	for _, agent := range []model.Agent{model.Claude, model.Codex} {
		t.Run(string(agent), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("USERPROFILE", dir)
			t.Setenv("APPDATA", filepath.Join(dir, "config"))
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "")
			t.Setenv("CLAUDE_AUTOCOMPACT_PCT_OVERRIDE", "")
			path := filepath.Join(dir, "config.toml")
			initial := "# untouched\nmodel_auto_compact_token_limit = 500000 # limit\n[profiles.alt]\nmodel_auto_compact_token_limit = 170000\n"
			if agent == model.Claude {
				path = filepath.Join(dir, "settings.json")
				initial = "{\n  \"other\": [1,  2], \"modelSettings\": {\"claude-opus-4-6\": {\"autoCompactWindow\": 500000}}\n}\n"
			}
			if e := os.WriteFile(path, []byte(initial), 0600); e != nil {
				t.Fatal(e)
			}
			name := "claude-opus-4-6-20260101"
			for _, n := range []uint64{220000, 260000} {
				if e := c.Apply(agent, name, n); e != nil {
					t.Fatal(e)
				}
			}
			saved, _ := os.ReadFile(path)
			written := "260000"
			previous := "220000"
			label := c.CodexGroup
			if agent == model.Claude {
				written = "293000"
				previous = "253000"
				label = "claude-opus-4-6"
			}
			if got := c.CurrentThresholds(agent, "local")[[2]string{"local", label}]; got != 260000 {
				t.Fatal(got)
			}
			external := strings.Replace(string(saved), written, previous, 1)
			os.WriteFile(path, []byte(external), 0600)
			if c.Undo(agent, name) == nil || c.Apply(agent, name, 300000) == nil {
				t.Fatal("overwrote external edit")
			}
			os.WriteFile(path, saved, 0600)
			if e := c.Undo(agent, "claude-opus-4.6"); e != nil {
				t.Fatal(e)
			}
			restored, _ := os.ReadFile(path)
			if string(restored) != initial {
				t.Fatalf("format changed:\n%s", restored)
			}
			if c.Undo(agent, name) == nil {
				t.Fatal("undo journal not cleared")
			}
		})
	}
}
func TestAbsentAndOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", filepath.Join(dir, "config"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	initial := `{"other": true}`
	os.WriteFile(path, []byte(initial), 0600)
	if e := c.Apply(model.Claude, "claude-sonnet-4-6", 200000); e != nil {
		t.Fatal(e)
	}
	if e := c.Undo(model.Claude, "claude-sonnet-4-6"); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	var obj map[string]any
	if json.Unmarshal(b, &obj) != nil || obj["other"] != true {
		t.Fatal(string(b))
	}
	os.WriteFile(path, []byte(`{"env":{"CLAUDE_CODE_AUTO_COMPACT_WINDOW":"300000"}}`), 0600)
	if c.Apply(model.Claude, "claude-sonnet-4-6", 200000) == nil {
		t.Fatal("ignored override")
	}
}

func TestRustUndoAndConfirmationConflict(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("APPDATA", filepath.Join(h, "config"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, "config"))
	t.Setenv("CODEX_HOME", h)
	path := filepath.Join(h, "config.toml")
	if err := os.WriteFile(path, []byte("model_auto_compact_token_limit = 220000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(dir, "devin-usage-metrics")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// An actual Rust-shaped journal, independently authored rather than emitted by Apply.
	records := map[string]any{path + ":model_auto_compact_token_limit": map[string]any{"before": 500000, "written": 220000, "previous_written": nil}}
	b, _ := json.Marshal(records)
	if err = os.WriteFile(filepath.Join(dir, "compaction-undo.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	names, err := c.Pending(model.Codex)
	if err != nil || len(names) != 1 {
		t.Fatalf("lost Rust undo: %v %v", names, err)
	}
	if err = c.Undo(model.Codex, ""); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if !strings.Contains(string(before), "500000") {
		t.Fatal(string(before))
	}
	apply, err := c.Prepare(model.Codex, "", 260000)
	if err != nil {
		t.Fatal(err)
	}
	external := []byte("model_auto_compact_token_limit = 350000\n")
	if err = os.WriteFile(path, external, 0600); err != nil {
		t.Fatal(err)
	}
	if err = apply(); err == nil {
		t.Fatal("confirmation overwrote concurrent edit")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(external) {
		t.Fatal("external edit changed")
	}
}
