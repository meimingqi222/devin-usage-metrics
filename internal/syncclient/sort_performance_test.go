package syncclient

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func sessionSortFixture(n int) []model.Session {
	out := make([]model.Session, n)
	for i := range out {
		out[i] = model.Session{Agent: model.Agents[i%len(model.Agents)], DeviceID: "device", Key: fmt.Sprintf("session-%d", n-i)}
	}
	return out
}

func BenchmarkPublishSessionSort(b *testing.B) {
	input := sessionSortFixture(2000)
	b.ReportAllocs()
	for b.Loop() {
		values := append([]model.Session(nil), input...)
		sortPublishSessions(values)
	}
}

func TestPublishSessionSortReference(t *testing.T) {
	input := sessionSortFixture(200)
	input = append(input, model.Session{Agent: "future", Key: "unknown"}, model.Session{Agent: model.Claude, Key: "你好\x00雪"})
	want := append([]model.Session(nil), input...)
	sort.Slice(want, func(i, j int) bool { return v3SessionID(want[i]) < v3SessionID(want[j]) })
	sortPublishSessions(input)
	if !reflect.DeepEqual(input, want) {
		t.Fatal("session publication order changed")
	}
}
