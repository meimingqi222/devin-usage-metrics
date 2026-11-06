package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
)

// antigravityDoc is a keychain-shaped credential with a stable identity.
func antigravityDoc(access, refresh, email string) string {
	b, _ := json.Marshal(map[string]any{
		"token":    map[string]any{"access_token": access, "refresh_token": refresh, "expiry": "2026-11-03T12:00:00Z"},
		"id_token": loginJWT(map[string]any{"email": email, "sub": email}),
	})
	return string(b)
}

// TestReconcileDoesNotFileUnknownLoginUnderSwitchedProfile guards the
// overwrite behind a failed switch: a live document without identity and
// without token overlap must not be captured into the named account the
// user just switched to.
func TestReconcileDoesNotFileUnknownLoginUnderSwitchedProfile(t *testing.T) {
	h := fixture(t)
	write(t, h, ".gemini/oauth_creds.json", antigravityDoc("a1", "ra1", "a@example.com"))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(quota.Discover(false)); err != nil {
		t.Fatal(err)
	}
	target, err := s.Add("antigravity", map[string]any{
		"token":    map[string]any{"access_token": "b1", "refresh_token": "rb1"},
		"id_token": loginJWT(map[string]any{"email": "b@example.com", "sub": "b@example.com"}),
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Switch("antigravity", target.ID, live(t, "antigravity")); err != nil {
		t.Fatal(err)
	}
	// A dying client wrote something unattributable back into the live
	// store: no identity claims, no token shared with any saved copy.
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"stale","refresh_token":"stale-r"}`)
	if _, err := s.Reconcile(quota.Discover(false)); err != nil {
		t.Fatal(err)
	}
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("unknown login grew profiles: %+v", got)
	}
	for _, p := range got {
		doc, err := s.readCredential(p)
		if err != nil {
			t.Fatal(err)
		}
		_, refresh := tokenPair("antigravity", doc)
		if refresh == "stale-r" {
			t.Fatalf("unknown login was filed under %s (%s): %#v", p.DisplayName(), p.ID, doc)
		}
	}
	for _, p := range got {
		if p.Email == "b@example.com" && !p.Active {
			t.Fatalf("switched account lost the active flag: %+v", got)
		}
	}
}

// TestHideLegacyDuplicatesOnlyHidesTokenDuplicates keeps distinct saved
// accounts visible once the active profile has an identity; only a copy
// sharing the active login's tokens is a duplicate.
func TestHideLegacyDuplicatesOnlyHidesTokenDuplicates(t *testing.T) {
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	active := Profile{ID: "named", Provider: "antigravity", Label: "a@example.com", Email: "a@example.com", Kind: "keychain", CreatedAt: 1}
	distinct := Profile{ID: "distinct", Provider: "antigravity", Label: "Antigravity (distin)", Kind: "keychain", CreatedAt: 2}
	duplicate := Profile{ID: "duplicate", Provider: "antigravity", Label: "Antigravity (duplic)", Kind: "keychain", CreatedAt: 3}
	s.data.Profiles = append(s.data.Profiles, active, distinct, duplicate)
	s.data.Version = 1
	s.active["antigravity"] = active.ID
	if err := s.writeCredential(active, map[string]any{"token": map[string]any{"access_token": "ta", "refresh_token": "tr"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeCredential(distinct, map[string]any{"token": map[string]any{"access_token": "tb", "refresh_token": "tb-r"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeCredential(duplicate, map[string]any{"token": map[string]any{"access_token": "ta", "refresh_token": "other"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	got := s.List()
	var seenDistinct, seenDuplicate bool
	for _, p := range got {
		switch p.ID {
		case "distinct":
			seenDistinct = !p.Hidden
		case "duplicate":
			seenDuplicate = !p.Hidden
		}
	}
	if !seenDistinct {
		t.Fatalf("a distinct unnamed account was hidden: %+v", got)
	}
	if seenDuplicate {
		t.Fatalf("a token-sharing duplicate stayed visible: %+v", got)
	}
}

// TestSwitchAppRestartCapturesPreviousLogin pins the restart order: the
// previous login is captured under its own profile before the target is
// written, even when it was never reconciled before the switch.
func TestSwitchAppRestartCapturesPreviousLogin(t *testing.T) {
	h := fixture(t)
	write(t, h, ".gemini/oauth_creds.json", antigravityDoc("a1", "ra1", "a@example.com"))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	target, err := s.Add("antigravity", map[string]any{
		"token":    map[string]any{"access_token": "b1", "refresh_token": "rb1"},
		"id_token": loginJWT(map[string]any{"email": "b@example.com", "sub": "b@example.com"}),
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	oldFind, oldStop, oldStart, oldLS := findAntigravity, stopAntigravity, startAntigravity, languageServers
	defer func() {
		findAntigravity, stopAntigravity, startAntigravity, languageServers = oldFind, oldStop, oldStart, oldLS
	}()
	languageServers = func(context.Context, antigravityApp) []int { return nil }
	stopped, started := false, false
	findAntigravity = func(context.Context) (antigravityApp, error) {
		return antigravityApp{running: true, pids: []int{1}, path: "/Applications/Antigravity.app"}, nil
	}
	stopAntigravity = func(context.Context, antigravityApp) error { stopped = true; return nil }
	startAntigravity = func(context.Context, antigravityApp) error { started = true; return nil }
	if err := s.SwitchApp(context.Background(), "antigravity", target.ID, live(t, "antigravity")); err != nil {
		t.Fatal(err)
	}
	if !stopped || !started {
		t.Fatalf("app lifecycle: stopped=%v started=%v", stopped, started)
	}
	var previous, switched *Profile
	for _, p := range s.List() {
		switch {
		case p.Email == "a@example.com":
			previous = &p
		case p.Email == "b@example.com":
			switched = &p
		}
	}
	if previous == nil || switched == nil {
		t.Fatalf("previous login was not captured on restart: %+v", s.List())
	}
	if !switched.Active {
		t.Fatalf("target is not active after the restart switch: %+v", s.List())
	}
	doc, err := s.readCredential(*previous)
	if err != nil {
		t.Fatal(err)
	}
	access, _ := tokenPair("antigravity", doc)
	if access != "a1" {
		t.Fatalf("previous login captured the wrong tokens: %#v", doc)
	}
}

// TestVerifySwitched accepts a refreshed access token for the same account
// and rejects a different login.
func TestVerifySwitched(t *testing.T) {
	h := fixture(t)
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"old","refresh_token":"old-r"}`)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	p, err := s.Add("antigravity", map[string]any{
		"token":    map[string]any{"access_token": "b1", "refresh_token": "rb1"},
		"id_token": loginJWT(map[string]any{"email": "b@example.com"}),
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	liveAcc := live(t, "antigravity")
	// The client refreshed the access token right after the switch: the
	// refresh token still identifies the account.
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"b2","refresh_token":"rb1"}`)
	if err := s.verifySwitched("antigravity", p.ID, liveAcc); err != nil {
		t.Fatalf("refreshed access token rejected: %v", err)
	}
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"old","refresh_token":"old-r"}`)
	if err := s.verifySwitched("antigravity", p.ID, liveAcc); err == nil {
		t.Fatal("a different login passed verification")
	}
}

// TestWriteAntigravityStateMergesTopicAndClearsStaleKeys covers the state
// database mirror: the oauthToken topic keeps unrelated sentinel rows, and
// stale keys from the previous account are cleared.
func TestWriteAntigravityStateMergesTopicAndClearsStaleKeys(t *testing.T) {
	h := fixture(t)
	path := filepath.Join(h, "Library", "Application Support", "Antigravity", "User", "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	oldEntry := func(access string) []byte {
		info := pbString(1, access)
		info = append(info, pbString(2, "Bearer")...)
		info = append(info, pbString(3, "old-refresh")...)
		row := pbString(1, base64.StdEncoding.EncodeToString(info))
		entry := append(pbString(1, "oauthTokenInfoSentinelKey"), pbLen(2, row)...)
		return pbLen(1, entry)
	}
	precious := func() []byte {
		row := pbString(1, base64.StdEncoding.EncodeToString(pbString(3, "keep-me")))
		entry := append(pbString(1, "otherSentinelKey"), pbLen(2, row)...)
		return pbLen(1, entry)
	}
	topic := append(oldEntry("old-access"), precious()...)
	seed := func(key, value string) {
		if _, err := db.Execute(ctx, "INSERT OR REPLACE INTO ItemTable(key,value) VALUES(?,?)", key, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Execute(ctx, "CREATE TABLE IF NOT EXISTS ItemTable(key TEXT PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	seed("antigravityUnifiedStateSync.oauthToken", base64.StdEncoding.EncodeToString(topic))
	seed("jetskiStateSync.agentManagerInitState", "stale")
	seed("antigravityUnifiedStateSync.enterprisePreferences", "stale-project")
	db.Close()

	doc := map[string]any{
		"token":    map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "expiry": "2026-11-03T12:00:00Z"},
		"id_token": loginJWT(map[string]any{"email": "b@example.com"}),
	}
	if err := writeAntigravityState(ctx, "b@example.com", doc); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(ctx, path, sqlite.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(ctx, "SELECT key, value FROM ItemTable ORDER BY key")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, row := range rows.Values {
		if v, ok := row[1].(string); ok {
			values[row[0].(string)] = v
		}
	}
	oauth, err := base64.StdEncoding.DecodeString(values["antigravityUnifiedStateSync.oauthToken"])
	if err != nil {
		t.Fatal(err)
	}
	// The row payloads sit base64-encoded inside their entries; decode each
	// sentinel's payload back out.
	payload := func(key string) []byte {
		t.Helper()
		for _, f := range pbFields(oauth) {
			if f.wire != 2 || antigravityTopicEntryKey(f.data) != key {
				continue
			}
			for _, rf := range pbFields(f.data) {
				if rf.num == 2 && rf.wire == 2 {
					for _, rowf := range pbFields(rf.data) {
						if rowf.num == 1 && rowf.wire == 2 {
							b, err := base64.StdEncoding.DecodeString(string(rowf.data))
							if err != nil {
								t.Fatalf("row payload is not base64: %v", err)
							}
							return b
						}
					}
				}
			}
		}
		return nil
	}
	if got := string(payload("otherSentinelKey")); got != "\x1a\x07keep-me" {
		t.Fatalf("unrelated sentinel row was dropped or changed: %q", got)
	}
	info := payload("oauthTokenInfoSentinelKey")
	if info == nil {
		t.Fatal("the new oauthTokenInfo entry is missing")
	}
	fields := map[int]string{}
	for _, f := range pbFields(info) {
		if f.wire == 2 {
			fields[f.num] = string(f.data)
		}
	}
	if fields[1] != "new-access" || fields[3] != "new-refresh" {
		t.Fatalf("the new token entry is wrong: %v", fields)
	}
	if strings.Count(string(oauth), "oauthTokenInfoSentinelKey") != 1 {
		t.Fatalf("the previous token entry survived: %x", oauth)
	}
	if _, ok := values["jetskiStateSync.agentManagerInitState"]; ok {
		t.Fatalf("stale agent init state survived: %v", values)
	}
	if _, ok := values["antigravityUnifiedStateSync.enterprisePreferences"]; ok {
		t.Fatalf("stale enterprise preference survived: %v", values)
	}
	if values["antigravityUnifiedStateSync.userStatus"] != antigravityUserStatus("b@example.com") {
		t.Fatalf("user status was not written: %q", values["antigravityUnifiedStateSync.userStatus"])
	}
	if values["antigravityOnboarding"] != "true" {
		t.Fatalf("onboarding flag was not written: %q", values["antigravityOnboarding"])
	}
}

// TestWriteAntigravityFilesMatchesClientShapes checks the file fallbacks
// the client and CLI read.
func TestWriteAntigravityFilesMatchesClientShapes(t *testing.T) {
	h := fixture(t)
	doc := map[string]any{
		"token":       map[string]any{"access_token": "a1", "refresh_token": "r1", "expiry": "2026-11-03T12:00:00Z"},
		"auth_method": "consumer", "id_token": loginJWT(map[string]any{"email": "b@example.com"}),
	}
	if err := writeAntigravityFiles("b@example.com", doc); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) map[string]any {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(h, ".gemini", rel))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	standalone := read("jetski-standalone-oauth-token")
	tok := standalone["token"].(map[string]any)
	if tok["access_token"] != "a1" || tok["refresh_token"] != "r1" {
		t.Fatalf("standalone token: %#v", standalone)
	}
	if _, ok := tok["expiry"].(string); !ok {
		t.Fatalf("standalone expiry must be a timestamp: %#v", tok)
	}
	if standalone["id_token_email"] != "b@example.com" || standalone["auth_method"] != "consumer" {
		t.Fatalf("standalone identity: %#v", standalone)
	}
	creds := read("oauth_creds.json")
	if creds["access_token"] != "a1" || creds["refresh_token"] != "r1" || creds["scope"] == "" {
		t.Fatalf("oauth_creds: %#v", creds)
	}
	if ms, ok := creds["expiry_date"].(float64); !ok || int64(ms) != time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("oauth_creds expiry must be milliseconds: %#v", creds["expiry_date"])
	}
	accounts := read("google_accounts.json")
	if accounts["active"] != "b@example.com" {
		t.Fatalf("google_accounts: %#v", accounts)
	}
}
