package desktop

import (
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
)

func TestSettingsTabsAndDraftCancel(t *testing.T) {
	a := fixture(t)
	if err := syncclient.SaveConfig(syncclient.Config{APIURL: "https://saved.example"}); err != nil {
		t.Fatal(err)
	}
	a.settings = true
	tt := ui.NewTester(a.settingsView, 800, 700)
	if !tt.HasText("Self-hosted sync API") || tt.HasText("Add Devin account") {
		t.Fatalf("incorrect settings content: %v", tt.Texts())
	}
	a.settingsUI.apiURL = "https://discard.example"
	click(t, tt, "General")
	if !tt.HasText("About this app") || !tt.HasText("简体中文") {
		t.Fatalf("general content missing: %v", tt.Texts())
	}
	click(t, tt, "Cancel")
	if a.settings || syncclient.ReadConfig().APIURL != "https://saved.example" {
		t.Fatal("Cancel persisted the URL draft")
	}
}

func TestSettingsSaveNormalizesEndpointAndDisablesSync(t *testing.T) {
	a := fixture(t)
	if err := syncclient.SaveConfig(syncclient.Config{Enabled: true, APIURL: "https://old.example"}); err != nil {
		t.Fatal(err)
	}
	a.settings = true
	tt := ui.NewTester(a.settingsView, 800, 700)
	a.settingsUI.apiURL = "  https://new.example///  "
	click(t, tt, "Save")
	cfg := syncclient.ReadConfig()
	if a.settings || cfg.Enabled || cfg.APIURL != "https://new.example" {
		t.Fatalf("unsafe or unsaved endpoint: %+v", cfg)
	}
}

func TestSettingsSourceLabelsAndControlGeometry(t *testing.T) {
	a := fixture(t)
	a.lang = "zh"
	a.settings = true
	tt := ui.NewTester(func(c *ui.Context) {
		a.theme(c)
		a.settingsView(c)
	}, 800, 700)
	for _, label := range []string{"多设备同步", "常规偏好", "取消", "保存"} {
		r, ok := tt.Find(label)
		if !ok || r.H != 20 {
			t.Fatalf("%s: expected 20px text line, got %+v (found %v)", label, r, ok)
		}
	}
	r, ok := tt.Find("同步 API 地址")
	if !ok || r.W != 346 || r.H != 20 {
		t.Fatalf("expected 346px content width inside 380px panel: %+v (found %v)", r, ok)
	}
	click(t, tt, "常规偏好")
	if !tt.HasText("关于本应用") {
		t.Fatal("Chinese general tab did not open")
	}
}
