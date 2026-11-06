package quota

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/meimingqi222/devin-usage-metrics/internal/sqlite"
)

func TestClaudeFallbackRefreshPersistence(t *testing.T) {
	fixtureHome(t)
	fixture(t, ".claude/.credentials.json", `{"other":42,"claudeAiOauth":{"accessToken":"old","refreshToken":"r","expiresAt":1,"scopes":["keep"]}}`)
	localServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			fmt.Fprint(w, `{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)
		case "/api/oauth/usage":
			if r.Header.Get("Authorization") != "Bearer new" {
				t.Error("old token used")
			}
			fmt.Fprint(w, `{"five_hour":{"utilization":1}}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	_, e := fetch(context.Background(), Account{Provider: "claude", source: "keychain:nonexistent-fixture"})
	if e != nil {
		t.Fatal(e)
	}
	d, e := readDoc(homePath(".claude/.credentials.json"))
	if e != nil || str(at(d, "claudeAiOauth", "refreshToken")) != "rotated" || num(d["other"]) != 42 || len(at(d, "claudeAiOauth", "scopes").([]any)) != 1 {
		t.Fatal("fallback was not preserved", e)
	}
}
func TestClaude401RereadsWithoutRefreshToken(t *testing.T) {
	fixtureHome(t)
	fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"old"}}`)
	calls := 0
	localServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/oauth/usage" && r.URL.RawQuery == "" { // not the hourly grants read
			calls++
			if calls == 1 {
				fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"rotated"}}`)
				w.WriteHeader(401)
				return
			}
			if r.Header.Get("Authorization") != "Bearer rotated" {
				t.Error("not reread")
			}
			fmt.Fprint(w, `{"five_hour":{"utilization":1}}`)
		} else {
			fmt.Fprint(w, `{}`)
		}
	})
	if _, e := fetch(context.Background(), Account{Provider: "claude", source: homePath(".claude/.credentials.json")}); e != nil {
		t.Fatal(e)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}
func TestAntigravityStateReadOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.vscdb")
	ctx := context.Background()
	db, e := sqlite.Open(ctx, p, sqlite.OpenOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Execute(ctx, "CREATE TABLE ItemTable(key TEXT PRIMARY KEY,value TEXT)"); e != nil {
		t.Fatal(e)
	}
	info := append(pbBytes(1, []byte("access")), pbBytes(3, []byte("refresh"))...)
	wrapped := pbBytes(2, pbBytes(1, []byte(base64.StdEncoding.EncodeToString(info))))
	entry := append(pbBytes(1, []byte("oauthTokenInfoSentinelKey")), wrapped...)
	state := base64.StdEncoding.EncodeToString(pbBytes(1, entry))
	for k, v := range map[string]string{"antigravityUnifiedStateSync.oauthToken": state, "antigravityAuthStatus": `{"email":"fixture@example.test"}`} {
		if _, e = db.Execute(ctx, "INSERT INTO ItemTable VALUES(?,?)", k, v); e != nil {
			t.Fatal(e)
		}
	}
	db.Close()
	before, _ := os.ReadFile(p)
	a := Account{Provider: "antigravity", source: p}
	d, e := loadAccount(a)
	if e != nil || str(d["access_token"]) != "access" || str(d["refresh_token"]) != "refresh" {
		t.Fatal(d, e)
	}
	if readAntigravityStateEmail(p) != "fixture@example.test" {
		t.Fatal("email")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("database modified")
	}
}

func TestAntigravityStateWriteRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.vscdb")
	writeAntigravityStateFixture(t, p, "access", "refresh")
	a := Account{Provider: "antigravity", source: p}
	if !a.Writable() || a.ReadOnlyReason() != "" {
		t.Fatalf("state database should be switchable: %q", a.ReadOnlyReason())
	}
	if err := a.WriteCredential(object{
		"access_token": "a2", "token_type": "Bearer", "refresh_token": "r2",
		"expiry_date": 1.8e12, "id_token": "id2",
	}); err != nil {
		t.Fatal(err)
	}
	d, e := loadAccount(a)
	if e != nil || str(d["access_token"]) != "a2" || str(d["refresh_token"]) != "r2" {
		t.Fatalf("switched tokens did not read back: %v %v", d, e)
	}
	if readAntigravityStateEmail(p) != "fixture@example.test" {
		t.Fatal("account name row was disturbed")
	}
	// The id token is stored in the row even though the reader leaves it out.
	raw, err := readStateValue(p, "antigravityUnifiedStateSync.oauthToken")
	if err != nil {
		t.Fatal(err)
	}
	state, _ := base64.StdEncoding.DecodeString(raw)
	for _, entry := range stateFields(state, 1) {
		if string(stateFirst(entry, 1)) != "oauthTokenInfoSentinelKey" {
			continue
		}
		info, err := base64.StdEncoding.DecodeString(string(stateFirst(stateFirst(entry, 2), 1)))
		if err != nil {
			t.Fatal(err)
		}
		if string(stateFirst(info, 5)) != "id2" || string(stateFirst(info, 1)) != "a2" {
			t.Fatalf("encoded entry lost fields: %q %q", stateFirst(info, 1), stateFirst(info, 5))
		}
		return
	}
	t.Fatal("token entry missing")
}

// writeAntigravityStateFixture creates the state database rows Antigravity's
// client writes: an oauth token entry and the account name beside it.
func writeAntigravityStateFixture(t *testing.T, p, access, refresh string) {
	t.Helper()
	ctx := context.Background()
	db, e := sqlite.Open(ctx, p, sqlite.OpenOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Execute(ctx, "CREATE TABLE ItemTable(key TEXT PRIMARY KEY,value TEXT)"); e != nil {
		t.Fatal(e)
	}
	info := append(pbBytes(1, []byte(access)), pbBytes(3, []byte(refresh))...)
	wrapped := pbBytes(2, pbBytes(1, []byte(base64.StdEncoding.EncodeToString(info))))
	entry := append(pbBytes(1, []byte("oauthTokenInfoSentinelKey")), wrapped...)
	state := base64.StdEncoding.EncodeToString(pbBytes(1, entry))
	for k, v := range map[string]string{"antigravityUnifiedStateSync.oauthToken": state, "antigravityAuthStatus": `{"email":"fixture@example.test"}`} {
		if _, e = db.Execute(ctx, "INSERT INTO ItemTable VALUES(?,?)", k, v); e != nil {
			t.Fatal(e)
		}
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestClaude401RereadsAfterProactiveRefresh(t *testing.T) {
	fixtureHome(t)
	fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"old","refreshToken":"r","expiresAt":1}}`)
	refreshes, uses := 0, 0
	localServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			refreshes++
			fmt.Fprint(w, `{"access_token":"new","refresh_token":"r2","expires_in":3600}`)
		case "/api/oauth/usage":
			if r.URL.RawQuery != "" { // the hourly grants read
				fmt.Fprint(w, `{}`)
				return
			}
			uses++
			if uses == 1 {
				fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"cli-rotated"}}`)
				w.WriteHeader(401)
				return
			}
			if r.Header.Get("Authorization") != "Bearer cli-rotated" {
				t.Error("CLI rotation not reread")
			}
			fmt.Fprint(w, `{"five_hour":{"utilization":1}}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	if _, e := fetch(context.Background(), Account{Provider: "claude", source: homePath(".claude/.credentials.json")}); e != nil {
		t.Fatal(e)
	}
	if uses != 2 || refreshes != 1 {
		t.Fatal("retry budget", uses, refreshes)
	}
}
