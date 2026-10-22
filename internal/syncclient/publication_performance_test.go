package syncclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func BenchmarkPublicationCycle(b *testing.B) {
	for _, mode := range []string{"first", "incremental"} {
		b.Run(mode, func(b *testing.B) {
			setup(b)
			resetMemoryCaches()
			s := &objectServer{objects: map[string][]byte{}}
			server := httptest.NewServer(http.HandlerFunc(s.serve))
			defer server.Close()
			if err := SaveConfig(Config{true, server.URL}); err != nil {
				b.Fatal(err)
			}
			if err := saveSession(session{"token", "user", time.Now().Unix() + 3600}); err != nil {
				b.Fatal(err)
			}
			now := time.Now().Unix()
			d := model.Data{TurnsStart: now - 31*86400, TurnsEnd: now + 3600}
			for i := 0; i < 200; i++ {
				d.Sessions = append(d.Sessions, model.Session{Agent: model.Claude, Key: fmt.Sprint(i), CreatedAt: now - 30*86400, LastActivityAt: now})
			}
			for i := 0; i < 6000; i++ {
				d.Turns = append(d.Turns, model.Turn{Agent: model.Claude, SessionKey: d.Sessions[i%200].Key, CreatedAt: now - int64(i%30)*86400 - int64(i), Model: "claude-sonnet-4-5", InputTokens: float64(i + 100), OutputTokens: 30})
			}
			if mode == "incremental" {
				if _, err := Cycle(context.Background(), d); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if mode == "first" {
					b.StopTimer()
					s.mu.Lock()
					s.objects = map[string][]byte{}
					s.mu.Unlock()
					resetMemoryCaches()
					b.StartTimer()
				} else {
					d.Turns = append(d.Turns, model.Turn{Agent: model.Claude, SessionKey: "0", CreatedAt: now, InputTokens: float64(i + 1)})
				}
				if _, err := Cycle(context.Background(), d); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
