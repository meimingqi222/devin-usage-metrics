package accounts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

// CLILogin runs an agent's own browser sign-in in an isolated config
// directory, then returns the credential it wrote. The live login is never
// touched.
func CLILogin(ctx context.Context, provider string) (map[string]any, string, error) {
	dir, err := os.MkdirTemp("", "aum-login-"+provider+"-*")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	var bin string
	var args []string
	var env []string
	switch provider {
	case "claude":
		bin = envOr("CLAUDE_BIN", "claude")
		args = []string{"auth", "login", "--claudeai"}
		env = loginEnv(dir, "CLAUDE_CONFIG_DIR", map[string]bool{
			"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "ANTHROPIC_BASE_URL": true,
			"ANTHROPIC_MODEL": true, "ANTHROPIC_CUSTOM_HEADERS": true,
			"CLAUDE_CODE_OAUTH_TOKEN": true, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN": true,
			"CLAUDE_CODE_OAUTH_SCOPES": true, "CLAUDE_CONFIG_DIR": true,
			"CLAUDE_CODE_USE_BEDROCK": true, "CLAUDE_CODE_USE_VERTEX": true,
			"CLAUDE_CODE_USE_FOUNDRY": true, "CLAUDE_CODE_USE_GATEWAY": true,
			"CLAUDE_CODE_USE_MANTLE": true, "CLAUDE_CODE_USE_ANTHROPIC_AWS": true,
			"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD": true,
			"CLAUDE_SECURESTORAGE_CONFIG_DIR":        true, "CLAUDECODE": true,
			"CLAUDE_CODE_ENTRYPOINT": true,
		})
	case "codex":
		bin = envOr("CODEX_BIN", "codex")
		args = []string{"login"}
		env = loginEnv(dir, "CODEX_HOME", map[string]bool{"OPENAI_API_KEY": true, "CODEX_HOME": true})
	case "grok":
		bin = envOr("GROK_BIN", "grok")
		args = []string{"login"}
		env = loginEnv(dir, "GROK_HOME", map[string]bool{"GROK_HOME": true, "XAI_API_KEY": true, "GROK_API_KEY": true})
	default:
		return nil, "", fmt.Errorf("%s does not support adding an account here", provider)
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, "", fmt.Errorf("could not find the %s CLI; install it or set %s", bin, strings.ToUpper(provider)+"_BIN")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		_ = stdin.Close()
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		_ = stdin.Close()
		return nil, "", ctx.Err()
	}
	if err != nil {
		return nil, "", fmt.Errorf("%s sign-in did not finish: %s", bin, lastLine(out.String()))
	}
	switch provider {
	case "claude":
		doc, kind, err := readClaudeDir(dir)
		if err != nil {
			return nil, "", err
		}
		// The isolated copy is imported below; remove both possible stores
		// so the account has one holder again.
		clearClaudeDir(dir)
		return doc, kind, nil
	case "codex", "grok":
		b, err := os.ReadFile(filepath.Join(dir, "auth.json"))
		if err != nil {
			return nil, "", fmt.Errorf("%s sign-in finished but auth.json was not written", provider)
		}
		var doc map[string]any
		if json.Unmarshal(b, &doc) != nil {
			return nil, "", errors.New(provider + " wrote an unreadable sign-in")
		}
		return doc, "file", nil
	}
	return nil, "", errors.New("unsupported provider")
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func loginEnv(dir, key string, clear map[string]bool) []string {
	out := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || clear[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+dir)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			if strings.Contains(line, "http://") || strings.Contains(line, "https://") {
				return "the browser sign-in did not finish"
			}
			return line
		}
	}
	return "no output"
}

// Claude Code stores a per-CLAUDE_CONFIG_DIR login in the keychain on macOS
// (a service named after the directory) and in .credentials.json elsewhere.
func readClaudeDir(dir string) (map[string]any, string, error) {
	if runtime.GOOS == "darwin" {
		if service := claudeDirService(dir); service != "" {
			if b, err := securityRead(service); err == nil {
				if doc, err := decodeJSON(b); err == nil {
					return doc, "keychain", nil
				}
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil {
		return nil, "", errors.New("Claude Code sign-in finished but no credential was found")
	}
	doc, err := decodeJSON(b)
	if err != nil {
		return nil, "", err
	}
	return doc, "file", nil
}

func clearClaudeDir(dir string) {
	if runtime.GOOS == "darwin" {
		service := claudeDirService(dir)
		_ = exec.Command("security", "delete-generic-password", "-s", service, "-a", claudeAccount()).Run()
	}
	_ = os.Remove(filepath.Join(dir, ".credentials.json"))
}

func claudeDirService(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:4])
}

func claudeAccount() string {
	if user := strings.TrimSpace(os.Getenv("USER")); user != "" {
		return user
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Base(home)
	}
	return "claude-code-user"
}

func securityRead(service string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", claudeAccount(), "-w").Output()
	if err == nil {
		return out, nil
	}
	// Some Claude Code builds name the item without an account, or under a
	// different one; read the service's first item as a fallback.
	return exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-w").Output()
}

func decodeJSON(b []byte) (map[string]any, error) {
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil || doc == nil {
		return nil, errors.New("the agent wrote an unreadable sign-in")
	}
	return doc, nil
}

// google endpoints are variables so tests can point them at a local server.
var (
	googleAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL     = "https://oauth2.googleapis.com/token"
	googleUserInfoURL  = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
)

// googleAccountEmail names an Antigravity login that carries no id_token.
func googleAccountEmail(ctx context.Context, access string) string {
	if access == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", googleUserInfoURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v)
	return strings.TrimSpace(v.Email)
}

// AntigravityLogin signs in with Google in the browser and returns the
// credential in the shape Antigravity stores it. open receives the sign-in
// URL; the desktop opens it in the user's browser.
func AntigravityLogin(ctx context.Context, open func(string)) (map[string]any, string, error) {
	clientID, clientSecret := quota.AntigravityOAuthClient()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d/oauth-callback", port)
	state := randomURL(16)
	verifier := randomURL(32)
	challenge := sha256.Sum256([]byte(verifier))
	auth := googleAuthorizeURL + "?" + url.Values{
		"client_id":              {clientID},
		"redirect_uri":           {redirect},
		"response_type":          {"code"},
		"scope":                  {"openid email profile https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs"},
		"access_type":            {"offline"},
		"prompt":                 {"consent"},
		"include_granted_scopes": {"true"},
		"state":                  {state},
		"code_challenge":         {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method":  {"S256"},
	}.Encode()
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth-callback" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- errors.New("Google sign-in state did not match")
			return
		}
		if e := r.URL.Query().Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			errCh <- fmt.Errorf("Google sign-in: %s", e)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- errors.New("Google sign-in returned no code")
			return
		}
		codeCh <- code
		_, _ = io.WriteString(w, "<html><body>Sign-in complete. You can close this window.</body></html>")
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Shutdown(context.Background())
	if open != nil {
		open(auth)
	}
	select {
	case code := <-codeCh:
		return exchangeGoogle(ctx, clientID, clientSecret, redirect, verifier, code)
	case err := <-errCh:
		return nil, "", err
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case <-time.After(5 * time.Minute):
		return nil, "", errors.New("Google sign-in timed out")
	}
}

func exchangeGoogle(ctx context.Context, clientID, clientSecret, redirect, verifier, code string) (map[string]any, string, error) {
	form := url.Values{
		"code": {code}, "client_id": {clientID}, "client_secret": {clientSecret},
		"redirect_uri": {redirect}, "grant_type": {"authorization_code"}, "code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Google token exchange failed: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if json.Unmarshal(b, &tok) != nil || tok.AccessToken == "" || tok.RefreshToken == "" {
		return nil, "", errors.New("Google returned an incomplete sign-in")
	}
	exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC()
	kind := "file"
	doc := map[string]any{
		"access_token": tok.AccessToken, "token_type": "Bearer",
		"refresh_token": tok.RefreshToken, "expiry_date": exp.UnixMilli(), "id_token": tok.IDToken,
	}
	if runtime.GOOS == "darwin" {
		kind = "keychain"
		doc = map[string]any{
			"token": map[string]any{
				"access_token": tok.AccessToken, "token_type": "Bearer",
				"refresh_token": tok.RefreshToken, "expiry": exp.Format(time.RFC3339Nano),
			},
			"auth_method": "consumer", "id_token": tok.IDToken,
		}
	}
	return doc, kind, nil
}

func randomURL(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
