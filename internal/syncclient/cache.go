package syncclient

import (
	"container/list"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

// Caches hold only content-addressed data: manifests and blocks are named by
// the SHA-256 of their bytes, and every use re-checks that hash or the
// manifest's reference to it. Mutable heads are always fetched. Recovery reads
// bypass caches. Keys are scoped by server and account, never by raw token, so
// renewing a login keeps the cache and one account never sees another's.

// lru is a small size-bounded least-recently-used map.
type lru[V any] struct {
	mu          sync.Mutex
	budget, max int
	used        int
	order       *list.List
	items       map[string]*list.Element
}

type lruEntry[V any] struct {
	key  string
	v    V
	cost int
}

func newLRU[V any](budget, max int) *lru[V] {
	return &lru[V]{budget: budget, max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru[V]) get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*lruEntry[V]).v, true
	}
	var zero V
	return zero, false
}

func (c *lru[V]) put(key string, v V, cost int) {
	if cost > c.budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.used -= e.Value.(*lruEntry[V]).cost
		c.order.Remove(e)
	}
	c.items[key] = c.order.PushFront(&lruEntry[V]{key, v, cost})
	c.used += cost
	for c.used > c.budget || len(c.items) > c.max {
		e := c.order.Back()
		entry := e.Value.(*lruEntry[V])
		c.order.Remove(e)
		delete(c.items, entry.key)
		c.used -= entry.cost
	}
}

func (c *lru[V]) remove(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.used -= e.Value.(*lruEntry[V]).cost
		c.order.Remove(e)
		delete(c.items, key)
	}
}

func (t transport) scope() string { return hash([]byte(t.base + "\x00" + t.token)) }

// account scopes caches by server and sync tenant. The tenant is read from the
// session payload without trusting it for anything but cache partitioning;
// an unrecognised token falls back to the token itself.
func (t transport) account() string {
	tenant := t.token
	if payload, _, ok := strings.Cut(t.token, "."); ok {
		if b, err := base64.RawURLEncoding.DecodeString(payload); err == nil {
			var claims struct {
				Tenant string `json:"tenant"`
			}
			if json.Unmarshal(b, &claims) == nil && claims.Tenant != "" {
				tenant = "tenant:" + claims.Tenant
			}
		}
	}
	return hash([]byte(t.base + "\x00" + tenant))
}

var manifestCache = newLRU[[]byte](32<<20, 512)

func (t transport) cachedManifest(name string) []byte {
	b, _ := manifestCache.get(t.account() + "/" + name)
	return b
}

func (t transport) rememberManifest(name string, b []byte) {
	manifestCache.put(t.account()+"/"+name, b, len(b))
}

// decodedBlock is a block's records before per-reference validation, which
// callers repeat on every use because a reference's claims can differ.
type decodedBlock struct {
	Sessions []model.Session
	Turns    []model.Turn
}

// Decoding dominates sync CPU: a device that publishes a new manifest usually
// changes one or two blocks, so caching per block keeps every other one.
var blockCache = newLRU[decodedBlock](96<<20, 1<<16)

func (t transport) cachedDecoded(name string) (decodedBlock, bool) {
	return blockCache.get(t.account() + "/" + name)
}

func (t transport) rememberDecoded(name string, b decodedBlock, rawSize int) {
	blockCache.put(t.account()+"/"+name, b, rawSize+(len(b.Sessions)+len(b.Turns))*256+256)
}

// Protected by cycleMu. A successful publication is reusable only for the
// exact revalidated head and actual input content, never a caller's hint hash.
var lastExport struct {
	scope, device, head, input string
	expires                    int64
}

func exportFingerprint(data model.Data) (string, error) {
	end := data.TurnsEnd
	// Desktop queries use now+1h. Moving an empty future boundary is not a
	// content change; publication separately checks actual covered time and expiry.
	if end > time.Now().Unix() {
		end = 0
	}
	b, err := json.Marshal([]any{data.Sessions, data.Turns, data.TurnsStart, end})
	return hash(b), err
}

func clonePackage(p devicePackage) devicePackage {
	p.Sessions = cloneSessions(p.Sessions)
	p.Turns = cloneTurns(p.Turns)
	return p
}

func cloneSessions(in []model.Session) []model.Session {
	out := append([]model.Session(nil), in...)
	for i := range out {
		v := &out[i]
		if v.RealModel != nil {
			x := *v.RealModel
			v.RealModel = &x
		}
		if v.RecordedCost != nil {
			x := *v.RecordedCost
			v.RecordedCost = &x
		}
	}
	return out
}

func cloneTurns(in []model.Turn) []model.Turn {
	out := append([]model.Turn(nil), in...)
	for i := range out {
		if out[i].RecordedCost != nil {
			x := *out[i].RecordedCost
			out[i].RecordedCost = &x
		}
	}
	return out
}

var packageCache = newLRU[devicePackage](64<<20, 256)

func cachedPackage(key string) (devicePackage, bool) {
	p, ok := packageCache.get(key)
	if !ok {
		return p, false
	}
	return clonePackage(p), true
}

func rememberPackage(key string, p devicePackage) {
	// JSON length accounts for variable strings, plus conservative per-record
	// overhead. Bound entries too, including empty packages.
	b, err := json.Marshal(p)
	if err != nil {
		return
	}
	packageCache.put(key, clonePackage(p), len(b)+len(p.Sessions)*512+len(p.Turns)*512+1024)
}

// lastGood keeps each remote device's newest valid package, protected by
// cycleMu, so a device that fails to load keeps showing its last data.
var lastGood = map[string]devicePackage{}
