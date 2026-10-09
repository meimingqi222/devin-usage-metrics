package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testAuthSecret = "test-auth-secret-with-at-least-thirty-two-characters"
const testTenant = "github-123"

func newTestCore(root string, quota int64) *server {
	return &server{
		root:           root,
		githubClientID: "test-client-id",
		authSecret:     []byte(testAuthSecret),
		githubOAuthURL: "http://127.0.0.1:1",
		githubAPIURL:   "http://127.0.0.1:1",
		httpClient:     http.DefaultClient,
		quotaBytes:     quota,
		grace:          time.Hour,
	}
}

func newTestServer(t *testing.T) http.Handler {
	return newTestServerWithQuota(t, 0)
}

func newTestServerWithQuota(t *testing.T, quota int64) http.Handler {
	t.Helper()
	s := newTestCore(t.TempDir(), quota)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/github/device", s.githubDevice)
	mux.HandleFunc("POST /v1/auth/github/token", s.githubToken)
	mux.HandleFunc("GET /v1/objects", s.list)
	mux.HandleFunc("GET /v1/usage", s.usage)
	mux.HandleFunc("POST /v1/gc", s.gc)
	mux.HandleFunc("/v1/objects/", s.object)
	return mux
}

func request(handler http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	token, _ := issueSession([]byte(testAuthSecret), sessionClaims{Tenant: testTenant, Login: "octo", Expires: time.Now().Add(time.Hour).Unix()})
	r.Header.Set("Authorization", "Bearer "+token)
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestQuotaStillAllowsUsageAndRejectsNewWrite(t *testing.T) {
	handler := newTestServerWithQuota(t, 5)
	if got := request(handler, http.MethodPut, "/v1/objects/a", "12345", nil); got.Code != http.StatusNoContent {
		t.Fatalf("initial write status=%d", got.Code)
	}
	if got := request(handler, http.MethodPut, "/v1/objects/b", "x", nil); got.Code != http.StatusInsufficientStorage {
		t.Fatalf("quota write status=%d body=%s", got.Code, got.Body.String())
	}
	if got := request(handler, http.MethodGet, "/v1/usage", "", nil); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "used_bytes") {
		t.Fatalf("usage unavailable at quota: status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestGCMarksHeadManifestReferencesAndRemovesOldOrphan(t *testing.T) {
	dir := t.TempDir()
	s := newTestCore(dir, 0)
	tenant := filepath.Join(dir, "tester")
	if err := os.MkdirAll(tenant, 0o700); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	block := "v3-block-" + hash + ".zst"
	manifestBytes := []byte(`{"blocks":[{"file":"` + block + `"}],"sessions":[]}`)
	manifestHash := strings.Trim(quoteETag(manifestBytes), `"`)
	manifest := "v3-manifest-" + manifestHash + ".json"
	if err := os.WriteFile(filepath.Join(tenant, block), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tenant, manifest), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tenant, "v3-head-device.json"), []byte(`{"manifest":"`+manifestHash+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(tenant, "v3-block-"+strings.Repeat("c", 64)+".zst")
	if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.gcTenant("tester", time.Now())
	if err != nil || deleted != 1 {
		t.Fatalf("gc deleted=%d err=%v", deleted, err)
	}
	if _, err := os.Stat(filepath.Join(tenant, block)); err != nil {
		t.Fatalf("referenced block removed: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still exists: %v", err)
	}
}

func TestGCRemovesLegacyOnlyAfterEveryDeviceMigratesForFourteenDays(t *testing.T) {
	dir := t.TempDir()
	s := newTestCore(dir, 0)
	tenant := filepath.Join(dir, "tester")
	if err := os.MkdirAll(tenant, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"v2-head-abc.json", "v2-object-" + strings.Repeat("a", 64) + ".json.zst", "device-abc.json", "v3-head-abc.json"} {
		body := []byte("{}")
		if strings.HasPrefix(name, "v3-head-") {
			hash := strings.Trim(quoteETag([]byte(`{"blocks":[],"sessions":[]}`)), `"`)
			body = []byte(`{"manifest":"` + hash + `"}`)
			if err := os.WriteFile(filepath.Join(tenant, "v3-manifest-"+hash+".json"), []byte(`{"blocks":[],"sessions":[]}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(tenant, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := os.WriteFile(filepath.Join(tenant, "v3-migrated-abc.txt"), []byte(strconv.FormatInt(now.Add(-15*24*time.Hour).Unix(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	for _, name := range []string{"v2-head-abc.json", "v2-object-" + strings.Repeat("a", 64) + ".json.zst", "device-abc.json"} {
		if err := os.Chtimes(filepath.Join(tenant, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(tenant)
	if err != nil || !v2MigrationReady(tenant, entries, now) {
		t.Fatalf("migration should be ready: err=%v", err)
	}
	deleted, err := s.gcTenant("tester", now)
	if err != nil || deleted != 3 {
		t.Fatalf("gc deleted=%d err=%v", deleted, err)
	}
	if _, err := os.Stat(filepath.Join(tenant, "v3-head-abc.json")); err != nil {
		t.Fatalf("v3 head removed: %v", err)
	}
}

func TestGCKeepsLegacyUntilEveryDeviceHasCompletedMigrationGrace(t *testing.T) {
	dir := t.TempDir()
	s := newTestCore(dir, 0)
	tenant := filepath.Join(dir, "tester")
	if err := os.MkdirAll(tenant, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, name := range []string{"v2-head-first.json", "v2-head-second.json", "v3-head-first.json", "v3-head-second.json"} {
		body := []byte("{}")
		if strings.HasPrefix(name, "v3-head-") {
			hash := strings.Trim(quoteETag([]byte(`{"blocks":[],"sessions":[]}`)), `"`)
			body = []byte(`{"manifest":"` + hash + `"}`)
			if err := os.WriteFile(filepath.Join(tenant, "v3-manifest-"+hash+".json"), []byte(`{"blocks":[],"sessions":[]}`), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(tenant, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(tenant, "v3-migrated-first.txt"), []byte(strconv.FormatInt(now.Add(-15*24*time.Hour).Unix(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tenant, "v3-migrated-second.txt"), []byte(strconv.FormatInt(now.Add(-13*24*time.Hour).Unix(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(tenant, "device-first.json")
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(legacy, old, old); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(tenant)
	if err != nil || v2MigrationReady(tenant, entries, now) {
		t.Fatalf("migration must remain blocked before every device reaches grace: err=%v", err)
	}
	if deleted, err := s.gcTenant("tester", now); err != nil || deleted != 0 {
		t.Fatalf("gc deleted=%d err=%v", deleted, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy object removed before migration grace: %v", err)
	}
}

func TestGCExpiresV3ObjectsOutsideRetention(t *testing.T) {
	dir := t.TempDir()
	s := newTestCore(dir, 0)
	tenant := filepath.Join(dir, "tester")
	if err := os.MkdirAll(tenant, 0o700); err != nil {
		t.Fatal(err)
	}
	blockHash := strings.Repeat("a", 64)
	block := "v3-block-" + blockHash + ".zst"
	manifestBytes := []byte(`{"blocks":[{"file":"` + block + `","utc_day":"2000-01-01"}],"sessions":[]}`)
	manifestHash := strings.Trim(quoteETag(manifestBytes), `"`)
	manifest := "v3-manifest-" + manifestHash + ".json"
	if err := os.WriteFile(filepath.Join(tenant, block), []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tenant, manifest), manifestBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tenant, "v3-head-device.json"), []byte(`{"manifest":"`+manifestHash+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(tenant, block), old, old); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.gcTenant("tester", now); err != nil || deleted != 1 {
		t.Fatalf("gc deleted=%d err=%v", deleted, err)
	}
	if _, err := os.Stat(filepath.Join(tenant, block)); !os.IsNotExist(err) {
		t.Fatalf("expired v3 block still exists: %v", err)
	}
}

func TestObjectLifecycleAndConditions(t *testing.T) {
	handler := newTestServer(t)
	first := request(handler, http.MethodPut, "/v1/objects/object-a", "first", map[string]string{"If-None-Match": "*"})
	if first.Code != http.StatusNoContent {
		t.Fatalf("first PUT status=%d body=%s", first.Code, first.Body.String())
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("PUT did not return ETag")
	}
	if got := request(handler, http.MethodPut, "/v1/objects/object-a", "second", map[string]string{"If-None-Match": "*"}); got.Code != http.StatusPreconditionFailed {
		t.Fatalf("immutable overwrite status=%d", got.Code)
	}
	if got := request(handler, http.MethodPut, "/v1/objects/object-a", "second", map[string]string{"If-Match": etag}); got.Code != http.StatusNoContent {
		t.Fatalf("conditional update status=%d body=%s", got.Code, got.Body.String())
	}
	get := request(handler, http.MethodGet, "/v1/objects/object-a", "", nil)
	if get.Code != http.StatusOK || get.Body.String() != "second" {
		t.Fatalf("GET status=%d body=%q", get.Code, get.Body.String())
	}
	if _, valid := objectName("/v1/objects/../secret"); valid {
		t.Fatal("path traversal object name was accepted")
	}
}

func TestTenantAndAuthenticationIsolation(t *testing.T) {
	handler := newTestServer(t)
	if got := request(handler, http.MethodGet, "/v1/objects", "", nil); got.Code != http.StatusOK || got.Body.String() != "[]\n" {
		t.Fatalf("empty list status=%d body=%q", got.Code, got.Body.String())
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/objects", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status=%d", w.Code)
	}
}

func TestGitHubDeviceLoginMapsSameAccountToStableTenant(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != "test-client-id" || r.Form.Get("scope") != "read:user" {
				t.Fatalf("unexpected device request: form=%v err=%v", r.Form, err)
			}
			_, _ = w.Write([]byte(`{"device_code":"device-code","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","expires_in":900,"interval":1}`))
		case "/login/oauth/access_token":
			if err := r.ParseForm(); err != nil || r.Form.Get("device_code") != "device-code" {
				t.Fatalf("unexpected token request: form=%v err=%v", r.Form, err)
			}
			_, _ = w.Write([]byte(`{"access_token":"github-access-token"}`))
		case "/user":
			if got := r.Header.Get("Authorization"); got != "Bearer github-access-token" {
				t.Fatalf("unexpected GitHub authorization: %q", got)
			}
			_, _ = w.Write([]byte(`{"id":42,"login":"octocat"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer github.Close()

	s := newTestCore(t.TempDir(), 0)
	s.githubOAuthURL = github.URL
	s.githubAPIURL = github.URL
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/github/device", s.githubDevice)
	mux.HandleFunc("POST /v1/auth/github/token", s.githubToken)
	mux.HandleFunc("GET /v1/objects", s.list)

	deviceRequest := httptest.NewRequest(http.MethodPost, "/v1/auth/github/device", nil)
	deviceResponse := httptest.NewRecorder()
	mux.ServeHTTP(deviceResponse, deviceRequest)
	if deviceResponse.Code != http.StatusOK {
		t.Fatalf("device flow start status=%d body=%s", deviceResponse.Code, deviceResponse.Body.String())
	}
	var device githubDeviceCode
	if err := json.Unmarshal(deviceResponse.Body.Bytes(), &device); err != nil || device.DeviceCode != "device-code" {
		t.Fatalf("device response=%s err=%v", deviceResponse.Body.String(), err)
	}

	tokenRequest := httptest.NewRequest(http.MethodPost, "/v1/auth/github/token", strings.NewReader(`{"device_code":"device-code"}`))
	tokenRequest.Header.Set("Content-Type", "application/json")
	tokenResponse := httptest.NewRecorder()
	mux.ServeHTTP(tokenResponse, tokenRequest)
	if tokenResponse.Code != http.StatusOK {
		t.Fatalf("device flow completion status=%d body=%s", tokenResponse.Code, tokenResponse.Body.String())
	}
	var session struct {
		SyncToken string `json:"sync_token"`
		Login     string `json:"login"`
	}
	if err := json.Unmarshal(tokenResponse.Body.Bytes(), &session); err != nil || session.SyncToken == "" || session.Login != "octocat" {
		t.Fatalf("session response=%s err=%v", tokenResponse.Body.String(), err)
	}
	claims, err := parseSession(s.authSecret, session.SyncToken)
	if err != nil || claims.Tenant != "github-42" {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}

	objectRequest := httptest.NewRequest(http.MethodGet, "/v1/objects", nil)
	objectRequest.Header.Set("Authorization", "Bearer "+session.SyncToken)
	objectResponse := httptest.NewRecorder()
	mux.ServeHTTP(objectResponse, objectRequest)
	if objectResponse.Code != http.StatusOK {
		t.Fatalf("session cannot access stable tenant: status=%d body=%s", objectResponse.Code, objectResponse.Body.String())
	}
}
func TestGCStopsOnCorruptRoots(t *testing.T) {
	for _, corrupt := range []string{"head", "manifest", "missing-manifest", "valid-json-corruption", "missing-arrays", "invalid-reference"} {
		t.Run(corrupt, func(t *testing.T) {
			root := t.TempDir()
			s := newTestCore(root, 0)
			dir := filepath.Join(root, "tester")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			block := "v3-block-" + strings.Repeat("b", 64) + ".zst"
			manifest := `{"blocks":[{"file":"` + block + `"}],"sessions":[]}`
			if corrupt == "missing-arrays" {
				manifest = "{}"
			}
			if corrupt == "invalid-reference" {
				manifest = `{"blocks":[{"file":"invalid"}],"sessions":[]}`
			}
			hash := strings.Trim(quoteETag([]byte(manifest)), `"`)
			head := `{"manifest":"` + hash + `"}`
			if corrupt == "head" {
				head = "{"
			}
			if corrupt == "manifest" {
				manifest = "{"
			}
			if corrupt == "valid-json-corruption" {
				manifest = `{"blocks":[],"sessions":[]}`
			}
			files := map[string]string{"v3-head-device.json": head, block: "valuable data"}
			if corrupt != "missing-manifest" {
				files["v3-manifest-"+hash+".json"] = manifest
			}
			old := time.Now().Add(-2 * time.Hour)
			for name, body := range files {
				p := filepath.Join(dir, name)
				if err := os.WriteFile(p, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(p, old, old); err != nil {
					t.Fatal(err)
				}
			}
			deleted, err := s.gcTenant("tester", time.Now())
			if err == nil || deleted != 0 {
				t.Fatalf("unsafe GC: deleted=%d err=%v", deleted, err)
			}
			if _, err := os.Stat(filepath.Join(dir, block)); err != nil {
				t.Fatalf("referenced data removed: %v", err)
			}
		})
	}
}

func TestConcurrentWritesRespectCachedQuota(t *testing.T) {
	handler := newTestServerWithQuota(t, 100)
	results := make(chan int, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			results <- request(handler, http.MethodPut, "/v1/objects/obj-"+strconv.Itoa(i), "0123456789", nil).Code
		}(i)
	}
	accepted := 0
	for i := 0; i < 20; i++ {
		switch status := <-results; status {
		case http.StatusNoContent:
			accepted++
		case http.StatusInsufficientStorage:
		default:
			t.Fatalf("unexpected PUT status %d", status)
		}
	}
	if accepted != 10 {
		t.Fatalf("quota accepted %d writes; want 10", accepted)
	}
	got := request(handler, http.MethodGet, "/v1/usage", "", nil)
	var usage struct {
		Bytes   int64 `json:"used_bytes"`
		Objects int   `json:"objects"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.Bytes != 100 || usage.Objects != 10 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestCachedUsageTracksOverwriteAndGC(t *testing.T) {
	core := newTestCore(t.TempDir(), 100)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/usage", core.usage)
	mux.HandleFunc("POST /v1/gc", core.gc)
	mux.HandleFunc("/v1/objects/", core.object)
	for _, body := range []string{"12345", "12"} {
		if got := request(mux, http.MethodPut, "/v1/objects/a", body, nil); got.Code != http.StatusNoContent {
			t.Fatal(got.Code)
		}
	}
	state := core.tenantState(testTenant)
	if state.bytes != 2 || state.objects != 1 {
		t.Fatalf("overwrite accounting %+v", state)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(core.root, testTenant, "a"), old, old); err != nil {
		t.Fatal(err)
	}
	if got := request(mux, http.MethodPost, "/v1/gc", "", nil); got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
	if state.usageReady {
		t.Fatal("GC must invalidate usage totals")
	}
	if got := request(mux, http.MethodGet, "/v1/usage", "", nil); got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
	if state.bytes != 0 || state.objects != 0 {
		t.Fatalf("GC accounting %+v", state)
	}
	if got := request(mux, http.MethodPut, "/v1/objects/b", "123", nil); got.Code != http.StatusNoContent {
		t.Fatal(got.Code)
	}
	if state.bytes != 3 || state.objects != 1 {
		t.Fatalf("post-GC accounting %+v", state)
	}
}

func TestBusyTenantDoesNotBlockOtherTenant(t *testing.T) {
	core := newTestCore(t.TempDir(), 100)
	busy := core.tenantState("github-456")
	busy.mu.Lock()
	defer busy.mu.Unlock()
	finished := make(chan int, 1)
	go func() {
		finished <- request(http.HandlerFunc(core.object), http.MethodPut, "/v1/objects/a", "123", nil).Code
	}()
	select {
	case status := <-finished:
		if status != http.StatusNoContent {
			t.Fatal(status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a different tenant is blocked by the busy tenant")
	}
}
