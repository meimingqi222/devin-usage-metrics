// devin-usage-metrics-sync is a small self-hosted object API for v3 sync data.
// It intentionally has no database or external dependencies: a bearer token maps
// to one tenant directory and object revisions are SHA-256 ETags.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxObjectBytes = 8 << 20
const defaultQuotaBytes int64 = 2 << 30
const defaultGrace = 7 * 24 * time.Hour
const v3Retention = 366 * 24 * time.Hour
const v2MigrationGrace = 14 * 24 * time.Hour

type server struct {
	root           string
	githubClientID string
	authSecret     []byte
	githubOAuthURL string
	githubAPIURL   string
	httpClient     *http.Client
	quotaBytes     int64
	grace          time.Duration
	mu             sync.Mutex
}

func main() {
	root := envOr("SYNC_DATA_DIR", "./data")
	githubClientID := os.Getenv("GITHUB_CLIENT_ID")
	if githubClientID == "" {
		log.Fatal("GITHUB_CLIENT_ID is required")
	}
	authSecret := []byte(os.Getenv("SYNC_AUTH_SECRET"))
	if len(authSecret) < 32 {
		log.Fatal("SYNC_AUTH_SECRET is required and must be at least 32 characters")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		log.Fatalf("create data directory: %v", err)
	}
	s := &server{
		root:           root,
		githubClientID: githubClientID,
		authSecret:     authSecret,
		githubOAuthURL: envOr("GITHUB_OAUTH_URL", "https://github.com"),
		githubAPIURL:   envOr("GITHUB_API_URL", "https://api.github.com"),
		httpClient:     &http.Client{Timeout: 15 * time.Second},
		quotaBytes:     intEnv("SYNC_QUOTA_BYTES", defaultQuotaBytes),
		grace:          durationEnv("SYNC_GC_GRACE", defaultGrace),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/auth/github/device", s.githubDevice)
	mux.HandleFunc("POST /v1/auth/github/token", s.githubToken)
	mux.HandleFunc("GET /v1/objects", s.list)
	mux.HandleFunc("GET /v1/usage", s.usage)
	mux.HandleFunc("POST /v1/gc", s.gc)
	mux.HandleFunc("/v1/objects/", s.object)
	addr := envOr("SYNC_LISTEN", ":8080")
	go s.dailyGC()
	log.Printf("sync API listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, securityHeaders(mux)))
}

func intEnv(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		log.Printf("invalid %s=%q; using %d", key, value, fallback)
		return fallback
	}
	return parsed
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		log.Printf("invalid %s=%q; using %s", key, value, fallback)
		return fallback
	}
	return parsed
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func validTenant(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (s *server) tenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	const prefix = "Bearer "
	given := strings.TrimPrefix(r.Header.Get("Authorization"), prefix)
	if given == r.Header.Get("Authorization") {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return "", false
	}
	claims, err := parseSession(s.authSecret, given)
	if err != nil || !validTenant(claims.Tenant) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "invalid or expired session", http.StatusUnauthorized)
		return "", false
	}
	return claims.Tenant, true
}

type sessionClaims struct {
	Tenant  string `json:"tenant"`
	Login   string `json:"login"`
	Expires int64  `json:"expires"`
}

func issueSession(secret []byte, claims sessionClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func parseSession(secret []byte, token string) (sessionClaims, error) {
	var claims sessionClaims
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return claims, errors.New("malformed session")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return claims, errors.New("invalid session signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return claims, errors.New("invalid session payload")
	}
	if claims.Expires <= time.Now().Unix() {
		return claims, errors.New("expired session")
	}
	return claims, nil
}

func objectName(path string) (string, bool) {
	name := strings.TrimPrefix(path, "/v1/objects/")
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") || len(name) > 255 {
		return "", false
	}
	return name, true
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

type githubDeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

func (s *server) githubDevice(w http.ResponseWriter, _ *http.Request) {
	response, err := s.githubForm("/login/device/code", url.Values{
		"client_id": {s.githubClientID},
		"scope":     {"read:user"},
	})
	if err != nil {
		http.Error(w, "start GitHub login", http.StatusBadGateway)
		return
	}
	var device githubDeviceCode
	if json.Unmarshal(response, &device) != nil || device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" {
		http.Error(w, "invalid GitHub device response", http.StatusBadGateway)
		return
	}
	if device.Interval < 1 {
		device.Interval = 5
	}
	writeJSON(w, http.StatusOK, device)
}

func (s *server) githubToken(w http.ResponseWriter, r *http.Request) {
	var request struct {
		DeviceCode string `json:"device_code"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&request) != nil || request.DeviceCode == "" {
		http.Error(w, "device_code is required", http.StatusBadRequest)
		return
	}
	response, err := s.githubForm("/login/oauth/access_token", url.Values{
		"client_id":   {s.githubClientID},
		"device_code": {request.DeviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	})
	if err != nil {
		http.Error(w, "complete GitHub login", http.StatusBadGateway)
		return
	}
	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Interval    int    `json:"interval"`
	}
	if json.Unmarshal(response, &result) != nil {
		http.Error(w, "invalid GitHub token response", http.StatusBadGateway)
		return
	}
	if result.Error == "authorization_pending" || result.Error == "slow_down" {
		interval := result.Interval
		if interval < 1 {
			interval = 5
		}
		w.Header().Set("Retry-After", strconv.Itoa(interval))
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "pending"})
		return
	}
	if result.Error != "" || result.AccessToken == "" {
		http.Error(w, "GitHub login was not completed", http.StatusUnauthorized)
		return
	}
	user, err := s.githubUser(result.AccessToken)
	if err != nil {
		http.Error(w, "read GitHub identity", http.StatusBadGateway)
		return
	}
	claims := sessionClaims{
		Tenant:  fmt.Sprintf("github-%d", user.ID),
		Login:   user.Login,
		Expires: time.Now().Add(90 * 24 * time.Hour).Unix(),
	}
	token, err := issueSession(s.authSecret, claims)
	if err != nil {
		http.Error(w, "create sync session", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sync_token": token,
		"login":      user.Login,
		"expires_at": claims.Expires,
	})
}

func (s *server) githubForm(path string, form url.Values) ([]byte, error) {
	request, err := http.NewRequest(http.MethodPost, strings.TrimRight(s.githubOAuthURL, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "devin-usage-metrics-sync")
	response, err := s.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("GitHub OAuth request failed")
	}
	return body, nil
}

func (s *server) githubUser(accessToken string) (struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}, error) {
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	request, err := http.NewRequest(http.MethodGet, strings.TrimRight(s.githubAPIURL, "/")+"/user", nil)
	if err != nil {
		return user, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("User-Agent", "devin-usage-metrics-sync")
	response, err := s.httpClient.Do(request)
	if err != nil {
		return user, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&user) != nil || user.ID <= 0 || user.Login == "" {
		return user, errors.New("invalid GitHub user response")
	}
	return user, nil
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	entries, err := os.ReadDir(filepath.Join(s.root, tenant))
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, []string{})
		return
	}
	if err != nil {
		http.Error(w, "read storage", http.StatusInternalServerError)
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, names)
}

func (s *server) usage(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	used, objects, err := s.tenantUsage(tenant)
	if err != nil {
		http.Error(w, "read storage", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"used_bytes":  used,
		"quota_bytes": s.quotaBytes,
		"warning":     s.quotaBytes > 0 && used*100 >= s.quotaBytes*80,
		"objects":     objects,
	})
}

// gc is intentionally authenticated with the tenant token. It is useful for an
// operator after a migration; normal cleanup is also scheduled once a day.
func (s *server) gc(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted, err := s.gcTenant(tenant, time.Now())
	if err != nil {
		http.Error(w, "gc storage", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *server) object(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenant(w, r)
	if !ok {
		return
	}
	name, ok := objectName(r.URL.Path)
	if !ok {
		http.Error(w, "invalid object name", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.root, tenant, name)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.read(w, r, path)
	case http.MethodPut:
		s.write(w, r, path)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) read(w http.ResponseWriter, r *http.Request, path string) {
	bytes, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "read object", http.StatusInternalServerError)
		return
	}
	w.Header().Set("ETag", quoteETag(bytes))
	w.Header().Set("Content-Length", fmt.Sprint(len(bytes)))
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bytes)
	}
}

func (s *server) write(w http.ResponseWriter, r *http.Request, path string) {
	body := http.MaxBytesReader(w, r.Body, maxObjectBytes)
	defer body.Close()
	bytes, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "object exceeds 8 MiB limit", http.StatusRequestEntityTooLarge)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "read object", http.StatusInternalServerError)
		return
	}
	exists := err == nil
	if !conditionsMatch(r, existing, exists) {
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
		return
	}
	// Only a newly written/changed object consumes quota. Reads, list and GC
	// remain available at the hard limit so a client can always recover space.
	oldSize := int64(0)
	if exists {
		oldSize = int64(len(existing))
	}
	used, _, usageErr := s.tenantUsage(filepath.Base(filepath.Dir(path)))
	if usageErr != nil {
		http.Error(w, "read storage", http.StatusInternalServerError)
		return
	}
	if s.quotaBytes > 0 && used-oldSize+int64(len(bytes)) > s.quotaBytes {
		w.Header().Set("X-Sync-Quota-Bytes", strconv.FormatInt(s.quotaBytes, 10))
		http.Error(w, "storage quota exceeded", http.StatusInsufficientStorage)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		http.Error(w, "create storage", http.StatusInternalServerError)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".object-*")
	if err != nil {
		http.Error(w, "create object", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(bytes); err != nil || tmp.Sync() != nil || tmp.Close() != nil {
		http.Error(w, "write object", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		http.Error(w, "commit object", http.StatusInternalServerError)
		return
	}
	if !exists && strings.HasPrefix(filepath.Base(path), "v3-head-") {
		// 首次 v3 发布时落一个不可变迁移时间点。后续 head 更新不会重置它，
		// 因而可以在所有旧设备连续使用 v3 满 14 天后安全清理 v1/v2。
		_ = s.writeMigrationMarker(filepath.Dir(path), filepath.Base(path), time.Now())
	}
	w.Header().Set("ETag", quoteETag(bytes))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) writeMigrationMarker(dir, head string, now time.Time) error {
	device := strings.TrimSuffix(strings.TrimPrefix(head, "v3-head-"), ".json")
	if device == "" {
		return nil
	}
	path := filepath.Join(dir, "v3-migrated-"+device+".txt")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(strconv.FormatInt(now.Unix(), 10)), 0o600)
}

func (s *server) tenantUsage(tenant string) (int64, int, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, tenant))
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var bytes int64
	objects := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, 0, err
		}
		bytes += info.Size()
		objects++
	}
	return bytes, objects, nil
}

func (s *server) dailyGC() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		entries, err := os.ReadDir(s.root)
		if err != nil {
			log.Printf("list storage for gc: %v", err)
			s.mu.Unlock()
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !validTenant(entry.Name()) {
				continue
			}
			tenant := entry.Name()
			if deleted, err := s.gcTenant(tenant, time.Now()); err != nil {
				log.Printf("gc tenant=%s: %v", tenant, err)
			} else if deleted > 0 {
				log.Printf("gc tenant=%s deleted=%d", tenant, deleted)
			}
		}
		s.mu.Unlock()
	}
}

// gcTenant implements mark-and-sweep without a database. Current and previous
// heads are roots; manifests from the recent grace window are extra roots for
// interrupted concurrent uploads. Unreferenced files need to survive the same
// grace period before deletion.
func (s *server) gcTenant(tenant string, now time.Time) (int, error) {
	dir := filepath.Join(s.root, tenant)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	marked := make(map[string]bool)
	manifests := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "v3-head-") && strings.HasSuffix(name, ".json") {
			marked[name] = true
			bytes, err := os.ReadFile(filepath.Join(dir, name))
			if err == nil {
				markHeadManifests(bytes, manifests)
			}
		}
		if strings.HasPrefix(name, "v3-manifest-") && strings.HasSuffix(name, ".json") {
			info, infoErr := entry.Info()
			if infoErr == nil && now.Sub(info.ModTime()) <= s.grace {
				manifests[name] = true
			}
		}
	}
	for name := range manifests {
		marked[name] = true
		bytes, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		markManifestObjects(bytes, marked, now.Add(-v3Retention).UTC().Format("2006-01-02"))
	}
	legacyReady := v2MigrationReady(dir, entries, now)
	deleted := 0
	for _, entry := range entries {
		if !entry.Type().IsRegular() || marked[entry.Name()] {
			continue
		}
		// v2/v1 的清理需要客户端确认所有设备已稳定迁移至少 14 天；首期服务
		// 没有设备确认数据库，因此宁可保留旧协议对象，绝不由通用 GC 提前删掉。
		if strings.HasPrefix(entry.Name(), "v2-") || strings.HasPrefix(entry.Name(), "device-") {
			if !legacyReady {
				continue
			}
		}
		if strings.HasPrefix(entry.Name(), "v3-migrated-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < s.grace {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func v2MigrationReady(dir string, entries []os.DirEntry, now time.Time) bool {
	devices := make(map[string]bool)
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "v2-head-") && strings.HasSuffix(name, ".json") {
			devices[strings.TrimSuffix(strings.TrimPrefix(name, "v2-head-"), ".json")] = true
		}
		if strings.HasPrefix(name, "device-") && strings.HasSuffix(name, ".json") {
			devices[strings.TrimSuffix(strings.TrimPrefix(name, "device-"), ".json")] = true
		}
	}
	if len(devices) == 0 {
		return false
	}
	for device := range devices {
		if _, err := os.Stat(filepath.Join(dir, "v3-head-"+device+".json")); err != nil {
			return false
		}
		bytes, err := os.ReadFile(filepath.Join(dir, "v3-migrated-"+device+".txt"))
		if err != nil {
			return false
		}
		at, err := strconv.ParseInt(strings.TrimSpace(string(bytes)), 10, 64)
		if err != nil || now.Sub(time.Unix(at, 0)) < v2MigrationGrace {
			return false
		}
	}
	return true
}

func markHeadManifests(bytes []byte, marked map[string]bool) {
	var value struct {
		Manifest         string `json:"manifest"`
		PreviousManifest string `json:"previous_manifest"`
	}
	if json.Unmarshal(bytes, &value) != nil {
		return
	}
	for _, hash := range []string{value.Manifest, value.PreviousManifest} {
		if validHash(hash) {
			marked["v3-manifest-"+hash+".json"] = true
		}
	}
}

func markManifestObjects(bytes []byte, marked map[string]bool, retentionDay string) {
	var value struct {
		Blocks []struct {
			File   string `json:"file"`
			UTCDay string `json:"utc_day"`
		} `json:"blocks"`
		Sessions []struct {
			File   string `json:"file"`
			UTCDay string `json:"utc_day"`
		} `json:"sessions"`
	}
	if json.Unmarshal(bytes, &value) != nil {
		return
	}
	for _, reference := range value.Blocks {
		if (reference.UTCDay == "" || reference.UTCDay >= retentionDay) && validObjectName(reference.File, "v3-block-", ".zst") {
			marked[reference.File] = true
		}
	}
	for _, reference := range value.Sessions {
		if (reference.UTCDay == "" || reference.UTCDay >= retentionDay) && validObjectName(reference.File, "v3-session-", ".zst") {
			marked[reference.File] = true
		}
	}
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func validObjectName(value, prefix, suffix string) bool {
	return strings.HasPrefix(value, prefix) && strings.HasSuffix(value, suffix) &&
		validHash(strings.TrimSuffix(strings.TrimPrefix(value, prefix), suffix))
}

func conditionsMatch(r *http.Request, existing []byte, exists bool) bool {
	if value := r.Header.Get("If-None-Match"); value != "" {
		return value == "*" && !exists
	}
	if value := r.Header.Get("If-Match"); value != "" {
		return exists && value == quoteETag(existing)
	}
	return true
}

func quoteETag(bytes []byte) string {
	sum := sha256.Sum256(bytes)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
