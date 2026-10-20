package desktop

import (
	"context"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/data"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

func fixture(t *testing.T) *App {
	t.Helper()
	h := t.TempDir()
	for k, v := range map[string]string{"HOME": h, "USERPROFILE": h, "APPDATA": filepath.Join(h, "config"), "XDG_CONFIG_HOME": filepath.Join(h, "config"), "XDG_CACHE_HOME": filepath.Join(h, "cache"), "LANG": "en_US.UTF-8", "DEVIN_USAGE_DISABLE_MODELS_FETCH": "1"} {
		t.Setenv(k, v)
	}
	a := New(context.Background())
	a.lang = "en"
	a.agent = model.Claude
	id := data.DeviceID()
	now := time.Now()
	a.loaded[model.Claude] = model.Data{Sessions: []model.Session{{Agent: model.Claude, Key: "claude-code/local", ID: "local", Title: "Implement streaming parser", WorkingDirectory: "/projects/usage-metrics", SelectedModel: "claude-sonnet-4-5", DeviceID: id, DeviceName: "Workstation", InputTokens: 37000, OutputTokens: 8000, CachedTokens: 120000, CacheCreationTokens: 3000, AgentMessages: 2, CreatedAt: now.AddDate(0, 0, -5).Unix(), LastActivityAt: now.Unix()}}, Turns: []model.Turn{{Agent: model.Claude, SessionKey: "claude-code/local", Model: "claude-sonnet-4-5", DeviceID: id, CreatedAt: now.AddDate(0, 0, -4).Unix(), InputTokens: 12000, OutputTokens: 3000, CacheReadTokens: 40000, CacheCreationTokens: 1000, TTFTMs: 150}, {Agent: model.Claude, SessionKey: "claude-code/local", Model: "claude-sonnet-4-5", DeviceID: id, CreatedAt: now.Unix(), InputTokens: 25000, OutputTokens: 5000, CacheReadTokens: 80000, CacheCreationTokens: 2000, TTFTMs: 240}}}
	a.remote = model.Data{Sessions: []model.Session{{Agent: model.Claude, Key: "claude-code/remote", ID: "remote", Title: "Review API changes", SelectedModel: "claude-opus-4-6", DeviceID: "laptop", DeviceName: "Laptop", InputTokens: 8000, OutputTokens: 1000, CachedTokens: 20000, CreatedAt: now.Unix(), LastActivityAt: now.Unix()}}, Turns: []model.Turn{{Agent: model.Claude, SessionKey: "claude-code/remote", Model: "claude-opus-4-6", DeviceID: "laptop", CreatedAt: now.Unix(), InputTokens: 8000, OutputTokens: 1000, CacheReadTokens: 20000}}}
	for _, agent := range model.Agents {
		if agent != model.Claude {
			a.loaded[agent] = model.Data{}
		}
	}
	return a
}

func capture(t *testing.T, tt *ui.Tester, name string) {
	t.Helper()
	dir := os.Getenv("AUM_SCREENSHOTS")
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = png.Encode(f, tt.Image()); err != nil {
		t.Fatal(err)
	}
}

func click(t *testing.T, tt *ui.Tester, text string) {
	t.Helper()
	if err := tt.Click(text); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardFiltersAndSessionNavigation(t *testing.T) {
	a := fixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	if !tt.HasText("197.0K") {
		t.Fatalf("wrong total: %q", tt.Texts())
	}
	sidebar, ok := tt.Find("Sidebar")
	if !ok || sidebar.W != 160 {
		t.Fatalf("original sidebar width changed: %+v", sidebar)
	}
	title, ok := tt.Find("Agent Usage\nMetrics")
	if !ok || title.X < sidebar.X+16 || title.X+title.W > sidebar.X+sidebar.W-16 || title.H < 40 {
		t.Fatalf("sidebar title must fit on two lines with 16px side padding: %+v", title)
	}
	toolbar, ok := tt.Find("Toolbar")
	if !ok || toolbar.H != 48 || toolbar.X != 160 {
		t.Fatalf("original toolbar layout changed: %+v", toolbar)
	}
	chart, ok := tt.Find("Stacked token usage chart")
	if !ok {
		t.Fatal("missing chart")
	}
	table, ok := tt.Find("Period details")
	if !ok {
		t.Fatal("missing independently scrolling details")
	}
	tt.Scroll(table.X+20, table.Y+20, 0, 240)
	after, _ := tt.Find("Stacked token usage chart")
	if after != chart {
		t.Fatalf("scrolling table moved chart: %+v → %+v", chart, after)
	}
	capture(t, tt, "go-usage")
	click(t, tt, "Laptop")
	if !tt.HasText("29.0K") {
		t.Fatal("device filter did not restrict usage")
	}
	click(t, tt, "All devices")
	click(t, tt, "Sessions")
	row, ok := tt.Find("Implement streaming parser")
	if !ok || row.H != 28 {
		t.Fatalf("original dense session row changed: %+v", row)
	}
	capture(t, tt, "go-sessions")
	click(t, tt, "Implement streaming parser")
	if !tt.HasText("Median TTFT 240 ms") || !tt.HasText("Workdir") || !tt.HasText("168.0K") {
		t.Fatalf("missing session details: %q", tt.Texts())
	}
	capture(t, tt, "go-session-detail")
	click(t, tt, "Close session details")
	click(t, tt, "Collapse All")
	if tt.HasText("Implement streaming parser") {
		t.Fatal("collapsed project still shows sessions")
	}
	click(t, tt, "Expand All")
	click(t, tt, "Group by Project")
	if !tt.HasText("Implement streaming parser") || !tt.HasText("Flat List") {
		t.Fatal("flat list lost sessions")
	}
	capture(t, tt, "go-sessions-flat")
	click(t, tt, "Hide")
	click(t, tt, "Recycle Bin (1)")
	if !tt.HasText("Restore") {
		t.Fatal("hidden session missing from recycle bin")
	}
	capture(t, tt, "go-sessions-recycle")
	click(t, tt, "Restore")
	click(t, tt, "Active")
	click(t, tt, "Usage")
	click(t, tt, "Previous period")
	if !tt.HasText("0") {
		t.Fatal("empty previous period did not render")
	}
	capture(t, tt, "go-empty-history")
}

func TestQuotaCompactionAndSettingsStates(t *testing.T) {
	a := fixture(t)
	a.screen = "quota"
	a.accounts = []accountResult{{account: quota.Account{Key: "fixture", Provider: "Claude", Label: "Fixture account"}, result: quota.Result{Plan: "Max", Windows: []quota.Window{{Label: "5-hour", UsedPercent: 37.5, ResetsAt: time.Now().Add(time.Hour).Unix()}, {Label: "7-day", UsedPercent: 82, ResetsAt: time.Now().AddDate(0, 0, 3).Unix()}}, Stale: "Cached · rate limited; retry later"}}}
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	if !tt.HasText("38%") || !tt.HasText("82%") {
		t.Fatal("quota percentages missing")
	}
	capture(t, tt, "go-quotas")
	click(t, tt, "Claude Code")
	click(t, tt, "Compaction")
	if !tt.HasText("No eligible data. Claude Code and Codex require at least 50 calls with context metadata and 3 observed compactions.") {
		t.Fatal("missing insufficient-data explanation")
	}
	capture(t, tt, "go-compaction")
	click(t, tt, "Settings")
	if !tt.HasText("Self-hosted sync API") {
		t.Fatal("settings did not open")
	}
	capture(t, tt, "go-settings")
	tt.Key(0, ui.KeyEscape)
	if a.settings {
		t.Fatal("Escape did not dismiss settings")
	}
	click(t, tt, "Settings")
	click(t, tt, "General")
	capture(t, tt, "go-settings-general")
	click(t, tt, "简体中文")
	click(t, tt, "取消")
	if !tt.HasText("实时配额") {
		t.Fatal("language did not change")
	}
	capture(t, tt, "go-chinese")
	if language() != "zh" {
		t.Fatal("language preference not persisted")
	}
}

func TestCompactionRequiresConfirmationAndSupportsUndo(t *testing.T) {
	a := fixture(t)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	path := filepath.Join(home, "config.toml")
	initial := "# keep my settings\nmodel = \"gpt-5.2\"\n"
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	a.agent, a.tab = model.Codex, "compaction"
	d := model.Data{}
	contextTokens := 30000
	for i := 0; i < 1000; i++ {
		d.Turns = append(d.Turns, model.Turn{Agent: model.Codex, SessionKey: "replay", DeviceID: a.localID, DeviceName: "Workstation", ContextStream: "main", CreatedAt: time.Now().Add(-10*time.Hour).Unix() + int64(i)*30, Model: "gpt-5.2", InputTokens: 5000, CacheReadTokens: float64(contextTokens - 5000), OutputTokens: 800})
		contextTokens += 5000
		if contextTokens >= 960000 {
			contextTokens = 60000
		}
	}
	a.loaded[model.Codex] = d
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	capture(t, tt, "go-compaction-recommendation")
	click(t, tt, "Apply recommendation")
	before, _ := os.ReadFile(path)
	if string(before) != initial {
		t.Fatal("configuration changed before confirmation")
	}
	capture(t, tt, "go-compaction-confirmation")
	click(t, tt, "Cancel")
	before, _ = os.ReadFile(path)
	if string(before) != initial {
		t.Fatal("cancel modified configuration")
	}
	click(t, tt, "Apply recommendation")
	click(t, tt, "Apply")
	after, _ := os.ReadFile(path)
	if string(after) == initial || !tt.HasText("Applied. Restart your agent session.") {
		t.Fatal("confirmed recommendation was not applied")
	}
	click(t, tt, "Undo local change")
	restored, _ := os.ReadFile(path)
	if string(restored) != initial {
		t.Fatalf("undo did not restore original settings: %s", restored)
	}
}
