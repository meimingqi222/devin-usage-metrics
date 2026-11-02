package desktop

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func TestAntigravityCLISourceTag(t *testing.T) {
	a := fixture(t)
	a.agent = model.Antigravity
	a.storeLoaded(model.Antigravity, model.Data{Sessions: []model.Session{{Agent: model.Antigravity, Key: "antigravity-cli/x", ID: "x", Source: "cli", Title: "Terminal session", DeviceID: a.localID, DeviceName: "Workstation", LastActivityAt: time.Now().Unix()}}})
	tt := ui.NewTester(func(c *ui.Context) {
		a.theme(c)
		ui.Column(c).Fill().Padding(16).Children(func() { a.sessionsView(c) })
	}, 1080, 732)
	if !tt.HasText("Terminal session") {
		t.Fatalf("missing session row: %v", tt.Texts())
	}
	for _, text := range tt.Texts() {
		if text == "CLI" {
			return
		}
	}
	t.Fatalf("missing CLI source tag: %v", tt.Texts())
}

func TestSessionsOriginalControlsAndEmptyStates(t *testing.T) {
	a := fixture(t)
	a.agent = model.Claude
	a.remote = model.Data{}
	view := func(c *ui.Context) {
		a.theme(c)
		ui.Column(c).Fill().Padding(16).Gap(12).Children(func() { a.sessionsView(c) })
	}
	tt := ui.NewTester(view, 1080, 732)
	for _, label := range []string{"ID", "Hide", "Resume", "Hide Reviews", "Group by Project"} {
		if !tt.HasText(label) {
			t.Fatalf("missing original control %q: %v", label, tt.Texts())
		}
	}
	search, ok := tt.Find("Search sessions")
	if !ok || search.W != 240 || search.H != 28 {
		t.Fatalf("search geometry: %+v", search)
	}
	click(t, tt, "Hide Reviews")
	if !tt.HasText("Show Reviews") {
		t.Fatal("reviews toggle did not update label")
	}
	click(t, tt, "Hide")
	if !tt.HasText("No Claude Code sessions found") {
		t.Fatalf("missing empty card: %v", tt.Texts())
	}
	click(t, tt, "Recycle Bin (1)")
	if !tt.HasText("Active") || !tt.HasText("Restore") {
		t.Fatalf("missing recycle controls: %v", tt.Texts())
	}
	click(t, tt, "Restore")
	click(t, tt, "Active")
	if !tt.HasText("Implement streaming parser") {
		t.Fatal("restore lost session")
	}
}
