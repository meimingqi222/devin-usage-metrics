package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func grokEntry(d object) (string, object) {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := obj(d[k])
		if _, ok := v["refresh_token"]; ok {
			return k, v
		}
	}
	return "", nil
}
func fetch(ctx context.Context, a Account) (Result, error) {
	if a.Provider == "devin" {
		if a.Key == "devin-cli" {
			t, _ := devinCLI()
			return fetchCLI(ctx, t)
		}
		i, e := strconv.Atoi(strings.TrimPrefix(a.source, "stored:"))
		s := loadStore()
		if e != nil || i < 0 || i >= len(s.Devin) {
			return Result{}, errors.New("account no longer exists")
		}
		d := s.Devin[i]
		return fetchDevin(ctx, d.Cookie, d.Org)
	}
	d, e := loadAccount(a)
	if e != nil {
		return Result{}, e
	}
	c := d
	entryKey := ""
	tokenKey := "access_token"
	expiryKey := "expired"
	switch a.Provider {
	case "claude":
		c = obj(d["claudeAiOauth"])
		tokenKey = "accessToken"
		expiryKey = "expiresAt"
	case "grok":
		entryKey, c = grokEntry(d)
		tokenKey = "key"
		expiryKey = "expires_at"
	case "antigravity":
		expiryKey = "expiry_date"
	}
	if c == nil {
		return Result{}, errors.New("missing OAuth credentials")
	}
	token := str(c[tokenKey])
	if token == "" {
		if a.Provider == "codex" {
			return Result{Plan: "API MODE"}, nil
		}
		return Result{}, errors.New("missing access token")
	}
	refreshKey := "refresh_token"
	if a.Provider == "claude" {
		refreshKey = "refreshToken"
	}
	if a.Provider == "antigravity" && str(c[refreshKey]) != "" {
		if cached := antigravityCachedToken(str(c[refreshKey])); cached != "" {
			token = cached
		}
	}
	refreshed := false
	refresh := func() error {
		latest, source, le := loadAccountSource(a)
		if a.Provider == "claude" && le != nil {
			return le
		}
		if le == nil && a.Provider == "claude" {
			lc := obj(latest["claudeAiOauth"])
			if str(lc[tokenKey]) != token && (when(lc[expiryKey]) == 0 || when(lc[expiryKey]) > time.Now().Unix()+300) {
				d = latest
				c = lc
				token = str(c[tokenKey])
				return nil
			}
			d = latest
			c = lc
		}
		rt := str(c[refreshKey])
		if rt == "" {
			return errors.New("no refresh token; sign in again")
		}
		v, er := refreshToken(ctx, a.Provider, c, rt)
		if er != nil {
			var he *httpError
			if a.Provider == "claude" && errors.As(er, &he) && (he.status == 400 || he.status == 401) && invalidGrant(he.body) {
				latest, le := loadAccount(a)
				lc := obj(latest["claudeAiOauth"])
				if le == nil && str(lc[refreshKey]) != "" && str(lc[refreshKey]) != rt {
					d = latest
					c = lc
					token = str(c[tokenKey])
					return nil
				}
				return errors.New("Session expired (invalid_grant); run claude to sign in again")
			}
			return er
		}
		token = str(v["access_token"])
		if token == "" {
			return errors.New("refresh response missing access_token")
		}
		c[tokenKey] = token
		if s := str(v["refresh_token"]); s != "" {
			c[refreshKey] = s
		}
		if s := str(v["id_token"]); s != "" {
			c["id_token"] = s
		}
		exp := time.Now().Unix() + int64(num(v["expires_in"]))
		if a.Provider == "antigravity" {
			antigravitySaveToken(str(c[refreshKey]), token, exp)
		}
		switch a.Provider {
		case "claude", "antigravity":
			c[expiryKey] = exp * 1000
		case "codex":
			c[expiryKey] = time.Unix(exp, 0).UTC().Format(time.RFC3339)
			d["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
		case "grok":
			if num(c[expiryKey]) > 1e12 {
				c[expiryKey] = exp * 1000
			} else {
				c[expiryKey] = exp
			}
		}
		if a.Provider == "claude" {
			current, ce := loadAccount(Account{Provider: a.Provider, source: source})
			if ce == nil && str(at(current, "claudeAiOauth", refreshKey)) == rt {
				oauth := obj(current["claudeAiOauth"])
				for _, k := range []string{tokenKey, refreshKey, expiryKey} {
					oauth[k] = c[k]
				}
				d = current
			} else {
				return nil
			}
		}
		if a.Provider == "grok" {
			d[entryKey] = c
		}
		if a.Provider == "claude" {
			if strings.HasPrefix(source, "keychain:") {
				_ = keyWrite("Claude Code-credentials", d)
			} else {
				_ = atomicWrite(source, d)
			}
		} else if !strings.HasPrefix(a.source, "keychain:") && filepath.Ext(a.source) != ".vscdb" {
			_ = atomicWrite(a.source, d)
		}
		return nil
	}
	if a.Provider == "claude" && when(c[expiryKey]) != 0 && when(c[expiryKey]) <= time.Now().Unix()+300 && str(c[refreshKey]) != "" {
		refreshed = true
		if e = refresh(); e != nil && when(c[expiryKey]) <= time.Now().Unix() {
			return Result{}, e
		}
	}
	reread := false
	for {
		h := bearer(token)
		var usage object
		switch a.Provider {
		case "claude":
			h["anthropic-beta"] = "oauth-2025-04-20"
			h["User-Agent"] = "claude-cli/2.1.294 (external, cli)"
			usage, e = jsonRequest(ctx, "GET", anthropicOrigin+claudeUsagePath, "", h, false)
		case "codex":
			h["User-Agent"] = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
			usage, e = jsonRequest(ctx, "GET", "https://chatgpt.com/backend-api/wham/usage", "", h, false)
		case "grok":
			h["x-xai-token-auth"] = "xai-grok-cli"
			h["x-grok-client-version"] = "0.2.91"
			h["User-Agent"] = "grok-pager/0.2.91 grok-shell/0.2.91 (windows; x86_64)"
			usage, e = jsonRequest(ctx, "GET", "https://cli-chat-proxy.grok.com/v1/billing?format=credits", "", h, false)
		case "antigravity":
			h["User-Agent"] = "antigravity/2.11.0"
			h["x-goog-api-client"] = "antigravity/2.11.0"
			usage, e = jsonRequest(ctx, "POST", "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "{}", h, false)
		default:
			return Result{}, errors.New("unsupported provider")
		}
		if status(e) == 401 && a.Provider == "claude" && !reread {
			reread = true
			if latest, err := loadAccount(a); err == nil && str(at(latest, "claudeAiOauth", tokenKey)) != "" && str(at(latest, "claudeAiOauth", tokenKey)) != token {
				d = latest
				c = obj(d["claudeAiOauth"])
				token = str(c[tokenKey])
				continue
			}
		}
		if status(e) == 401 && !refreshed && str(c[refreshKey]) != "" {
			refreshed = true
			if e = refresh(); e != nil {
				return Result{}, e
			}
			continue
		}
		if e != nil {
			if a.Provider == "grok" && status(e) >= 400 && status(e) < 500 && status(e) != 401 && status(e) != 403 && status(e) != 408 && status(e) != 429 {
				return Result{Windows: []Window{{Label: "Weekly"}}}, nil
			}
			return Result{}, e
		}
		switch a.Provider {
		case "claude":
			plan := ""
			p, _ := jsonRequest(ctx, "GET", anthropicOrigin+"/api/oauth/profile", "", h, false)
			if at(p, "account", "has_claude_max") == true {
				plan = "Max"
			} else if at(p, "account", "has_claude_pro") == true {
				plan = "Pro"
			} else if strings.EqualFold(str(at(p, "organization", "organization_type")), "claude_team") && strings.EqualFold(str(at(p, "organization", "subscription_status")), "active") {
				plan = "Team"
			}
			return ParseClaudeUsage(usage, plan), nil
		case "codex":
			return ParseCodexUsage(usage, str(usage["plan_type"])), nil
		case "grok":
			m, _ := jsonRequest(ctx, "GET", "https://cli-chat-proxy.grok.com/v1/billing", "", h, false)
			return ParseGrokBilling(usage, m), nil
		default:
			return ParseAntigravityQuota(usage), nil
		}
	}
}
func invalidGrant(body string) bool {
	var v object
	if json.Unmarshal([]byte(body), &v) != nil {
		return false
	}
	return str(v["error"]) == "invalid_grant" || str(v["error_description"]) == "invalid_grant" || str(at(v, "error", "type")) == "invalid_grant" || str(at(v, "error", "code")) == "invalid_grant"
}
func refreshToken(ctx context.Context, p string, c object, rt string) (object, error) {
	u := ""
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}}
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"}
	switch p {
	case "claude":
		b, _ := json.Marshal(object{"client_id": "9d1c250a-e61b-44d9-88ed-5944d1962f5e", "grant_type": "refresh_token", "refresh_token": rt, "scope": "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"})
		h["Content-Type"] = "application/json"
		h["User-Agent"] = "axios/1.15.2"
		return jsonRequest(ctx, "POST", "https://platform.claude.com/v1/oauth/token", string(b), h, true)
	case "codex":
		u = "https://auth.openai.com/oauth/token"
		form.Set("client_id", "app_EMoamEEZ73f0CkXaXp7hrann")
		form.Set("scope", "openid profile email")
	case "grok":
		issuer := strings.TrimRight(str(c["oidc_issuer"]), "/")
		if issuer == "" {
			issuer = "https://auth.x.ai"
		}
		u = issuer + "/oauth2/token"
		d, _ := jsonRequest(ctx, "GET", issuer+"/.well-known/openid-configuration", "", nil, false)
		if x, er := url.Parse(str(d["token_endpoint"])); er == nil && x.Scheme == "https" && (x.Hostname() == "x.ai" || strings.HasSuffix(x.Hostname(), ".x.ai")) {
			u = x.String()
		}
		id := str(c["oidc_client_id"])
		if id == "" {
			id = "b1a00492-073a-47ea-816f-4c329264a828"
		}
		form.Set("client_id", id)
	case "antigravity":
		u = "https://oauth2.googleapis.com/token"
		id, secret := os.Getenv("ANTIGRAVITY_CLIENT_ID"), os.Getenv("ANTIGRAVITY_CLIENT_SECRET")
		if id == "" || secret == "" {
			id, secret = antigravityClient()
		}
		form.Set("client_id", id)
		form.Set("client_secret", secret)
	}
	return jsonRequest(ctx, "POST", u, form.Encode(), h, true)
}
func findOrg(v any) string {
	if s := str(v); strings.HasPrefix(s, "org-") {
		return s
	}
	for _, x := range arr(v) {
		if s := findOrg(x); s != "" {
			return s
		}
	}
	keys := []string{}
	for k := range obj(v) {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s := findOrg(obj(v)[k]); s != "" {
			return s
		}
	}
	return ""
}
func DiscoverDevinOrg(ctx context.Context, cookie string) (string, error) {
	v, e := jsonRequest(ctx, "GET", "https://app.devin.ai/api/organizations", "", map[string]string{"Cookie": cookie, "Accept": "application/json"}, false)
	if e != nil {
		return "", e
	}
	s := findOrg(v)
	if s == "" {
		return "", errors.New("no org found in organizations")
	}
	return s, nil
}
func fetchDevin(ctx context.Context, cookie, org string) (Result, error) {
	if org == "" {
		var e error
		org, e = DiscoverDevinOrg(ctx, cookie)
		if e != nil {
			return Result{}, e
		}
	}
	h := map[string]string{"Cookie": cookie, "Accept": "application/json"}
	base := "https://app.devin.ai/api/" + url.PathEscape(org) + "/billing/"
	v, e := jsonRequest(ctx, "GET", base+"quota/usage", "", h, false)
	if e != nil {
		return Result{}, e
	}
	s, _ := jsonRequest(ctx, "GET", base+"status", "", h, false)
	return ParseDevinQuota(v, str(s["plan_slug"])), nil
}

func ParseClaudeUsage(v object, plan string) Result {
	r := Result{Plan: plan}
	for _, k := range []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet"} {
		w := obj(v[k])
		if _, ok := w["utilization"].(float64); !ok {
			continue
		}
		label := map[string]string{"five_hour": "5h", "seven_day": "7d", "seven_day_oauth_apps": "7d OAuth apps", "seven_day_opus": "7d Opus", "seven_day_sonnet": "7d Sonnet"}[k]
		reset := when(w["resets_at"])
		if reset == 0 {
			delta := int64(7 * 86400)
			if k == "five_hour" {
				delta = 5 * 3600
			}
			reset = time.Now().Unix() + delta
		}
		r.Windows = append(r.Windows, Window{label, num(w["utilization"]), reset})
	}
	if reset, ok := parseClaudeReset(obj(v["cedar_ember"]), time.Now().Unix()); ok {
		r.Reset = &reset
	}
	x := obj(v["extra_usage"])
	if x["is_enabled"] == true {
		if n, ok := x["used_credits"].(float64); ok {
			r.Extra = fmt.Sprintf("$%.2f", n)
			if l, ok := x["monthly_limit"].(float64); ok {
				r.Extra += fmt.Sprintf(" / $%.2f", l)
			}
		}
	}
	return r
}
func codexWindow(v any) (Window, bool) {
	w := obj(v)
	p, ok := w["used_percent"].(float64)
	if !ok {
		return Window{}, false
	}
	s := num(w["limit_window_seconds"])
	label := "Weekly"
	switch {
	case s >= 16000 && s < 20000:
		label = "5h"
	case s >= 600000 && s < 610000:
		label = "7d"
	case s >= 2400000 && s < 2800000:
		label = "Monthly"
	}
	reset := when(w["reset_at"])
	if reset == 0 {
		if n, ok := w["reset_after_seconds"].(float64); ok {
			reset = time.Now().Unix() + int64(n)
		}
	}
	return Window{label, p, reset}, true
}
func ParseCodexUsage(v object, plan string) Result {
	r := Result{Plan: plan}
	for _, k := range []string{"primary_window", "secondary_window"} {
		if w, ok := codexWindow(at(v, "rate_limit", k)); ok {
			r.Windows = append(r.Windows, w)
		}
	}
	for _, x := range arr(v["additional_rate_limits"]) {
		if w, ok := codexWindow(at(x, "rate_limit")); ok {
			w.Label = str(at(x, "limit_name"))
			if w.Label == "" {
				w.Label = "limit"
			}
			r.Windows = append(r.Windows, w)
		}
	}
	return r
}
func ParseDevinQuota(v object, plan string) Result {
	r := Result{Plan: plan}
	for _, k := range []string{"daily", "weekly"} {
		if k == "daily" && v["hide_daily_quota"] == true {
			continue
		}
		if n, ok := v[k+"_percentage"].(float64); ok {
			label := "Daily"
			if k == "weekly" {
				label = "Weekly"
			}
			r.Windows = append(r.Windows, Window{label, 100 - n, when(v[k+"_reset_at"])})
		}
	}
	if n := num(v["overage_balance"]); n > 0 {
		r.Extra = fmt.Sprintf("%.2f ACU", n)
	}
	return r
}
func either(v object, a, b string) any {
	if x, ok := v[a]; ok {
		return x
	}
	return v[b]
}
func cents(v any) (float64, bool) {
	if m := obj(v); m != nil {
		n, ok := m["val"].(float64)
		return n / 100, ok
	}
	n, ok := v.(float64)
	return n, ok
}
func ParseGrokBilling(v, m object) Result {
	r := Result{}
	c := obj(v["config"])
	if n, ok := either(c, "creditUsagePercent", "credit_usage_percent").(float64); ok {
		p := obj(either(c, "currentPeriod", "current_period"))
		kind := strings.ToLower(strings.TrimPrefix(str(p["type"]), "USAGE_PERIOD_TYPE_"))
		if kind == "" {
			kind = "weekly"
		}
		r.Plan = kind
		label := "Weekly"
		if kind == "monthly" {
			label = "Monthly"
		}
		r.Windows = append(r.Windows, Window{label, n, when(p["end"])})
	}
	c = obj(m["config"])
	l, lok := cents(either(c, "monthlyLimit", "monthly_limit"))
	u, uok := cents(c["used"])
	if lok && uok && l > 0 {
		r.Windows = append(r.Windows, Window{"Monthly", min(999, u/l*100), when(either(c, "billingPeriodEnd", "billing_period_end"))})
	}
	return r
}
func ParseAntigravityQuota(v object) Result {
	r := Result{}
	for _, g := range arr(v["groups"]) {
		for _, b := range arr(at(g, "buckets")) {
			label := str(at(b, "window"))
			if label == "weekly" {
				label = "Weekly"
			}
			remaining := 1.0
			if n, ok := at(b, "remainingFraction").(float64); ok {
				remaining = n
			}
			r.Windows = append(r.Windows, Window{str(at(g, "displayName")) + " – " + label, max(0, min(999, (1-remaining)*100)), when(at(b, "resetTime"))})
		}
	}
	return r
}
