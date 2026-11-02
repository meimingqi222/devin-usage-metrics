// Package data reads local agent histories without modifying their databases.
package data

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

var loadLocks sync.Map
var deviceMu sync.Mutex

func home(p string) string { h, _ := os.UserHomeDir(); return filepath.Join(h, p) }
func cacheDir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return d
	}
	d, _ := os.UserCacheDir()
	return d
}
func configPath() string {
	p, _ := os.UserConfigDir()
	return filepath.Join(p, "devin-usage-metrics", "device.json")
}
func readDevice() map[string]string {
	v := map[string]string{}
	b, _ := os.ReadFile(configPath())
	_ = json.Unmarshal(b, &v)
	if v == nil {
		v = map[string]string{}
	}
	return v
}
func atomicJSON(path string, v any) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".aum-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	b, e := json.Marshal(v)
	if e == nil {
		_, e = f.Write(b)
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), path)
}

// DeviceID returns the persisted identity shared with the Rust application.
func DeviceID() string {
	deviceMu.Lock()
	defer deviceMu.Unlock()
	unlock, e := lockDevice()
	if e != nil {
		if id := readDevice()["id"]; id != "" {
			return id
		}
		return fallbackDeviceID()
	}
	defer unlock()
	return deviceID()
}
func deviceID() string {
	v := readDevice()
	if v["id"] != "" {
		return v["id"]
	}
	b := make([]byte, 16)
	if _, e := rand.Read(b); e == nil {
		b[6] = b[6]&15 | 64
		b[8] = b[8]&63 | 128
		v["id"] = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
		if atomicJSON(configPath(), v) == nil {
			return v["id"]
		}
	}
	return fallbackDeviceID()
}
func fallbackDeviceID() string {
	h, _ := os.Hostname()
	var n uint64 = 0xcbf29ce484222325
	for _, c := range []byte(h) {
		n ^= uint64(c)
		n *= 0x100000001b3
	}
	return fmt.Sprintf("fallback-%x", n)
}
func DeviceName() string {
	deviceMu.Lock()
	defer deviceMu.Unlock()
	if n := strings.TrimSpace(readDevice()["name"]); n != "" {
		return n
	}
	h, _ := os.Hostname()
	if h == "" {
		h = "This Device"
	}
	return h
}

// Bump when parsing changes so persisted records cannot bypass importer fixes.
const snapshotVersion = 4

type snapshot struct {
	Version   int
	At        int64
	Data      model.Data
	Databases map[string]string
	Files     map[string]string
	// FreshSources is false after an incremental update of a changed source.
	// A manual reload must still rebuild historical edits outside its watermark.
	FreshSources bool
}

func empty(start, end int64) model.Data {
	return model.Data{TurnsStart: start, TurnsEnd: end, FileMtimes: map[string]int64{}, FileSessions: map[string]string{}}
}
func fail(d *model.Data, a model.Agent, e any) {
	d.Errors = append(d.Errors, model.DataError{Agent: a, Message: fmt.Sprint(e)})
}

// Load returns the selected agent's records. Refresh bypasses all cached records.
func Load(a model.Agent, start, end int64, refresh bool) model.Data {
	return LoadContext(context.Background(), a, start, end, refresh)
}

// LoadContext preserves the caller's diagnostic run across source workers.
func LoadContext(ctx context.Context, a model.Agent, start, end int64, refresh bool) model.Data {
	mode := cachedLoad
	if refresh {
		mode = rebuildLoad
	}
	return load(syncdiag.WithRun(ctx), a, start, end, mode)
}

// CheckContext checks sources immediately, bypassing the five-minute snapshot
// TTL like Reload, but resumes Devin from its incremental watermark instead of
// rebuilding changed databases. Historical edits outside the overlap are only
// picked up by a later Reload.
func CheckContext(ctx context.Context, a model.Agent, start, end int64) model.Data {
	return load(syncdiag.WithRun(ctx), a, start, end, checkLoad)
}

// Reload checks sources immediately, bypassing the five-minute snapshot TTL.
// Unchanged sources may reuse their parsed snapshot; changed sources are fully
// parsed, including historical edits. Load(..., true) always rebuilds instead.
func Reload(a model.Agent, start, end int64) model.Data {
	return ReloadContext(context.Background(), a, start, end)
}

func ReloadContext(ctx context.Context, a model.Agent, start, end int64) model.Data {
	return load(syncdiag.WithRun(ctx), a, start, end, reloadLoad)
}

type loadMode int

const (
	cachedLoad  loadMode = iota // serve a covering snapshot younger than five minutes
	checkLoad                   // check sources now; Devin stays incremental
	reloadLoad                  // check sources now; rebuild changed sources
	rebuildLoad                 // rebuild every source
)

func load(ctx context.Context, a model.Agent, start, end int64, lm loadMode) (result model.Data) {
	refresh, reuseUnchanged := lm >= reloadLoad, lm == reloadLoad
	ctx = syncdiag.WithFields(ctx, "agent", diagnosticAgent(a), "refresh", refresh, "reuse_unchanged", reuseUnchanged, "check_now", lm == checkLoad)
	ctx, finish := syncdiag.Watch(ctx, "agent_load")
	started := time.Now()
	stats := new(decodeStats)
	ctx = context.WithValue(ctx, decodeStatsKey{}, stats)
	mode := "invalid"
	defer func() {
		logDecodeTotals(ctx, stats)
		syncdiag.Log(ctx, "agent_load_result", "mode", mode, "elapsed_ms", stageMS(started), "sessions", len(result.Sessions), "turns", len(result.Turns), "source_errors", len(result.Errors))
		finish(sourceResultError(result))
	}()
	cache := cacheDir()
	path := filepath.Join(cache, "devin-usage-metrics", "go-data-"+string(a)+".json")
	lock, _ := loadLocks.LoadOrStore(path, new(sync.Mutex))
	_, locked := syncdiag.Watch(ctx, "agent_lock_wait")
	lock.(*sync.Mutex).Lock()
	locked(nil)
	defer lock.(*sync.Mutex).Unlock()
	valid := false
	for _, known := range model.Agents {
		if a == known {
			valid = true
			break
		}
	}
	if !valid || end <= start {
		d := empty(start, end)
		fail(&d, a, "unsupported agent or invalid time range")
		return d
	}
	var old snapshot
	_, readSnapshot := syncdiag.Watch(ctx, "read_snapshot")
	readStarted := time.Now()
	b, readErr := os.ReadFile(path)
	readMS := stageMS(readStarted)
	readSnapshot(readErr)
	_, decodeSnapshot := syncdiag.Watch(ctx, "decode_snapshot")
	decodeStarted := time.Now()
	decodeErr := json.Unmarshal(b, &old)
	decodeMS := stageMS(decodeStarted)
	decodeSnapshot(decodeErr)
	now := time.Now().Unix()
	coversStart := old.Version == snapshotVersion && old.Data.TurnsStart <= start
	covers := coversStart && old.Data.TurnsEnd >= min(end, now)
	age := int64(-1)
	if old.At > 0 {
		age = max(0, now-old.At)
	}
	syncdiag.Log(ctx, "snapshot_result", "reason", snapshotReason(old, start, end, now, lm, readErr, decodeErr), "read_ms", readMS, "decode_ms", decodeMS, "bytes", len(b), "age_seconds", age, "covers_start", coversStart, "covers_end", covers, "fresh_sources", old.FreshSources, "read_error", syncdiag.ErrorCode(readErr), "decode_error", syncdiag.ErrorCode(decodeErr))
	if lm == cachedLoad && covers && now-old.At <= 300 {
		mode = "snapshot"
		return filter(old.Data, start, end)
	}
	mode = "sources"
	d := empty(start, end)
	// Devin re-queries newer records from its watermark even when the cached
	// window has expired. Other importers require coverage before reusing data.
	var previous *model.Data
	canSeed := covers || a == model.Devin && coversStart && old.Data.TurnsEnd > start
	if (!refresh || reuseUnchanged && old.FreshSources) && canSeed && len(old.Data.Errors) == 0 {
		previous = &old.Data
	}
	var databases map[string]string
	var sourceFiles map[string]string
	switch a {
	case model.Claude, model.Codex, model.Pi, model.Omp, model.WorkBuddy, model.Grok:
		// Decoded transcripts are keyed by size and mtime (and appends are
		// verified), so a reload reuses them; only a full refresh decodes all.
		sourceFiles = loadFiles(ctx, &d, a, refresh && !reuseUnchanged, previous, old.Files)
	case model.Devin, model.ZCode, model.OpenCode, model.MimoCode, model.Antigravity:
		databases = loadDBs(ctx, &d, a, refresh, previous, old.Databases)
	default:
		fail(&d, a, "unsupported agent")
	}
	_, sorted := syncdiag.Watch(ctx, "sort_records")
	sort.SliceStable(d.Turns, func(i, j int) bool { return d.Turns[i].CreatedAt < d.Turns[j].CreatedAt })
	sort.Slice(d.Sessions, func(i, j int) bool { return d.Sessions[i].Key < d.Sessions[j].Key })
	sorted(nil)
	_, stamped := syncdiag.Watch(ctx, "stamp_device")
	id, name := DeviceID(), DeviceName()
	for i := range d.Sessions {
		d.Sessions[i].DeviceID = id
		d.Sessions[i].DeviceName = name
	}
	for i := range d.Turns {
		d.Turns[i].DeviceID = id
		d.Turns[i].DeviceName = name
	}
	stamped(nil)
	_, hashed := syncdiag.Watch(ctx, "hash_records")
	raw, hashErr := json.Marshal([]any{d.Sessions, d.Turns})
	d.SyncContentHash = fmt.Sprintf("%x", sha256.Sum256(raw))
	hashed(hashErr)
	unchanged := maps.Equal(databases, old.Databases) && maps.Equal(sourceFiles, old.Files)
	freshSources := len(d.Errors) == 0 && (refresh || old.FreshSources && covers && unchanged)
	_, saved := syncdiag.Watch(ctx, "write_snapshot")
	if e := atomicJSON(path, snapshot{Version: snapshotVersion, At: now, Data: d, Databases: databases, Files: sourceFiles, FreshSources: freshSources}); e != nil {
		saved(e)
		fail(&d, a, fmt.Errorf("save cache: %w", e))
	} else {
		saved(nil)
	}
	return d
}
func filter(d model.Data, start, end int64) model.Data {
	turns := []model.Turn{}
	for _, t := range d.Turns {
		if t.CreatedAt >= start && t.CreatedAt < end {
			turns = append(turns, t)
		}
	}
	d.Turns = turns
	d.TurnsStart = start
	d.TurnsEnd = end
	return d
}
func LoadAll(start, end int64, refresh bool) model.Data {
	return LoadAllContext(context.Background(), start, end, refresh)
}

func LoadAllContext(ctx context.Context, start, end int64, refresh bool) (result model.Data) {
	ctx, finish := syncdiag.Watch(syncdiag.WithRun(ctx), "collect_agents")
	syncdiag.Log(ctx, "collection_runtime", append(syncdiag.BuildFields(), syncdiag.RuntimeFields()...)...)
	defer func() {
		syncdiag.Log(ctx, "local_collection_result", append([]any{"sessions", len(result.Sessions), "turns", len(result.Turns), "source_errors", len(result.Errors)}, syncdiag.RuntimeFields()...)...)
		finish(sourceResultError(result))
	}()
	d := empty(start, end)
	parts := make([]model.Data, len(model.Agents))
	var wg sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i, a := range model.Agents {
		wg.Go(func() {
			agentCtx := syncdiag.WithFields(ctx, "agent", diagnosticAgent(a))
			queued := time.Now()
			syncdiag.Log(agentCtx, "agent_queue_start")
			limit <- struct{}{}
			defer func() { <-limit }()
			syncdiag.Log(agentCtx, "agent_queue_result", "elapsed_ms", stageMS(queued))
			parts[i] = LoadContext(agentCtx, a, start, end, refresh)
		})
	}
	wg.Wait()
	_, merged := syncdiag.Stage(ctx, "merge_agents")
	for _, p := range parts {
		d.Sessions = append(d.Sessions, p.Sessions...)
		d.Turns = append(d.Turns, p.Turns...)
		d.Errors = append(d.Errors, p.Errors...)
		for k, v := range p.FileMtimes {
			d.FileMtimes[k] = v
		}
		for k, v := range p.FileSessions {
			d.FileSessions[k] = v
		}
	}
	merged(nil)
	_, sorted := syncdiag.Stage(ctx, "sort_collection")
	sort.SliceStable(d.Turns, func(i, j int) bool { return d.Turns[i].CreatedAt < d.Turns[j].CreatedAt })
	sorted(nil)
	return d
}
