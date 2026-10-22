package desktop

import (
	"fmt"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestSessionListCacheFiltersAndInvalidation(t *testing.T) {
	a := fixture(t)
	list, _, _ := a.visibleSessions()
	if len(list) == 0 {
		t.Fatal("empty fixture")
	}
	key := list[0].Key
	if n := testing.AllocsPerRun(100, func() { a.visibleSessions() }); n != 0 {
		t.Fatalf("unchanged list allocated %v", n)
	}
	a.search = "no match 9357"
	list, _, _ = a.visibleSessions()
	if len(list) != 0 {
		t.Fatal("search cache stale")
	}
	a.search = ""
	a.sessionsUI.hidden[key] = true
	a.saveSessionUI()
	list, _, _ = a.visibleSessions()
	for _, s := range list {
		if s.Key == key {
			t.Fatal("hidden row still visible")
		}
	}
	a.sessionsUI.showHidden = true
	list, _, _ = a.visibleSessions()
	if len(list) != 1 || list[0].Key != key {
		t.Fatal("recycle filter stale")
	}
	a.sessionsUI.showHidden = false
	a.remote.Sessions = append(a.remote.Sessions, model.Session{Agent: a.agent, Key: "new", Title: "New", LastActivityAt: 1 << 40})
	list, _, _ = a.visibleSessions()
	if len(list) == 0 || list[0].Key != "new" {
		t.Fatal("remote update did not invalidate list")
	}
	a.device = "nonexistent"
	list, _, _ = a.visibleSessions()
	if len(list) != 0 {
		t.Fatal("device filter stale")
	}
}

func BenchmarkSessionListCached(b *testing.B) {
	d := model.Data{}
	for i := 0; i < 10000; i++ {
		d.Sessions = append(d.Sessions, model.Session{Agent: model.Claude, Key: fmt.Sprint(i), LastActivityAt: int64(i)})
	}
	a := &App{agent: model.Claude, loaded: map[model.Agent]model.Data{model.Claude: d}}
	a.visibleSessions()
	b.ReportAllocs()
	for b.Loop() {
		a.visibleSessions()
	}
}
