package analytics_test

import (
	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"testing"
	"time"
)

func TestCalendarPagesAndDST(t *testing.T) {
	loc, e := time.LoadLocation("America/New_York")
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, loc)
	for _, p := range []analytics.Period{analytics.Day, analytics.Week, analytics.Month} {
		s, e := analytics.Window(p, 0, now)
		_, pe := analytics.Window(p, 1, now)
		if !pe.Equal(s) {
			t.Fatal("nonadjacent pages")
		}
		b := analytics.Build(model.Data{}, p, 0, "", "", now)
		if b[0].Start != s.Unix() || b[len(b)-1].End != e.Unix() {
			t.Fatal("buckets disagree with window")
		}
		for i := 1; i < len(b); i++ {
			if b[i-1].End != b[i].Start {
				t.Fatal("gap")
			}
		}
		if p == analytics.Day && b[len(b)-1].End-b[len(b)-1].Start != 23*3600 {
			t.Fatal("DST day must be 23 hours")
		}
	}
}
func TestTotalsFiltersAdaptiveAndSessionIdentity(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	zero := 0.0
	data := model.Data{Sessions: []model.Session{{Agent: model.Devin, Key: "same", DeviceID: "a", SelectedModel: "adaptive", RealModel: ptr("swe-2-high")}, {Agent: model.Claude, Key: "same", DeviceID: "b", SelectedModel: "claude-opus-5"}}, Turns: []model.Turn{
		{Agent: model.Devin, SessionKey: "same", DeviceID: "a", CreatedAt: now.Unix(), InputTokens: 100, OutputTokens: 20, CacheReadTokens: 300, CacheCreationTokens: 500, CacheCreation5mTokens: 200, CacheCreation1hTokens: 300},
		{Agent: model.Devin, SessionKey: "same", DeviceID: "a", CreatedAt: now.Unix(), Model: "compactor", RecordedCost: &zero},
		{Agent: model.Claude, SessionKey: "same", DeviceID: "b", CreatedAt: now.Unix(), InputTokens: 100, OutputTokens: 20, CacheReadTokens: 300, CacheCreationTokens: 500},
	}}
	for _, p := range []analytics.Period{analytics.Day, analytics.Week, analytics.Month} {
		bs := analytics.Build(data, p, 0, "", "", now)
		b := bs[len(bs)-1]
		if b.Total() != 1840 || b.CacheWrite != 1000 || b.Turns != 3 || b.Sessions != 2 {
			t.Fatalf("bad totals %+v", b)
		}
		m := b.ByModel["swe-2-high(adaptive)"]
		if m.Total() != 920 || !m.Priced || m.Sessions != 1 {
			t.Fatalf("bad adaptive usage %+v", m)
		}
		if !b.ByModel["compactor"].Priced {
			t.Fatal("recorded zero not priced")
		}
		if s := b.CacheSavings(); b.TokenCost <= 0 || s <= 0 || s >= 1 {
			t.Fatalf("cache savings %v from %v of %v", s, b.TokenCost, b.UncachedCost)
		}
		if _, ok := b.ByModel["compactor(adaptive)"]; ok {
			t.Fatal("internal routing label")
		}
		filtered := analytics.Build(data, p, 0, model.Devin, "a", now)
		f := filtered[len(filtered)-1]
		if f.Total() != 920 || f.Turns != 2 || f.Sessions != 1 {
			t.Fatal("filter mismatch")
		}
		none := analytics.Build(data, p, 0, model.Claude, "a", now)
		if none[len(none)-1].Turns != 0 {
			t.Fatal("crossed device boundary")
		}
	}
}
func ptr(s string) *string { return &s }
func TestHalfOpenAndTurnModelPrecedence(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s, e := analytics.Window(analytics.Day, 0, now)
	data := model.Data{Sessions: []model.Session{{Agent: model.Codex, Key: "s", SelectedModel: "unknown"}}, Turns: []model.Turn{{Agent: model.Codex, SessionKey: "s", CreatedAt: s.Unix(), Model: "gpt-5.1", InputTokens: 1}, {Agent: model.Codex, SessionKey: "s", CreatedAt: e.Unix(), InputTokens: 999}, {Agent: model.Codex, SessionKey: "s", CreatedAt: s.Unix() - 1, InputTokens: 999}}}
	b := analytics.Build(data, analytics.Day, 0, "", "", now)
	if b[0].Turns != 1 || b[0].Input != 1 || !b[0].ByModel["gpt-5.1"].Priced {
		t.Fatal("boundary or model fallback mismatch")
	}
}

func TestSessionProjectionPreservesFallbackAndLastDuplicate(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	d := model.Data{Sessions: []model.Session{
		{Agent: model.Devin, Key: "s", SelectedModel: "old"},
		{Agent: model.Devin, Key: "s", SelectedModel: "adaptive", RealModel: ptr("swe-2-high")},
	}, Turns: []model.Turn{
		{Agent: model.Devin, SessionKey: "s", CreatedAt: now.Unix(), InputTokens: 10},
		{Agent: model.Claude, SessionKey: "absent", CreatedAt: now.Unix(), InputTokens: 20},
	}}
	bs := analytics.Build(d, analytics.Day, 0, "", "", now)
	last := bs[len(bs)-1]
	if last.ByModel["swe-2-high(adaptive)"].Input != 10 || last.ByModel["unknown"].Input != 20 {
		t.Fatal("session fallback projection changed")
	}
}

func TestCacheHitRateWeightsByPrompt(t *testing.T) {
	if _, ok := analytics.CacheHitRate(0, 0, 0); ok {
		t.Fatal("no prompt has no rate")
	}
	// A 1,000-token prompt fully cached and a 99,000-token prompt with no
	// cache: per-request rates average to 50%, the session read 1%.
	r, ok := analytics.CacheHitRate(0+99000, 1000+0, 0)
	if !ok || r != 0.01 {
		t.Fatalf("rate %v %v, want 0.01", r, ok)
	}
	// Cache writes are prompt tokens too.
	if r, _ := analytics.CacheHitRate(1000, 3000, 1000); r != 0.6 {
		t.Fatalf("rate %v, want 0.6", r)
	}
}

func TestDecodeSeparatesStreamsFromBursts(t *testing.T) {
	for _, c := range []struct {
		name             string
		out, ttft, total float64
		window           float64
		want             analytics.SpeedResult
	}{
		{"streamed", 1000, 900, 10900, 10000, analytics.SpeedMeasured},
		{"no timing", 100, 0, 0, 0, analytics.SpeedNoTiming},
		{"total only", 100, 0, 5000, 0, analytics.SpeedNoTTFT},
		{"window just under 100 ms", 1, 50, 149, 0, analytics.SpeedBurst},
		{"window of 100 ms", 1, 10, 110, 100, analytics.SpeedMeasured},
		{"over 10,000 tok/s", 2001, 100, 300, 0, analytics.SpeedBurst},
		// A sub-second window after a wait over four times as long.
		{"held then released", 392, 8200, 8450, 0, analytics.SpeedBurst},
		{"real Devin tool call", 75, 1642, 1693, 0, analytics.SpeedBurst},
		{"total before first token", 10, 2000, 1500, 0, analytics.SpeedBurst},
		{"nothing written", 0, 500, 5000, 0, analytics.SpeedBurst},
	} {
		w, r := analytics.Decode(c.out, c.ttft, c.total)
		if w != c.window || r != c.want {
			t.Errorf("%s: window %v result %v, want %v %v", c.name, w, r, c.window, c.want)
		}
	}
}

func TestFloorBoundsLongRepliesWithoutTTFT(t *testing.T) {
	for _, c := range []struct {
		name       string
		out, total float64
		window     float64
		ok         bool
	}{
		{"long reply", 1500, 15000, 15000, true},
		{"exact threshold", 1000, 1000, 1000, true},
		{"too few tokens", 999, 10000, 0, false},
		{"too short", 1200, 999, 0, false},
		{"implausibly fast", 20000, 1000, 0, false},
	} {
		w, ok := analytics.Floor(c.out, c.total)
		if w != c.window || ok != c.ok {
			t.Errorf("%s: floor %v %v, want %v %v", c.name, w, ok, c.window, c.ok)
		}
	}
}
