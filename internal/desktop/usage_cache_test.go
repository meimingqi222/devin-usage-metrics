package desktop

import (
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestUsageSummaryCacheInvalidatesWithInputs(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	a := &App{agent: model.Claude, period: analytics.Day, loaded: map[model.Agent]model.Data{}}
	a.loaded[a.agent] = model.Data{Turns: []model.Turn{
		{Agent: model.Claude, DeviceID: "one", SessionKey: "s", CreatedAt: now.Unix(), InputTokens: 10},
		{Agent: model.Claude, DeviceID: "two", SessionKey: "s", CreatedAt: now.Unix(), InputTokens: 20},
	}}
	check := func(sessions, turns int, input float64) {
		t.Helper()
		a.usageData(now)
		if a.usageCache.sessions != sessions || a.usageCache.total.Turns != turns || a.usageCache.total.Input != input {
			t.Fatalf("summary: sessions=%d turns=%d input=%v", a.usageCache.sessions, a.usageCache.total.Turns, a.usageCache.total.Input)
		}
	}
	check(2, 2, 30)
	if allocs := testing.AllocsPerRun(100, func() { a.usageData(now) }); allocs != 0 {
		t.Fatalf("unchanged overview allocated: %v", allocs)
	}
	a.device = "one"
	check(1, 1, 10)
	a.page = 1
	check(0, 0, 0)
	a.page = 0
	a.device = ""
	check(2, 2, 30)
	a.remote = model.Data{Turns: []model.Turn{{Agent: model.Claude, DeviceID: "three", SessionKey: "s", CreatedAt: now.Unix(), InputTokens: 40}}}
	check(3, 3, 70)
	a.invalidateDerived()
	check(3, 3, 70)
	a.agent = model.Codex
	check(0, 0, 0)
	a.agent = model.Claude
	now = now.AddDate(0, 0, 20)
	check(0, 0, 0)
}

func BenchmarkUsageSummaryCached(b *testing.B) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	turns := make([]model.Turn, 100000)
	for i := range turns {
		turns[i] = model.Turn{Agent: model.Claude, SessionKey: "s", CreatedAt: now.Unix(), InputTokens: 1}
	}
	a := &App{agent: model.Claude, period: analytics.Day, loaded: map[model.Agent]model.Data{model.Claude: {Turns: turns}}}
	a.usageData(now)
	b.ReportAllocs()
	for b.Loop() {
		a.usageData(now)
	}
}
