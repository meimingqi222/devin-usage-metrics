package analytics_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func BenchmarkBuildLargeHistory(b *testing.B) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	d := model.Data{}
	for i := 0; i < 1000; i++ {
		d.Sessions = append(d.Sessions, model.Session{Agent: model.Devin, Key: fmt.Sprint(i), SelectedModel: "swe-2-high"})
	}
	for i := 0; i < 100000; i++ {
		d.Turns = append(d.Turns, model.Turn{Agent: model.Devin, SessionKey: d.Sessions[i%1000].Key, CreatedAt: now.Unix() - int64(i%365)*86400, InputTokens: 100, OutputTokens: 20, CacheReadTokens: 300})
	}
	for _, period := range []analytics.Period{analytics.Day, analytics.Month} {
		b.Run(string(period), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				analytics.Build(d, period, 0, model.Devin, "", now)
			}
		})
	}
}
