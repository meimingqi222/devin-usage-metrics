package syncclient

import (
	"sort"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

// Compute wire identities once per record, not once per comparison. Swapping
// keys alongside records preserves the existing sort's tie behavior.
type sessionPublicationOrder struct {
	values []model.Session
	keys   []string
}

func (s sessionPublicationOrder) Len() int           { return len(s.values) }
func (s sessionPublicationOrder) Less(i, j int) bool { return s.keys[i] < s.keys[j] }
func (s sessionPublicationOrder) Swap(i, j int) {
	s.values[i], s.values[j] = s.values[j], s.values[i]
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
}
func sortPublishSessions(values []model.Session) {
	keys := make([]string, len(values))
	for i := range values {
		keys[i] = v3SessionID(values[i])
	}
	sort.Sort(sessionPublicationOrder{values, keys})
}
