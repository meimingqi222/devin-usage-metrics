package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
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

func TestReadClaudeDirStampsEmailFromClaudeJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","subscriptionType":"pro"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"isolated@example.com"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	doc, kind, err := readClaudeDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "file" || doc["email"] != "isolated@example.com" {
		t.Fatalf("expected email from .claude.json, got kind=%s doc=%#v", kind, doc)
	}
}

func TestAntigravityOAuthFlow(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
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
	exchanging := false
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
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(body), "Sign-in complete") {
				t.Error("callback claimed completion before the token exchange finished")
			}
		}()
	}, func() { exchanging = true })
	if err != nil {
		t.Fatal(err)
	}
	if kind == "" || doc == nil {
		t.Fatalf("empty sign-in: %s %#v", kind, doc)
	}
	if !exchanging {
		t.Fatal("authorization progress was not reported")
	}
	b, _ := json.Marshal(doc)
	if !strings.Contains(string(b), `"refresh_token":"r"`) || !strings.Contains(string(b), `"id_token":"x.`) {
		t.Fatalf("credential: %s", b)
	}
}

type googleRoundTripFunc func(*http.Request) (*http.Response, error)

func (f googleRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGoogleTokenExchangeDeadlineAndDiagnostics(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	oldClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = oldClient })
	transportErr := errors.New("fixture transport error with private-code")
	http.DefaultClient = &http.Client{Transport: googleRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			t.Error("token exchange has no independent 30-second deadline")
		}
		return nil, transportErr
	})}
	_, _, err := exchangeGoogle(context.Background(), "private-client", "private-secret", "http://localhost/private-redirect", "private-verifier", "private-code")
	if !errors.Is(err, transportErr) {
		t.Fatalf("exchange error: %v", err)
	}
	b, err := os.ReadFile(syncdiag.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-client", "private-secret", "private-redirect", "private-verifier", "private-code"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("diagnostics contain %s", secret)
		}
	}
	for _, event := range []string{`"stage":"google_token_exchange"`, `"event":"stage_start"`, `"event":"stage_end"`, `"status":"failed"`} {
		if !strings.Contains(string(b), event) {
			t.Errorf("missing diagnostic %s", event)
		}
	}
}

func TestGoogleTokenExchangeCancelsWhileReadingBody(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	oldToken := googleTokenURL
	t.Cleanup(func() { googleTokenURL = oldToken })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()
	googleTokenURL = ts.URL
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := exchangeGoogle(ctx, "fixture-client", "fixture-secret", "http://localhost", "fixture-verifier", "fixture-code")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body read timeout was lost: %v", err)
	}
}

func TestGoogleTokenRejectionDiagnosticsAreSanitized(t *testing.T) {
	for _, code := range []string{"invalid_grant", "invalid_client", "private-server-data"} {
		t.Run(code, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			oldToken := googleTokenURL
			t.Cleanup(func() { googleTokenURL = oldToken })
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q,"error_description":"private-response-body"}`, code)
			}))
			defer ts.Close()
			googleTokenURL = ts.URL
			_, _, err := exchangeGoogle(context.Background(), "fixture-client", "fixture-secret", "http://localhost", "fixture-verifier", "fixture-code")
			want := code
			if code == "private-server-data" {
				want = "unknown_error"
			}
			if err == nil || !strings.Contains(err.Error(), "HTTP 400 ("+want+")") {
				t.Fatalf("wrong rejection: %v", err)
			}
			b, readErr := os.ReadFile(syncdiag.Path())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(b), `"oauth_error":"`+want+`"`) {
				t.Error("missing sanitized OAuth rejection code")
			}
			if strings.Contains(string(b)+err.Error(), "private-") {
				t.Error("upstream response details escaped sanitization")
			}
		})
	}
}

func loginJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "x." + base64.RawURLEncoding.EncodeToString(b) + ".y"
}

func TestAntigravityOAuthRepeatedCallbackCanCancel(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	oldToken := googleTokenURL
	t.Cleanup(func() { googleTokenURL = oldToken })
	started := make(chan struct{})
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(release) })
	googleTokenURL = ts.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callback := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, _, err := AntigravityLogin(ctx, func(raw string) {
			u, _ := url.Parse(raw)
			q := u.Query()
			callback <- q.Get("redirect_uri") + "?state=" + url.QueryEscape(q.Get("state")) + "&code=fixture-code"
		}, nil)
		done <- err
	}()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for i, target := 0, <-callback; i < 3; i++ {
		resp, err := client.Get(target)
		if err != nil {
			t.Errorf("callback %d blocked: %v", i+1, err)
			break
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if i == 0 {
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("token exchange did not start")
			}
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("login stayed stuck after cancellation with repeated callbacks")
	}
}

func TestAntigravityOAuthCancelWithBrowserPreconnect(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, _, err := AntigravityLogin(ctx, func(raw string) { opened <- raw }, nil)
		done <- err
	}()
	u, _ := url.Parse(<-opened)
	redirect, _ := url.Parse(u.Query().Get("redirect_uri"))
	conn, err := net.Dial("tcp", redirect.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A partial header proves the server accepted this speculative browser connection.
	_, _ = io.WriteString(conn, "GET /oauth-callback HTTP/1.1\r\nHost: "+redirect.Host+"\r\n")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("login cancellation waited indefinitely for browser connection")
	}
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
