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
	// The issuer does not answer the planted stale tokens, so the login
	// stays claims-less and unprovable.
	googleUserInfoFake(t)
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

// TestClaimsLessLiveAttributesBySavedTokens files a state-database login
// that carries no identity claims under the saved copy holding its tokens,
// not under whichever account was active before. Without the token match a
// claims-less login fell back to the previous active record, which both
// showed the wrong "in use" account and rendered the real profile as a
// duplicate tab beside the live one.
func TestClaimsLessLiveAttributesBySavedTokens(t *testing.T) {
	h := fixture(t)
	googleUserInfoFake(t)
	write(t, h, ".gemini/oauth_creds.json", antigravityDoc("a1", "ra1", "a@example.com"))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(quota.Discover(false)); err != nil {
		t.Fatal(err)
	}
	// An unnamed copy of a second login, as a pre-identity capture left it.
	unnamed, err := s.Add("antigravity", map[string]any{
		"token": map[string]any{"access_token": "b1", "refresh_token": "rb1"},
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	// The live store now holds the second login as bare tokens: no claims,
	// and the issuer does not answer this access token.
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"b1","refresh_token":"rb1"}`)
	profiles, err := s.Reconcile(quota.Discover(false))
	if err != nil {
		t.Fatal(err)
	}
	var sawUnnamed bool
	for _, p := range profiles {
		switch p.ID {
		case unnamed.ID:
			sawUnnamed = true
			if !p.Active {
				t.Fatalf("the token-sharing copy was not filed as active: %+v", profiles)
			}
		default:
			if p.Active {
				t.Fatalf("the previous account stayed active under another login: %+v", profiles)
			}
		}
	}
	if !sawUnnamed || len(profiles) != 2 {
		t.Fatalf("profiles drifted: %+v", profiles)
	}
}

// TestClaimsLessLiveNamedByAttestedCopy reproduces the crossed-copy
// corruption: the active profile's saved copy was overwritten with a
// second account's tokens and stamped with the first account's name. A
// claims-less live document sharing those tokens must be named by the
// claims its own copies carry, not by the poisoned active record - the
// stamp files the live login under the real account and leaves the
// crossed copy flagged instead of relabeling it once more.
func TestClaimsLessLiveNamedByAttestedCopy(t *testing.T) {
	h := fixture(t)
	googleUserInfoFake(t)
	write(t, h, ".gemini/oauth_creds.json", antigravityDoc("a1", "ra1", "a@example.com"))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	profiles, err := s.Reconcile(quota.Discover(false))
	if err != nil || len(profiles) != 1 {
		t.Fatalf("capture: %+v %v", profiles, err)
	}
	crossed := profiles[0]
	// B's real account, signed in once so its copy holds signed claims.
	b, err := s.Add("antigravity", map[string]any{
		"token":    map[string]any{"access_token": "b1", "refresh_token": "rb1"},
		"id_token": loginJWT(map[string]any{"email": "b@example.com", "sub": "sub-b"}),
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	// The capture bug: A's saved file now holds B's refresh token under an
	// "email" stamp naming A. Nothing signed calls this copy A's.
	if err := s.writeCredential(crossed, map[string]any{
		"access_token": "b1x", "refresh_token": "rb1", "email": "a@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	// The live store holds the same login as bare tokens; the issuer does
	// not answer this access token.
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"b1x","refresh_token":"rb1"}`)
	profiles, err = s.Reconcile(quota.Discover(false))
	if err != nil {
		t.Fatal(err)
	}
	var sawB, sawCrossed bool
	for _, p := range profiles {
		switch p.ID {
		case b.ID:
			sawB = true
			if !p.Active || p.Email != "b@example.com" {
				t.Fatalf("the tokens' own account was not filed as active: %+v", profiles)
			}
		case crossed.ID:
			sawCrossed = true
			if p.Active {
				t.Fatalf("the crossed copy stayed filed as the live login: %+v", profiles)
			}
			if p.Email != "a@example.com" {
				t.Fatalf("the crossed copy was relabeled again instead of flagged: %+v", profiles)
			}
			if !s.suspect["antigravity/"+crossed.ID] {
				t.Fatalf("the crossed copy was not retired from switching: %+v", profiles)
			}
		}
		doc, err := s.readCredential(p)
		if err != nil {
			t.Fatal(err)
		}
		if p.ID == crossed.ID {
			if _, refresh := tokenPair("antigravity", doc); refresh != "rb1" {
				t.Fatalf("the crossed copy's tokens were touched: %#v", doc)
			}
		}
	}
	if !sawB || !sawCrossed || len(profiles) != 2 {
		t.Fatalf("profiles drifted: %+v", profiles)
	}
}

// TestAntigravityEmailStableRecordsFold merges the pair sync creates when
// one device records a Google account as "email" and another as
// "email|sub": one account, two records, both labelled with the address.
func TestAntigravityEmailStableRecordsFold(t *testing.T) {
	fixture(t)
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	rich := Profile{ID: "rich000000000001", Provider: "antigravity", Label: "a@example.com", Email: "a@example.com", StableID: "a@example.com|sub-a", Kind: "file", CreatedAt: 1}
	bare := Profile{ID: "bare000000000001", Provider: "antigravity", Label: "a@example.com", Email: "a@example.com", StableID: "a@example.com", Kind: "file", CreatedAt: 2}
	s.data.Profiles = append(s.data.Profiles, rich, bare)
	s.data.Version = 1
	// The rich copy carries signed claims (email|sub); the bare one only a
	// stamped email, the shape a sync-imported record can take.
	if err := s.writeCredential(rich, map[string]any{
		"token":    map[string]any{"access_token": "a1", "refresh_token": "ra1"},
		"id_token": loginJWT(map[string]any{"email": "a@example.com", "sub": "sub-a"}),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeCredential(bare, map[string]any{
		"access_token": "a1", "refresh_token": "ra1", "email": "a@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	// writeCredential stamps ModifiedAt at write time; the rich record is
	// the newer copy of the pair, so it must win the fold.
	s.data.Profiles[0].ModifiedAt = 2
	s.data.Profiles[1].ModifiedAt = 1
	if err := s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	got := s.List()
	if len(got) != 1 {
		t.Fatalf("the two records of one account did not fold: %+v", got)
	}
	if got[0].ID != rich.ID {
		t.Fatalf("the newer copy should survive the fold: %+v", got)
	}
}

// TestEmailOnlyLiveAttributesByEmail files a login whose identity claims
// name only an email under the profile of that account, wherever the
// previous active flag pointed. Upsert already merges on email; matching
// must reach the same profile so the live login is filed under it instead
// of reading as a second copy.
func TestEmailOnlyLiveAttributesByEmail(t *testing.T) {
	h := fixture(t)
	write(t, h, ".gemini/oauth_creds.json", antigravityDoc("a1", "ra1", "a@example.com"))
	s := Open()
	if err := s.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile(quota.Discover(false)); err != nil {
		t.Fatal(err)
	}
	second, err := s.Add("antigravity", map[string]any{
		"token":    map[string]any{"access_token": "b1", "refresh_token": "rb1"},
		"id_token": loginJWT(map[string]any{"email": "b@example.com", "sub": "sub-b"}),
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	// Capture is off: matching alone must file the live login, so the
	// email-only identity cannot fall back to the previous active record.
	if err := s.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	write(t, h, ".gemini/oauth_creds.json", `{"access_token":"nb","refresh_token":"rnb","email":"b@example.com"}`)
	profiles, err := s.Reconcile(quota.Discover(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		active := p.Active
		if p.ID == second.ID && !active {
			t.Fatalf("the email's own profile was not filed as active: %+v", profiles)
		}
		if p.ID != second.ID && active {
			t.Fatalf("the previous account stayed active under another login: %+v", profiles)
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
	fixture(t)
	// writeAntigravityState mirrors onto the databases this platform
	// resolves, so the fixture must sit at the first of those.
	path := antigravityStateDBs()[0]
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
