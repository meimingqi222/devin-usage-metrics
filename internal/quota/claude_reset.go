package quota

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Claude offers some subscriptions a few "reset grants" (the cedar_ember
// program): spending one clears the listed usage windows early. The usage
// endpoint reports them only when asked with cedar_ember=1; skip_spend
// drops spend figures. The contract follows Claude Code, openusage and the
// CLIProxyAPI management center, which refuse anything they cannot validate.
const (
	claudeUsagePath  = "/api/oauth/usage"
	claudeGrantsPath = "/api/oauth/usage?cedar_ember=1&skip_spend=1"
)

// claudeGrantsInterval spaces grant reads per login. Anthropic rate-limits
// the cedar_ember read hard when polled: sending it on every quota refresh
// drew hour-long 429s on the usage endpoint itself (openusage #1355).
const claudeGrantsInterval = time.Hour

type claudeGrantsCheck struct {
	at       time.Time
	reset    *ClaudeReset
	identity string
}

var claudeGrants = struct {
	sync.Mutex
	m map[string]claudeGrantsCheck
}{m: map[string]claudeGrantsCheck{}}

// claudeResetGrants returns the login's reset grants, asking Anthropic at
// most once per claudeGrantsInterval. A failed read keeps the previous
// status and still counts as the check, so a 429 is retried an hour later
// rather than on every refresh; a cancelled read is not a verdict.
func claudeResetGrants(ctx context.Context, login string, h map[string]string) (*ClaudeReset, error) {
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(h["Authorization"])))
	claudeGrants.Lock()
	prev, seen := claudeGrants.m[login]
	claudeGrants.Unlock()
	if prev.identity != identity {
		prev, seen = claudeGrantsCheck{}, false
	}
	if seen && time.Since(prev.at) < claudeGrantsInterval {
		return cloneReset(prev.reset), nil
	}
	v, e := jsonRequest(ctx, "GET", anthropicOrigin+claudeGrantsPath, "", h, false)
	if ctx.Err() != nil {
		return cloneReset(prev.reset), ctx.Err()
	}
	check := claudeGrantsCheck{at: time.Now(), reset: prev.reset, identity: identity}
	if e == nil {
		check.reset = nil
		if r, ok := parseClaudeReset(obj(v["cedar_ember"]), time.Now().Unix()); ok {
			check.reset = &r
		}
	}
	claudeGrants.Lock()
	claudeGrants.m[login] = check
	claudeGrants.Unlock()
	if status(e) == 429 {
		return cloneReset(check.reset), e
	}
	return cloneReset(check.reset), nil
}

// forgetClaudeResetGrants makes the next refresh re-read a login's grants,
// after a claim changed them.
func forgetClaudeResetGrants(login string) {
	claudeGrants.Lock()
	delete(claudeGrants.m, login)
	claudeGrants.Unlock()
}

func cloneReset(r *ClaudeReset) *ClaudeReset {
	if r == nil {
		return nil
	}
	x := *r
	x.Clears = slices.Clone(x.Clears)
	return &x
}

// anthropicOrigin is a variable only so tests can point it elsewhere.
var anthropicOrigin = "https://api.anthropic.com"

var (
	claudeGrantID   = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	claudeOrgID     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	claudeResetWins = []string{"five_hour", "seven_day", "seven_day_overage_included"}
)

// ClaudeReset summarises an account's reset grants. Left and Total count
// every grant; GrantID, Clears and Blocker describe the one a reset spends.
type ClaudeReset struct {
	Left    int      `json:"left"`
	Total   int      `json:"total"`
	EndsAt  int64    `json:"ends_at,omitempty"`
	GrantID string   `json:"grant_id,omitempty"`
	Clears  []string `json:"clears,omitempty"`
	// Blocker says why no reset can be spent now: ineligible, cooldown,
	// not_limited (the windows are not at their limit), exhausted, paused or
	// not_usable. Empty means a reset is available.
	Blocker       string `json:"blocker,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
}

type claudeGrant struct {
	id                       string
	left, total              int
	starts, ends             int64
	clears                   []string
	paused, usable, needsCap bool
}

// parseClaudeReset reads the cedar_ember block. A malformed block yields
// nothing rather than a guess, since the button spends a scarce grant.
func parseClaudeReset(block object, now int64) (ClaudeReset, bool) {
	eligible, ok := block["eligible"].(bool)
	if !ok {
		return ClaudeReset{}, false
	}
	var grants []claudeGrant
	seen := map[string]bool{}
	for _, raw := range arr(block["grants"]) {
		g := obj(raw)
		id := str(g["id"])
		left, lok := g["resets_left"].(float64)
		total, tok := g["resets_total"].(float64)
		if !claudeGrantID.MatchString(id) || seen[id] || !lok || !tok || left < 0 || left > total || left != float64(int(left)) || total != float64(int(total)) {
			return ClaudeReset{}, false
		}
		seen[id] = true
		var clears []string
		for _, w := range arr(g["clears"]) {
			if s := str(w); slices.Contains(claudeResetWins, s) && !slices.Contains(clears, s) {
				clears = append(clears, s)
			}
		}
		// Missing flags take the refusing side.
		usable, _ := g["usable_now"].(bool)
		needsCap := true
		if v, ok := g["use_requires_limit"].(bool); ok {
			needsCap = v
		}
		paused, _ := g["paused"].(bool)
		grants = append(grants, claudeGrant{id, int(left), int(total), when(g["starts_at"]), when(g["ends_at"]), clears, paused, usable, needsCap})
	}
	if !eligible && len(grants) == 0 {
		return ClaudeReset{}, false
	}
	atLimit, _ := block["at_limit"].(bool)
	r := ClaudeReset{CooldownUntil: when(block["cooldown_until"])}
	for _, g := range grants {
		r.Left += g.left
		r.Total += g.total
		if g.left > 0 && g.ends > 0 && (r.EndsAt == 0 || g.ends < r.EndsAt) {
			r.EndsAt = g.ends
		}
	}
	blocker := func(g claudeGrant) string {
		switch {
		case !eligible:
			return "ineligible"
		case g.paused:
			return "paused"
		case !g.usable || g.starts > now || (g.ends > 0 && g.ends <= now):
			return "not_usable"
		case g.left <= 0:
			return "exhausted"
		case g.needsCap && !atLimit:
			return "not_limited"
		case r.CooldownUntil > now:
			return "cooldown"
		}
		return ""
	}
	// Prefer the grant upstream recommends, then stable ID order.
	slices.SortFunc(grants, func(a, b claudeGrant) int { return strings.Compare(a.id, b.id) })
	next := str(block["next_grant_id"])
	pick := -1
	for i, g := range grants {
		if blocker(g) == "" && (pick < 0 || g.id == next) {
			pick = i
		}
	}
	if pick < 0 {
		for i, g := range grants {
			if g.left > 0 && (pick < 0 || g.id == next) {
				pick = i
			}
		}
	}
	if pick < 0 {
		r.Blocker = "exhausted"
		if !eligible {
			r.Blocker = "ineligible"
		}
		return r, true
	}
	g := grants[pick]
	r.GrantID, r.Clears, r.Blocker = g.id, g.clears, blocker(g)
	return r, true
}

// Claim outcomes from Claude's reset endpoint, plus the two HTTP refusals
// that prove nothing was spent.
var claudeResetResults = []string{"reset", "already_used", "not_limited", "cooldown", "ineligible", "unavailable", "rate_limited", "auth_error"}

// ErrClaudeResetUnknown means the claim may have been sent without an
// answer; the caller must re-read quotas rather than retry.
var ErrClaudeResetUnknown = errors.New("claude reset outcome unknown")

// ClaimClaudeReset spends one reset grant of a. It re-reads the grant
// status first and refuses unless a reset is available now. It never
// retries: a lost answer returns ErrClaudeResetUnknown.
func ClaimClaudeReset(ctx context.Context, a Account) (string, error) {
	if a.Provider != "claude" || a.source == "" {
		return "", errors.New("not a Claude account")
	}
	// A claim changes the cached grant count and windows. Never restore the
	// pre-claim snapshot after a restart, even if its response is lost.
	defer os.Remove(quotaCachePath(quotaKey(a)))
	d, e := loadAccount(a)
	if e != nil {
		return "", e
	}
	c := obj(d["claudeAiOauth"])
	token := str(c["accessToken"])
	if token == "" {
		return "", errors.New("missing access token")
	}
	if exp := when(c["expiresAt"]); exp != 0 && exp <= time.Now().Unix() {
		return "", errors.New("Claude login expired; refresh quotas first")
	}
	h := bearer(token)
	h["anthropic-beta"] = "oauth-2025-04-20"
	h["User-Agent"] = "claude-cli/2.1.294 (external, cli)"
	profile, e := jsonRequest(ctx, "GET", anthropicOrigin+"/api/oauth/profile", "", h, false)
	if e != nil {
		return "", e
	}
	org := str(at(profile, "organization", "uuid"))
	if !claudeOrgID.MatchString(org) {
		return "", errors.New("Claude organization unavailable")
	}
	usage, e := jsonRequest(ctx, "GET", anthropicOrigin+claudeGrantsPath, "", h, false)
	if e != nil {
		return "", e
	}
	r, ok := parseClaudeReset(obj(usage["cedar_ember"]), time.Now().Unix())
	if !ok || r.GrantID == "" {
		return "", errors.New("no Claude reset grant")
	}
	// This read is fresh: it is the hour's check. Once a claim is sent,
	// whatever its answer, the next refresh must read the grants again.
	claudeGrants.Lock()
	claudeGrants.m[a.source] = claudeGrantsCheck{at: time.Now(), reset: cloneReset(&r), identity: fmt.Sprintf("%x", sha256.Sum256([]byte(h["Authorization"])))}
	claudeGrants.Unlock()
	if r.Blocker != "" {
		return r.Blocker, nil
	}
	defer forgetClaudeResetGrants(a.source)
	id := make([]byte, 16)
	if _, e := rand.Read(id); e != nil {
		return "", e
	}
	body, _ := json.Marshal(map[string]string{"program": "cedar_ember", "grant_id": r.GrantID, "request_id": hex.EncodeToString(id)})
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/api/organizations/%s/reset_rate_limits", anthropicOrigin, strings.ToLower(org)), strings.NewReader(string(body)))
	if e != nil {
		return "", e
	}
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, e := client.Do(req)
	if e != nil {
		return "", ErrClaudeResetUnknown
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 429:
		return "rate_limited", nil
	case 401, 403:
		return "auth_error", nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var v struct {
		Result string `json:"result"`
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && json.Unmarshal(b, &v) == nil && slices.Contains(claudeResetResults[:6], v.Result) {
		return v.Result, nil
	}
	return "", ErrClaudeResetUnknown
}
