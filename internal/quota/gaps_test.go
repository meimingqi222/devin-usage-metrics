package quota

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/plugins/sqlite"
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
		if r.URL.Path == "/api/oauth/usage" {
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
	if os.Getenv("MYGO_SQLITE_LIBRARY") == "" {
		t.Skip("set MYGO_SQLITE_LIBRARY to a local plugin ABI library; this fixture never downloads native dependencies")
	}
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
