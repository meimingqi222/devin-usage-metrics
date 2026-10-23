package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

// countingServer records every request so tests can assert what a cycle sent.
type countingServer struct {
	*objectServer
	mu       sync.Mutex
	requests []string
}

func (c *countingServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
		c.mu.Unlock()
		c.serve(w, r)
	}
}

func (c *countingServer) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.requests
	c.requests = nil
	return out
}

func count(requests []string, method, contains string) int {
	n := 0
	for _, r := range requests {
		if strings.HasPrefix(r, method+" ") && strings.Contains(r, contains) {
			n++
		}
	}
	return n
}

func resetMemoryCaches() {
	manifestCache = newLRU[[]byte](32<<20, 512)
	blockCache = newLRU[decodedBlock](96<<20, 1<<16)
	packageCache = newLRU[devicePackage](64<<20, 256)
	lastGood = map[string]devicePackage{}
	lastAudit = map[string]time.Time{}
	lastExport.head = ""
}

func days(now int64, n int, agent model.Agent) model.Data {
	d := model.Data{TurnsStart: now - int64(n)*86400, TurnsEnd: now}
	for i := 0; i < n; i++ {
		d.Turns = append(d.Turns, model.Turn{Agent: agent, SessionKey: "s", CreatedAt: now - int64(i)*86400, InputTokens: float64(i + 1)})
	}
	return d
}

func syncTo(t *testing.T, url string) {
	t.Helper()
	if err := SaveConfig(Config{true, url}); err != nil {
		t.Fatal(err)
	}
	if err := saveSession(session{"token", "user", time.Now().Unix() + 3600}); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteFailureNeitherBlocksPublishNorDropsOtherDevices(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &countingServer{objectServer: &objectServer{objects: map[string][]byte{}}}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	syncTo(t, server.URL)
	now := time.Now().Unix()
	ctx := context.Background()
	for _, d := range []string{"good", "flaky"} {
		if err := (transport{server.URL, "token"}).publish(ctx, d, days(now, 2, model.Codex), nil); err != nil {
			t.Fatal(err)
		}
	}
	remote, err := Cycle(ctx, days(now, 1, model.Claude))
	if err != nil || len(remote.Turns) != 4 {
		t.Fatalf("baseline: turns=%d err=%v", len(remote.Turns), err)
	}
	s.mu.Lock()
	s.objects["v3-head-flaky.json"] = []byte(`{"protocol":3}`)
	delete(s.objects, "v3-head-local.json")
	s.mu.Unlock()
	remote, err = Cycle(ctx, days(now, 1, model.Claude))
	if !errors.Is(err, ErrPartial) || !strings.Contains(err.Error(), "flaky") {
		t.Fatalf("failure not reported as partial: %v", err)
	}
	if len(remote.Turns) != 4 {
		t.Fatalf("the failing device must keep its last good data and others stay: turns=%d", len(remote.Turns))
	}
	s.mu.Lock()
	_, published := s.objects["v3-head-local.json"]
	s.mu.Unlock()
	if !published {
		t.Fatal("another device's failure blocked publishing this one")
	}
}

func TestUnchangedHeadsSkipManifestAndListOnlyHeads(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &countingServer{objectServer: &objectServer{objects: map[string][]byte{}}}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	syncTo(t, server.URL)
	now := time.Now().Unix()
	ctx := context.Background()
	if err := (transport{server.URL, "token"}).publish(ctx, "other", days(now, 3, model.Codex), nil); err != nil {
		t.Fatal(err)
	}
	local := days(now, 3, model.Claude)
	// The first cycle publishes; the second runs this process's daily audit.
	for range 2 {
		if _, err := Cycle(ctx, local); err != nil {
			t.Fatal(err)
		}
	}
	s.take()
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	got := s.take()
	if count(got, "GET", "v3-manifest-") != 0 || count(got, "GET", ".zst") != 0 {
		t.Fatalf("unchanged heads still downloaded manifests or blocks: %v", got)
	}
	full := 0
	for _, r := range got {
		if r == "GET /v1/objects" {
			full++
		}
	}
	if count(got, "GET", "/v1/objects?prefix=") != 1 || full != 0 {
		t.Fatalf("listing must ask only for heads: %v", got)
	}
	if count(got, "HEAD", "v3-manifest-") != 1 {
		t.Fatalf("own stored manifest must be checked once: %v", got)
	}
}

func TestDiskCacheSurvivesRestartAndRejectsDamage(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &countingServer{objectServer: &objectServer{objects: map[string][]byte{}}}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	syncTo(t, server.URL)
	now := time.Now().Unix()
	ctx := context.Background()
	seed := httptest.NewServer(http.HandlerFunc(s.serve))
	defer seed.Close()
	// Seed through another URL so this server's scope starts cold.
	if err := (transport{seed.URL, "token"}).publish(ctx, "other", days(now, 4, model.Codex), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Cycle(ctx, model.Data{}); err != nil {
		t.Fatal(err)
	}
	cold := count(s.take(), "GET", ".zst")
	resetMemoryCaches() // a restart keeps only the disk cache
	remote, err := Cycle(ctx, model.Data{})
	warm := count(s.take(), "GET", ".zst")
	if err != nil || len(remote.Turns) != 4 || cold == 0 || warm != 0 {
		t.Fatalf("restart re-downloaded blocks: cold=%d warm=%d turns=%d err=%v", cold, warm, len(remote.Turns), err)
	}
	// A damaged cached copy is dropped and fetched again, never trusted.
	var damaged string
	filepath.WalkDir(filepath.Dir(transport{server.URL, "token"}.diskPath("x")), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && damaged == "" {
			damaged = p
		}
		return nil
	})
	if err := os.WriteFile(damaged, []byte("rot"), 0o600); err != nil {
		t.Fatal(err)
	}
	resetMemoryCaches()
	remote, err = Cycle(ctx, model.Data{})
	if err != nil || len(remote.Turns) != 4 || count(s.take(), "GET", ".zst") != 1 {
		t.Fatalf("damaged cache entry not replaced: turns=%d err=%v", len(remote.Turns), err)
	}
}

func TestPublishSendsOnlyNewObjectsAndRepairsMissingOnes(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &countingServer{objectServer: &objectServer{objects: map[string][]byte{}}}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	syncTo(t, server.URL)
	now := time.Now().Unix()
	ctx := context.Background()
	local := days(now, 6, model.Claude)
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	s.take()
	local.Turns = append(local.Turns, model.Turn{Agent: model.Claude, SessionKey: "s", CreatedAt: now, OutputTokens: 9})
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	got := s.take()
	if count(got, "GET", ".zst") != 0 || count(got, "PUT", "v3-block-") != 1 {
		t.Fatalf("one changed day must upload one block without probing others: %v", got)
	}
	// Objects that exist but the parent manifest does not hold (e.g. written
	// by a client whose blocks differ) cost one conditional PUT each and no
	// download: the 412 carries the stored ETag.
	s.mu.Lock()
	var h head
	unmarshal(t, s.objects["v3-head-local.json"], &h)
	var m manifest
	unmarshal(t, s.objects["v3-manifest-"+h.Manifest+".json"], &m)
	m.Blocks = []reference{}
	s.mu.Unlock()
	rewriteHead(t, s.objectServer, m, h)
	resetMemoryCaches()
	local.Turns[0].InputTokens++
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	got = s.take()
	if count(got, "GET", ".zst") != 0 || count(got, "PUT", "v3-block-") < 6 {
		t.Fatalf("existing objects were downloaded to prove they exist: %v", got)
	}
	// A referenced object the server lost is found by the audit and re-sent.
	s.mu.Lock()
	unmarshal(t, s.objects["v3-head-local.json"], &h)
	unmarshal(t, s.objects["v3-manifest-"+h.Manifest+".json"], &m)
	lost := m.Blocks[len(m.Blocks)-1].File
	delete(s.objects, lost)
	s.mu.Unlock()
	resetMemoryCaches()
	local.Turns = append(local.Turns, model.Turn{Agent: model.Claude, SessionKey: "s", CreatedAt: now, OutputTokens: 10})
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	unmarshal(t, s.objects["v3-head-local.json"], &h)
	unmarshal(t, s.objects["v3-manifest-"+h.Manifest+".json"], &m)
	for _, r := range append(m.Blocks, m.Sessions...) {
		if _, ok := s.objects[r.File]; !ok {
			s.mu.Unlock()
			t.Fatalf("published manifest references missing %s", r.File)
		}
	}
	s.mu.Unlock()
}

func unmarshal(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// rewriteHead stores m as the device's current manifest under a new head.
func rewriteHead(t *testing.T, s *objectServer, m manifest, h head) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	h.Manifest = hash(b)
	hb, _ := json.Marshal(h)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects["v3-manifest-"+h.Manifest+".json"] = b
	s.objects["v3-head-"+h.DeviceID+".json"] = hb
}

func TestExistingImmutableObjectsAreNotLoggedAsFailures(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &countingServer{objectServer: &objectServer{objects: map[string][]byte{}}}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	syncTo(t, server.URL)
	now := time.Now().Unix()
	ctx := context.Background()
	local := days(now, 4, model.Claude)
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	// Drop the parent's references so every block is re-sent and answered 412.
	s.mu.Lock()
	var h head
	unmarshal(t, s.objects["v3-head-local.json"], &h)
	var m manifest
	unmarshal(t, s.objects["v3-manifest-"+h.Manifest+".json"], &m)
	m.Blocks = []reference{}
	s.mu.Unlock()
	rewriteHead(t, s.objectServer, m, h)
	resetMemoryCaches()
	s.take()
	local.Turns[0].InputTokens++
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	if got := s.take(); count(got, "PUT", "v3-block-") < 3 {
		t.Fatalf("fixture did not re-send existing blocks: %v", got)
	}
	b, err := os.ReadFile(syncdiag.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"status":412`) {
		t.Fatal("expected 412 answers to immutable uploads were logged as failures")
	}
}

// The next cycle re-reads this device's head; the package it would assemble is
// the data just published, so it must already be cached under that manifest.
func TestPublishedPackageIsCachedForTheNextCycle(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &countingServer{objectServer: &objectServer{objects: map[string][]byte{}}}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	syncTo(t, server.URL)
	now := time.Now().Unix()
	local := days(now, 5, model.Claude)
	local.Sessions = []model.Session{{Agent: model.Claude, Key: "s", CreatedAt: now - 5*86400, LastActivityAt: now}}
	if _, err := Cycle(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	var h head
	s.mu.Lock()
	unmarshal(t, s.objects["v3-head-local.json"], &h)
	s.mu.Unlock()
	tr := transport{server.URL, "token"}
	p, ok := cachedPackage(tr.packageKey("local", "v3-manifest-"+h.Manifest, day(time.Now().Unix()-366*86400)))
	if !ok || len(p.Turns) != 5 || len(p.Sessions) != 1 || p.DeviceID != "local" {
		t.Fatalf("published package not cached: ok=%v turns=%d sessions=%d", ok, len(p.Turns), len(p.Sessions))
	}
	// The cached package must equal what reading the manifest assembles.
	packageCache = newLRU[devicePackage](64<<20, 256)
	read, err := tr.loadManifestFresh(context.Background(), "local", 3, h.Manifest)
	if err != nil || len(read.Turns) != len(p.Turns) || len(read.Sessions) != len(p.Sessions) || read.Start != p.Start || read.End != p.End || read.DeviceName != p.DeviceName {
		t.Fatalf("cached package differs from the manifest: %+v vs %+v (%v)", p, read, err)
	}
}

// Head creation sends If-None-Match: * like immutable uploads, but its 412 is a
// real CAS conflict between two first publishers and must stay in the log.
func TestInitialHeadConflictIsLogged(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &objectServer{objects: map[string][]byte{}}
	var conflicted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "v3-head-local.json") && !conflicted {
			conflicted = true // another publisher won the first head
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		s.serve(w, r)
	}))
	defer server.Close()
	syncTo(t, server.URL)
	if _, err := Cycle(context.Background(), days(time.Now().Unix(), 2, model.Claude)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(syncdiag.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !conflicted || !strings.Contains(string(b), `"resource":"v3-head"`) || !strings.Contains(string(b), `"status":412`) {
		t.Fatal("the initial head CAS conflict left no http_result record")
	}
}

// A read skips session shards dated before the cutoff, so the package cached
// after publishing must skip them too, even when a recent turn kept one.
func TestPublishedPackageSkipsSessionsBeforeCutoff(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &objectServer{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	defer server.Close()
	syncTo(t, server.URL)
	now := time.Now().Unix()
	old := now - 400*86400
	local := model.Data{TurnsStart: old, TurnsEnd: now,
		Sessions: []model.Session{{Agent: model.Claude, Key: "ancient", CreatedAt: old, LastActivityAt: old}, {Agent: model.Claude, Key: "fresh", CreatedAt: now, LastActivityAt: now}},
		Turns:    []model.Turn{{Agent: model.Claude, SessionKey: "ancient", CreatedAt: now - 86400, InputTokens: 1}, {Agent: model.Claude, SessionKey: "fresh", CreatedAt: now, InputTokens: 2}}}
	if _, err := Cycle(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	var h head
	s.mu.Lock()
	unmarshal(t, s.objects["v3-head-local.json"], &h)
	s.mu.Unlock()
	tr := transport{server.URL, "token"}
	cached, ok := cachedPackage(tr.packageKey("local", "v3-manifest-"+h.Manifest, day(now-366*86400)))
	packageCache = newLRU[devicePackage](64<<20, 256)
	read, err := tr.loadManifestFresh(context.Background(), "local", 3, h.Manifest)
	if err != nil || !ok {
		t.Fatalf("cached=%v err=%v", ok, err)
	}
	if len(cached.Sessions) != len(read.Sessions) || len(cached.Turns) != len(read.Turns) {
		t.Fatalf("cache holds %d sessions/%d turns, a read %d/%d", len(cached.Sessions), len(cached.Turns), len(read.Sessions), len(read.Turns))
	}
}

// A turn whose values change between publications (a parser fix, a turn still
// streaming) is keyed by those values; its old version must not stay
// published beside the new one. Sessions the local data no longer holds keep
// their published turns.
func TestPublishReplacesChangedTurnsOfLocalSessions(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &objectServer{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	defer server.Close()
	syncTo(t, server.URL)
	now := time.Now().Unix()
	load := func(sessions []model.Session, turns []model.Turn) {
		t.Helper()
		if _, err := Cycle(context.Background(), model.Data{TurnsStart: now - 86400, TurnsEnd: now + 1, Sessions: sessions, Turns: turns}); err != nil {
			t.Fatal(err)
		}
	}
	live := model.Session{Agent: model.Antigravity, Key: "live", CreatedAt: now, LastActivityAt: now}
	gone := model.Session{Agent: model.Antigravity, Key: "gone", CreatedAt: now, LastActivityAt: now}
	load([]model.Session{live, gone}, []model.Turn{
		{Agent: model.Antigravity, SessionKey: "live", CreatedAt: now, InputTokens: 1, CacheCreationTokens: 5},
		{Agent: model.Antigravity, SessionKey: "gone", CreatedAt: now, InputTokens: 7},
	})
	load([]model.Session{live}, []model.Turn{{Agent: model.Antigravity, SessionKey: "live", CreatedAt: now, InputTokens: 2}})
	var h head
	s.mu.Lock()
	unmarshal(t, s.objects["v3-head-local.json"], &h)
	s.mu.Unlock()
	packageCache = newLRU[devicePackage](64<<20, 256)
	read, err := transport{server.URL, "token"}.loadManifestFresh(context.Background(), "local", 3, h.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, v := range read.Turns {
		got[v.SessionKey] += v.InputTokens + v.CacheCreationTokens
	}
	if len(read.Turns) != 2 || got["live"] != 2 || got["gone"] != 7 {
		t.Fatalf("published turns %+v", read.Turns)
	}
}

// Repairing a damaged object uses If-Match; its 412 means another writer
// changed it and must stay in the log, unlike the first upload's expected 412.
func TestRepairConflictIsLogged(t *testing.T) {
	setup(t)
	resetMemoryCaches()
	s := &objectServer{objects: map[string][]byte{}}
	var repairs int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && r.Header.Get("If-Match") != "" && strings.Contains(r.URL.Path, "v3-block-") {
			repairs++
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		s.serve(w, r)
	}))
	defer server.Close()
	syncTo(t, server.URL)
	local := days(time.Now().Unix(), 3, model.Claude)
	if _, err := Cycle(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	// Damage a block the parent no longer holds, so the next publish re-sends
	// it, finds the stored copy wrong and attempts a repair.
	s.mu.Lock()
	var h head
	unmarshal(t, s.objects["v3-head-local.json"], &h)
	var m manifest
	unmarshal(t, s.objects["v3-manifest-"+h.Manifest+".json"], &m)
	s.objects[m.Blocks[0].File] = []byte("damaged")
	m.Blocks = []reference{}
	s.mu.Unlock()
	rewriteHead(t, s, m, h)
	resetMemoryCaches()
	// A turn on a new day changes the input without changing existing blocks,
	// so every one of them, the damaged one included, is sent again.
	local.Turns = append(local.Turns, model.Turn{Agent: model.Claude, SessionKey: "s", CreatedAt: time.Now().Unix() - 5*86400, InputTokens: 1})
	Cycle(context.Background(), local) // the repair conflict fails publication
	b, err := os.ReadFile(syncdiag.Path())
	if err != nil {
		t.Fatal(err)
	}
	if repairs == 0 || !strings.Contains(string(b), `"status":412`) {
		t.Fatalf("repair conflict not logged (repairs=%d)", repairs)
	}
}
