package syncclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	stdatomic "sync/atomic"
	"testing"
	"time"
)

func setup(t testing.TB) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("APPDATA", h)
	t.Setenv("XDG_CONFIG_HOME", h)
	t.Setenv("XDG_CACHE_HOME", h)
	t.Setenv("LOCALAPPDATA", h)
	if e := atomic("device.json", []byte(`{"id":"local"}`)); e != nil {
		t.Fatal(e)
	}
}
func TestConfigPreservesUnknownAndMigrates(t *testing.T) {
	setup(t)
	if e := atomic("sync.json", []byte(`{"enabled":true,"sync_server_url":"https://old.example","future":{"x":1}}`)); e != nil {
		t.Fatal(e)
	}
	if c := ReadConfig(); !c.Enabled || c.APIURL != "https://old.example" {
		t.Fatal(c)
	}
	if e := SaveConfig(Config{true, "https://new.example"}); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path("sync.json"))
	if !strings.Contains(string(b), `"future":{"x":1}`) {
		t.Fatal(string(b))
	}
}
func TestLoginRustEncryptedStorageContract(t *testing.T) {
	setup(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/github/device":
			io.WriteString(w, `{"device_code":"private-code","user_code":"ABCD","verification_uri":"https://github.com/login/device","expires_in":600,"interval":1}`)
		case "/v1/auth/github/token":
			b, _ := io.ReadAll(r.Body)
			if string(b) != `{"device_code":"private-code"}` {
				t.Errorf("wrong auth payload")
			}
			json.NewEncoder(w).Encode(map[string]any{"sync_token": "private-token", "login": "octocat", "expires_at": time.Now().Unix() + 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a, e := BeginLogin(context.Background(), server.URL)
	if e != nil {
		t.Fatal(e)
	}
	if e = CompleteLogin(context.Background(), server.URL, a); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path("github-sync-session.enc"))
	if string(b[:5]) != "DUMS1" || strings.Contains(string(b), "private-token") {
		t.Fatal("wrong secure storage format")
	}
	s, e := loadSession()
	if e != nil || s.Token != "private-token" || SessionLabel() != "octocat" {
		t.Fatal("session roundtrip", e)
	}
	info, _ := os.Stat(path("github-sync-session.enc"))
	// Windows reports synthetic POSIX modes; ACLs are checked during native acceptance.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if e = Logout(); e != nil {
		t.Fatal(e)
	}
	if SessionLabel() != "" {
		t.Fatal("metadata not deleted")
	}
}
func TestRustStableIdentityFixtures(t *testing.T) {
	turn := model.Turn{DeviceID: "d", Agent: model.Claude, SessionKey: "s", CreatedAt: 1, InputTokens: 1}
	if got := turnID(turn); got != "a2b9f550d05ecd5065ee9ba225a518e91a47ce2c78ffc0a85bcc611688510953" {
		t.Fatal(got)
	}
	turn.DedupKey = "local-only"
	if turnID(turn) != "a2b9f550d05ecd5065ee9ba225a518e91a47ce2c78ffc0a85bcc611688510953" {
		t.Fatal("local dedup key changed wire identity")
	}
	if got := sessionID(model.Session{DeviceID: "d", Agent: model.Claude, Key: "s"}); got != "0bdbd3aaba3b3f6667678b63afcb024949624105dea6f4297ee4ff5d0549ca96" {
		t.Fatal(got)
	}
	// Independent SHA-256 of v2.rs Canon JSON with serde's literal Unicode
	// separators; escaped backslash text must remain distinct.
	for name, want := range map[string]string{
		"\u2028\u2029": "1a774ea199b3c8d3ed753af58b7ce794eb010ddf84e809ef81d0da4df7675f92",
		`\u2028\u2029`: "b3f53193476f70c93f0e62fe8d5a65b5222723e5d2a830505190c7eac74b02ff",
	} {
		turn.Model = name
		if got := turnID(turn); got != want {
			t.Fatalf("model %q: %s", name, got)
		}
	}
}

type objectServer struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	gets    map[string]int
}

func (s *objectServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer token" {
		w.WriteHeader(401)
		return
	}
	if r.URL.Path == "/v1/objects" {
		names := []string{}
		for k := range s.objects {
			names = append(names, k)
		}
		json.NewEncoder(w).Encode(names)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/objects/")
	old, exists := s.objects[name]
	etag := `"` + hash(old) + `"`
	switch r.Method {
	case "GET", "HEAD":
		if r.Method == "GET" {
			if s.gets == nil {
				s.gets = map[string]int{}
			}
			s.gets[name]++
		}
		if !exists {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write(old)
	case "PUT":
		if r.Header.Get("If-None-Match") == "*" && exists {
			w.Header().Set("ETag", etag)
			w.WriteHeader(412)
			return
		}
		if match := r.Header.Get("If-Match"); match != "" && (!exists || match != etag) {
			w.WriteHeader(412)
			return
		}
		s.objects[name], _ = io.ReadAll(r.Body)
		s.puts++
		w.WriteHeader(201)
	default:
		w.WriteHeader(405)
	}
}
func TestCycleV1MigrationReuseAndReadOnlyMerge(t *testing.T) {
	setup(t)
	s := &objectServer{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	defer server.Close()
	if e := SaveConfig(Config{true, server.URL}); e != nil {
		t.Fatal(e)
	}
	if e := saveSession(session{"token", "user", time.Now().Unix() + 3600}); e != nil {
		t.Fatal(e)
	}
	now := time.Now().Unix()
	pkg := devicePackage{Schema: 1, DeviceID: "other", Sessions: []model.Session{{DeviceID: "other", Key: "remote", CreatedAt: now}}}
	s.objects["device-other.json"], _ = json.Marshal(pkg)
	legacy := devicePackage{Schema: 1, DeviceID: "local", Start: now, End: now, Sessions: []model.Session{{Agent: model.Claude, DeviceID: "local", Key: "legacy", CreatedAt: now}}}
	s.objects["device-local.json"], _ = json.Marshal(legacy)
	local := model.Data{TurnsStart: 0, TurnsEnd: now + 3600, Turns: []model.Turn{{Agent: model.Claude, SessionKey: "local-session", CreatedAt: now, InputTokens: 2}}}
	ctx := context.Background()
	remote, e := Cycle(ctx, local)
	if e != nil {
		t.Fatal(e)
	}
	if len(remote.Sessions) != 1 || remote.Sessions[0].Key != "remote" {
		t.Fatal(remote)
	}
	count := s.puts
	s.gets = map[string]int{}
	start := time.Now()
	if _, e = Cycle(ctx, local); e != nil {
		t.Fatal(e)
	}
	blockGets, totalGets := 0, 0
	for name, n := range s.gets {
		totalGets += n
		if strings.HasSuffix(name, ".zst") {
			blockGets += n
		}
	}
	t.Logf("unchanged own cycle: block GETs=%d object GETs=%d elapsed=%s", blockGets, totalGets, time.Since(start))
	if blockGets != 0 {
		t.Fatal("unchanged own cycle downloaded blocks")
	}
	if s.puts != count {
		t.Fatalf("unchanged data wrote %d objects", s.puts-count)
	}
	local.TurnsEnd++
	if _, e = Cycle(ctx, local); e != nil {
		t.Fatal(e)
	}
	if s.puts != count {
		t.Fatal("moving future boundary republished unchanged history")
	}
	var h head
	json.Unmarshal(s.objects["v3-head-local.json"], &h)
	p, e := (transport{server.URL, "token"}).load(ctx, "local", h)
	if e != nil || len(p.Turns) != 1 {
		t.Fatal(p, e)
	}
	if len(p.Sessions) != 1 || p.Sessions[0].Key != "legacy" {
		t.Fatal("v1 local migration lost history")
	}
	for _, v := range p.Sessions {
		if v.DeviceID == "other" {
			t.Fatal("remote records republished")
		}
	}
	if len(local.Turns) != 1 || local.Turns[0].DeviceID != "" {
		t.Fatal("caller data mutated")
	}
	// Other devices read this manifest from the server: a damaged stored copy
	// is restored from the verified local copy rather than ignored.
	name := "v3-manifest-" + h.Manifest + ".json"
	good := s.objects[name]
	s.objects[name] = []byte(`{}`)
	if _, e = Cycle(ctx, local); e != nil {
		t.Fatal(e)
	}
	if string(s.objects[name]) != string(good) {
		t.Fatal("damaged manifest was not repaired")
	}
	// Without a verified copy the damage is reported, as before.
	manifestCache = newLRU[[]byte](32<<20, 512)
	packageCache = newLRU[devicePackage](64<<20, 256)
	s.objects[name] = []byte(`{}`)
	if _, e = Cycle(ctx, local); e == nil {
		t.Fatal("manifest corruption accepted")
	}
}

func TestCycleV2RustRecordContractAndPreviousFallback(t *testing.T) {
	setup(t)
	// Field order and IDs mirror v2::stable_turn_id's explicit Canon, not Go record serialization.
	raw := []byte(`{"protocol":2,"data_schema":1,"records":[{"kind":"turn","record_id":"a2b9f550d05ecd5065ee9ba225a518e91a47ce2c78ffc0a85bcc611688510953","value":{"agent":"claude","session_key":"s","created_at":1,"input_tokens":1.0,"output_tokens":0.0,"cache_read_tokens":0.0,"ttft_ms":0.0,"total_time_ms":0.0,"device_id":"d"}}]}`)
	// Independent reference libzstd v1.5.4 fixture: printf '%s' "$raw" | zstd -3 -q -c | base64 -w0.
	// No Rust toolchain was available; this is not claimed to be Rust-generated.
	z, e := base64.StdEncoding.DecodeString("KLUv/QRYhQYAMg8tH2BHrAOoVIQkjCW8ohiQKyUZAQCIiCyQNkviQgyCwC7PwPPc8b4DO2Zet0U7Mq2T7uSUZePC6qZu4lEaKPYJWYndujHA19oct7Lco8+zF7DSYHxz8jw6u0aXPXtDJtA77Nwsejg6wDqSWoZRKFXsIsvQtEliEJbgJQSoOthQSxdBKHIwryItt7ySS2slB0Lo2d1rh7Lpszv4lDsC7NPJuCyPFLsyXNjZtNUdEexL3NhRGu4ECQBuM8rNfWJWgXCfKgTBdcP9n14z1+/cAzX0hDc=")
	if e != nil {
		t.Fatal(e)
	}
	digest := hash(z)
	ref := reference{File: "v2-object-" + digest + ".json.zst", Hash: digest, Agent: model.Claude, Month: "1970-01", Compressed: len(z), Raw: len(raw), Turns: 1}
	m := manifest{Protocol: 2, Schema: 1, DeviceID: "d", DeviceName: "rust-device", Shards: []reference{ref}, TurnCount: 1, Content: strings.Repeat("b", 64), Source: strings.Repeat("a", 64)}
	b, _ := json.Marshal(m)
	generation := hash(b)
	bad := strings.Repeat("0", 64)
	h, _ := json.Marshal(head{Protocol: 2, DeviceID: "d", Generation: bad, PreviousGeneration: &generation})
	s := &objectServer{objects: map[string][]byte{ref.File: z, "v2-generation-" + generation + ".json": b, "v2-head-d.json": h}}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	defer server.Close()
	if e = SaveConfig(Config{true, server.URL}); e != nil {
		t.Fatal(e)
	}
	if e = saveSession(session{"token", "user", time.Now().Unix() + 3600}); e != nil {
		t.Fatal(e)
	}
	remote, e := Cycle(context.Background(), model.Data{})
	if e != nil {
		t.Fatal(e)
	}
	if len(remote.Turns) != 1 || remote.Turns[0].InputTokens != 1 {
		t.Fatal(remote)
	}
	if s.puts != 0 {
		t.Fatal("read-only import unexpectedly published objects")
	}
	s.objects[ref.File] = []byte("corrupt")
	if _, e = Cycle(context.Background(), model.Data{}); e == nil {
		t.Fatal("corrupt compressed object accepted")
	}
}

func TestLoginCancellationDoesNotPersistCredentials(t *testing.T) {
	setup(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if e := CompleteLogin(ctx, server.URL, Authorization{DeviceCode: "code", Interval: 1, ExpiresIn: 60}); e == nil {
		t.Fatal("cancelled polling succeeded")
	}
	if _, e := os.Stat(path("github-sync-session.enc")); !os.IsNotExist(e) {
		t.Fatal("cancelled login saved a session")
	}
}

func TestCancelledTransportSurfacesDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, e := (transport{base: server.URL, token: "token"}).get(ctx, "object.json", 1024); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context deadline", e)
	}
}

func TestCycleImmutableReuseAndScope(t *testing.T) {
	setup(t)
	s := &objectServer{objects: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(s.serve))
	defer server.Close()
	if err := SaveConfig(Config{true, server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := saveSession(session{"token", "user", time.Now().Unix() + 3600}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	local := model.Data{TurnsStart: now, TurnsEnd: now, Turns: []model.Turn{{Agent: model.Claude, SessionKey: "s", CreatedAt: now, InputTokens: 1}}}
	ctx := context.Background()
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	// Exercise the public reader against a second device's real publication.
	if err := atomic("device.json", []byte(`{"id":"reader"}`)); err != nil {
		t.Fatal(err)
	}
	if err := saveSession(session{"token", "user", now + 3600}); err != nil {
		t.Fatal(err)
	}
	measure := func() (model.Data, int, time.Duration) {
		s.mu.Lock()
		s.gets = map[string]int{}
		s.mu.Unlock()
		start := time.Now()
		remote, err := Cycle(ctx, model.Data{})
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for name, count := range s.gets {
			if strings.HasSuffix(name, ".zst") {
				n += count
			}
		}
		return remote, n, elapsed
	}
	remote, cold, coldTime := measure()
	if len(remote.Turns) != 1 {
		t.Fatal(remote)
	}
	remote.Turns[0].InputTokens = 999 // Caller ownership must not poison the cache.
	remote, warm, warmTime := measure()
	t.Logf("block GETs cold=%d warm=%d; cycle cold=%s warm=%s", cold, warm, coldTime, warmTime)
	if warm != 0 || remote.Turns[0].InputTokens != 1 {
		t.Fatalf("cache reuse: GETs=%d data=%+v", warm, remote)
	}
	// The same digest on a different server must not inherit cached blocks.
	server2 := httptest.NewServer(http.HandlerFunc(s.serve))
	defer server2.Close()
	if err := SaveConfig(Config{true, server2.URL}); err != nil {
		t.Fatal(err)
	}
	_, scoped, _ := measure()
	if scoped == 0 {
		t.Fatal("server scope reused another server's cache")
	}
	// Different auth scope on the same URL must fetch independently too.
	server3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer second-token" && r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(401)
			return
		}
		r.Header.Set("Authorization", "Bearer token")
		s.serve(w, r)
	}))
	defer server3.Close()
	if err := SaveConfig(Config{true, server3.URL}); err != nil {
		t.Fatal(err)
	}
	measure() // Warm this URL under the first account before changing tokens.
	if err := saveSession(session{"second-token", "second", now + 3600}); err != nil {
		t.Fatal(err)
	}
	_, scoped, _ = measure()
	if scoped == 0 {
		t.Fatal("auth scope did not fetch blocks")
	}
	// Publish changed actual content with an unchanged/empty hint hash.
	if err := SaveConfig(Config{true, server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := atomic("device.json", []byte(`{"id":"local"}`)); err != nil {
		t.Fatal(err)
	}
	if err := saveSession(session{"token", "user", now + 3600}); err != nil {
		t.Fatal(err)
	}
	local.Turns[0].InputTokens = 2
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	// A changed head with a copied source hash is not evidence of unchanged
	// content. The publisher must merge/recompute under the new CAS base.
	s.mu.Lock()
	var changedHead head
	json.Unmarshal(s.objects["v3-head-local.json"], &changedHead)
	var changedManifest manifest
	json.Unmarshal(s.objects["v3-manifest-"+changedHead.Manifest+".json"], &changedManifest)
	changedManifest.DeviceName = "renamed remotely"
	changedBytes, _ := json.Marshal(changedManifest)
	injected := hash(changedBytes)
	s.objects["v3-manifest-"+injected+".json"] = changedBytes
	changedHead.Manifest = injected
	s.objects["v3-head-local.json"], _ = json.Marshal(changedHead)
	s.mu.Unlock()
	if _, err := Cycle(ctx, local); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	json.Unmarshal(s.objects["v3-head-local.json"], &changedHead)
	s.mu.Unlock()
	if changedHead.Manifest == injected {
		t.Fatal("changed head trusted a stale source hash")
	}
	if err := atomic("device.json", []byte(`{"id":"reader"}`)); err != nil {
		t.Fatal(err)
	}
	if err := saveSession(session{"token", "user", now + 3600}); err != nil {
		t.Fatal(err)
	}
	remote, _, _ = measure()
	found := false
	for _, turn := range remote.Turns {
		found = found || turn.InputTokens == 2
	}
	if !found {
		t.Fatal("changed input did not propagate")
	}
	// A previous-generation recovery must validate the server's blocks even
	// when that generation has already been read as a healthy current head.
	s.mu.Lock()
	var h head
	if err := json.Unmarshal(s.objects["v3-head-local.json"], &h); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	previous := h.Manifest
	h.Previous, h.Manifest = &previous, strings.Repeat("0", 64)
	s.objects["v3-head-local.json"], _ = json.Marshal(h)
	var m manifest
	json.Unmarshal(s.objects["v3-manifest-"+previous+".json"], &m)
	s.objects[m.Blocks[0].File] = []byte("corrupt")
	s.mu.Unlock()
	if _, err := Cycle(ctx, model.Data{}); err == nil {
		t.Fatal("cached current generation hid corruption during fallback")
	}
}

func BenchmarkUnchangedCycle(b *testing.B) {
	setup(b)
	s := &objectServer{objects: map[string][]byte{}}
	var requests, blocks, delay stdatomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(delay.Load()))
		if r.Method == "GET" {
			requests.Add(1)
			if strings.HasSuffix(r.URL.Path, ".zst") {
				blocks.Add(1)
			}
		}
		s.serve(w, r)
	}))
	defer server.Close()
	if err := SaveConfig(Config{true, server.URL}); err != nil {
		b.Fatal(err)
	}
	if err := saveSession(session{"token", "user", time.Now().Unix() + 3600}); err != nil {
		b.Fatal(err)
	}
	now := time.Now().Unix()
	makeData := func(id string) model.Data {
		d := model.Data{TurnsStart: 0, TurnsEnd: now + 3600}
		for i := 0; i < 100; i++ {
			d.Sessions = append(d.Sessions, model.Session{Agent: model.Claude, DeviceID: id, Key: fmt.Sprintf("s-%d", i), CreatedAt: now - 86400, LastActivityAt: now})
		}
		for i := 0; i < 12000; i++ {
			turn := model.Turn{Agent: model.Claude, DeviceID: id, SessionKey: fmt.Sprintf("s-%d", i%100), Model: "claude-sonnet-4-5", CreatedAt: now - int64(i%14)*86400}
			turn.InputTokens = float64(i + 200)
			turn.OutputTokens = 37
			d.Turns = append(d.Turns, turn)
		}
		return d
	}
	ctx := context.Background()
	if err := (transport{server.URL, "token"}).publish(ctx, "other", makeData("other"), nil); err != nil {
		b.Fatal(err)
	}
	local := makeData("local")
	if _, err := Cycle(ctx, local); err != nil {
		b.Fatal(err)
	}
	for _, latency := range []time.Duration{0, 20 * time.Millisecond} {
		b.Run(latency.String(), func(b *testing.B) {
			delay.Store(int64(latency))
			requests.Store(0)
			blocks.Store(0)
			for i := 0; i < b.N; i++ {
				local.TurnsEnd = time.Now().Unix() + 3600 + int64(i)
				remote, err := Cycle(ctx, local)
				if err != nil {
					b.Fatal(err)
				}
				if len(remote.Turns) != 12000 {
					b.Fatal("remote records lost")
				}
			}
			b.ReportMetric(float64(requests.Load())/float64(b.N), "GET/op")
			b.ReportMetric(float64(blocks.Load())/float64(b.N), "blockGET/op")
		})
	}
}

// identityOf replaces turnID as the publication merge key; both must agree on
// which turns are the same record, field by field.
func TestTurnIdentityMatchesTurnID(t *testing.T) {
	cost, other, zero, negZero := 1.5, 2.5, 0.0, math.Copysign(0, -1)
	base := model.Turn{DeviceID: "d", Agent: model.Claude, SessionKey: "s", CreatedAt: 10, Model: "m", InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: 4, CacheCreation5mTokens: 5, CacheCreation1hTokens: 6, TTFTMs: 7, TotalTimeMs: 8, RecordedCost: &cost}
	variants := []func(*model.Turn){
		func(*model.Turn) {},
		func(t *model.Turn) { t.DeviceID = "e" },
		func(t *model.Turn) { t.Agent = model.Codex },
		func(t *model.Turn) { t.SessionKey = "s2" },
		func(t *model.Turn) { t.CreatedAt++ },
		func(t *model.Turn) { t.Model = "m2" },
		func(t *model.Turn) { t.InputTokens = 9 },
		func(t *model.Turn) { t.OutputTokens = 9 },
		func(t *model.Turn) { t.CacheReadTokens = 9 },
		func(t *model.Turn) { t.CacheCreationTokens = 9 },
		func(t *model.Turn) { t.CacheCreation5mTokens = 9 },
		func(t *model.Turn) { t.CacheCreation1hTokens = 9 },
		func(t *model.Turn) { t.TTFTMs = 9 },
		func(t *model.Turn) { t.TotalTimeMs = 9 },
		func(t *model.Turn) { t.RecordedCost = nil },
		func(t *model.Turn) { t.RecordedCost = &other },
		func(t *model.Turn) { t.RecordedCost = &zero },
		func(t *model.Turn) { t.RecordedCost = &negZero },
		func(t *model.Turn) { t.InputTokens = negZero },
		func(t *model.Turn) { t.TTFTMs = 0 },
		// encoding/json writes each invalid UTF-8 byte as U+FFFD, so turnID
		// merges these; the identity must agree.
		func(t *model.Turn) { t.SessionKey = "\xff" },
		func(t *model.Turn) { t.SessionKey = "\xfe" },
		func(t *model.Turn) { t.SessionKey = "\xff\xfe" },
		func(t *model.Turn) { t.SessionKey = "\ufffd" },
		func(t *model.Turn) { t.SessionKey = "\ufffd\ufffd" },
		func(t *model.Turn) { t.Model = "m\x80" },
		func(t *model.Turn) { t.Model = "m\xc3" },
		func(t *model.Turn) { t.DeviceID = "\xff" },
		// Fields outside the identity must not split a record.
		func(t *model.Turn) { t.DeviceName = "renamed"; t.DedupKey = "k"; t.ContextStream = "c" },
	}
	turns := make([]model.Turn, len(variants))
	for i, change := range variants {
		turns[i] = base
		c := cost
		turns[i].RecordedCost = &c
		change(&turns[i])
	}
	for i := range turns {
		for j := range turns {
			if (identityOf(turns[i]) == identityOf(turns[j])) != (turnID(turns[i]) == turnID(turns[j])) {
				t.Fatalf("variants %d and %d: identity and turnID disagree", i, j)
			}
		}
	}
}
