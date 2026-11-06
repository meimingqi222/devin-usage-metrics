package desktop

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

func rosterProfile(t *testing.T, a *App, email, org string) accounts.Profile {
	t.Helper()
	p, err := a.accountStore.Add("claude", map[string]any{
		"claudeAiOauth":    map[string]any{"emailAddress": email, "accessToken": "test-" + org, "refreshToken": "test-refresh-" + org},
		"organizationUuid": org,
	}, "file")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func waitQuota(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for a.busy["quota"] && time.Now().Before(deadline) {
		a.drain()
		time.Sleep(time.Millisecond)
	}
	if a.busy["quota"] {
		t.Fatal("quota refresh did not finish")
	}
}

func rosterFixture(t *testing.T) *App {
	a := fixture(t)
	if err := a.accountStore.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestQuotaRosterKeepsEverySavedProfile(t *testing.T) {
	a := rosterFixture(t)
	live := rosterProfile(t, a, "same@example.com", "one")
	rosterProfile(t, a, "same@example.com", "two")
	missing := rosterProfile(t, a, "missing@example.com", "three")
	body, err := os.ReadFile(a.accountStore.CredentialPath("claude", live.ID))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("HOME"), ".claude", ".credentials.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.accountStore.CredentialPath("claude", missing.ID)); err != nil {
		t.Fatal(err)
	}
	refreshRosterFixture(t, a)
	if len(a.accounts) != len(a.profiles) || len(a.accounts) != 3 {
		t.Fatalf("quota has %d accounts, manager has %d; want all 3", len(a.accounts), len(a.profiles))
	}
	seen := map[string]bool{}
	for _, ar := range a.accounts {
		if seen[ar.profileID] {
			t.Fatal("profile appears twice")
		}
		seen[ar.profileID] = true
		if ar.isLive != (ar.profileID == live.ID) {
			t.Fatal("live account bound to wrong profile")
		}
		for _, p := range a.profiles {
			if p.ID == ar.profileID && ar.account.Label != a.profileName(p.Provider, p) {
				t.Fatal("quota and manager names differ")
			}
		}
		if ar.profileID == missing.ID && ar.err == nil {
			t.Fatal("missing credential should be visible with an error")
		}
	}
}

func TestAccountChangesUpdateQuotaRosterImmediately(t *testing.T) {
	a := rosterFixture(t)
	rosterProfile(t, a, "first@example.com", "one")
	refreshRosterFixture(t, a)
	p := rosterProfile(t, a, "second@example.com", "two")
	a.profiles = a.accountStore.List()
	a.busy["quota"] = true // Keep requests pending; no network is needed for roster edits.
	a.accountChanged()
	if len(a.accounts) != 2 {
		t.Fatalf("new account absent: quota has %d, manager has 2", len(a.accounts))
	}
	if err := a.accountStore.Rename("claude", p.ID, "Renamed account"); err != nil {
		t.Fatal(err)
	}
	a.profiles = a.accountStore.List()
	a.accountChanged()
	found := false
	for _, ar := range a.accounts {
		found = found || ar.account.Label == "Renamed account"
	}
	if !found {
		t.Fatal("quota retained old name")
	}
	if err := a.accountStore.Remove("claude", p.ID); err != nil {
		t.Fatal(err)
	}
	a.profiles = a.accountStore.List()
	a.accountChanged()
	if len(a.accounts) != 1 {
		t.Fatal("removed account still visible")
	}
	if !a.quotaUI.refreshPending {
		t.Fatal("edit during refresh must queue a new refresh")
	}
}

func TestQuotaActiveBadgeUsesProfileID(t *testing.T) {
	a := rosterFixture(t)
	a.profiles = []accounts.Profile{
		{ID: "active", Provider: "antigravity", Email: "same@example.com", Label: "same@example.com", Active: true},
		{ID: "other", Provider: "antigravity", Email: "same@example.com", Label: "same@example.com"},
	}
	ar := accountResult{account: quota.Account{Provider: "antigravity", Label: "same@example.com"}, profileID: "other"}
	if a.isAccountLive("antigravity", ar) {
		t.Fatal("same email marked a different saved profile active")
	}
}

func TestAntigravityRosterMatchesManager(t *testing.T) {
	a := rosterFixture(t)
	a.lang, a.screen = "zh", "quota"
	for _, email := range []string{"first@example.com", "second@example.com", "third@example.com", "fourth@example.com"} {
		if _, err := a.accountStore.Add("antigravity", map[string]any{"email": email, "access_token": "test-" + email, "refresh_token": "test-refresh-" + email}, "file"); err != nil {
			t.Fatal(err)
		}
	}
	refreshRosterFixture(t, a)
	first := a.profiles[0]
	live := quota.ProfileAccount("antigravity-live", first.Provider, first.Label, a.accountStore.CredentialPath(first.Provider, first.ID))
	profiles, err := a.accountStore.Reconcile([]quota.Account{live})
	if err != nil {
		t.Fatal(err)
	}
	a.profiles = profiles
	a.accounts = quotaRoster(a.accountStore, profiles, []quota.Account{live})
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	if !tt.HasText("4 个已绑定账号") {
		t.Fatalf("incorrect count: %q", tt.Texts())
	}
	if !tt.HasText("4 个账号 · 1 个当前使用") || !tt.HasText("当前使用") {
		t.Fatal("summary and live badge do not match the roster")
	}
	for _, p := range a.profiles {
		if !tt.HasText(a.profileName(p.Provider, p)) {
			t.Fatal("quota omitted saved account")
		}
	}
	capture(t, tt, "go-antigravity-four-accounts")
}

// Exercise discovery and reconciliation without querying provider endpoints.
func refreshRosterFixture(t *testing.T, a *App) {
	t.Helper()
	live := quota.Discover(false)
	profiles, err := a.accountStore.Reconcile(live)
	if err != nil {
		t.Fatal(err)
	}
	a.profiles = profiles
	a.accounts = quotaRoster(a.accountStore, profiles, live)
}

func TestQuotaRefreshCannotRestoreRemovedProfile(t *testing.T) {
	a := rosterFixture(t)
	p := rosterProfile(t, a, "removed@example.com", "one")
	if err := os.Remove(a.accountStore.CredentialPath("claude", p.ID)); err != nil {
		t.Fatal(err)
	}
	// Missing credentials produce a local error, with no provider request.
	a.refreshQuota(false)
	if err := a.accountStore.Remove("claude", p.ID); err != nil {
		t.Fatal(err)
	}
	a.profiles = a.accountStore.List()
	a.accountChanged()
	waitQuota(t, a)
	if len(a.accounts) != 0 || len(a.profiles) != 0 {
		t.Fatal("in-flight refresh restored removed profile")
	}
	if a.quotaUI.refreshPending {
		t.Fatal("queued refresh was not drained")
	}
}

func TestQuotaRosterCacheDoesNotCrossAccounts(t *testing.T) {
	old := accountResult{account: quota.Account{Key: "live", Label: "Old"}, profileID: "one", result: quota.Result{Plan: "old-plan"}}
	existing := map[string]accountResult{"live": old}
	target := old
	target.account.Label = "Renamed"
	target.result = quota.Result{}
	got := cachedQuotaRoster([]accountResult{target}, existing)[0]
	if got.account.Label != "Renamed" || got.result.Plan != "old-plan" {
		t.Fatal("rename must preserve quota but update its label")
	}
	target.profileID = "two"
	got = cachedQuotaRoster([]accountResult{target}, existing)[0]
	if got.result.Plan != "" {
		t.Fatal("switch showed previous account quota")
	}
}

func TestSavedQuotaPreviewIsNotALiveSwitchDestination(t *testing.T) {
	a := rosterFixture(t)
	rosterProfile(t, a, "backup@example.com", "one")
	refreshRosterFixture(t, a)
	if _, ok := a.liveAccount("claude"); ok {
		t.Fatal("saved quota preview must not become the live credential destination")
	}
}
