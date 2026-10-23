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
	"runtime"
	"strings"
	"testing"
	"time"
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
	claudeGrants.Lock()
	claudeGrants.m = map[string]claudeGrantsCheck{}
	claudeGrants.Unlock()
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
	if e := saveDevinAccount("manual", "fixture-secret", "org-test"); e != nil {
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
	if e != nil || (runtime.GOOS != "windows" && s.Mode().Perm() != 0600) {
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
func TestClaudeCooldownSurvivesRestartAndBacksOff(t *testing.T) {
	for _, tc := range []struct {
		header  string
		minimum int64
	}{{"", 300}, {"0", 300}, {"invalid", 300}, {"-3", 300}, {time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 300}, {"7200", 7200}, {time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat), 7200}} {
		t.Run(tc.header, func(t *testing.T) {
			fixtureHome(t)
			fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"private-fixture-token","refreshToken":"private-refresh"}}`)
			a := Discover(false)[0]
			limited, calls := false, 0
			localServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/oauth/usage" && r.URL.RawQuery == "" {
					calls++
					if limited {
						w.Header().Set("Retry-After", tc.header)
						w.WriteHeader(429)
						fmt.Fprint(w, `{"error":"private-upstream-body"}`)
						return
					}
					fmt.Fprint(w, `{"five_hour":{"utilization":37}}`)
					return
				}
				fmt.Fprint(w, `{}`)
			})
			ctx := context.Background()
			if _, err := Fetch(ctx, a, false); err != nil {
				t.Fatal(err)
			}
			limited = true
			check := func(seconds int64) {
				t.Helper()
				r, err := Fetch(ctx, a, true)
				if err != nil || len(r.Windows) != 1 || r.Windows[0].UsedPercent != 37 || r.Stale == "" || r.RetryIn < seconds-2 || r.RetryIn > seconds+1 {
					t.Fatalf("cached windows/cooldown: %+v %v, need >= %ds", r, err, seconds)
				}
			}
			check(tc.minimum)
			b, err := os.ReadFile(quotaCachePath(quotaKey(a)))
			if err != nil || strings.Contains(string(b), "private-") {
				t.Fatal("quota cache missing or contains credentials/upstream error")
			}
			// Drop all process-local quota and grants state: the next Fetch can
			// only recover the cooldown and last-good windows from disk.
			registry.m = map[string]*entry{}
			claudeGrants.m = map[string]claudeGrantsCheck{}
			check(tc.minimum)
			if calls != 2 {
				t.Fatal("restart or forced refresh bypassed cooldown")
			}
			if tc.header == "0" {
				for _, seconds := range []int64{600, 1200, 2400, 3600, 3600} {
					registry.m[quotaKey(a)].until = time.Now().Add(-time.Second)
					check(seconds)
				}
				registry.m[quotaKey(a)].until = time.Now().Add(-time.Second)
				limited = false
				if r, err := Fetch(ctx, a, true); err != nil || r.Stale != "" || r.RetryIn != 0 {
					t.Fatalf("successful refresh did not recover: %+v %v", r, err)
				}
				limited = true
				check(300)
				if r, _ := Fetch(ctx, a, true); r.RetryIn > 301 {
					t.Fatal("success did not reset exponential backoff")
				}
			}
		})
	}
}

func TestClaudeAuxiliary429SharesCooldownAndKeepsFreshUsage(t *testing.T) {
	for _, path := range []string{"profile", "grants"} {
		t.Run(path, func(t *testing.T) {
			fixtureHome(t)
			fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"first-login"}}`)
			a := Discover(false)[0]
			calls := 0
			localServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.RawQuery != "" || r.URL.Path == "/api/oauth/profile" && path == "profile" {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(429)
					return
				}
				fmt.Fprint(w, `{"five_hour":{"utilization":61}}`)
			})
			r, err := Fetch(context.Background(), a, false)
			if err != nil || len(r.Windows) != 1 || r.Windows[0].UsedPercent != 61 || r.RetryIn < 299 {
				t.Fatalf("auxiliary 429 discarded fresh bars: %+v %v", r, err)
			}
			before := calls
			registry.m = map[string]*entry{}
			claudeGrants.m = map[string]claudeGrantsCheck{}
			if _, err = Fetch(context.Background(), a, true); err != nil || calls != before {
				t.Fatal("auxiliary 429 cooldown not restored")
			}
			if path == "grants" {
				registry.m[quotaKey(a)].until = time.Now().Add(-time.Second)
				r, err = Fetch(context.Background(), a, true)
				if err != nil || r.Stale != "" || calls != before+2 {
					t.Fatal("restart lost the hourly grants check")
				}
				before = calls
			}
			// A different login in the same file must not inherit the prior
			// login's cooldown, bars or hourly grant status.
			fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"second-login"}}`)
			if _, err = Fetch(context.Background(), a, false); err != nil || calls == before {
				t.Fatal("new login inherited old quota cache")
			}
		})
	}
}

func TestClaudeFirst429RestoresCooldownWithoutCachedUsage(t *testing.T) {
	fixtureHome(t)
	fixture(t, ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"first-login"}}`)
	a := Discover(false)[0]
	calls := 0
	localServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
	})
	for i := 0; i < 2; i++ {
		registry.m = map[string]*entry{}
		if _, err := Fetch(context.Background(), a, true); err == nil || !strings.Contains(err.Error(), "429") {
			t.Fatal("missing first-query rate limit")
		}
	}
	if calls != 1 {
		t.Fatal("cold 429 cooldown was lost on restart")
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
	if e := saveDevinAccount("manual", "session=fake", "org-fixture"); e != nil {
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

// saveDevinAccount stores a cookie account the way the Rust app's quota
// store holds them; the Go app only reads and removes them.
func saveDevinAccount(label, cookie, org string) error {
	storeMu.Lock()
	defer storeMu.Unlock()
	s := loadStore()
	s.Devin = append(s.Devin, storedDevin{label, cookie, org})
	return atomicWrite(configPath("quota-accounts.json"), s)
}
