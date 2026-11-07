package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func addClaude(t *testing.T, s *Store, email, org, access, refresh string) Profile {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(claudeDoc(email, org, access, refresh)), &doc); err != nil {
		t.Fatal(err)
	}
	p, err := s.Add("claude", doc, "file")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSyncSkipsRecordWithMissingCopy pins that one record whose saved copy
// is gone no longer fails every sync and export: it is skipped, kept, and
// reported by name so the user can act on it.
func TestSyncSkipsRecordWithMissingCopy(t *testing.T) {
	s, p := syncStore(t)
	gone := addClaude(t, s, "gone@example.com", "org-9", "a9", "r9")
	if err := os.Remove(s.CredentialPath("claude", gone.ID)); err != nil {
		t.Fatal(err)
	}
	b, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot failed on one missing copy: %v", err)
	}
	if len(b.Entries) != 1 {
		t.Fatalf("snapshot entries = %d, want only the readable account", len(b.Entries))
	}
	r, err := s.ApplySync(b)
	if err != nil {
		t.Fatalf("sync failed on one missing copy: %v", err)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].ProfileID != gone.ID || r.Skipped[0].Reason != SkipMissingCopy {
		t.Fatalf("missing copy not reported: %+v", r.Skipped)
	}
	ids := map[string]bool{}
	for _, q := range s.List() {
		ids[q.ID] = true
	}
	if !ids[p.ID] || !ids[gone.ID] {
		t.Fatalf("sync dropped a record: %v", ids)
	}
	if u := s.Unsyncable(); len(u) != 1 || u[0].ProfileID != gone.ID {
		t.Fatalf("unsyncable list: %+v", u)
	}
}

// TestLongRenameStaysSyncable pins the name limit at the rename: a name
// past the sync format's limit used to fail every later sync and export.
func TestLongRenameStaysSyncable(t *testing.T) {
	s, p := syncStore(t)
	if err := s.Rename(p.Provider, p.ID, strings.Repeat("名", 600)); err != nil {
		t.Fatal(err)
	}
	b, err := s.Snapshot()
	if err != nil {
		t.Fatalf("snapshot failed after a long rename: %v", err)
	}
	if n := utf8.RuneCountInString(b.Entries[0].Name); n != MaxAccountNameRunes {
		t.Fatalf("name runes = %d, want %d", n, MaxAccountNameRunes)
	}
}

// TestOldTombstonesArePruned pins the deletion-record budget: records older
// than the retention window are dropped and the newest are kept under the
// cap, so a long-lived tenant never trips the 4096-record validation that
// rejected every snapshot.
func TestOldTombstonesArePruned(t *testing.T) {
	now := time.Now()
	deleted := map[string]int64{
		"claude/00000000000000aa": now.Add(-(TombstoneRetention + time.Hour)).UnixNano(),
		"claude/00000000000000bb": now.Add(-time.Hour).UnixNano(),
	}
	for i := range 5000 {
		deleted[fmt.Sprintf("codex/%016x", i)] = now.Add(-time.Duration(i) * time.Minute).UnixNano()
	}
	merged := MergeSync(Backup{Version: 1, Deleted: deleted})
	if _, ok := merged.Deleted["claude/00000000000000aa"]; ok {
		t.Fatal("a deletion past retention survived")
	}
	if _, ok := merged.Deleted["claude/00000000000000bb"]; !ok {
		t.Fatal("a recent deletion was dropped")
	}
	if len(merged.Deleted) > MaxTombstones {
		t.Fatalf("deletions = %d, over the cap", len(merged.Deleted))
	}
	if err := validateBackup(merged); err != nil {
		t.Fatalf("pruned snapshot invalid: %v", err)
	}
}

// TestSyncRestoresMissingCopyFromAnotherDevice pins the repair path: when
// another device still holds the account, the missing copy is written back
// under the same record instead of a second record being filed beside it.
func TestSyncRestoresMissingCopyFromAnotherDevice(t *testing.T) {
	s, _ := syncStore(t)
	gone := addClaude(t, s, "gone@example.com", "org-9", "a9", "r9")
	remote, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.CredentialPath("claude", gone.ID)); err != nil {
		t.Fatal(err)
	}
	r, err := s.ApplySync(remote)
	if err != nil || len(r.Skipped) != 0 {
		t.Fatalf("restore: %+v %v", r, err)
	}
	if _, err := os.Stat(s.CredentialPath("claude", gone.ID)); err != nil {
		t.Fatalf("missing copy not restored: %v", err)
	}
	count := 0
	for _, q := range s.List() {
		if q.ID == gone.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("records for the restored account = %d", count)
	}
}
