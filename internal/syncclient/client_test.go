package syncclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func setup(t *testing.T) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("APPDATA", h)
	t.Setenv("XDG_CONFIG_HOME", h)
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
	if info.Mode().Perm() != 0600 {
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
	case "GET":
		if !exists {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("ETag", etag)
		w.Write(old)
	case "PUT":
		if r.Header.Get("If-None-Match") == "*" && exists {
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
	local := model.Data{TurnsStart: now, TurnsEnd: now, Turns: []model.Turn{{Agent: model.Claude, SessionKey: "local-session", CreatedAt: now, InputTokens: 2}}}
	ctx := context.Background()
	remote, e := Cycle(ctx, local)
	if e != nil {
		t.Fatal(e)
	}
	if len(remote.Sessions) != 1 || remote.Sessions[0].Key != "remote" {
		t.Fatal(remote)
	}
	count := s.puts
	if _, e = Cycle(ctx, local); e != nil {
		t.Fatal(e)
	}
	if s.puts != count {
		t.Fatalf("unchanged data wrote %d objects", s.puts-count)
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
	s.objects["v3-manifest-"+h.Manifest+".json"] = []byte(`{}`)
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
