package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEncryptedAccountBackupRetentionAndCapability(t *testing.T) {
	root := t.TempDir()
	s := newTestCore(root, 0)
	dir := filepath.Join(root, "tester")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	valid := "accounts-v1-" + strings.Repeat("a", 64) + ".json"
	for _, name := range []string{valid, "accounts-v1-invalid.json", "orphan"} {
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, []byte(`{"ciphertext":"fixture"}`), 0600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-365 * 24 * time.Hour)
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := s.gcTenant("tester", time.Now())
	if err != nil || deleted != 2 {
		t.Fatalf("retention: deleted=%d error=%v", deleted, err)
	}
	if _, err := os.Stat(filepath.Join(dir, valid)); err != nil {
		t.Fatal("GC removed encrypted account backup")
	}
	handler := newTestServer(t)
	w := request(handler, "GET", "/v1/capabilities", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"account_backups":1`) {
		t.Fatal("missing capability")
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest("GET", "/v1/capabilities", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatal("capability bypassed sync authentication")
	}
}

func TestDeleteEncryptedAccountBackupOnly(t *testing.T) {
	handler := newTestServer(t)
	valid := "accounts-v1-" + strings.Repeat("a", 64) + ".json"
	if got := request(handler, http.MethodPut, "/v1/objects/"+valid, `{"ciphertext":"fixture"}`, nil); got.Code != http.StatusNoContent {
		t.Fatalf("put backup: %d", got.Code)
	}
	if got := request(handler, http.MethodDelete, "/v1/objects/"+valid, "", nil); got.Code != http.StatusNoContent {
		t.Fatalf("delete backup: %d", got.Code)
	}
	if got := request(handler, http.MethodGet, "/v1/objects/"+valid, "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("deleted backup still present: %d", got.Code)
	}
	if got := request(handler, http.MethodDelete, "/v1/objects/"+valid, "", nil); got.Code != http.StatusNoContent {
		t.Fatalf("idempotent delete: %d", got.Code)
	}
	if got := request(handler, http.MethodPut, "/v1/objects/orphan", "usage", nil); got.Code != http.StatusNoContent {
		t.Fatalf("put usage: %d", got.Code)
	}
	if got := request(handler, http.MethodDelete, "/v1/objects/orphan", "", nil); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("usage delete allowed: %d", got.Code)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodDelete, "/v1/objects/"+valid, nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatal("delete bypassed sync authentication")
	}
}
