package desktop

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/quota"
)

func TestTrayChinesePopulatedVisual(t *testing.T) {
	a := fixture(t)
	a.lang = "zh"
	a.trayUI.updatedAt = "12:43:24"
	for agent, cost := range map[model.Agent]float64{model.Claude: 266.46, model.Devin: 57.79, model.Codex: 49.76} {
		price := cost
		a.loaded[agent] = model.Data{Turns: []model.Turn{{Agent: agent, CreatedAt: time.Now().Unix(), RecordedCost: &price}}}
	}
	a.loaded[model.Antigravity] = model.Data{Turns: []model.Turn{{Agent: model.Antigravity, CreatedAt: time.Now().Unix(), InputTokens: 501779800}}}
	a.accounts = []accountResult{
		{account: quota.Account{Key: "claude", Provider: "claude"}, result: quota.Result{Plan: "Pro", Windows: []quota.Window{
			{Label: "5h", ResetsAt: time.Now().Add(5 * time.Hour).Unix()},
			{Label: "7d", ResetsAt: time.Now().Add(131 * time.Hour).Unix()},
		}}},
		{account: quota.Account{Key: "antigravity", Provider: "antigravity"}, result: quota.Result{Windows: []quota.Window{
			{Label: "Gemini Models – Weekly", UsedPercent: 65},
			{Label: "Gemini Models – 5h", UsedPercent: 85},
		}}},
	}
	tt := ui.NewTester(a.trayView, 350, 520)
	tt.SetScale(2)
	for _, label := range []string{"订阅配额监控", "更新于 12:43:24", "费用与用量", "今天", "昨天", "30 天", "实时配额与限额", "5 小时窗口", "7 天窗口", "剩余 100%", "已用 0.0%", "剩余 35%", "额度预警 · 剩余 15%", "$374.01", "(71%)", "打开控制台 →"} {
		if !tt.HasText(label) {
			t.Fatalf("missing %q: %q", label, tt.Texts())
		}
	}
	if got := tt.Image().Bounds().Size(); got.X != 700 || got.Y != 1040 {
		t.Fatalf("expected 350x520 logical at 2x, got %v", got)
	}
	capture(t, tt, "go-tray-zh-populated")
	click(t, tt, "昨天")
	if !tt.HasText("此周期内暂无会话记录") {
		t.Fatal("Chinese period control did not update overview")
	}
}

func TestTrayContentsAndActions(t *testing.T) {
	a := fixture(t)
	a.accounts = []accountResult{
		{account: quota.Account{Key: "quota", Provider: "claude", Label: "fixture@example.com"}, result: quota.Result{Plan: "Max", Windows: []quota.Window{{Label: "5h", UsedPercent: 85, ResetsAt: time.Now().Add(time.Hour).Unix()}}}},
		{account: quota.Account{Key: "api", Provider: "codex"}, result: quota.Result{Plan: "API MODE"}},
	}
	opened, closed := false, false
	a.trayUI.openDashboard = func() { opened = true }
	a.trayUI.closePopup = func() { closed = true }
	tt := ui.NewTester(a.trayView, 350, 520)
	tt.SetScale(2)
	for _, text := range []string{"Subscription Quotas", "Cost & Usage", "Today", "Yesterday", "30 Days", "Claude Code", "fixture@example.com", "Max", "5-hour window", "Limit alert · 15% left", "Used: 85.0%", "Open Dashboard →"} {
		if !tt.HasText(text) {
			t.Fatalf("missing %q: %q", text, tt.Texts())
		}
	}
	if tt.HasText("Codex (API Mode)") {
		t.Fatal("other sources must start collapsed")
	}
	capture(t, tt, "go-tray-quotas")
	click(t, tt, "▸ Other Sources (1 idle / API Mode)")
	if !tt.HasText("Codex (API Mode)") {
		t.Fatal("expansion did not expose idle account")
	}
	capture(t, tt, "go-tray-expanded")
	click(t, tt, "Today")
	if a.trayUI.period != 1 {
		t.Fatal("period click did not change overview")
	}
	click(t, tt, "Open Dashboard →")
	click(t, tt, "Close")
	if !opened || !closed {
		t.Fatal("host callbacks were not dispatched")
	}
}

func TestTrayOverviewIndependentOfDashboardAndRemote(t *testing.T) {
	a := fixture(t)
	a.agent = model.Devin
	a.device = "laptop"
	a.accounts = nil
	// Only local history from all supported sources belongs in the tray.
	now := time.Now().Unix()
	price := 2.0
	a.loaded[model.Codex] = model.Data{Turns: []model.Turn{{Agent: model.Codex, CreatedAt: now, DeviceID: a.localID, RecordedCost: &price}}}
	tt := ui.NewTester(a.trayView, 350, 520)
	tt.SetScale(2)
	if !tt.HasText("Claude Code") || !tt.HasText("Codex") || tt.HasText("Devin") {
		t.Fatalf("overview followed dashboard filter: %q", tt.Texts())
	}
	capture(t, tt, "go-tray")
	click(t, tt, "Yesterday")
	if !tt.HasText("No session records in this period") {
		t.Fatalf("empty period missing: %q", tt.Texts())
	}
}
