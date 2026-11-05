package accounts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

func syncStore(t *testing.T) (*Store, Profile) {
	t.Helper()
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	p, err := s.Add("claude", backupFixture().Entries[0].Credential, "file")
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestAccountSyncUpdatesNamesCredentialsAndDeletions(t *testing.T) {
	s, p := syncStore(t)
	old, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	remote, _ := s.Snapshot()
	remote.Entries[0].Name = "Renamed on laptop"
	remote.Entries[0].NameModifiedAt = time.Now().Add(time.Second).UnixNano()
	remote.Entries[0].ModifiedAt = time.Now().Add(time.Second).UnixNano()
	obj(remote.Entries[0].Credential["claudeAiOauth"])["accessToken"] = "remote-new-token"
	r, err := s.ApplySync(remote)
	if err != nil || r.Updated != 1 {
		t.Fatalf("update: %+v %v", r, err)
	}
	doc, err := s.ProfileCredential(p.Provider, p.ID)
	if err != nil || str(obj(doc["claudeAiOauth"])["accessToken"]) != "remote-new-token" {
		t.Fatal("remote token update was not applied")
	}
	profiles := s.List()
	if len(profiles) != 1 || profiles[0].Name != "Renamed on laptop" || profiles[0].Active {
		t.Fatal("name update or actual active-account detection failed")
	}
	deleted := Backup{Version: 1, Deleted: map[string]int64{backupIdentity(remote.Entries[0]): time.Now().Add(2 * time.Second).UnixNano()}}
	r, err = s.ApplySync(deleted)
	if err != nil || r.Removed != 1 || len(s.List()) != 0 {
		t.Fatalf("delete: %+v %v", r, err)
	}
	if _, err := os.Stat(s.CredentialPath(p.Provider, p.ID)); !os.IsNotExist(err) {
		t.Fatal("deleted saved credential file remains")
	}
	s = Open()
	r, err = s.ApplySync(old)
	if err != nil || r.Added != 0 || len(s.List()) != 0 {
		t.Fatal("an offline stale device resurrected a deleted account")
	}
}

func TestAccountSyncProtectsTheLiveSignIn(t *testing.T) {
	s, p := syncStore(t)
	s.active[p.Provider] = p.ID
	live := filepath.Join(os.Getenv("HOME"), ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(live), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("live-agent-credential-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.CredentialPath(p.Provider, p.ID))
	remote, _ := s.Snapshot()
	remote.Entries[0].ModifiedAt = time.Now().Add(time.Second).UnixNano()
	obj(remote.Entries[0].Credential["claudeAiOauth"])["accessToken"] = "new-remote-token"
	r, err := s.ApplySync(remote)
	if err != nil || r.Protected != 1 {
		t.Fatalf("protection: %+v %v", r, err)
	}
	after, _ := os.ReadFile(s.CredentialPath(p.Provider, p.ID))
	if !bytes.Equal(before, after) {
		t.Fatal("current account backup was replaced during a live session")
	}
	remote.Deleted = map[string]int64{backupIdentity(remote.Entries[0]): time.Now().Add(2 * time.Second).UnixNano()}
	r, err = s.ApplySync(remote)
	if err != nil || r.Protected != 1 || len(s.List()) != 1 {
		t.Fatal("remote deletion removed the currently used account")
	}
	actual, _ := os.ReadFile(live)
	if string(actual) != "live-agent-credential-fixture" {
		t.Fatal("sync wrote the agent's live sign-in")
	}
	delete(s.active, p.Provider)
	r, err = s.ApplySync(remote)
	if err != nil || r.Removed != 1 {
		t.Fatal("deletion did not finish after switching away from the account")
	}
}

func TestAccountSyncRollsBackCredentialReplacement(t *testing.T) {
	s, p := syncStore(t)
	before, _ := os.ReadFile(s.CredentialPath(p.Provider, p.ID))
	remote, _ := s.Snapshot()
	remote.Entries[0].ModifiedAt = time.Now().Add(time.Second).UnixNano()
	obj(remote.Entries[0].Credential["claudeAiOauth"])["accessToken"] = "new-remote-token"
	if err := os.Remove(s.manifestPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.manifestPath(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplySync(remote); err == nil {
		t.Fatal("manifest failure was hidden")
	}
	after, _ := os.ReadFile(s.CredentialPath(p.Provider, p.ID))
	if !bytes.Equal(before, after) {
		t.Fatal("failed sync left a partially replaced credential")
	}
}

func TestAccountSyncMergesConcurrentRenameAndTokenRefresh(t *testing.T) {
	base := backupFixture()
	rename := backupFixture()
	rename.Entries[0].Name = "Laptop"
	rename.Entries[0].NameModifiedAt = 300
	refresh := backupFixture()
	refresh.Entries[0].ModifiedAt = 400
	obj(refresh.Entries[0].Credential["claudeAiOauth"])["accessToken"] = "refreshed"
	a := MergeSync(base, rename, refresh)
	b := MergeSync(refresh, rename, base)
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if !bytes.Equal(aj, bj) {
		t.Fatal("merge depends on device listing order")
	}
	for _, e := range a.Entries {
		if e.Provider == "claude" && (e.Name != "Laptop" || str(obj(e.Credential["claudeAiOauth"])["accessToken"]) != "refreshed") {
			t.Fatal("concurrent rename or refresh was lost")
		}
	}
}

func TestAccountSyncDeletionSurvivesQuotaDiscoveryAndExplicitRestore(t *testing.T) {
	s, p := syncStore(t)
	doc, err := s.ProfileCredential(p.Provider, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	write(t, os.Getenv("HOME"), ".claude/.credentials.json", string(raw))
	if err := s.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	deleted, _ := s.Snapshot()
	s = Open()
	profiles, err := s.Reconcile(quota.Discover(false))
	if err != nil || len(profiles) != 0 {
		t.Fatal("quota discovery recreated a deleted account")
	}
	if _, err := s.Add(p.Provider, doc, p.Kind); err != nil {
		t.Fatal(err)
	}
	r, err := s.ApplySync(deleted)
	if err != nil || r.Removed != 0 || len(s.List()) != 1 {
		t.Fatal("explicit capture could not restore a previously deleted account")
	}
}

func TestAccountSyncStableIdentityAndReadd(t *testing.T) {
	token1 := backupFixture()
	token2 := backupFixture()
	obj(token2.Entries[0].Credential["claudeAiOauth"])["accessToken"] = "rotated-token"
	token2.Entries[0].ModifiedAt = token1.Entries[0].ModifiedAt + 10
	merged := MergeSync(token1, token2)
	claude := 0
	for _, e := range merged.Entries {
		if e.Provider != "claude" {
			continue
		}
		claude++
		if str(obj(e.Credential["claudeAiOauth"])["accessToken"]) != "rotated-token" {
			t.Fatal("token refresh did not keep the newer credential")
		}
	}
	if claude != 1 {
		t.Fatalf("token refresh forked a second account: %d", claude)
	}
	opaque := Backup{Version: 1, Entries: []BackupEntry{
		{Provider: "codex", ModifiedAt: 1, Credential: map[string]any{"tokens": map[string]any{"access_token": "one"}}},
		{Provider: "codex", ModifiedAt: 2, Credential: map[string]any{"tokens": map[string]any{"access_token": "two"}}},
	}}
	if n := len(MergeBackups(opaque).Entries); n != 2 {
		t.Fatalf("file merge dropped opaque credentials: %d", n)
	}
	if n := len(MergeSync(opaque).Entries); n != 0 {
		t.Fatalf("sync merge published accounts without a stable identity: %d", n)
	}

	s, first := syncStore(t)
	second, err := s.Add("claude", map[string]any{"claudeAiOauth": map[string]any{"emailAddress": "other@example.test", "accessToken": "other-access", "refreshToken": "other-refresh"}}, "file")
	if err != nil {
		t.Fatal(err)
	}
	local, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var remoteEntries []BackupEntry
	for _, e := range local.Entries {
		if e.Provider == first.Provider && str(obj(e.Credential["claudeAiOauth"])["emailAddress"]) == first.Email {
			remoteEntries = append(remoteEntries, e)
		}
	}
	r, err := s.ApplySync(Backup{Version: 1, Entries: remoteEntries})
	if err != nil || r.Removed != 0 || len(s.List()) != 2 {
		t.Fatalf("absence deleted an account the user never removed: %+v %v", r, err)
	}

	if err := s.Remove("claude", second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("claude", map[string]any{"claudeAiOauth": map[string]any{"emailAddress": "other@example.test", "accessToken": "restored-access", "refreshToken": "other-refresh"}}, "file"); err != nil {
		t.Fatal(err)
	}
	stale := Backup{Version: 1, Deleted: map[string]int64{syncIdentity(BackupEntry{Provider: "claude", Credential: map[string]any{"claudeAiOauth": map[string]any{"emailAddress": "other@example.test", "accessToken": "other-access", "refreshToken": "other-refresh"}}}): 1}}
	r, err = s.ApplySync(stale)
	if err != nil || r.Removed != 0 {
		t.Fatalf("re-added account lost to an older tombstone: %+v %v", r, err)
	}
	found := false
	for _, p := range s.List() {
		if p.Email == "other@example.test" {
			found = true
		}
	}
	if !found {
		t.Fatal("re-added account was not kept")
	}
}

func TestAccountSyncMatchesStableIDAndIgnoresUntouchedMtime(t *testing.T) {
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	p, err := s.Add("codex", map[string]any{"tokens": map[string]any{"access_token": "one"}}, "file")
	if err != nil {
		t.Fatal(err)
	}
	s.data.Profiles[0].StableID = "codex@example.test"
	s.data.Profiles[0].Email = "codex@example.test"
	before, err := s.Snapshot()
	if err != nil || len(before.Entries) != 1 {
		t.Fatalf("file snapshot: %v", err)
	}
	if len(Syncable(before).Entries) != 0 {
		t.Fatal("sync snapshot published an account without a stable identity claim")
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(s.CredentialPath(p.Provider, p.ID), future, future); err != nil {
		t.Fatal(err)
	}
	after, err := s.Snapshot()
	if err != nil || after.Entries[0].ModifiedAt != before.Entries[0].ModifiedAt {
		t.Fatal("mtime-only touch advanced credential time")
	}
	remote := Backup{Version: 1, Entries: []BackupEntry{{
		Provider:   "codex",
		ModifiedAt: time.Now().Add(time.Second).UnixNano(),
		Credential: map[string]any{"tokens": map[string]any{"access_token": "two"}, "email": "codex@example.test"},
	}}}
	r, err := s.ApplySync(remote)
	if err != nil || r.Added != 0 || r.Updated != 1 || len(s.List()) != 1 {
		t.Fatalf("stable identity was not matched in place: %+v %v", r, err)
	}
	doc, err := s.ProfileCredential("codex", p.ID)
	if err != nil || str(obj(doc["tokens"])["access_token"]) != "two" {
		t.Fatal("matched account did not take the newer credential")
	}
}
