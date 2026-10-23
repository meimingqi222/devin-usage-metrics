package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/updater"
)

func TestUpdatePrefsShareRustConfig(t *testing.T) {
	a := fixture(t)
	if !a.updatePrefs.AutoCheck {
		t.Fatal("automatic checks must default on, as in Rust")
	}
	path := preferencesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	rust := `{"lang":"zh","auto_check_updates":false,"skipped_update_version":"0.3.0","last_update_check_at":1700000000,"sessions_group_by_project":true}`
	if err := os.WriteFile(path, []byte(rust), 0600); err != nil {
		t.Fatal(err)
	}
	p := loadUpdatePrefs()
	if p.AutoCheck || p.Skipped != "0.3.0" || p.LastCheckAt != 1700000000 {
		t.Fatalf("Rust update prefs not read: %+v", p)
	}
	a.updatePrefs = p
	a.release = updater.Release{Version: "0.3.1"}
	a.skipUpdate()
	var cfg map[string]any
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &cfg); err != nil || cfg["skipped_update_version"] != "0.3.1" || cfg["lang"] != "zh" || cfg["auto_check_updates"] != false {
		t.Fatalf("skip must persist without dropping other settings: %s", b)
	}
	if a.release.Version != "" {
		t.Fatal("skipped release still offered")
	}
}

func TestAutomaticUpdateCheckNeedsPackagedInstall(t *testing.T) {
	a := fixture(t)
	a.checkUpdates(false)
	if a.busy["update"] {
		t.Fatal("development build started an automatic update check")
	}
}

// Install errors stay in the dialog: the ready message used to replace them,
// so a failed install on Windows looked like a button that did nothing.
func TestFailedInstallShowsItsError(t *testing.T) {
	a := fixture(t)
	quit := false
	a.quit = func() { quit = true }
	a.release = updater.Release{Version: "0.3.1"}
	a.payload = filepath.Join(t.TempDir(), "devin-usage-metrics.exe")
	a.settingsUI.updateOpen = true
	tt := ui.NewTester(a.updateDialog, 800, 600)
	click(t, tt, "Install and restart")
	if quit || a.settingsUI.updateMessage == "" || !tt.HasText(a.settingsUI.updateMessage) || tt.HasText("Update ready. Restart to install.") || !tt.HasText("Update failed") || !tt.HasText("Retry") {
		t.Fatalf("install error hidden: quit=%v message=%q texts=%v", quit, a.settingsUI.updateMessage, tt.Texts())
	}
	capture(t, tt, "update-install-failed")
}

// With nothing offered, the subtitle shows the installed version, not a bare "v".
func TestUpToDateDialogShowsCurrentVersion(t *testing.T) {
	a := fixture(t)
	a.settingsUI.updateOpen = true
	a.settingsUI.updateMessage = "Already up to date"
	tt := ui.NewTester(a.updateDialog, 800, 600)
	if !tt.HasText("v"+updater.CurrentVersion) || !tt.HasText("Later") || !tt.HasText("Check for updates") {
		t.Fatalf("up-to-date dialog: %v", tt.Texts())
	}
	capture(t, tt, "update-up-to-date")
}
