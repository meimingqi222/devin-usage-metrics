package desktop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

func accountFixture(t *testing.T) *App {
	a := quotaFixture(t)
	if err := a.accountStore.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	a.profiles = []accounts.Profile{
		{ID: "work", Provider: "claude", Label: "work@example.com", Kind: "file", Active: true, Writable: true, LastUsedAt: time.Now().Unix()},
		{ID: "home", Provider: "claude", Label: "home@example.com", Plan: "Pro", Kind: "file", Writable: true, LastUsedAt: time.Now().Add(-48 * time.Hour).Unix()},
	}
	for i := range a.accounts {
		switch a.accounts[i].account.Key {
		case "claude-1":
			a.accounts[i].profileID, a.accounts[i].isLive = "work", true
		case "claude-2":
			a.accounts[i].profileID = "home"
		}
	}
	return a
}

func TestQuotaCardAccountSwitcher(t *testing.T) {
	a := accountFixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	if !tt.HasText("work@example.com") {
		t.Fatalf("active account missing: %q", tt.Texts())
	}
	// Multi-account cards show tabs for each account
	if !tt.HasText("home@example.com") {
		t.Fatalf("inactive account tab missing: %q", tt.Texts())
	}
	// Click inactive tab to preview its quota without switching credentials
	if err := tt.Click("home@example.com"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("切为此账号") && !tt.HasText("Switch to this account") {
		t.Fatalf("switch button missing: %q", tt.Texts())
	}
}

func TestAccountsManagerDialog(t *testing.T) {
	a := accountFixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	click(t, tt, "Accounts")
	tt.Frame()
	for _, text := range []string{"Account switching", "Claude Code", "work@example.com", "home@example.com", "Add account", "Remove other saved accounts"} {
		if !tt.HasText(text) {
			t.Fatalf("accounts manager missing %q: %q", text, tt.Texts())
		}
	}
	click(t, tt, "+ Add account")
	tt.Frame()
	for _, text := range []string{"Sign in with Claude Code", "Accounts already signed in on this device are added automatically."} {
		if !tt.HasText(text) {
			t.Fatalf("add account panel missing %q: %q", text, tt.Texts())
		}
	}
	if tt.HasText("Capture current login") {
		t.Fatalf("manual capture is back in the add panel: %q", tt.Texts())
	}
	capture(t, tt, "go-accounts-dialog")
}

func TestDedicatedAddBackupModalAndCleanManager(t *testing.T) {
	a := accountFixture(t)
	a.lang = "zh"
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)

	// 1. Click + 备用号 on the Antigravity card
	a.openAddAccount("antigravity")
	tt.Frame()

	for _, text := range []string{"添加 Antigravity 备用号", "使用 Google 登录", "本机已登录的账号会被自动识别并加入，无需手动捕获。"} {
		if !tt.HasText(text) {
			t.Fatalf("dedicated add modal missing %q: %q", text, tt.Texts())
		}
	}
	capture(t, tt, "go-backup-modal-zh")

	// 2. Devin does not support switching, so it must not open an add modal.
	a.accountUI.add = ""
	a.openAddAccount("devin")
	tt.Frame()
	if tt.HasText("添加 Devin 备用号") {
		t.Fatalf("Devin add modal must not open: %q", tt.Texts())
	}

	// 3. Open Accounts manager in Chinese
	a.accountUI.add = ""
	a.openAccounts()
	tt.Frame()
	for _, text := range []string{"账号", "账号切换", "Claude Code", "Codex", "work@example.com", "当前使用", "home@example.com", "PRO · 备用副本", "切为此账号"} {
		if !tt.HasText(text) {
			t.Fatalf("cleaned accounts manager missing %q: %q", text, tt.Texts())
		}
	}
	capture(t, tt, "go-accounts-manager-zh")
}

func TestTrayAccountSwitcher(t *testing.T) {
	a := accountFixture(t)
	a.accounts = []accountResult{{account: quota.Account{Key: "claude", Provider: "claude", Label: "work@example.com"}, result: quota.Result{Plan: "Max", Windows: []quota.Window{{Label: "5h", UsedPercent: 20, ResetsAt: time.Now().Add(time.Hour).Unix()}}}}}
	tt := ui.NewTester(a.trayView, trayWidth, 520)
	tt.SetScale(2)
	if err := tt.Click("Account switcher"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("home@example.com") || !tt.HasText("Manage accounts…") {
		t.Fatalf("tray account menu missing: %q", tt.Texts())
	}
	capture(t, tt, "go-tray-account-menu")
}

func TestAccountSwitchFromMenuWritesLive(t *testing.T) {
	a := fixture(t)
	home := os.Getenv("HOME")
	path := filepath.Join(home, ".claude", ".credentials.json")
	write := func(body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	doc := func(email, org, access, refresh string) string {
		b, _ := json.Marshal(map[string]any{
			"claudeAiOauth":    map[string]any{"emailAddress": email, "accessToken": access, "refreshToken": refresh},
			"organizationUuid": org,
		})
		return string(b)
	}
	write(doc("work@example.com", "org-1", "a1", "r1"))
	if err := a.accountStore.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.accountStore.Reconcile(quota.Discover(false)); err != nil {
		t.Fatal(err)
	}
	write(doc("home@example.com", "org-2", "a2", "r2"))
	profiles, err := a.accountStore.Reconcile(quota.Discover(false))
	if err != nil {
		t.Fatal(err)
	}
	var work accounts.Profile
	var liveAccount quota.Account
	for _, p := range profiles {
		if p.Email == "work@example.com" {
			work = p
		}
	}
	for _, l := range quota.Discover(false) {
		if l.Provider == "claude" {
			liveAccount = l
		}
	}
	a.profiles = profiles
	a.screen = "quota"
	a.accounts = []accountResult{{account: liveAccount, result: quota.Result{Plan: "Max"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel() // quota refresh after the switch must not reach the network
	a.ctx = ctx

	tt := ui.NewTester(a.View, 1240, 780)
	if err := tt.Click("Account switcher"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("work@example.com"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for (a.accountUI.switching["claude"] || a.busy["switch/claude"]) && time.Now().Before(deadline) {
		tt.Frame()
		time.Sleep(10 * time.Millisecond)
	}
	if a.accountUI.switching["claude"] {
		t.Fatalf("switch did not finish: busy=%v msg=%+v", a.busy, a.accountUI.message)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"refreshToken":"r1"`) && !strings.Contains(string(b), `"refreshToken": "r1"`) {
		t.Fatalf("live sign-in was not switched: %s", b)
	}
	if work.ID == "" {
		t.Fatal("work profile missing")
	}
}

func TestDuplicateAccountNamesGetShortSuffix(t *testing.T) {
	a := accountFixture(t)
	a.profiles = append(a.profiles,
		accounts.Profile{ID: "aaaa1111", Provider: "antigravity", Label: "Antigravity", Kind: "keychain", Writable: true},
		accounts.Profile{ID: "bbbb2222", Provider: "antigravity", Label: "Antigravity", Kind: "keychain", Writable: true},
	)
	a.accountUI.manager = true
	tt := ui.NewTester(a.View, 1240, 780)
	if !tt.HasText("Antigravity (aaaa)") || !tt.HasText("Antigravity (bbbb)") {
		t.Fatalf("duplicate names not distinguished: %q", tt.Texts())
	}
}

func TestParallelQuotaDashboardAndSwitch(t *testing.T) {
	a := accountFixture(t)
	a.lang = "zh"
	a.screen = "quota"
	a.accounts = []accountResult{
		{
			account:   quota.Account{Key: "claude-live", Provider: "claude", Label: "work@example.com"},
			result:    quota.Result{Plan: "Max", Windows: []quota.Window{{Label: "5h", UsedPercent: 20}}},
			isLive:    true,
			profileID: "work",
		},
		{
			account:   quota.Account{Key: "profile-home", Provider: "claude", Label: "home@example.com"},
			result:    quota.Result{Plan: "Pro", Windows: []quota.Window{{Label: "5h", UsedPercent: 80}}},
			isLive:    false,
			profileID: "home",
		},
	}
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)

	// Both accounts are visible simultaneously on the dashboard tabs
	if !tt.HasText("work@example.com") || !tt.HasText("home@example.com") {
		t.Fatalf("both accounts should be visible in parallel tabs: %q", tt.Texts())
	}
	// Active account has active indicator
	if !tt.HasText("当前使用") {
		t.Fatalf("active indicator missing: %q", tt.Texts())
	}
	// Switch tab to inactive account to preview its quota without switching credentials
	if err := tt.Click("home@example.com"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	// Inactive account preview shows direct switch button
	if !tt.HasText("切为此账号") {
		t.Fatalf("switch button on inactive preview missing: %q", tt.Texts())
	}

	// Test Tray also reflects both accounts
	trayTester := ui.NewTester(a.trayView, trayWidth, 520)
	trayTester.SetScale(2)
	if !trayTester.HasText("work@example.com") || !trayTester.HasText("home@example.com") {
		t.Fatalf("tray should show both accounts: %q", trayTester.Texts())
	}
	if !trayTester.HasText("当前") {
		t.Fatalf("tray active tag missing: %q", trayTester.Texts())
	}
	if !trayTester.HasText("切换") {
		t.Fatalf("tray switch button missing: %q", trayTester.Texts())
	}
}
