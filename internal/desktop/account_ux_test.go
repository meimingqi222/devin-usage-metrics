package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
)

// TestDestructiveAccountActionsAskFirst pins that removing a saved account
// and removing a tracked Devin account both confirm: the buttons sit beside
// Rename and beside the active account's status, and a stray click used to
// delete outright (and sync the removal).
func TestDestructiveAccountActionsAskFirst(t *testing.T) {
	a := accountFixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)

	click(t, tt, "Remove")
	tt.Frame()
	if a.accountUI.devinRemove != 0 || !a.accountUI.devinRemoveOpen || !tt.HasText("Remove Devin account") {
		t.Fatalf("Devin removal did not ask first: %+v %q", a.accountUI, tt.Texts())
	}
	click(t, tt, "Cancel")
	tt.Frame()
	if a.accountUI.devinRemoveOpen {
		t.Fatal("cancel left the Devin confirmation open")
	}

	a.openAccounts()
	tt.Frame()
	click(t, tt, "Remove home@example.com")
	tt.Frame()
	if a.accountUI.removeID != "home" || !tt.HasText("Remove account") {
		t.Fatalf("account removal did not ask first: %+v %q", a.accountUI, tt.Texts())
	}
	if len(a.profiles) != 2 {
		t.Fatalf("account removed before confirming: %+v", a.profiles)
	}
	capture(t, tt, "go-account-remove-confirm")
	click(t, tt, "Cancel")
	tt.Frame()
	if a.accountUI.removeID != "" || len(a.profiles) != 2 {
		t.Fatalf("cancel did not keep the account: %+v", a.accountUI)
	}
}

// TestTurningSwitchingOffWithAccountSyncAsks pins the confirmation: switching
// off also pauses account sync and may need the sync password again, which
// the bare toggle never said.
func TestTurningSwitchingOffWithAccountSyncAsks(t *testing.T) {
	a := accountFixture(t)
	if err := syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{Enabled: true, Scope: "s"}); err != nil {
		t.Fatal(err)
	}
	a.openAccounts()
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	tt.Frame()
	// The toggle sits just left of its "On" status text.
	on, ok := tt.Find("On")
	if !ok {
		t.Fatalf("switching status missing: %q", tt.Texts())
	}
	tt.ClickAt(on.X-24, on.Y+on.H/2)
	tt.Frame()
	if !a.accountUI.disableConfirm || !a.accountStore.Enabled() {
		t.Fatalf("switching turned off without asking: confirm=%v enabled=%v", a.accountUI.disableConfirm, a.accountStore.Enabled())
	}
	click(t, tt, "Turn off")
	tt.Frame()
	if a.accountStore.Enabled() {
		t.Fatal("confirming did not turn switching off")
	}
}

// TestQuotaCardKeepsAddEntryAndNamesPreview pins two quota-card cues: the
// add entry stays visible with switching off and leads to the manager, and
// previewing another account names it so its bars are not read as the live
// account's.
func TestQuotaCardKeepsAddEntryAndNamesPreview(t *testing.T) {
	a := quotaFixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	if !tt.HasText("+ Add account") {
		t.Fatalf("add entry hidden with switching off: %q", tt.Texts())
	}
	click(t, tt, "Add Claude account")
	tt.Frame()
	if !a.accountsOpen() {
		t.Fatal("add entry with switching off did not open the manager")
	}

	b := accountFixture(t)
	bt := ui.NewTester(b.View, 1240, 780)
	bt.SetScale(2)
	click(t, bt, "home@example.com")
	bt.Frame()
	if !bt.HasText("Previewing home@example.com · not in use") {
		t.Fatalf("preview banner does not name the account: %q", bt.Texts())
	}
}

func TestSwitchErrorsReadInChinese(t *testing.T) {
	a := accountFixture(t)
	a.lang = "zh"
	if got := a.switchError(errors.New("the live sign-in changed while switching; try again")); got != "切换期间 Agent 的登录发生了变化，请重试。" {
		t.Fatalf("switch error not translated: %q", got)
	}
	if got := a.switchError(errors.New("something new")); got != "something new" {
		t.Fatalf("unknown error changed: %q", got)
	}
	a.lang = "en"
	if got := a.switchError(errors.New("the live sign-in changed while switching; try again")); got != "the live sign-in changed while switching; try again" {
		t.Fatalf("English error changed: %q", got)
	}
}

// TestSkippedAccountIsNamedWhereItIsManaged pins the per-account report: a
// record a sync left out is named in the manager with what to do, and the
// sync status says how many, instead of the whole sync reading as failed.
func TestSkippedAccountIsNamedWhereItIsManaged(t *testing.T) {
	a := accountFixture(t)
	a.lang = "zh"
	if err := syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{Enabled: true, Scope: "s"}); err != nil {
		t.Fatal(err)
	}
	signInFixture(t)
	a.accountSync.password, a.accountSync.scope = "password-1234", "s"
	a.accountSync.lastAt = time.Now()
	a.accountSync.result.Total = 1
	a.accountSync.result.Skipped = []accounts.SkippedAccount{{Provider: "claude", ProfileID: "home", Label: "home@example.com", Reason: accounts.SkipMissingCopy}}
	a.openAccounts()
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	if !tt.HasText("1 个账号未同步") || !tt.HasText("本机缺少登录副本，未同步。请重新登录或移除。") {
		t.Fatalf("skipped account not reported: %q", tt.Texts())
	}
	capture(t, tt, "go-account-sync-skipped")
}

// TestRemoveOthersWaitsForTheAccountsInUse pins that "Remove other saved
// accounts" stays off until a refresh has bound the live logins; before it,
// the accounts in use are unknown and would be removed with the rest.
func TestRemoveOthersWaitsForTheAccountsInUse(t *testing.T) {
	a := accountFixture(t)
	a.screen = "history"
	a.openAccounts()
	tt := ui.NewTester(a.View, 1240, 1600)
	tt.SetScale(2)
	// The footer sits at the bottom of the manager's scroll area.
	scrollManagerToEnd(t, tt)
	if r, _ := tt.Find("Remove other saved accounts (finding the accounts in use…)"); r.W == 0 {
		t.Fatalf("remove others not on screen: %+v", r)
	}
	click(t, tt, "Remove other saved accounts (finding the accounts in use…)")
	tt.Frame()
	if a.accountUI.removeAll {
		t.Fatal("remove others opened before the accounts in use were known")
	}
	if _, err := a.accountStore.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	scrollManagerToEnd(t, tt)
	click(t, tt, "Remove other saved accounts")
	tt.Frame()
	if !a.accountUI.removeAll {
		t.Fatal("remove others stayed off after the refresh")
	}
}

// TestLockingAccountSyncKeepsDeferredQuotaRefresh pins that a quota refresh
// deferred behind an account sync still runs when the sync state resets.
func TestLockingAccountSyncKeepsDeferredQuotaRefresh(t *testing.T) {
	a := accountFixture(t)
	a.accountSync.quotaPending, a.accountSync.quotaForce = true, true
	a.lockAccountSync()
	if !a.busy["quota"] && !a.quotaUI.refreshPending {
		t.Fatal("the deferred quota refresh was dropped with the sync state")
	}
}

func scrollManagerToEnd(t *testing.T, tt *ui.Tester) {
	t.Helper()
	g, ok := tt.Find("Grok")
	if !ok {
		t.Fatalf("manager list missing: %q", tt.Texts())
	}
	tt.Wheel(g.X+g.W/2, g.Y+g.H/2, 0, 600)
	tt.Advance(2 * time.Second)
	tt.Frame()
}

// TestDangerConfirmDefaultsToCancel pins the destructive confirmation: the
// focus starts on Cancel, so Enter pressed out of habit keeps the account,
// and Escape closes it without removing anything.
func TestDangerConfirmDefaultsToCancel(t *testing.T) {
	a := accountFixture(t)
	a.screen = "history"
	a.openAccounts()
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	click(t, tt, "Remove home@example.com")
	tt.Frame()
	if !tt.Focused("Cancel") {
		t.Fatalf("the removal confirmation does not start on Cancel: %q", tt.Texts())
	}
	capture(t, tt, "go-danger-confirm")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if a.accountUI.removeID != "" || len(a.profiles) != 2 {
		t.Fatalf("Enter removed the account: %+v %+v", a.accountUI, a.profiles)
	}
	click(t, tt, "Remove home@example.com")
	tt.Frame()
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if a.accountUI.removeID != "" || len(a.profiles) != 2 {
		t.Fatalf("Escape did not cancel the removal: %+v", a.accountUI)
	}
}

// TestTrayManageAccountsOpensThePage pins the tray entry: "Manage accounts…"
// brings up the main window on the Accounts page, the one place accounts
// are managed, instead of a dialog drawn in a window that may be hidden.
func TestTrayManageAccountsOpensThePage(t *testing.T) {
	a := accountFixture(t)
	a.accounts = []accountResult{{account: quota.Account{Key: "claude", Provider: "claude", Label: "work@example.com"}, result: quota.Result{Plan: "Max", Windows: []quota.Window{{Label: "5h", UsedPercent: 20, ResetsAt: time.Now().Add(time.Hour).Unix()}}}}}
	shown := false
	a.trayUI.openDashboard = func() { shown = true }
	tt := ui.NewTester(a.trayView, trayWidth, 520)
	tt.SetScale(2)
	click(t, tt, "Account switcher")
	tt.Frame()
	click(t, tt, "Manage accounts…")
	tt.Frame()
	if !shown || !a.accountsOpen() {
		t.Fatalf("tray did not open the Accounts page: shown=%v screen=%q", shown, a.screen)
	}
	main := ui.NewTester(a.View, 1240, 900)
	main.SetScale(2)
	if !main.HasText("Switches the agent's own sign-in.") || !main.HasText("home@example.com") {
		t.Fatalf("accounts page missing: %q", main.Texts())
	}
	capture(t, main, "go-accounts-page")
}

// signInFixture writes the GitHub session marker the sync UI reads as
// "signed in", with an endpoint configured.
func signInFixture(t *testing.T) {
	t.Helper()
	dir, _ := os.UserConfigDir()
	dir = filepath.Join(dir, "devin-usage-metrics")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "github-sync-session.meta.json"), []byte(`{"login":"fixture-user"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncclient.SaveConfig(syncclient.Config{Enabled: true, APIURL: "https://sync.example.test"}); err != nil {
		t.Fatal(err)
	}
}

// TestTruncatedAccountNameShowsInFull pins the hover tooltip on account
// names cut with an ellipsis: the card header has room for only part of a
// long address.
func TestTruncatedAccountNameShowsInFull(t *testing.T) {
	a := quotaFixture(t)
	tt := ui.NewTester(a.View, 1240, 1000)
	tt.SetScale(2)
	const long = "a-very-long-account-label-that-must-ellipsize@example.com"
	count := func() int {
		n := 0
		for _, s := range tt.Texts() {
			if s == long {
				n++
			}
		}
		return n
	}
	before := count()
	r, ok := tt.Find(long)
	if !ok {
		t.Fatalf("long label missing: %q", tt.Texts())
	}
	tt.Move(r.X+r.W/2, r.Y+r.H/2)
	tt.Advance(2 * time.Second)
	tt.Frame()
	if count() <= before {
		t.Fatalf("hovering a truncated name showed no tooltip: %q", tt.Texts())
	}
	capture(t, tt, "go-account-name-tooltip")
}
