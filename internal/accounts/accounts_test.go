package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
)

func fixture(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	for k, v := range map[string]string{
		"HOME": h, "USERPROFILE": h,
		"APPDATA":         filepath.Join(h, "appdata"),
		"XDG_CONFIG_HOME": filepath.Join(h, "config"),
		"USER":            "tester",
	} {
		t.Setenv(k, v)
	}
	return h
}

func testConfig(t *testing.T) string {
	t.Helper()
	d, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func write(t *testing.T, home, rel, body string) {
	t.Helper()
	p := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func claudeDoc(email, org, access, refresh string) string {
	b, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"emailAddress": email, "accessToken": access,
			"refreshToken": refresh, "subscriptionType": "max",
		},
		"organizationUuid": org,
	})
	return string(b)
}

func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "x." + base64.RawURLEncoding.EncodeToString(b) + ".y"
}

func codexDoc(email, account, access, refresh string) string {
	b, _ := json.Marshal(map[string]any{"tokens": map[string]any{
		"access_token": access, "refresh_token": refresh, "account_id": account,
		"id_token": jwt(map[string]any{"email": email}),
	}})
	return string(b)
}

func live(t *testing.T, provider string) quota.Account {
	t.Helper()
	for _, a := range quota.Discover(false) {
		if a.Provider == provider {
			return a
		}
	}
	t.Fatalf("no live %s account", provider)
	return quota.Account{}
}

func TestCaptureAndSwitchClaude(t *testing.T) {
	h := fixture(t)
	write(t, h, ".claude/.credentials.json", claudeDoc("work@example.com", "org-1", "a1", "r1"))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	first, err := s.Reconcile(quota.Discover(false))
	if err != nil || len(first) != 1 {
		t.Fatalf("first capture: %+v %v", first, err)
	}
	if !first[0].Active || first[0].Email != "work@example.com" {
		t.Fatalf("unexpected profile: %+v", first[0])
	}

	write(t, h, ".claude/.credentials.json", claudeDoc("home@example.com", "org-2", "a2", "r2"))
	second, err := s.Reconcile(quota.Discover(false))
	if err != nil || len(second) != 2 {
		t.Fatalf("second capture: %+v %v", second, err)
	}
	var work, home Profile
	for _, p := range second {
		switch p.Email {
		case "work@example.com":
			work = p
		case "home@example.com":
			home = p
		}
	}
	if !home.Active || work.Active || work.ID == "" || home.ID == "" {
		t.Fatalf("active account wrong: %+v", second)
	}

	if err := s.Switch("claude", work.ID, live(t, "claude")); err != nil {
		t.Fatal(err)
	}
	got, err := live(t, "claude").CredentialDoc()
	if err != nil {
		t.Fatal(err)
	}
	if got["organizationUuid"] != "org-1" || got["claudeAiOauth"].(map[string]any)["refreshToken"] != "r1" {
		t.Fatalf("switch wrote the wrong document: %#v", got)
	}
	if _, err := os.Stat(filepath.Join(testConfig(t), "devin-usage-metrics", "accounts-switch.json")); !os.IsNotExist(err) {
		t.Fatal("switch journal was not cleared")
	}
	after, _ := s.Reconcile(quota.Discover(false))
	for _, p := range after {
		if p.ID == work.ID && !p.Active {
			t.Fatalf("switched account is not active: %+v", after)
		}
	}

	// The manifest must not contain a token; the credential copies may.
	b, err := os.ReadFile(filepath.Join(testConfig(t), "devin-usage-metrics", "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"a1", "r1", "a2", "r2", "accessToken", "refreshToken"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("manifest leaks %q: %s", secret, b)
		}
	}
	if runtime.GOOS != "windows" {
		p := filepath.Join(testConfig(t), "devin-usage-metrics", "accounts", "claude", work.ID+".json")
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("credential copy must be private: %v %v", st, err)
		}
	}
	if err := s.Remove("claude", work.ID); err == nil {
		t.Fatal("the active account must not be removable")
	}
	if err := s.Remove("claude", home.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledNeverCapturesOrSwitches(t *testing.T) {
	h := fixture(t)
	write(t, h, ".claude/.credentials.json", claudeDoc("a@example.com", "org", "a", "r"))
	s := Open()
	if _, err := s.Reconcile(quota.Discover(false)); err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 0 || s.Enabled() {
		t.Fatalf("disabled store captured accounts: %+v", got)
	}
	if err := s.Switch("claude", "missing", live(t, "claude")); err != ErrDisabled {
		t.Fatalf("switch while disabled: %v", err)
	}
}

func TestCodexNestedAuthAndWorkspaceIdentity(t *testing.T) {
	h := fixture(t)
	write(t, h, ".codex/auth.json", codexDoc("me@example.com", "acct-1", "a1", "r1"))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	profiles, err := s.Reconcile(quota.Discover(false))
	if err != nil || len(profiles) != 1 {
		t.Fatalf("codex capture: %+v %v", profiles, err)
	}
	if profiles[0].Email != "me@example.com" || profiles[0].StableID == "" {
		t.Fatalf("codex identity: %+v", profiles[0])
	}
	write(t, h, ".codex/auth.json", codexDoc("me@example.com", "acct-2", "a2", "r2"))
	profiles, _ = s.Reconcile(quota.Discover(false))
	if len(profiles) != 2 {
		t.Fatalf("same user in another workspace must be two accounts: %+v", profiles)
	}
}

func TestAntigravityFileDocument(t *testing.T) {
	h := fixture(t)
	id := jwt(map[string]any{"email": "g@example.com", "sub": "google-1"})
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"a1","refresh_token":"r1","id_token":"`+id+`"}`)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	profiles, err := s.Reconcile(quota.Discover(false))
	if err != nil || len(profiles) != 1 || profiles[0].Email != "g@example.com" {
		t.Fatalf("antigravity capture: %+v %v", profiles, err)
	}
}

func TestInterruptedSwitchIsRecoveredFromLive(t *testing.T) {
	h := fixture(t)
	write(t, h, ".claude/.credentials.json", claudeDoc("work@example.com", "org-1", "a1", "r1"))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	profiles, _ := s.Reconcile(quota.Discover(false))
	target := profiles[0]
	// Simulate a crash after the target was written but before the journal
	// was cleared: live is already the target, and the journal names it.
	write(t, h, ".claude/.credentials.json", claudeDoc("work@example.com", "org-1", "a1", "r1"))
	doc, _ := live(t, "claude").CredentialDoc()
	j := journalFile{Version: 1, Provider: "claude", Target: target.ID, BeforeHash: "other", TargetHash: docHash(doc), At: time.Now().Unix()}
	if err := s.writeJournal(j); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.Reconcile(quota.Discover(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || !recovered[0].Active {
		t.Fatalf("recovery did not mark live active: %+v", recovered)
	}
	if _, err := os.Stat(filepath.Join(testConfig(t), "devin-usage-metrics", "accounts-switch.json")); !os.IsNotExist(err) {
		t.Fatal("recovery left the journal")
	}
}

func TestAntigravityCredentialShapeConversion(t *testing.T) {
	keychain := map[string]any{
		"token": map[string]any{
			"access_token": "a", "refresh_token": "r",
			"expiry": "2026-11-03T12:00:00Z",
		},
		"auth_method": "consumer", "id_token": "id",
	}
	file := adaptCredential("antigravity", keychain, "keychain", "file")
	if file["access_token"] != "a" || file["refresh_token"] != "r" {
		t.Fatalf("keychain to file: %#v", file)
	}
	if _, ok := file["expiry_date"].(int64); !ok {
		t.Fatalf("file expiry must be milliseconds: %#v", file)
	}
	back := adaptCredential("antigravity", file, "file", "keychain")
	token := back["token"].(map[string]any)
	if token["access_token"] != "a" || !strings.HasPrefix(token["expiry"].(string), "2026-11-03T12:00:00") {
		t.Fatalf("file to keychain: %#v", back)
	}
}

func TestRepairNamesLegacyAntigravityProfiles(t *testing.T) {
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	docs := []map[string]any{
		{"token": map[string]any{"access_token": "a1", "refresh_token": "r1"}, "id_token": loginJWT(map[string]any{"email": "one@example.com", "sub": "s1"}), "auth_method": "consumer"},
		{"token": map[string]any{"access_token": "a2", "refresh_token": "r2"}, "id_token": loginJWT(map[string]any{"email": "two@example.com", "sub": "s2"}), "auth_method": "consumer"},
	}
	for i, doc := range docs {
		id := fmt.Sprintf("legacy-%d", i)
		p := Profile{ID: id, Provider: "antigravity", Label: "Antigravity", Kind: "keychain", CreatedAt: 1}
		s.data.Profiles = append(s.data.Profiles, p)
		if err := s.writeCredential(p, doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	got := s.List()
	if len(got) != 2 || got[0].Label == got[1].Label || got[0].Email == "" || got[1].Email == "" {
		t.Fatalf("legacy profiles were not named: %+v", got)
	}
}

func TestLegacyAntigravityProfileMergesWithFullLiveDocument(t *testing.T) {
	h := fixture(t)
	id := loginJWT(map[string]any{"email": "g@example.com", "sub": "sub-1"})
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"a1","refresh_token":"r1","id_token":"`+id+`"}`)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	legacy := Profile{ID: "legacy-antigravity", Provider: "antigravity", Label: "Antigravity", Kind: "file", CreatedAt: 1}
	s.data.Profiles = append(s.data.Profiles, legacy)
	if err := s.writeCredential(legacy, map[string]any{"access_token": "a1", "refresh_token": "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	got, err := s.Reconcile(quota.Discover(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != legacy.ID || got[0].Label != "g@example.com" || !got[0].Active {
		t.Fatalf("legacy profile was not merged: %+v", got)
	}
}

func TestAPIKeyModesAreNotAccounts(t *testing.T) {
	h := fixture(t)
	write(t, h, ".codex/auth.json", `{"OPENAI_API_KEY":"sk-test"}`)
	write(t, h, ".claude/.credentials.json", `{"claudeAiOauth":{}}`)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	profiles, err := s.Reconcile(quota.Discover(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 0 {
		t.Fatalf("API-key modes became accounts: %+v", profiles)
	}
	for _, a := range quota.Discover(false) {
		if a.Provider == "codex" && !strings.Contains(a.LoginNote(), "API-key mode") {
			t.Fatalf("codex API mode note: %q", a.LoginNote())
		}
		if a.Provider == "claude" && !strings.Contains(a.LoginNote(), "not signed in") {
			t.Fatalf("claude signed-out note: %q", a.LoginNote())
		}
	}
}

func TestSwitchFromCodexAPIKeyToOAuthAccount(t *testing.T) {
	h := fixture(t)
	write(t, h, ".codex/auth.json", `{"OPENAI_API_KEY":"sk-test"}`)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	oauth := map[string]any{"tokens": map[string]any{
		"access_token": "a", "refresh_token": "r", "account_id": "acct",
		"id_token": loginJWT(map[string]any{"email": "me@example.com"}),
	}}
	p, err := s.Add("codex", oauth, "file")
	if err != nil {
		t.Fatal(err)
	}
	var live quota.Account
	for _, a := range quota.Discover(false) {
		if a.Provider == "codex" {
			live = a
		}
	}
	if !live.Writable() {
		t.Fatalf("API-key auth.json must remain writable: %q", live.ReadOnlyReason())
	}
	if err := s.Switch("codex", p.ID, live); err != nil {
		t.Fatal(err)
	}
	got, err := live.CredentialDoc()
	if err != nil || str(obj(got["tokens"])["refresh_token"]) != "r" {
		t.Fatalf("switch from API mode: %#v %v", got, err)
	}
}

func TestAntigravityEmailIdentityDoesNotDuplicate(t *testing.T) {
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	full := map[string]any{
		"token":    map[string]any{"access_token": "a", "refresh_token": "r"},
		"id_token": loginJWT(map[string]any{"email": "same@example.com", "sub": "sub-1"}),
	}
	emailOnly := map[string]any{
		"token": map[string]any{"access_token": "a2", "refresh_token": "r2"},
		"email": "same@example.com",
	}
	for _, doc := range []map[string]any{full, emailOnly} {
		if _, err := s.upsertLocked("antigravity", doc, "keychain", docHash(doc)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 1 || got[0].Email != "same@example.com" {
		t.Fatalf("same email created duplicates: %+v", got)
	}
}

func TestSwitchAppRestartsAntigravity(t *testing.T) {
	h := fixture(t)
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"live","refresh_token":"live-r","email":"live@example.com"}`)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	target, err := s.Add("antigravity", map[string]any{"access_token": "new", "refresh_token": "new-r", "email": "new@example.com"}, "file")
	if err != nil {
		t.Fatal(err)
	}
	var live quota.Account
	for _, a := range quota.Discover(false) {
		if a.Provider == "antigravity" {
			live = a
		}
	}
	oldFind, oldStop, oldStart, oldLS, oldKill := findAntigravity, stopAntigravity, startAntigravity, languageServers, killLanguageServer
	defer func() {
		findAntigravity, stopAntigravity, startAntigravity, languageServers, killLanguageServer = oldFind, oldStop, oldStart, oldLS, oldKill
	}()
	languageServers = func(context.Context, antigravityApp) []int { return nil }
	stopped, started := false, false
	findAntigravity = func(context.Context) (antigravityApp, error) {
		return antigravityApp{running: true, pids: []int{1}, path: "/Applications/Antigravity.app"}, nil
	}
	stopAntigravity = func(context.Context, antigravityApp) error { stopped = true; return nil }
	startAntigravity = func(context.Context, antigravityApp) error { started = true; return nil }
	if err := s.SwitchApp(context.Background(), "antigravity", target.ID, live); err != nil {
		t.Fatal(err)
	}
	if !stopped || !started {
		t.Fatalf("app lifecycle: stopped=%v started=%v", stopped, started)
	}
	doc, err := live.CredentialDoc()
	if err != nil || doc["refresh_token"] != "new-r" {
		t.Fatalf("live credential: %#v %v", doc, err)
	}
}

// writeAntigravityStateFixture creates the desktop client's state database at
// the path this platform resolves, holding a signed-in token and its email.
func writeAntigravityStateFixture(t *testing.T, access, refresh, email string) string {
	t.Helper()
	p := antigravityStateDBs()[0]
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, p, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Execute(ctx, "CREATE TABLE ItemTable(key TEXT PRIMARY KEY,value TEXT)"); err != nil {
		t.Fatal(err)
	}
	info := append(pbString(1, access), pbString(3, refresh)...)
	wrapped := pbLen(2, pbString(1, base64.StdEncoding.EncodeToString(info)))
	entry := append(pbString(1, "oauthTokenInfoSentinelKey"), wrapped...)
	status, _ := json.Marshal(map[string]string{"email": email})
	for k, v := range map[string]string{
		"antigravityUnifiedStateSync.oauthToken": base64.StdEncoding.EncodeToString(pbLen(1, entry)),
		"antigravityAuthStatus":                  string(status),
	} {
		if _, err = db.Execute(ctx, "INSERT INTO ItemTable VALUES(?,?)", k, v); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// The login Antigravity keeps in its state database is read locally; enabling
// switching must file it as a recognized, switchable account without a
// manual capture step.
// A record whose credential file went missing must be rebuilt from a
// sibling record of the same account, or its switch button can only fail.
func TestHealMissingCredentialFromSibling(t *testing.T) {
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	first, err := s.Add("antigravity", map[string]any{
		"access_token": "a1", "refresh_token": "r1",
		"id_token": jwt(map[string]any{"email": "one@example.test"}),
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("antigravity", map[string]any{
		"access_token": "a2", "refresh_token": "r2",
	}, "file"); err != nil {
		t.Fatal(err)
	}
	// A duplicate record of the first account whose copy was lost.
	broken := Profile{ID: "broken000000001", Provider: "antigravity", StableID: first.StableID, Email: first.Email, Kind: "vscdb", CreatedAt: 1}
	s.mu.Lock()
	s.data.Profiles = append(s.data.Profiles, broken)
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	for _, p := range s.List() {
		if p.ID != broken.ID {
			continue
		}
		if !p.Writable {
			t.Fatalf("broken record was not healed: %+v", p)
		}
		doc, err := s.ProfileCredential("antigravity", broken.ID)
		if err != nil || doc["refresh_token"] != "r1" {
			t.Fatalf("healed copy is wrong: %v %v", doc, err)
		}
		return
	}
	t.Fatal("broken record missing from the list")
}

// Without a sibling copy the record stays visible but must not offer a
// switch that cannot succeed.
func TestMissingCredentialIsNotSwitchable(t *testing.T) {
	fixture(t)
	s := Open()
	s.mu.Lock()
	s.data.Profiles = append(s.data.Profiles, Profile{ID: "gone00000000001", Provider: "antigravity", StableID: "x@example.test", Email: "x@example.test", Kind: "file", CreatedAt: 1})
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	for _, p := range s.List() {
		if p.ID != "gone00000000001" {
			continue
		}
		if p.Writable || p.Reason == "" {
			t.Fatalf("missing-copy record still switchable: %+v", p)
		}
		return
	}
	t.Fatal("record missing from the list")
}

// googleUserInfoFake serves the userinfo oracle so a state-database login
// without identity claims can be named offline.
func googleUserInfoFake(t *testing.T, email string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-1" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"email":"`+email+`"}`)
	}))
	t.Cleanup(srv.Close)
	old := googleUserInfoURL
	googleUserInfoURL = srv.URL
	t.Cleanup(func() { googleUserInfoURL = old })
}

func TestStateLoginIsCapturedAndSwitchable(t *testing.T) {
	fixture(t)
	googleUserInfoFake(t, "one@example.test")
	state := writeAntigravityStateFixture(t, "access-1", "refresh-1", "one@example.test")
	live := quota.ProfileAccount("antigravity-local", "antigravity", "one@example.test", state)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	profiles, err := s.Reconcile([]quota.Account{live})
	if err != nil || len(profiles) != 1 {
		t.Fatalf("capture: %+v %v", profiles, err)
	}
	if !profiles[0].Active || profiles[0].Email != "one@example.test" || profiles[0].Kind != "vscdb" {
		t.Fatalf("state login was not recognized: %+v", profiles[0])
	}
	if !profiles[0].Writable {
		t.Fatalf("state login should be switchable: %s", profiles[0].Reason)
	}
}

// A saved copy holding the live account's tokens under another account's
// name must refuse to switch instead of silently restoring the copied
// account.
func TestSwitchRejectsContaminatedCopy(t *testing.T) {
	fixture(t)
	googleUserInfoFake(t, "one@example.test")
	state := writeAntigravityStateFixture(t, "access-1", "refresh-1", "one@example.test")
	live := quota.ProfileAccount("antigravity-local", "antigravity", "one@example.test", state)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile([]quota.Account{live}); err != nil {
		t.Fatal(err)
	}
	bad := Profile{ID: "bad000000000001", Provider: "antigravity", StableID: "two@example.test", Email: "two@example.test", Kind: "file", CreatedAt: 1}
	doc := map[string]any{
		"access_token": "access-1", "refresh_token": "refresh-1",
		"id_token": jwt(map[string]any{"email": "two@example.test"}),
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.data.Profiles = append(s.data.Profiles, bad)
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	if err := os.WriteFile(s.CredentialPath("antigravity", bad.ID), raw, 0600); err != nil {
		t.Fatal(err)
	}
	// Reconcile files the live login under the authoritative record and
	// marks the copy that carries the same tokens under another name.
	if _, err := s.Reconcile([]quota.Account{live}); err != nil {
		t.Fatal(err)
	}
	for _, p := range s.List() {
		if p.ID == bad.ID && p.Writable {
			t.Fatalf("contaminated copy not flagged: %+v", p)
		}
	}
	if err := s.Switch("antigravity", bad.ID, live); err == nil || !strings.Contains(err.Error(), "holds the current account's sign-in") {
		t.Fatalf("contaminated copy switched: %v", err)
	}
	// An honest copy of a different account still switches.
	good, err := s.Add("antigravity", map[string]any{
		"access_token": "access-3", "refresh_token": "refresh-3",
		"id_token": jwt(map[string]any{"email": "three@example.test"}),
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Switch("antigravity", good.ID, live); err != nil {
		t.Fatalf("honest switch failed: %v", err)
	}
	if d, err := live.CredentialDoc(); err != nil || d["refresh_token"] != "refresh-3" {
		t.Fatalf("switch did not deliver: %#v %v", d, err)
	}
}

func TestSwitchAppWritesStateDatabase(t *testing.T) {
	fixture(t)
	googleUserInfoFake(t, "one@example.test")
	state := writeAntigravityStateFixture(t, "access-1", "refresh-1", "one@example.test")
	live := quota.ProfileAccount("antigravity-local", "antigravity", "one@example.test", state)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile([]quota.Account{live}); err != nil {
		t.Fatal(err)
	}
	target, err := s.Add("antigravity", map[string]any{
		"access_token": "access-2", "refresh_token": "refresh-2",
		"id_token": jwt(map[string]any{"email": "two@example.test"}),
	}, "keychain")
	if err != nil {
		t.Fatal(err)
	}
	oldFind, oldStop, oldStart, oldLS := findAntigravity, stopAntigravity, startAntigravity, languageServers
	defer func() {
		findAntigravity, stopAntigravity, startAntigravity, languageServers = oldFind, oldStop, oldStart, oldLS
	}()
	findAntigravity = func(context.Context) (antigravityApp, error) {
		return antigravityApp{running: true, pids: []int{1}}, nil
	}
	languageServers = func(context.Context, antigravityApp) []int { return nil }
	stopped, started := false, false
	stopAntigravity = func(context.Context, antigravityApp) error { stopped = true; return nil }
	startAntigravity = func(context.Context, antigravityApp) error { started = true; return nil }
	if err := s.SwitchApp(context.Background(), "antigravity", target.ID, live); err != nil {
		t.Fatal(err)
	}
	if !stopped || !started {
		t.Fatalf("app lifecycle: stopped=%v started=%v", stopped, started)
	}
	doc, err := live.CredentialDoc()
	if err != nil || doc["refresh_token"] != "refresh-2" {
		t.Fatalf("state database kept the old sign-in: %#v %v", doc, err)
	}
	for _, p := range s.List() {
		if p.ID == target.ID && !p.Active {
			t.Fatal("target was not marked active")
		}
	}
}

func TestSwitchAppAbortsWhenQuitFails(t *testing.T) {
	h := fixture(t)
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"live","refresh_token":"live-r","email":"live@example.com"}`)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	target, err := s.Add("antigravity", map[string]any{"access_token": "new", "refresh_token": "new-r", "email": "new@example.com"}, "file")
	if err != nil {
		t.Fatal(err)
	}
	var live quota.Account
	for _, a := range quota.Discover(false) {
		if a.Provider == "antigravity" {
			live = a
		}
	}
	oldFind, oldStop, oldLS := findAntigravity, stopAntigravity, languageServers
	defer func() { findAntigravity, stopAntigravity, languageServers = oldFind, oldStop, oldLS }()
	languageServers = func(context.Context, antigravityApp) []int { return nil }
	findAntigravity = func(context.Context) (antigravityApp, error) {
		return antigravityApp{running: true, pids: []int{1}}, nil
	}
	stopAntigravity = func(context.Context, antigravityApp) error { return errors.New("still running") }
	if err := s.SwitchApp(context.Background(), "antigravity", target.ID, live); err == nil {
		t.Fatal("switch proceeded after a failed quit")
	}
	doc, _ := live.CredentialDoc()
	if doc["refresh_token"] != "live-r" {
		t.Fatalf("live changed after failed quit: %#v", doc)
	}
}

func TestRepeatedAntigravitySwitchDoesNotGrowProfiles(t *testing.T) {
	h := fixture(t)
	idA := loginJWT(map[string]any{"email": "a@example.com", "sub": "s1"})
	idB := loginJWT(map[string]any{"email": "b@example.com", "sub": "s2"})
	doc := func(idToken, access, refresh string) string {
		return `{"access_token":"` + access + `","refresh_token":"` + refresh + `","id_token":"` + idToken + `"}`
	}
	write(t, h, ".gemini/oauth_creds.json", doc(idA, "a1", "ra1"))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(quota.Discover(false)); err != nil {
		t.Fatal(err)
	}
	b, err := s.Add("antigravity", map[string]any{"access_token": "b1", "refresh_token": "rb1", "id_token": idB}, "file")
	if err != nil {
		t.Fatal(err)
	}
	a := s.List()
	var aid string
	for _, p := range a {
		if p.Email == "a@example.com" {
			aid = p.ID
		}
	}
	for i := 0; i < 4; i++ {
		target := aid
		if i%2 == 0 {
			target = b.ID
		}
		var live quota.Account
		for _, x := range quota.Discover(false) {
			if x.Provider == "antigravity" {
				live = x
			}
		}
		if err := s.Switch("antigravity", target, live); err != nil {
			t.Fatal(err)
		}
		// Antigravity rotates the access token while the app runs.
		id := idA
		if target == b.ID {
			id = idB
		}
		write(t, h, ".gemini/oauth_creds.json", doc(id, "rotated"+string(rune('a'+i)), "rotated-r"+string(rune('a'+i))))
		if _, err := s.Reconcile(quota.Discover(false)); err != nil {
			t.Fatal(err)
		}
		if got := s.List(); len(got) != 2 {
			t.Fatalf("switch %d grew profiles: %+v", i, got)
		}
	}
}

func TestSwitchAppHotSwitchesLanguageServer(t *testing.T) {
	h := fixture(t)
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"live","refresh_token":"live-r","email":"live@example.com"}`)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	target, err := s.Add("antigravity", map[string]any{"access_token": "new", "refresh_token": "new-r", "email": "new@example.com"}, "file")
	if err != nil {
		t.Fatal(err)
	}
	var live quota.Account
	for _, a := range quota.Discover(false) {
		if a.Provider == "antigravity" {
			live = a
		}
	}
	oldFind, oldStop, oldStart, oldLS, oldKill := findAntigravity, stopAntigravity, startAntigravity, languageServers, killLanguageServer
	defer func() {
		findAntigravity, stopAntigravity, startAntigravity, languageServers, killLanguageServer = oldFind, oldStop, oldStart, oldLS, oldKill
	}()
	findAntigravity = func(context.Context) (antigravityApp, error) {
		return antigravityApp{running: true, pids: []int{10}, path: "/Applications/Antigravity.app"}, nil
	}
	stopped, started, killed := false, false, false
	stopAntigravity = func(context.Context, antigravityApp) error { stopped = true; return nil }
	startAntigravity = func(context.Context, antigravityApp) error { started = true; return nil }
	languageServers = func(context.Context, antigravityApp) []int {
		if killed {
			return []int{11}
		}
		return []int{10}
	}
	killLanguageServer = func(context.Context, []int) { killed = true }
	if err := s.SwitchApp(context.Background(), "antigravity", target.ID, live); err != nil {
		t.Fatal(err)
	}
	if !killed || stopped || started {
		t.Fatalf("hot switch lifecycle: killed=%v stopped=%v started=%v", killed, stopped, started)
	}
	doc, _ := live.CredentialDoc()
	if doc["refresh_token"] != "new-r" {
		t.Fatalf("live credential: %#v", doc)
	}
}
