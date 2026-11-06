package desktop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/accounts"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncdiag"
)

func diagnosticRecords(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(syncdiag.Path())
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestAccountDiagnosticsExplainUnboundDuplicateWithoutSecrets(t *testing.T) {
	a := rosterFixture(t)
	email, secret := "private@example.com", "never-log-this-token"
	a.profiles = []accounts.Profile{
		{ID: "profile-one", Provider: "antigravity", Label: email, Email: email, StableID: "private-stable-identity", Credentials: secret},
		{ID: "profile-two", Provider: "antigravity", Label: "second@example.com"},
		{ID: "profile-three", Provider: "antigravity", Label: "third@example.com"},
	}
	a.accounts = []accountResult{{account: quota.Account{Key: "private-live-source", Provider: "antigravity", Label: email}, isLive: true}}
	for _, p := range a.profiles {
		a.accounts = append(a.accounts, accountResult{account: quota.Account{Key: "profile-" + p.ID, Provider: p.Provider, Label: p.Label}, profileID: p.ID, err: errors.New(secret)})
	}
	a.recordAccountDiagnostics()
	records := diagnosticRecords(t)
	var run any
	for _, r := range records {
		if r["event"] == "account_snapshot" {
			run = r["run"]
		}
	}
	if run == nil {
		t.Fatal("snapshot has no run ID")
	}
	found, anomaly, unmatched := false, false, false
	for _, r := range records {
		if r["operation"] != "account_snapshot" {
			continue
		}
		if r["run"] != run {
			t.Fatal("snapshot lost run correlation")
		}
		switch r["event"] {
		case "quota_roster":
			found = r["saved"] == float64(3) && r["targets"] == float64(4) && r["unmatched_live"] == float64(1) && r["duplicate_labels"] == float64(1) && r["missing_profiles"] == float64(0)
		case "quota_roster_anomaly":
			anomaly = true
		case "quota_target":
			if r["binding"] == "unmatched_live" {
				unmatched = r["profile"] == "" && r["label_ref"] == accountNameRef(email)
			}
		}
	}
	if !found || !anomaly || !unmatched {
		t.Fatal("log cannot explain three saved profiles plus an unmatched duplicate live label")
	}
	raw, _ := os.ReadFile(syncdiag.Path())
	for _, sensitive := range []string{email, secret, "private-stable-identity", "profile-one", "private-live-source", "second@example.com"} {
		if bytes.Contains(raw, []byte(sensitive)) {
			t.Fatal("diagnostic contains private account data")
		}
	}
	a.openAccounts()
	tt := ui.NewTester(a.View, 1240, 780)
	if !tt.HasText("Diagnostic log") {
		t.Fatal("account manager lacks diagnostics entry")
	}
	capture(t, tt, "go-account-diagnostics")
}

func TestAccountDiagnosticsBoundDetails(t *testing.T) {
	a := rosterFixture(t)
	for i := 0; i < 80; i++ {
		a.profiles = append(a.profiles, accounts.Profile{ID: fmt.Sprint(i), Provider: "claude"})
		a.accounts = append(a.accounts, accountResult{account: quota.Account{Provider: "claude"}, profileID: fmt.Sprint(i)})
	}
	a.recordAccountDiagnostics()
	profiles, targets, summary := 0, 0, false
	for _, r := range diagnosticRecords(t) {
		if r["operation"] != "account_snapshot" {
			continue
		}
		switch r["event"] {
		case "account_profile":
			profiles++
		case "quota_target":
			targets++
		case "quota_roster":
			summary = r["profiles_truncated"] == float64(16) && r["targets"] == float64(80)
		}
	}
	if profiles != 64 || targets != 64 || !summary {
		t.Fatal("detail cap hid totals or failed to bound logging")
	}
}

func TestQuotaDiagnosticsRecordDiscardedRefresh(t *testing.T) {
	a := rosterFixture(t)
	p := rosterProfile(t, a, "removed@example.com", "one")
	if err := os.Remove(a.accountStore.CredentialPath("claude", p.ID)); err != nil {
		t.Fatal(err)
	}
	a.refreshQuota(false)
	if err := a.accountStore.Remove("claude", p.ID); err != nil {
		t.Fatal(err)
	}
	a.profiles = a.accountStore.List()
	a.accountChanged()
	waitQuota(t, a)
	started, discarded, finished := false, false, false
	for _, r := range diagnosticRecords(t) {
		switch r["event"] {
		case "quota_refresh_start":
			started = true
		case "quota_snapshot_discarded":
			discarded = true
		case "quota_refresh_end":
			finished = true
		}
	}
	if !started || !discarded || !finished {
		t.Fatal("refresh diagnostics lost stale-result decision")
	}
}
