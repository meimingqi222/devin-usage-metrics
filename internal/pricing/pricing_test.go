package pricing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
)

func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10 {
		t.Fatalf("got %.12f want %.12f", got, want)
	}
}
func TestRequestBillingRegressions(t *testing.T) {
	for _, w := range []float64{997, 998} {
		turn := model.Turn{Agent: model.Devin, Model: "gpt-5-6-luna-high", InputTokens: 3, OutputTokens: 1000, CacheReadTokens: 271000, CacheCreationTokens: w}
		got, ok := pricing.Cost(turn, "")
		if !ok {
			t.Fatal("missing Luna")
		}
		want := (3*.2 + 1000*1.2 + 271000*.02 + w*.25) / 1e6
		if w == 998 {
			want = (3*.4 + 1000*1.8 + 271000*.04 + w*.5) / 1e6
		}
		near(t, got, want)
		turn.CreatedAt = 1
		old, _ := pricing.Cost(turn, "")
		near(t, old, got)
	}
	fast, ok := pricing.Cost(model.Turn{Agent: model.Devin, Model: "GPT-5.6 Luna High Thinking Fast", OutputTokens: 1e6, CacheReadTokens: 300000}, "")
	if !ok {
		t.Fatal("missing fast")
	}
	near(t, fast, 2.412)
	c, ok := pricing.Cost(model.Turn{Agent: model.Claude, Model: "claude-opus-5", InputTokens: 530, OutputTokens: 216178, CacheReadTokens: 32217103, CacheCreationTokens: 662052, CacheCreation1hTokens: 662052}, "")
	if !ok {
		t.Fatal("missing Claude")
	}
	near(t, c, 28.1361715)
	zero := 0.0
	c, ok = pricing.Cost(model.Turn{Model: "unknown", RecordedCost: &zero}, "")
	if !ok || c != 0 {
		t.Fatal("recorded zero lost")
	}
	for _, bad := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, ok := pricing.Cost(model.Turn{Model: "unknown", RecordedCost: &bad}, ""); ok {
			t.Fatal("invalid bill accepted")
		}
	}
	for _, name := range []string{"adaptive", "swe-1-7-high", "unknown-provider/claude-sonnet-5-5", "claude-opus", "gpt-5.1-fast", "gpt-5.1-free", "glm-5.3-unknown-flash"} {
		if _, ok := pricing.Cost(model.Turn{Agent: model.Codex, Model: name}, ""); ok {
			t.Fatalf("unsafe API fallback %s", name)
		}
	}
	for name, want := range map[string]float64{"anthropic/claude-sonnet-5-5": .1, "vivgrid/claude-sonnet-5-5": .2} {
		p, ok := pricing.Lookup(name)
		if !ok {
			t.Fatal(name)
		}
		near(t, p.CacheRead, want)
	}
	p, ok := pricing.Lookup("accounts/fireworks/models/kimi-k3")
	if !ok || p.Source != "fireworks-ai" {
		t.Fatal("lost provider")
	}
	a, _ := pricing.Lookup("claude-haiku-4-5-20251001")
	b, _ := pricing.Lookup("claude-haiku-4-5")
	near(t, a.Input, b.Input)
	for _, name := range []string{"gemini-3.8-flash-internal-test", "gemini-3.8-flash-lite-unknown", "gemini-3.8-flash-tts-unknown", "google/gemini-3.8-flash-unknown"} {
		_, ok := pricing.Cost(model.Turn{Agent: model.Antigravity, Model: name, InputTokens: 1}, "")
		if ok != (name == "gemini-3.8-flash-internal-test") {
			t.Fatalf("restricted fallback %s: %v", name, ok)
		}
	}
	// Antigravity's preview labels price as the model they preview.
	trial, ok := pricing.Cost(model.Turn{Agent: model.Antigravity, Model: "gemini-2.5-pro-trial", InputTokens: 1}, "")
	release, _ := pricing.Cost(model.Turn{Agent: model.Antigravity, Model: "gemini-2.5-pro", InputTokens: 1}, "")
	if !ok || trial != release {
		t.Fatalf("trial label priced %v, %v", trial, ok)
	}
	if _, ok := pricing.Cost(model.Turn{Agent: model.Claude, Model: "gemini-2.5-pro-trial", InputTokens: 1}, ""); ok {
		t.Fatal("trial label priced outside Antigravity")
	}
	// Rollout arms and -exp tags seen in Antigravity history bill as the base model.
	for name, base := range map[string]string{"gemini-3-flash-a": "gemini-3-flash-preview", "gemini-3.7-flash-exp-a": "gemini-3.7-flash", "gemini-3.8-flash-n": "gemini-3.8-flash"} {
		got, ok := pricing.Cost(model.Turn{Agent: model.Antigravity, Model: name, InputTokens: 1, OutputTokens: 1}, "")
		want, _ := pricing.Cost(model.Turn{Agent: model.Antigravity, Model: base, InputTokens: 1, OutputTokens: 1}, "")
		if !ok || got != want || want == 0 {
			t.Fatalf("%s priced %v %v, want %v", name, got, ok, want)
		}
	}
	if _, ok := pricing.Cost(model.Turn{Agent: model.Codex, Model: "gemini-3-flash-a", InputTokens: 1}, ""); ok {
		t.Fatal("rollout arm priced outside Antigravity")
	}
}

func TestRefreshPublicContract(t *testing.T) {
	// Keep tests isolated even though refresh deliberately hot-swaps process-wide quotes.
	originalAPI, e := os.ReadFile("models-dev-pricing.json")
	if e != nil {
		t.Fatal(e)
	}
	originalPro, e := os.ReadFile("devin-pro-pricing.json")
	if e != nil {
		t.Fatal(e)
	}
	var ps map[string]pricing.Price
	if e = json.Unmarshal(originalPro, &ps); e != nil {
		t.Fatal(e)
	}
	var rows []map[string]any
	families := map[string]any{}
	modelFamilies := map[string]string{}
	for id, p := range ps {
		rows = append(rows, map[string]any{"tier": "TEAMS_TIER_PRO", "model_uid": id, "label": p.Label, "input_cost_per_million_usd": p.Input, "output_cost_per_million_usd": p.Output, "cache_read_cost_per_million_usd": p.CacheRead, "cache_write_cost_per_million_usd": p.CacheWrite})
		for _, tier := range p.Tiers {
			families[id] = map[string]string{"threshold": fmt.Sprint(tier.Above), "input": fmt.Sprint(tier.Input), "output": fmt.Sprint(tier.Output), "cacheInput": fmt.Sprint(tier.CacheRead), "cacheWrite": fmt.Sprint(tier.CacheWrite)}
			modelFamilies[id] = id
		}
	}
	rb, _ := json.Marshal(rows)
	fb, _ := json.Marshal(families)
	mb, _ := json.Marshal(modelFamilies)
	originalDoc := "export const modelCostData = " + string(rb) + "; const longContextFamilies = " + string(fb) + "; const longContextModelFamilies = " + string(mb)
	catalog := `{"google":{"models":{"gemini-3.8-flash":{"cost":{"input":2,"output":10,"cache_read":0.1}},"gemini-4-argon":{"cost":{"input":4,"output":20,"cache_read":0.2}}}},"openai":{"models":{"gpt-test":{"cost":{"input":2,"output":10,"cache_read":0,"cache_write":0,"tiers":[{"tier":{"type":"context","size":100},"input":4,"output":15}]}}}},"reseller":{"models":{"gpt-test":{"cost":{"input":9,"output":90}},"novel":{"cost":{"input":1,"output":2}}}},"other":{"models":{"novel":{"cost":{"input":3,"output":6}}}}}`
	doc := `export const modelCostData = [{"tier":"TEAMS_TIER_PRO","model_uid":"future-fast","label":"Future Fast","input_cost_per_million_usd":1,"output_cost_per_million_usd":2,"cache_read_cost_per_million_usd":0.1,"cache_write_cost_per_million_usd":3}];
 const longContextFamilies = {'Future': {threshold:'123K',input:'$7.00',output:'$9',cacheInput:'$0.7',},};
 const longContextModelFamilies = {'future-fast':'Future',};`
	var hits atomic.Int64
	var fail, restore atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "offline", 503)
			return
		}
		if r.URL.Path == "/pro" {
			if restore.Load() {
				_, _ = w.Write([]byte(originalDoc))
			} else {
				_, _ = w.Write([]byte(doc))
			}
		} else {
			if restore.Load() {
				_, _ = w.Write(originalAPI)
			} else {
				_, _ = w.Write([]byte(catalog))
			}
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	t.Setenv("DEVIN_USAGE_DISABLE_MODELS_FETCH", "")
	t.Setenv("DEVIN_USAGE_MODELS_PATH", filepath.Join(dir, "api.json"))
	t.Setenv("DEVIN_USAGE_PRO_PRICING_PATH", filepath.Join(dir, "pro.md"))
	t.Setenv("DEVIN_USAGE_MODELS_URL", srv.URL+"/api")
	t.Setenv("DEVIN_USAGE_PRO_PRICING_URL", srv.URL+"/pro")
	defer func() { fail.Store(false); restore.Store(true); pricing.Refresh(context.Background(), true) }()
	if !pricing.Refresh(context.Background(), true) {
		t.Fatal("no update")
	}
	n := hits.Load()
	if pricing.Refresh(context.Background(), false) || hits.Load() != n {
		t.Fatal("TTL not honored")
	}
	for name, want := range map[string]float64{"gpt-test": 2, "reseller/gpt-test": 9} {
		c, ok := pricing.Cost(model.Turn{Model: name, InputTokens: 1}, "")
		if !ok {
			t.Fatal(name)
		}
		near(t, c, want/1e6)
	}
	if _, ok := pricing.Lookup("novel"); ok {
		t.Fatal("ambiguous alias")
	}
	for _, prompt := range []float64{100, 101} {
		c, ok := pricing.Cost(model.Turn{Model: "gpt-test", InputTokens: 1, CacheReadTokens: prompt - 1, OutputTokens: 2}, "")
		if !ok {
			t.Fatal("explicit zero cache rejected")
		}
		want := 22.0 / 1e6
		if prompt == 101 {
			want = 34.0 / 1e6
		}
		near(t, c, want)
	}
	turn := model.Turn{Model: "gemini-3.8-flash", InputTokens: 1, CacheCreationTokens: 10}
	if _, ok := pricing.Cost(turn, ""); ok {
		t.Fatal("missing write treated free")
	}
	turn.Agent = model.Antigravity
	c, ok := pricing.Cost(turn, "")
	if !ok {
		t.Fatal("Flash estimate missing")
	}
	near(t, c, 22e-6)
	// Every Antigravity argon label bills as gemini-4-argon, cache writes at the input rate.
	for _, name := range []string{"gemini-4-argon-c", "gemini-4-argon-abcd", "gemini-4-argon-trial", "gemini-4-argon-exp-b"} {
		c, ok := pricing.Cost(model.Turn{Agent: model.Antigravity, Model: name, InputTokens: 1, CacheCreationTokens: 10, OutputTokens: 1}, "")
		if !ok {
			t.Fatalf("%s unpriced", name)
		}
		near(t, c, 64e-6)
	}
	for _, prompt := range []float64{123000, 123001} {
		c, ok := pricing.Cost(model.Turn{Agent: model.Devin, Model: "Future Fast", CacheReadTokens: prompt, OutputTokens: 1}, "")
		if !ok {
			t.Fatal("document quote missing")
		}
		want := (prompt*.1 + 2) / 1e6
		if prompt > 123000 {
			want = (prompt*.7 + 9) / 1e6
		}
		near(t, c, want)
	}
	fail.Store(true)
	if pricing.Refresh(context.Background(), true) {
		t.Fatal("failed request updated")
	}
	n = hits.Load()
	if pricing.Refresh(context.Background(), false) || hits.Load() != n {
		t.Fatal("backoff not honored")
	}
	if _, ok := pricing.Lookup("gpt-test"); !ok {
		t.Fatal("lost last good quote")
	}
	t.Setenv("DEVIN_USAGE_DISABLE_MODELS_FETCH", "1")
	pricing.Refresh(context.Background(), true)
	if hits.Load() != n {
		t.Fatal("offline mode fetched")
	}
	t.Setenv("DEVIN_USAGE_DISABLE_MODELS_FETCH", "")
}
