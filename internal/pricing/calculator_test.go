package pricing

import (
	"fmt"
	"math"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestCalculatorMatchesRequestPricing(t *testing.T) {
	var c Calculator
	zero, bad := 0.0, math.NaN()
	names := []string{"swe-2-high", "GPT-5.6 Luna High Thinking Fast", "claude-opus-5", "gpt-5.4", "gemini-3.8-flash-high", "gemini-3.8-flash-tts", "unknown", " CLAUDE-OPUS-5 "}
	for _, agent := range model.Agents {
		for _, name := range names {
			for _, input := range []float64{0, 100, 199999, 200001, 1000000} {
				for _, recorded := range []*float64{nil, &zero, &bad} {
					turn := model.Turn{Agent: agent, Model: name, InputTokens: input, OutputTokens: 20, CacheReadTokens: 100, CacheCreationTokens: 30, CacheCreation5mTokens: 10, CacheCreation1hTokens: 20, RecordedCost: recorded}
					want, priced := Cost(turn, "")
					got, ok := c.Cost(turn, "")
					if got != want || ok != priced {
						t.Fatalf("cached pricing differs for %s/%s", agent, name)
					}
					turn.Model = ""
					got, ok = c.Cost(turn, name)
					if got != want || ok != priced {
						t.Fatalf("fallback pricing differs for %s/%s", agent, name)
					}
				}
			}
		}
	}
}

func TestCalculatorRefreshInvalidatesKnownAndMissingQuotes(t *testing.T) {
	mu.Lock()
	old := api
	api = map[string]Price{"known": {Input: 1}}
	catalogRevision++
	mu.Unlock()
	defer func() { mu.Lock(); api = old; catalogRevision++; mu.Unlock() }()
	var c Calculator
	known := model.Turn{Model: "known", InputTokens: 1e6}
	missing := model.Turn{Model: "new", InputTokens: 1e6}
	if v, ok := c.Cost(known, ""); !ok || v != 1 {
		t.Fatal("initial quote")
	}
	if _, ok := c.Cost(missing, ""); ok {
		t.Fatal("missing quote")
	}
	mu.Lock()
	api = map[string]Price{"known": {Input: 2}, "new": {Input: 3}}
	catalogRevision++
	mu.Unlock()
	if v, ok := c.Cost(known, ""); !ok || v != 2 {
		t.Fatal("retained stale quote")
	}
	if v, ok := c.Cost(missing, ""); !ok || v != 3 {
		t.Fatal("retained stale miss")
	}
	for i := 0; i < 1000; i++ {
		c.Cost(model.Turn{Model: fmt.Sprint(i)}, "")
	}
	if len(c.quotes) > 256 {
		t.Fatal("unbounded quote cache")
	}
}
