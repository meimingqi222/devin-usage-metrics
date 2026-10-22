package desktop

import (
	"errors"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

func quotaFixture(t *testing.T) *App {
	a := fixture(t)
	a.screen = "quota"
	now := time.Now()
	h := func(d time.Duration) int64 { return now.Add(d).Unix() }
	a.accounts = []accountResult{
		{account: quota.Account{Key: "claude-1", Provider: "claude", Label: "work@example.com"}, result: quota.Result{Plan: "Max", Windows: []quota.Window{{Label: "5h", UsedPercent: 37.5, ResetsAt: h(2 * time.Hour)}, {Label: "7d", UsedPercent: 82, ResetsAt: h(75 * time.Hour)}, {Label: "7d Opus", UsedPercent: 12, ResetsAt: h(75 * time.Hour)}}, Reset: &quota.ClaudeReset{Left: 2, Total: 3, EndsAt: h(24 * 24 * time.Hour), GrantID: "g", Clears: []string{"five_hour"}}}},
		{account: quota.Account{Key: "codex-1", Provider: "codex", Label: "a-very-long-account-label-that-must-ellipsize@example.com"}, result: quota.Result{Plan: "Plus", Windows: []quota.Window{{Label: "5h", UsedPercent: 5, ResetsAt: h(30 * time.Minute)}, {Label: "7d", UsedPercent: 64, ResetsAt: h(100 * time.Hour)}}}},
		{account: quota.Account{Key: "devin-0", Provider: "devin", Label: "team"}, result: quota.Result{Plan: "Teams", Extra: "120 ACUs left", Windows: []quota.Window{{Label: "Monthly", UsedPercent: 48, ResetsAt: h(240 * time.Hour)}}}},
		{account: quota.Account{Key: "grok-1", Provider: "grok", Label: "me"}, err: errors.New("HTTP 401")},
		{account: quota.Account{Key: "ag-1", Provider: "antigravity", Label: "g@example.com"}, result: quota.Result{Windows: []quota.Window{{Label: "Gemini 3 Pro – 5h", UsedPercent: 90, ResetsAt: h(time.Hour)}, {Label: "Claude Sonnet – 5h", UsedPercent: 0, ResetsAt: h(time.Hour)}}}},
		{account: quota.Account{Key: "devin-cli", Provider: "devin", Label: "cli"}, result: quota.Result{}},
		{account: quota.Account{Key: "claude-2", Provider: "claude", Label: "home@example.com"}, result: quota.Result{Windows: []quota.Window{{Label: "5h", UsedPercent: 100, ResetsAt: h(10 * time.Minute)}}, Stale: "HTTP 429", StaleAge: 300, RetryIn: 120, Reset: &quota.ClaudeReset{Left: 1, Total: 1, EndsAt: h(26*time.Hour + 30*time.Second), GrantID: "g", Blocker: "cooldown", CooldownUntil: h(30 * time.Minute)}}},
	}
	return a
}

func TestQuotaCardsFormAUniformGrid(t *testing.T) {
	a := quotaFixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	capture(t, tt, "go-quota-cards")
	rect := func(label string) ui.Rect {
		t.Helper()
		r, ok := tt.Find(label)
		if !ok {
			t.Fatalf("missing card %q", label)
		}
		return r
	}
	// Two columns of one width; cards in a row share its height even when
	// their content differs, and the next row starts below both.
	claude, codex, devin := rect("Claude · work@example.com"), rect("Codex · a-very-long-account-label-that-must-ellipsize@example.com"), rect("Devin · team")
	if codex.Y != claude.Y || codex.H != claude.H || codex.W != claude.W || codex.X <= claude.X {
		t.Fatalf("cards in one row differ: %+v vs %+v", codex, claude)
	}
	if devin.W != claude.W || devin.X != claude.X || devin.Y < claude.Y+claude.H {
		t.Fatalf("second row breaks the grid: %+v under %+v", devin, claude)
	}
	// The odd card out keeps the column width instead of filling the row.
	if last := rect("Claude · home@example.com"); last.W != claude.W {
		t.Fatalf("last card left the grid: %+v vs %+v", last, claude)
	}
	if !tt.HasText("7 accounts · 3 high-usage alerts") {
		t.Fatalf("missing summary: %q", tt.Texts())
	}
	if label, ok := tt.Find("a-very-long-account-label-that-must-ellipsize@example.com"); ok && label.X+label.W > codex.X+codex.W {
		t.Fatalf("account label overflows its card: %+v in %+v", label, codex)
	}
	for _, text := range []string{"5-hour window", "7-day · Opus", "Gemini 3 Pro · 5-hour window", "resets in 3d 3h", "Query failed: HTTP 401", "No record", "Extra: 120 ACUs left", "Cached data (5m ago) - rate limited, retry in 2m"} {
		if !tt.HasText(text) {
			t.Fatalf("missing %q: %q", text, tt.Texts())
		}
	}
	if remove, ok := tt.Find("Remove"); !ok || remove.H > 24 {
		t.Fatalf("remove must be a compact link, not a button: %+v", remove)
	}
}

// Claude reset grants show their count and expiry; the reset button asks
// before spending one, and a blocked grant shows why instead.
func TestClaudeResetGrantNeedsConfirmation(t *testing.T) {
	a := quotaFixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	// The expiry shows its hour and minute, since a grant lapses at an instant.
	expires := time.Unix(a.accounts[0].result.Reset.EndsAt, 0).Format("01-02 15:04")
	if !tt.HasText("Reset grants: 2 of 3 left · expires " + expires) {
		t.Fatalf("missing grant summary: %q", tt.Texts())
	}
	tt.Scroll(600, 400, 0, 400)
	soon := time.Unix(a.accounts[6].result.Reset.EndsAt, 0).Format("01-02 15:04")
	if !tt.HasText("Reset grants: 1 of 1 left · expires " + soon + " (in 1d 2h)") {
		t.Fatalf("missing blocked grant: %q", tt.Texts())
	}
	tt.Scroll(600, 400, 0, -400)
	click(t, tt, "Reset 5h")
	if !tt.HasText("Spend one Claude reset grant for work@example.com? This clears its usage window now and cannot be undone.") {
		t.Fatalf("reset did not ask first: %q", tt.Texts())
	}
	capture(t, tt, "go-claude-reset-confirm")
	click(t, tt, "Cancel")
	if a.resetAccount != nil || a.busy["claude-reset"] {
		t.Fatal("cancelling still spent a reset")
	}
}
