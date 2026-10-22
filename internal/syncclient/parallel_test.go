package syncclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	stdatomic "sync/atomic"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestColdSyncDownloadsInParallel(t *testing.T) {
	setup(t)
	s := &objectServer{objects: map[string][]byte{}}
	seed := httptest.NewServer(http.HandlerFunc(s.serve))
	defer seed.Close()
	now := time.Now().Unix()
	local := model.Data{TurnsStart: now - 16*86400, TurnsEnd: now}
	for i := 0; i < 16; i++ {
		local.Turns = append(local.Turns, model.Turn{Agent: model.Claude, CreatedAt: now - int64(i)*86400, SessionKey: "s"})
	}
	if err := (transport{seed.URL, "token"}).publish(context.Background(), "other", local, nil); err != nil {
		t.Fatal(err)
	}
	var active, peak stdatomic.Int32
	ready := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".zst") {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			if n >= 4 {
				once.Do(func() { close(ready) })
			}
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
		}
		s.serve(w, r)
	}))
	defer server.Close()
	if err := SaveConfig(Config{true, server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := saveSession(session{"token", "user", now + 3600}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	remote, err := Cycle(ctx, model.Data{})
	if err != nil || len(remote.Turns) != 16 {
		t.Fatalf("cold sync did not complete: turns=%d peak=%d err=%v", len(remote.Turns), peak.Load(), err)
	}
	if peak.Load() < 4 || peak.Load() > transferLimit {
		t.Fatalf("unexpected transfer concurrency %d", peak.Load())
	}
	// Recovery must bypass warm caches and reject the entire package if one
	// block disappeared. No successfully decoded sibling may leak to callers.
	s.mu.Lock()
	var h head
	json.Unmarshal(s.objects["v3-head-other.json"], &h)
	var m manifest
	json.Unmarshal(s.objects["v3-manifest-"+h.Manifest+".json"], &m)
	delete(s.objects, m.Blocks[len(m.Blocks)-1].File)
	s.mu.Unlock()
	partial, err := (transport{server.URL, "token"}).loadManifestFresh(ctx, "other", 3, h.Manifest)
	if err == nil || len(partial.Turns) != 0 || len(partial.Sessions) != 0 {
		t.Fatalf("failed parallel download exposed partial package: turns=%d sessions=%d err=%v", len(partial.Turns), len(partial.Sessions), err)
	}
}

func TestCycleFallsBackOnlyAfterInvalidV3(t *testing.T) {
	setup(t)
	now := time.Now().Unix()
	s := &objectServer{objects: map[string][]byte{}}
	pkg := devicePackage{Schema: 1, DeviceID: "other", Sessions: []model.Session{{DeviceID: "other", Key: "legacy", CreatedAt: now}}}
	s.objects["device-other.json"], _ = json.Marshal(pkg)
	s.objects["v3-head-other.json"] = []byte(`{"protocol":3,"device_id":"other","manifest":"broken"}`)
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	defer server.Close()
	if err := SaveConfig(Config{true, server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := saveSession(session{"token", "user", now + 3600}); err != nil {
		t.Fatal(err)
	}
	remote, err := Cycle(context.Background(), model.Data{})
	if err != nil || len(remote.Sessions) != 1 {
		t.Fatalf("legacy fallback lost: sessions=%d err=%v", len(remote.Sessions), err)
	}
	// Once v3 is complete, the legacy package must no longer be included.
	s.mu.Lock()
	delete(s.objects, "v3-head-other.json")
	s.mu.Unlock()
	if err = (transport{server.URL, "token"}).publish(context.Background(), "other", model.Data{TurnsStart: now, TurnsEnd: now, Turns: []model.Turn{{Agent: model.Claude, SessionKey: "new", CreatedAt: now}}}, nil); err != nil {
		t.Fatal(err)
	}
	remote, err = Cycle(context.Background(), model.Data{})
	if err != nil || len(remote.Sessions) != 0 || len(remote.Turns) != 1 {
		t.Fatalf("valid v3 did not replace legacy: sessions=%d turns=%d err=%v", len(remote.Sessions), len(remote.Turns), err)
	}
}

func TestPublishTransfersInParallelAndDoesNotPublishPartialHead(t *testing.T) {
	setup(t)
	now := time.Now().Unix()
	local := model.Data{TurnsStart: now - 8*86400, TurnsEnd: now}
	for i := 0; i < 8; i++ {
		local.Turns = append(local.Turns, model.Turn{Agent: model.Claude, CreatedAt: now - int64(i)*86400, SessionKey: "s"})
	}
	for _, fail := range []bool{false, true} {
		s := &objectServer{objects: map[string][]byte{}}
		ready := make(chan struct{})
		var active, peak stdatomic.Int32
		var once sync.Once
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PUT" && strings.HasSuffix(r.URL.Path, ".zst") {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				if n >= 4 {
					once.Do(func() { close(ready) })
				}
				select {
				case <-ready:
				case <-r.Context().Done():
					return
				}
			}
			if fail && r.Method == "PUT" && strings.HasSuffix(r.URL.Path, ".zst") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			s.serve(w, r)
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := (transport{server.URL, "token"}).publish(ctx, "local", local, nil)
		cancel()
		server.Close()
		s.mu.Lock()
		_, hasHead := s.objects["v3-head-local.json"]
		s.mu.Unlock()
		if peak.Load() < 4 || peak.Load() > transferLimit {
			t.Fatalf("upload concurrency=%d", peak.Load())
		}
		if fail {
			if err == nil || hasHead {
				t.Fatalf("partial upload committed a head: head=%v err=%v", hasHead, err)
			}
		} else if err != nil || !hasHead {
			t.Fatalf("publication incomplete: head=%v err=%v", hasHead, err)
		}
	}
}
