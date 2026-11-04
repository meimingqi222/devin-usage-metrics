package desktop

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
)

func settleAccountBackup(t *testing.T, a *App) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for a.busy["account-backup"] {
		a.drain()
		if time.Now().After(deadline) {
			t.Fatal("account backup worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAccountBackupExportPreviewAndImport(t *testing.T) {
	a := fixture(t)
	if err := a.accountStore.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	_, err := a.accountStore.Add("claude", map[string]any{"claudeAiOauth": map[string]any{"emailAddress": "fixture@example.test", "accessToken": "private-backup-token", "refreshToken": "private-refresh"}}, "file")
	if err != nil {
		t.Fatal(err)
	}
	a.profiles = a.accountStore.List()
	a.accountUI.manager = true
	file := filepath.Join(t.TempDir(), "test.aumaccounts")
	a.accountBackupFile = func(save bool, title string) (string, error) { return file, nil }
	tt := ui.NewTester(a.View, 1240, 780)
	click(t, tt, "Export accounts")
	click(t, tt, "Browse…")
	settleAccountBackup(t, a)
	a.backupUI.password = "fixture123"
	a.backupUI.confirm = a.backupUI.password
	tt.Frame()
	capture(t, tt, "go-accounts-export")
	click(t, tt, "Export encrypted backup")
	settleAccountBackup(t, a)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-backup-token")) || bytes.Contains(raw, []byte("fixture@example.test")) {
		t.Fatal("export is plaintext")
	}
	if a.backupUI.password != "" || a.backupUI.confirm != "" {
		t.Fatal("export retained password fields")
	}
	if err := a.accountStore.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	a.profiles = nil
	a.openAccountBackup("import")
	a.backupUI.file, a.backupUI.password = file, "fixture123"
	tt.Frame()
	click(t, tt, "Decrypt preview")
	settleAccountBackup(t, a)
	tt.Frame()
	if len(a.accountStore.List()) != 0 || a.backupUI.preview == nil || !tt.HasText("1 accounts in this backup") {
		t.Fatal("decrypt bypassed the import preview")
	}
	capture(t, tt, "go-accounts-import-preview")
	click(t, tt, "Import backup accounts")
	settleAccountBackup(t, a)
	if len(a.accountStore.List()) != 1 || a.backupUI.preview != nil || a.backupUI.open {
		t.Fatal("confirmed import did not finish")
	}
}

func TestAccountSyncSettingsLockAndEndpointChange(t *testing.T) {
	a := fixture(t)
	a.settings = true
	tt := ui.NewTester(func(c *ui.Context) { a.theme(c); a.settingsView(c); a.accountBackupDialog(c) }, 800, 780)
	if !tt.HasText("Encrypted account sync") || !tt.HasText("Off · enable separately from usage sync") {
		t.Fatal("independent opt-in account sync controls missing")
	}
	capture(t, tt, "go-account-sync-settings")
	ctx, cancel := context.WithCancel(context.Background())
	a.accountSync = accountSyncState{password: "fixture-backup-password", scope: "old-scope", cancel: cancel}
	if err := syncclient.SaveAccountSyncConfig(syncclient.AccountSyncConfig{Enabled: true, Scope: "old-scope"}); err != nil {
		t.Fatal(err)
	}
	if err := syncclient.SaveConfig(syncclient.Config{Enabled: true, APIURL: "https://old.example"}); err != nil {
		t.Fatal(err)
	}
	a.settingsUI.apiURL = "https://new.example"
	if !a.saveSettings() {
		t.Fatal("could not save new endpoint")
	}
	if ctx.Err() == nil || a.accountSync.password != "" || syncclient.ReadAccountSyncConfig().Enabled {
		t.Fatal("endpoint change retained an unlocked account vault")
	}
	a.openAccountBackup("sync")
	a.backupUI.password = "fixture-backup-password"
	a.closeAccountBackup()
	if a.backupUI.password != "" {
		t.Fatal("cancel retained draft password")
	}
}
