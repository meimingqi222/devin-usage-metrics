package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCLILoginImportsIsolatedClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
mkdir -p "$CLAUDE_CONFIG_DIR"
printf '%s' '{"claudeAiOauth":{"emailAddress":"new@example.com","accessToken":"a","refreshToken":"r"}}' > "$CLAUDE_CONFIG_DIR/.credentials.json"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", bin)
	doc, kind, err := CLILogin(context.Background(), "claude")
	if err != nil {
		t.Fatal(err)
	}
	if kind != "file" || doc["claudeAiOauth"].(map[string]any)["emailAddress"] != "new@example.com" {
		t.Fatalf("imported %s %#v", kind, doc)
	}
}

func TestAntigravityOAuthFlow(t *testing.T) {
	oldToken := googleTokenURL
	defer func() { googleTokenURL = oldToken }()
	id := loginJWT(map[string]any{"email": "g@example.com", "sub": "sub-1"})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") != "code-1" || r.Form.Get("code_verifier") == "" {
			t.Errorf("exchange form: %v", r.Form)
		}
		fmt.Fprintf(w, `{"access_token":"a","refresh_token":"r","id_token":%q,"expires_in":3600}`, id)
	}))
	defer ts.Close()
	googleTokenURL = ts.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	doc, kind, err := AntigravityLogin(ctx, func(raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			t.Error(err)
			return
		}
		q := u.Query()
		go func() {
			redirect := q.Get("redirect_uri")
			resp, err := http.Get(redirect + "?state=" + url.QueryEscape(q.Get("state")) + "&code=code-1")
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	})
	if err != nil {
		t.Fatal(err)
	}
	if kind == "" || doc == nil {
		t.Fatalf("empty sign-in: %s %#v", kind, doc)
	}
	b, _ := json.Marshal(doc)
	if !strings.Contains(string(b), `"refresh_token":"r"`) || !strings.Contains(string(b), `"id_token":"x.`) {
		t.Fatalf("credential: %s", b)
	}
}

func loginJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "x." + base64.RawURLEncoding.EncodeToString(b) + ".y"
}

func TestCLILoginImportsIsolatedCodex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
mkdir -p "$CODEX_HOME"
printf '%s' '{"tokens":{"access_token":"a","refresh_token":"r","id_token":"x.y.z","account_id":"acct"}}' > "$CODEX_HOME/auth.json"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_BIN", bin)
	doc, kind, err := CLILogin(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if kind != "file" || doc["tokens"].(map[string]any)["account_id"] != "acct" {
		t.Fatalf("imported %s %#v", kind, doc)
	}
}

func TestCLILoginImportsIsolatedGrok(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := filepath.Join(t.TempDir(), "grok")
	script := `#!/bin/sh
mkdir -p "$GROK_HOME"
printf '%s' '{"https://auth.x.ai::id":{"key":"k","refresh_token":"r","email":"grok@example.com","user_id":"u1","oidc_issuer":"https://auth.x.ai"}}' > "$GROK_HOME/auth.json"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GROK_BIN", bin)
	doc, kind, err := CLILogin(context.Background(), "grok")
	if err != nil {
		t.Fatal(err)
	}
	if kind != "file" || doc["https://auth.x.ai::id"] == nil {
		t.Fatalf("imported %s %#v", kind, doc)
	}
}
