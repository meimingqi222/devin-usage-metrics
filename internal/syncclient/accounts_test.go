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
	mu              sync.Mutex
	objects         map[string][]byte
	writes, deletes int
	capable         bool
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
		} else if r.Method == "DELETE" {
			delete(s.objects, name)
			s.deletes++
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

func TestAccountSyncWorksWithUsageSyncDisabled(t *testing.T) {
	cloud := &accountServerFixture{objects: map[string][]byte{}, capable: true}
	server := httptest.NewServer(http.HandlerFunc(cloud.serve))
	defer server.Close()
	store, scope := accountSyncDevice(t, server.URL, "independent-sync-device")
	if err := SaveConfig(Config{Enabled: false, APIURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	if current, err := AccountSyncScope(); err != nil || current != scope {
		t.Fatal("disabling usage sync invalidated account sign-in")
	}
	if _, err := SyncAccounts(context.Background(), store, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	if ReadConfig().Enabled {
		t.Fatal("account sync enabled usage sync")
	}
	if err := SaveAccountSyncConfig(AccountSyncConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncAccounts(context.Background(), store, accountTestPassword, scope, ""); !errors.Is(err, ErrSyncReenabled) {
		t.Fatal("account sync bypassed its own consent")
	}
	if err := saveSession(session{"token", "fixture-user", time.Now().Add(-time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, err := AccountSyncScope(); !errors.Is(err, ErrNotSignedIn) {
		t.Fatal("expired sign-in was accepted")
	}
}

func TestEncryptedAccountSyncTwoDevicesAndWrongPassword(t *testing.T) {
	cloud := &accountServerFixture{objects: map[string][]byte{}, capable: true}
	ts := httptest.NewServer(http.HandlerFunc(cloud.serve))
	defer ts.Close()
	first, scope := accountSyncDevice(t, ts.URL, "device-one")
	firstRoot := os.Getenv("APPDATA")
	addSyncAccount(t, first, "first@example.test", "private-first-token")
	if _, err := SyncAccounts(context.Background(), first, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	second, scope := accountSyncDevice(t, ts.URL, "device-two")
	secondRoot := os.Getenv("APPDATA")
	addSyncAccount(t, second, "second@example.test", "private-second-token")
	result, err := SyncAccounts(context.Background(), second, accountTestPassword, scope, "")
	if err != nil || result.Added != 1 || len(second.List()) != 2 {
		t.Fatalf("second device: added=%d error=%v", result.Added, err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, firstRoot)
	}
	result, err = SyncAccounts(context.Background(), first, accountTestPassword, scope, "")
	if err != nil || result.Added != 1 || len(first.List()) != 2 {
		t.Fatalf("first device: added=%d error=%v", result.Added, err)
	}
	var firstID string
	for _, p := range first.List() {
		if p.Email == "first@example.test" {
			firstID = p.ID
		}
	}
	if err := first.Rename("claude", firstID, "Laptop account"); err != nil {
		t.Fatal(err)
	}
	addSyncAccount(t, first, "first@example.test", "private-updated-token")
	if _, err := SyncAccounts(context.Background(), first, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, secondRoot)
	}
	result, err = SyncAccounts(context.Background(), second, accountTestPassword, scope, "")
	if err != nil || result.Updated != 1 {
		t.Fatalf("updated account not synchronized: %+v %v", result, err)
	}
	for _, p := range second.List() {
		if p.Email == "first@example.test" {
			if p.Name != "Laptop account" {
				t.Fatal("renamed account not synchronized")
			}
			doc, e := second.ProfileCredential(p.Provider, p.ID)
			if e != nil || doc["claudeAiOauth"].(map[string]any)["accessToken"] != "private-updated-token" {
				t.Fatal("updated credential not synchronized")
			}
		}
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, firstRoot)
	}
	if err := first.Remove("claude", firstID); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncAccounts(context.Background(), first, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, secondRoot)
	}
	result, err = SyncAccounts(context.Background(), second, accountTestPassword, scope, "")
	if err != nil || result.Removed != 1 || len(second.List()) != 1 {
		t.Fatalf("deleted account not synchronized: %+v %v", result, err)
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
	if _, err := SyncAccounts(context.Background(), second, "incorrect-password", scope, ""); !errors.Is(err, accounts.ErrBackupPassword) {
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
	if _, err := SyncAccounts(context.Background(), store, accountTestPassword, scope, ""); err == nil || !strings.Contains(err.Error(), "update the sync server") {
		t.Fatalf("old server was accepted: %v", err)
	}
	if err := SaveAccountSyncConfig(AccountSyncConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncAccounts(context.Background(), store, accountTestPassword, scope, ""); err == nil {
		t.Fatal("sync ran without consent")
	}
	if err := SaveAccountSyncConfig(AccountSyncConfig{Enabled: true, Scope: "other-scope"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncAccounts(context.Background(), store, accountTestPassword, scope, ""); err == nil {
		t.Fatal("sync crossed server/tenant scope")
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if cloud.writes != 0 {
		t.Fatal("unsupported or unauthorized sync uploaded credentials")
	}
}

func TestSyncAccountsPasswordRotation(t *testing.T) {
	cloud := &accountServerFixture{objects: map[string][]byte{}, capable: true}
	ts := httptest.NewServer(http.HandlerFunc(cloud.serve))
	defer ts.Close()
	const newPassword = "rotated-passphrase"
	first, scope := accountSyncDevice(t, ts.URL, "device-one")
	firstRoot := os.Getenv("APPDATA")
	addSyncAccount(t, first, "first@example.test", "private-first-token")
	if _, err := SyncAccounts(context.Background(), first, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	second, scope := accountSyncDevice(t, ts.URL, "device-two")
	secondRoot := os.Getenv("APPDATA")
	addSyncAccount(t, second, "second@example.test", "private-second-token")
	if _, err := SyncAccounts(context.Background(), second, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	// The first device rotates: knowing the old password, it reads every
	// snapshot and publishes sealed with the new one.
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, firstRoot)
	}
	result, err := SyncAccounts(context.Background(), first, newPassword, scope, accountTestPassword)
	if err != nil || result.Unreadable != 0 {
		t.Fatalf("rotation: unreadable=%d error=%v", result.Unreadable, err)
	}
	// A device still on the old password keeps syncing its own data but sees
	// the rotated snapshot as unreadable.
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, secondRoot)
	}
	result, err = SyncAccounts(context.Background(), second, accountTestPassword, scope, "")
	if err != nil || result.Unreadable != 1 || len(second.List()) != 2 {
		t.Fatalf("stale device: unreadable=%d error=%v", result.Unreadable, err)
	}
	// Entering the new password finishes the rotation; the old one still opens
	// this device's own previous snapshot.
	result, err = SyncAccounts(context.Background(), second, newPassword, scope, accountTestPassword)
	if err != nil || result.Unreadable != 0 || len(second.List()) != 2 {
		t.Fatalf("rotation catch-up: unreadable=%d error=%v", result.Unreadable, err)
	}
	// Everything is sealed with the new password now.
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, firstRoot)
	}
	result, err = SyncAccounts(context.Background(), first, newPassword, scope, "")
	if err != nil || result.Unreadable != 0 {
		t.Fatalf("post-rotation: unreadable=%d error=%v", result.Unreadable, err)
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	for _, raw := range cloud.objects {
		if bytes.Contains(raw, []byte(accountTestPassword)) || bytes.Contains(raw, []byte(newPassword)) {
			t.Error("server received a password")
		}
	}
}

func TestDeleteOwnAccountSnapshotAndUnreadable(t *testing.T) {
	cloud := &accountServerFixture{objects: map[string][]byte{}, capable: true}
	ts := httptest.NewServer(http.HandlerFunc(cloud.serve))
	defer ts.Close()
	first, scope := accountSyncDevice(t, ts.URL, "device-one")
	addSyncAccount(t, first, "first@example.test", "private-first-token")
	if _, err := SyncAccounts(context.Background(), first, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	second, scope := accountSyncDevice(t, ts.URL, "device-two")
	addSyncAccount(t, second, "second@example.test", "private-second-token")
	if _, err := SyncAccounts(context.Background(), second, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	stale, err := accounts.SealBackup(accounts.Backup{Version: 1}, "stale-password-xx")
	if err != nil {
		t.Fatal(err)
	}
	staleName := "accounts-v1-" + strings.Repeat("b", 64) + ".json"
	cloud.mu.Lock()
	cloud.objects[staleName] = stale
	cloud.mu.Unlock()
	if dropped, err := DropUnreadableAccountSnapshots(context.Background(), accountTestPassword, ""); err != nil || dropped != 1 {
		t.Fatalf("drop unreadable: dropped=%d err=%v", dropped, err)
	}
	cloud.mu.Lock()
	if _, ok := cloud.objects[staleName]; ok {
		t.Fatal("unreadable snapshot remains")
	}
	kept := len(cloud.objects)
	cloud.mu.Unlock()
	if kept != 2 {
		t.Fatalf("readable snapshots were deleted: %d", kept)
	}
	if dropped, err := DropUnreadableAccountSnapshots(context.Background(), "incorrect-password", ""); !errors.Is(err, accounts.ErrBackupPassword) || dropped != 0 {
		t.Fatalf("wrong password deleted snapshots: dropped=%d err=%v", dropped, err)
	}
	if err := DeleteAccountSnapshot(context.Background(), "v3-head-deadbeef.json"); err == nil {
		t.Fatal("usage object name was accepted")
	}
	own, err := accountSnapshotName()
	if err != nil {
		t.Fatal(err)
	}

	if err := DeleteOwnAccountSnapshotIfDisabled(context.Background()); err != nil {
		t.Fatal(err)
	}
	cloud.mu.Lock()
	if _, ok := cloud.objects[own]; !ok {
		t.Fatal("enabled sync still deleted this device snapshot")
	}
	cloud.mu.Unlock()
	if err := SaveAccountSyncConfig(AccountSyncConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := DeleteOwnAccountSnapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if _, ok := cloud.objects[own]; ok {
		t.Fatal("this device's snapshot remains after DeleteOwn")
	}
	if cloud.deletes < 2 {
		t.Fatalf("expected deletes, got %d", cloud.deletes)
	}
}

func TestDropForeignAccountSnapshots(t *testing.T) {
	cloud := &accountServerFixture{objects: map[string][]byte{}, capable: true}
	ts := httptest.NewServer(http.HandlerFunc(cloud.serve))
	defer ts.Close()
	first, scope := accountSyncDevice(t, ts.URL, "device-one")
	addSyncAccount(t, first, "first@example.test", "private-first-token")
	if _, err := SyncAccounts(context.Background(), first, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	second, scope := accountSyncDevice(t, ts.URL, "device-two")
	addSyncAccount(t, second, "second@example.test", "private-second-token")
	if _, err := SyncAccounts(context.Background(), second, accountTestPassword, scope, ""); err != nil {
		t.Fatal(err)
	}
	dropped, err := DropForeignAccountSnapshots(context.Background())
	if err != nil || dropped != 1 {
		t.Fatalf("drop foreign: dropped=%d err=%v", dropped, err)
	}
	own, err := accountSnapshotName()
	if err != nil {
		t.Fatal(err)
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if len(cloud.objects) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(cloud.objects))
	}
	if _, ok := cloud.objects[own]; !ok {
		t.Fatal("this device's snapshot was deleted")
	}
}
