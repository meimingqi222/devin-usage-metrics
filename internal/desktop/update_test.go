package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
