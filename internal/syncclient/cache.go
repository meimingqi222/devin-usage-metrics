package syncclient

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

// Immutable blocks and validated packages are cached for the current UTC day.
// Manifests and mutable heads are always revalidated; recovery reads deliberately
// bypass both caches. Tokens are hashed, never retained as cache keys or written
// to disk. Daily expiry also bounds how long server-side immutable-object damage
// can remain masked by a healthy cached copy.
var immutableCache = struct {
	sync.Mutex
	values map[string][]byte
	bytes  int
}{values: make(map[string][]byte)}

func (t transport) scope() string { return hash([]byte(t.base + "\x00" + t.token)) }

func (t transport) cachedBlock(name string) []byte {
	immutableCache.Lock()
	defer immutableCache.Unlock()
	return immutableCache.values[t.scope()+name+day(time.Now().Unix())]
}

func (t transport) rememberBlock(name string, b []byte) {
	const budget = 32 << 20
	if len(b) > budget {
		return
	}
	immutableCache.Lock()
	defer immutableCache.Unlock()
	k := t.scope() + name + day(time.Now().Unix())
	if _, ok := immutableCache.values[k]; ok {
		return
	}
	if immutableCache.bytes+len(b) > budget || len(immutableCache.values) >= 4096 {
		clear(immutableCache.values)
		immutableCache.bytes = 0
	}
	immutableCache.values[k] = b
	immutableCache.bytes += len(b)
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

var parsedCache = struct {
	sync.Mutex
	values map[string]devicePackage
	bytes  int
}{values: make(map[string]devicePackage)}

func clonePackage(p devicePackage) devicePackage {
	p.Sessions = append([]model.Session(nil), p.Sessions...)
	p.Turns = append([]model.Turn(nil), p.Turns...)
	for i := range p.Sessions {
		v := &p.Sessions[i]
		if v.RealModel != nil {
			x := *v.RealModel
			v.RealModel = &x
		}
		if v.RecordedCost != nil {
			x := *v.RecordedCost
			v.RecordedCost = &x
		}
	}
	for i := range p.Turns {
		if p.Turns[i].RecordedCost != nil {
			x := *p.Turns[i].RecordedCost
			p.Turns[i].RecordedCost = &x
		}
	}
	return p
}

func cachedPackage(key string) (devicePackage, bool) {
	parsedCache.Lock()
	defer parsedCache.Unlock()
	p, ok := parsedCache.values[key]
	return clonePackage(p), ok
}

func rememberPackage(key string, p devicePackage) {
	// JSON length accounts for variable strings, plus conservative per-record
	// overhead. Bound entries too, including empty packages.
	b, err := json.Marshal(p)
	charge := len(b) + len(p.Sessions)*512 + len(p.Turns)*512 + 1024
	if err != nil || charge > 32<<20 {
		return
	}
	parsedCache.Lock()
	defer parsedCache.Unlock()
	if _, ok := parsedCache.values[key]; ok {
		return
	}
	if parsedCache.bytes+charge > 32<<20 || len(parsedCache.values) >= 256 {
		clear(parsedCache.values)
		parsedCache.bytes = 0
	}
	parsedCache.values[key] = clonePackage(p)
	parsedCache.bytes += charge
}
