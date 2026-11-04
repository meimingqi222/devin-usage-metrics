package accounts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const backupTestPassword = "fixture123" // Exactly 10 characters, the supported minimum.

func backupFixture() Backup {
	return Backup{Version: 1, Entries: []BackupEntry{
		{Provider: "claude", Name: "Work", ModifiedAt: 123, Credential: map[string]any{"claudeAiOauth": map[string]any{"accessToken": "private-access", "refreshToken": "private-refresh", "emailAddress": "fixture@example.test"}}},
		{Provider: "antigravity", ModifiedAt: 234, Credential: map[string]any{"token": map[string]any{"access_token": "google-access", "refresh_token": "google-refresh", "expiry": "2026-11-04T22:00:00Z"}, "id_token": loginJWT(map[string]any{"email": "google@example.test", "sub": "sub-1"})}},
	}}
}

func TestAccountBackupEncryptionAndTampering(t *testing.T) {
	b := backupFixture()
	raw, err := SealBackup(b, backupTestPassword)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("private-access"), []byte("private-refresh"), []byte("fixture@example.test"), []byte("Work"), []byte(backupTestPassword)} {
		if bytes.Contains(raw, secret) {
			t.Fatal("plaintext leaked into encrypted backup")
		}
	}
	opened, err := OpenBackup(raw, backupTestPassword)
	if err != nil || len(opened.Entries) != 2 || opened.Entries[0].Name != "Work" {
		t.Fatalf("round trip: count=%d error=%v", len(opened.Entries), err)
	}
	if _, err := OpenBackup(raw, "incorrect-password"); !errors.Is(err, ErrBackupPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	var e backupEnvelope
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	e.Ciphertext[0] ^= 1
	damaged, _ := json.Marshal(e)
	if _, err := OpenBackup(damaged, backupTestPassword); !errors.Is(err, ErrBackupPassword) {
		t.Fatalf("modified ciphertext accepted: %v", err)
	}
	e.Iterations = 1 << 30
	damaged, _ = json.Marshal(e)
	if _, err := OpenBackup(damaged, backupTestPassword); err == nil {
		t.Fatal("unbounded KDF work accepted")
	}
	if _, err := SealBackup(b, "fixture12"); err == nil {
		t.Fatal("9-character password accepted")
	}
}

func TestAccountBackupImportPreservesLocalAndLiveCredentials(t *testing.T) {
	h := fixture(t)
	write(t, h, ".claude/.credentials.json", claudeDoc("live@example.test", "org", "live-access", "live-refresh"))
	livePath := filepath.Join(h, ".claude/.credentials.json")
	liveBefore, _ := os.ReadFile(livePath)
	s := Open()
	if _, _, err := s.ImportBackup(backupFixture()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled import: %v", err)
	}
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	added, kept, err := s.ImportBackup(backupFixture())
	if err != nil || added != 2 || kept != 0 {
		t.Fatalf("import: %d %d %v", added, kept, err)
	}
	for _, p := range s.List() {
		if p.Active {
			t.Fatal("import marked a backup as the active agent sign-in")
		}
	}
	b := backupFixture()
	b.Entries[0].Credential["claudeAiOauth"].(map[string]any)["accessToken"] = "remote-stale-access"
	added, kept, err = s.ImportBackup(b)
	if err != nil || added != 0 || kept != 2 {
		t.Fatalf("dedup: %d %d %v", added, kept, err)
	}
	copy, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range copy.Entries {
		if e.Provider == "claude" && e.ModifiedAt != 123 {
			t.Fatal("import manufactured a fresh timestamp for an old credential")
		}
		if e.Provider == "claude" && str(obj(e.Credential["claudeAiOauth"])["accessToken"]) != "private-access" {
			t.Fatal("existing local credential overwritten")
		}
	}
	liveAfter, _ := os.ReadFile(livePath)
	if !bytes.Equal(liveBefore, liveAfter) {
		t.Fatal("import wrote the agent's live sign-in")
	}
	if err := s.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	// Reload to prove local removal markers survive a process restart.
	s = Open()
	if result, err := s.ApplySync(b); err != nil || result.Added != 0 {
		t.Fatalf("sync resurrected removed accounts: %d %v", result.Added, err)
	}
	if added, _, err := s.ImportBackup(b); err != nil || added != 2 {
		t.Fatalf("explicit import could not restore backup: %d %v", added, err)
	}
}

func TestAccountBackupImportRollsBackOnWriteFailure(t *testing.T) {
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	// Force the final manifest write to fail after credential files were staged.
	if err := os.Remove(s.manifestPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.manifestPath(), 0700); err != nil {
		t.Fatal(err)
	}
	added, _, err := s.ImportBackup(backupFixture())
	if err == nil || added != 0 || len(s.data.Profiles) != 0 {
		t.Fatalf("partial import escaped rollback: added=%d profiles=%d error=%v", added, len(s.data.Profiles), err)
	}
	for _, e := range backupFixture().Entries {
		id := backupIdentity(e)
		if _, err := os.Stat(filepath.Join(s.dir, "accounts", id+".json")); !os.IsNotExist(err) {
			t.Fatal("staged credential left after failed import")
		}
	}
}

func TestAccountBackupUnknownIdentityDoesNotReplaceActive(t *testing.T) {
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	first := Backup{Version: 1, Entries: []BackupEntry{{Provider: "codex", Credential: map[string]any{"tokens": map[string]any{"access_token": "one"}}}}}
	if _, _, err := s.ImportBackup(first); err != nil {
		t.Fatal(err)
	}
	p := s.data.Profiles[0]
	s.active["codex"] = p.ID
	second := Backup{Version: 1, Entries: []BackupEntry{{Provider: "codex", Credential: map[string]any{"tokens": map[string]any{"access_token": "two"}}}}}
	if added, _, err := s.ImportBackup(second); err != nil || added != 1 {
		t.Fatalf("anonymous import fell back to active profile: %d %v", added, err)
	}
	doc, err := s.ProfileCredential("codex", p.ID)
	if err != nil || str(obj(doc["tokens"])["access_token"]) != "one" {
		t.Fatal("anonymous import replaced the active credential")
	}
}

func TestAccountBackupFreshnessAndEncryptedFile(t *testing.T) {
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	doc := backupFixture().Entries[0].Credential
	p, err := s.Add("claude", doc, "file")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(s.CredentialPath(p.Provider, p.ID), old, old); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Snapshot()
	if _, err := s.Add("claude", doc, "file"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Snapshot()
	if before.Entries[0].ModifiedAt != after.Entries[0].ModifiedAt {
		t.Fatal("unchanged credential was falsely refreshed by reconciliation")
	}
	rotated, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"emailAddress": "fixture@example.test", "accessToken": "rotated-by-quota", "refreshToken": "rotated-refresh"}})
	if err := os.WriteFile(s.CredentialPath(p.Provider, p.ID), rotated, 0600); err != nil {
		t.Fatal(err)
	}
	refreshed, err := s.Snapshot()
	if err != nil || refreshed.Entries[0].ModifiedAt <= before.Entries[0].ModifiedAt {
		t.Fatal("backup missed a credential rotated outside Store.Add")
	}
	file := filepath.Join(t.TempDir(), "backup.aumaccounts")
	if err := WriteBackupFile(file, after, backupTestPassword); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadBackupFile(file, backupTestPassword); err != nil || len(got.Entries) != 1 {
		t.Fatalf("file backup: %d %v", len(got.Entries), err)
	}
	newer := after
	newer.Entries = append([]BackupEntry(nil), after.Entries...)
	newer.Entries[0].ModifiedAt++
	newer.Entries[0].Name = "New name"
	merged := MergeBackups(after, newer)
	if len(merged.Entries) != 1 || merged.Entries[0].Name != "New name" {
		t.Fatal("newest credential snapshot did not win")
	}
}
