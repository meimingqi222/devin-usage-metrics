package syncclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

const accountTestPassword = "fixture123" // Exercise sync at the 10-character minimum.

type accountServerFixture struct {
	mu      sync.Mutex
	objects map[string][]byte
	writes  int
	capable bool
}

func (s *accountServerFixture) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.URL.Path == "/v1/capabilities":
		if !s.capable {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"account_backups":1}`)
	case r.URL.Path == "/v1/objects":
		names := []string{}
		for name := range s.objects {
			names = append(names, name)
		}
		json.NewEncoder(w).Encode(names)
	case strings.HasPrefix(r.URL.Path, "/v1/objects/"):
		name := strings.TrimPrefix(r.URL.Path, "/v1/objects/")
		if r.Method == "PUT" {
			s.objects[name], _ = io.ReadAll(r.Body)
			s.writes++
			w.WriteHeader(http.StatusNoContent)
		} else if raw, ok := s.objects[name]; ok {
			w.Write(raw)
		} else {
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func accountSyncDevice(t *testing.T, base, id string) (*accounts.Store, string) {
	t.Helper()
	setup(t)
	if err := atomic("device.json", []byte(`{"id":"`+id+`"}`)); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(Config{true, base}); err != nil {
		t.Fatal(err)
	}
	if err := saveSession(session{"token", "fixture-user", time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	scope, err := AccountSyncScope()
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAccountSyncConfig(AccountSyncConfig{Enabled: true, Scope: scope}); err != nil {
		t.Fatal(err)
	}
	store := accounts.Open()
	if err := store.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	return store, scope
}

func addSyncAccount(t *testing.T, store *accounts.Store, email, access string) {
	t.Helper()
	_, err := store.Add("claude", map[string]any{"claudeAiOauth": map[string]any{"emailAddress": email, "accessToken": access, "refreshToken": "private-refresh"}}, "file")
	if err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedAccountSyncTwoDevicesAndWrongPassword(t *testing.T) {
	cloud := &accountServerFixture{objects: map[string][]byte{}, capable: true}
	ts := httptest.NewServer(http.HandlerFunc(cloud.serve))
	defer ts.Close()
	first, scope := accountSyncDevice(t, ts.URL, "device-one")
	firstRoot := os.Getenv("APPDATA")
	addSyncAccount(t, first, "first@example.test", "private-first-token")
	if _, err := SyncAccounts(context.Background(), first, accountTestPassword, scope); err != nil {
		t.Fatal(err)
	}
	second, scope := accountSyncDevice(t, ts.URL, "device-two")
	secondRoot := os.Getenv("APPDATA")
	addSyncAccount(t, second, "second@example.test", "private-second-token")
	result, err := SyncAccounts(context.Background(), second, accountTestPassword, scope)
	if err != nil || result.Added != 1 || len(second.List()) != 2 {
		t.Fatalf("second device: added=%d error=%v", result.Added, err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, firstRoot)
	}
	result, err = SyncAccounts(context.Background(), first, accountTestPassword, scope)
	if err != nil || result.Added != 1 || len(first.List()) != 2 {
		t.Fatalf("first device: added=%d error=%v", result.Added, err)
	}
	cloud.mu.Lock()
	writes := cloud.writes
	for _, raw := range cloud.objects {
		for _, secret := range []string{"private-first-token", "private-second-token", "private-refresh", "@example.test", accountTestPassword} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Error("server received plaintext account data")
			}
		}
	}
	cloud.mu.Unlock()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, secondRoot)
	}
	if _, err := SyncAccounts(context.Background(), second, "incorrect-password", scope); !errors.Is(err, accounts.ErrBackupPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	cloud.mu.Lock()
	if cloud.writes != writes {
		t.Error("wrong password overwrote a remote backup")
	}
	cloud.mu.Unlock()
	for _, file := range []string{path("account-sync.json"), syncdiag.Path()} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"private-first-token", "private-second-token", "@example.test", accountTestPassword} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Error("local configuration or diagnostics leaked a secret")
			}
		}
	}
}

func TestAccountSyncRequiresConsentScopeAndServerCapability(t *testing.T) {
	cloud := &accountServerFixture{objects: map[string][]byte{}}
	ts := httptest.NewServer(http.HandlerFunc(cloud.serve))
	defer ts.Close()
	store, scope := accountSyncDevice(t, ts.URL, "device-one")
	addSyncAccount(t, store, "fixture@example.test", "private-token")
	if _, err := SyncAccounts(context.Background(), store, accountTestPassword, scope); err == nil || !strings.Contains(err.Error(), "update the sync server") {
		t.Fatalf("old server was accepted: %v", err)
	}
	if err := SaveAccountSyncConfig(AccountSyncConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncAccounts(context.Background(), store, accountTestPassword, scope); err == nil {
		t.Fatal("sync ran without consent")
	}
	if err := SaveAccountSyncConfig(AccountSyncConfig{true, "other-scope"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncAccounts(context.Background(), store, accountTestPassword, scope); err == nil {
		t.Fatal("sync crossed server/tenant scope")
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if cloud.writes != 0 {
		t.Fatal("unsupported or unauthorized sync uploaded credentials")
	}
}
