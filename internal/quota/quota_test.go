package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type localTransport struct {
	base *url.URL
	next http.RoundTripper
}

func (l localTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	u := *r.URL
	u.Scheme = l.base.Scheme
	u.Host = l.base.Host
	r.URL = &u
	return l.next.RoundTrip(r)
}
func fixtureHome(t *testing.T) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("APPDATA", filepath.Join(h, ".config"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h, ".config"))
	registry.Lock()
	registry.m = map[string]*entry{}
	registry.Unlock()
}
func fixture(t *testing.T, p, data string) {
	t.Helper()
	p = homePath(p)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
}
func localServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	old := client
	client = &http.Client{Transport: localTransport{u, http.DefaultTransport}}
	t.Cleanup(func() { client = old })
}
func TestDiscoveryAndPrivateStore(t *testing.T) {
	fixtureHome(t)
	for p, data := range map[string]string{".claude/.credentials.json": `{"claudeAiOauth":{"accessToken":"fixture-secret","emailAddress":"a@example.test"}}`, ".codex/auth.json": `{"OPENAI_API_KEY":"fixture-secret"}`, ".grok/auth.json": `{"issuer":{"key":"fixture-secret","refresh_token":"r"}}`, ".gemini/oauth_creds.json": `{"access_token":"fixture-secret"}`} {
		fixture(t, p, data)
	}
	if KeychainAllowed() {
		t.Fatal("consent must default false")
	}
	if e := SetKeychainAllowed(true); e != nil {
		t.Fatal(e)
	}
	if !KeychainAllowed() {
		t.Fatal("preference not saved")
	}
	if e := SaveDevinAccount("manual", "fixture-secret", "org-test"); e != nil {
		t.Fatal(e)
	}
	a := Discover(false)
	if len(a) != 5 {
		t.Fatalf("accounts: %v", a)
	}
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), "fixture-secret") || strings.Contains(fmt.Sprintf("%#v", a), "fixture-secret") {
		t.Fatal("credential leak")
	}
	s, e := os.Stat(configPath("quota-accounts.json"))
	if e != nil || s.Mode().Perm() != 0600 {
		t.Fatal("store must be private")
	}
	if e := RemoveDevinAccount(0); e != nil {
		t.Fatal(e)
	}
	if len(Discover(false)) != 4 {
		t.Fatal("remove failed")
	}
}
func TestFetchCacheForceCooldownAndClone(t *testing.T) {
	fixtureHome(t)
	fixture(t, ".codex/auth.json", `{"access_token":"fake"}`)
	calls := 0
	limited := false
	localServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("Authorization") != "Bearer fake" {
			t.Error("request contract")
		}
		if limited {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"secret":"never render"}`)
			return
		}
		fmt.Fprint(w, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_at":1788163200000}}}`)
	})
	a := Discover(false)[0]
	ctx := context.Background()
	r, e := Fetch(ctx, a, false)
	if e != nil || r.Plan != "pro" || len(r.Windows) != 1 || r.Windows[0].ResetsAt != 1788163200 {
		t.Fatalf("%+v %v", r, e)
	}
	r.Windows[0].UsedPercent = 99
	r, e = Fetch(ctx, a, false)
	if e != nil || r.Windows[0].UsedPercent != 25 || calls != 1 {
		t.Fatal("cache alias or miss")
	}
	limited = true
	r, e = Fetch(ctx, a, true)
	if e != nil || r.Stale == "" || strings.Contains(r.Stale, "never render") {
		t.Fatalf("fallback %+v %v", r, e)
	}
	_, _ = Fetch(ctx, a, true)
	if calls != 2 {
		t.Fatal("force bypassed cooldown")
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = Fetch(c, a, false); e != context.Canceled {
		t.Fatal(e)
	}
}
func TestRefreshPersistsWithoutDiscardingFields(t *testing.T) {
	fixtureHome(t)
	fixture(t, ".codex/auth.json", `{"access_token":"old","refresh_token":"refresh","other":42}`)
	localServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			if e := r.ParseForm(); e != nil || r.Form.Get("refresh_token") != "refresh" || r.Form.Get("client_id") != "app_EMoamEEZ73f0CkXaXp7hrann" {
				t.Error("refresh contract")
			}
			fmt.Fprint(w, `{"access_token":"new","refresh_token":"rotated","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"plan_type":"pro"}`)
	})
	_, e := Fetch(context.Background(), Discover(false)[0], true)
	if e != nil {
		t.Fatal(e)
	}
	d, e := readDoc(homePath(".codex/auth.json"))
	if e != nil || str(d["access_token"]) != "new" || str(d["refresh_token"]) != "rotated" || num(d["other"]) != 42 {
		t.Fatalf("writeback: %v %v", d, e)
	}
}
func TestProviderWindows(t *testing.T) {
	tests := []struct {
		name, doc string
		parse     func(object) Result
		count     int
		used      float64
	}{{"claude", `{"five_hour":{"utilization":23.5},"seven_day_opus":{"utilization":0}}`, func(v object) Result { return ParseClaudeUsage(v, "Max") }, 2, 23.5}, {"devin", `{"daily_percentage":42,"weekly_percentage":17,"overage_balance":193.45}`, func(v object) Result { return ParseDevinQuota(v, "pro") }, 2, 58}, {"grok", `{"config":{"creditUsagePercent":25,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY"}}}`, func(v object) Result { return ParseGrokBilling(v, nil) }, 1, 25}, {"antigravity", `{"groups":[{"displayName":"Gemini","buckets":[{"window":"weekly","remainingFraction":0.75}]}]}`, ParseAntigravityQuota, 1, 25}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v object
			_ = json.Unmarshal([]byte(tt.doc), &v)
			r := tt.parse(v)
			if len(r.Windows) != tt.count || r.Windows[0].UsedPercent != tt.used {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestDevinManualAndCLI(t *testing.T) {
	fixtureHome(t)
	if e := SaveDevinAccount("manual", "session=fake", "org-fixture"); e != nil {
		t.Fatal(e)
	}
	fixture(t, ".local/share/devin/credentials.toml", "windsurf_api_key = \"devin-session-token$fake\"\n")
	fixture(t, ".config/devin/config.json", `{"devin":{"org_id":"org-fixture"}}`)
	localServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "GetUserStatus"):
			if r.Header.Get("Authorization") != "Basic devin-session-token$fake-devin-session-token$fake" {
				t.Error("CLI auth")
			}
			q := append(pbBytes(1, pbBytes(2, []byte("Pro"))), []byte{0x70, 72, 0x78, 68}...)
			_, _ = w.Write(pbBytes(1, pbBytes(13, q)))
		case strings.HasSuffix(r.URL.Path, "quota/usage"):
			if r.Header.Get("Cookie") != "session=fake" {
				t.Error("cookie")
			}
			fmt.Fprint(w, `{"daily_percentage":42,"weekly_percentage":17}`)
		default:
			fmt.Fprint(w, `{"plan_slug":"pro"}`)
		}
	})
	a := Discover(false)
	if len(a) != 2 {
		t.Fatal(a)
	}
	for _, account := range a {
		r, e := Fetch(context.Background(), account, true)
		if e != nil || r.Plan != "pro" || len(r.Windows) != 2 {
			t.Fatalf("%s %+v %v", account, r, e)
		}
	}
}
