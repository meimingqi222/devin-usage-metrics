package pricing

import "github.com/meimingqi222/devin-usage-metrics/internal/model"

type quoteKey struct {
	agent model.Agent
	name  string
}
type quoteResult struct {
	price Price
	found bool
}

// Calculator reuses model resolution within an aggregation. Its zero value is
// ready to use; keep it local to one goroutine. Catalog refreshes invalidate its
// quotes, and the entry limit bounds memory for histories with arbitrary names.
// Token counts, context tiers and recorded billing are evaluated on every call.
type Calculator struct {
	revision uint64
	quotes   map[quoteKey]quoteResult
}

func (c *Calculator) Cost(t model.Turn, fallback string) (float64, bool) {
	if t.RecordedCost != nil && valid(*t.RecordedCost) {
		return *t.RecordedCost, true
	}
	key := quoteKey{t.Agent, costModel(t, fallback)}
	mu.RLock()
	if c.revision != catalogRevision {
		c.quotes = nil
		c.revision = catalogRevision
	}
	q, exists := c.quotes[key]
	if !exists {
		q.price, q.found = resolveLocked(key.agent, key.name)
		if c.quotes == nil {
			c.quotes = make(map[quoteKey]quoteResult)
		}
		if len(c.quotes) >= 256 {
			clear(c.quotes)
		}
		c.quotes[key] = q
	}
	mu.RUnlock()
	return costWithPrice(t, q.price, q.found)
}
