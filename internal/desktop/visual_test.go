package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
)

// Dense, synthetic history exercises the same visual states as the original
// screenshots without depending on a user's transcripts or credentials.
func denseFixture(t *testing.T) *App {
	a := fixture(t)
	a.agent, a.lang, a.device = model.Devin, "zh", a.localID
	a.loadedAt = "10:52:53"
	a.syncUI.completedAt = time.Now().Add(-53 * time.Minute)
	a.remote = model.Data{Sessions: []model.Session{{DeviceID: "laptop", DeviceName: "LAPTOP-9MA1G6"}}}
	if err := syncclient.SaveConfig(syncclient.Config{Enabled: true, APIURL: "https://sync.example.test"}); err != nil {
		t.Fatal(err)
	}
	for _, agent := range model.Agents {
		a.installed[agent] = true
		delete(a.counts, agent)
	}
	configDir, _ := os.UserConfigDir()
	if err := os.WriteFile(filepath.Join(configDir, "devin-usage-metrics/github-sync-session.meta.json"), []byte(`{"login":"example-user"}`), 0600); err != nil {
		t.Fatal(err)
	}
	d := model.Data{}
	for i := 0; i < 21; i++ {
		project := "quick-cleaner"
		if i == 20 {
			project = "publish"
		}
		d.Sessions = append(d.Sessions, model.Session{Agent: model.Devin, DeviceID: a.localID, Key: fmt.Sprint(i), ID: fmt.Sprintf("sample-session-%02d", i), Title: []string{"评估项目合理性、清理通用性与代码复用性", "Devin CLI 历史版本缓存清理分析", "评估 blazing-vortex-neon 计划", "Commit changes", "Git Commit"}[i%5], WorkingDirectory: "/projects/" + project, SelectedModel: []string{"fusion-claude-opus-5-5", "glm-5-2", "swe-2-high"}[i%3], AgentMode: []string{"bypass", "smart"}[i%2], InputTokens: 1100000, CachedTokens: 2400000, AgentMessages: float64(i * 9), LastActivityAt: time.Now().Add(-time.Duration(i) * 24 * time.Hour).Unix()})
	}
	for _, v := range []struct {
		name                             string
		ago, calls                       int
		input, output, write, read, cost float64
	}{
		{"swe-2-high", 0, 359, 1400000, 219100, 0, 56380900, 24.37},
		{"claude-opus-5-5-medium", 0, 82, 11100, 60500, 246200, 12882200, 5.06},
		{"compactor", 0, 3, 200700, 17300, 0, 0, 0},
		{"swe-2-high", 1, 26, 49300, 16100, 0, 1200000, .75},
		{"swe-2-high", 4, 18, 81800, 5600, 0, 361300, .44},
		{"swe-2-high", 6, 13, 18000, 2700, 0, 256700, 0},
		{"swe-2-high", 12, 100, 220000, 39000, 0, 4100000, 0},
	} {
		for i := 0; i < v.calls; i++ {
			n := float64(v.calls)
			turn := model.Turn{Agent: model.Devin, DeviceID: a.localID, SessionKey: fmt.Sprint(v.ago % 5), Model: v.name, CreatedAt: time.Now().AddDate(0, 0, -v.ago).Unix(), InputTokens: v.input / n, OutputTokens: v.output / n, CacheReadTokens: v.read / n, CacheCreationTokens: v.write / n}
			if v.cost > 0 {
				cost := v.cost / n
				turn.RecordedCost = &cost
			}
			d.Turns = append(d.Turns, turn)
		}
	}
	a.storeLoaded(model.Devin, d)
	return a
}

func TestOriginalDenseVisualStates(t *testing.T) {
	a := denseFixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	if !tt.HasText("隐藏无活动日期") || !tt.HasText("上次同步") || !tt.HasText("53 分钟前") {
		t.Fatal("missing original toolbar/footer controls")
	}
	capture(t, tt, "parity-usage")
	label := "▸  " + time.Now().Format("01-02 Mon")
	click(t, tt, label)
	for _, label := range []string{"共 3 个模型", "占比", "81.2%", "compactor", "$24.37", "$5.06"} {
		if !tt.HasText(label) {
			t.Fatalf("expanded model table missing %q: %v", label, tt.Texts())
		}
	}
	capture(t, tt, "parity-models-expanded")
	click(t, tt, "▾  "+time.Now().Format("01-02 Mon"))
	click(t, tt, "隐藏无活动日期")
	if tt.HasText("    " + time.Now().AddDate(0, 0, -2).Format("01-02 Mon")) {
		t.Fatal("inactive date was not hidden")
	}
	if !tt.HasText("▸  " + time.Now().Format("01-02 Mon")) {
		t.Fatal("hiding inactive dates removed active usage")
	}
	capture(t, tt, "parity-hide-inactive")
	click(t, tt, "会话")
	if !tt.HasText("窗口费用") || !tt.HasText("隐藏") || !tt.HasText("ID") {
		t.Fatal("original session table controls missing")
	}
	capture(t, tt, "parity-sessions")
	click(t, tt, "设置")
	if !tt.HasText("多设备同步") || !tt.HasText("常规偏好") {
		t.Fatal("original settings tabs missing")
	}
	capture(t, tt, "parity-settings-sync")
	click(t, tt, "常规偏好")
	capture(t, tt, "parity-settings-general")
}

func TestAdaptiveSessionModelDisplay(t *testing.T) {
	for _, agent := range []model.Agent{model.Devin, model.Omp} {
		t.Run(string(agent), func(t *testing.T) {
			a := fixture(t)
			a.agent = agent
			selected := "adaptive"
			if agent == model.Omp {
				selected = "devin/adaptive"
			}
			upstream := "gpt-6-1-sol-low"
			now := time.Now().Unix()
			a.storeLoaded(agent, model.Data{Sessions: []model.Session{
				{Agent: agent, DeviceID: a.localID, Key: "routed", ID: "routed", Title: "Routed session", SelectedModel: selected, RealModel: &upstream, LastActivityAt: now},
				{Agent: agent, DeviceID: a.localID, Key: "legacy", ID: "legacy", Title: "Legacy adaptive session", SelectedModel: selected, LastActivityAt: now},
			}})
			tt := ui.NewTester(a.View, 1240, 780)
			click(t, tt, "Sessions")
			if !tt.HasText("adaptive → gpt-6-1-sol-low") || !tt.HasText("adaptive") || tt.HasText("adaptive → adaptive") {
				t.Fatalf("adaptive models are not displayed consistently: %v", tt.Texts())
			}
			click(t, tt, "Routed session")
			if !tt.HasText("adaptive → gpt-6-1-sol-low") || !tt.HasText("Close session details") {
				t.Fatalf("session details lost adaptive routing: %v", tt.Texts())
			}
			capture(t, tt, string(agent)+"-adaptive-session")
		})
	}
}

// Long hostnames end with an ellipsis inside the toolbar's device pill
// instead of pushing the reload button off the window.
func TestDeviceNamesFitDeviceScope(t *testing.T) {
	a := fixture(t)
	a.lang = "en"
	a.device = "laptop"
	a.remote = model.Data{Sessions: []model.Session{{DeviceID: "laptop", DeviceName: "LAPTOP-9MA1G6D-WITH-A-VERY-LONG-HOSTNAME.example.internal"}}}
	tt := ui.NewTester(a.View, 1240, 780)
	pill, ok := tt.Find("Device scope")
	if !ok || pill.W > 260 {
		t.Fatalf("device pill grew with the hostname: %+v", pill)
	}
	// The name and the arrow stay inside the pill's border and padding.
	for _, text := range []string{"LAPTOP-9MA1G6D-WITH-A-VERY-LONG-HOSTNAME.example.internal", "▾"} {
		r, ok := tt.Find(text)
		if !ok || r.X+r.W > pill.X+pill.W-8 {
			t.Fatalf("%q overflows the pill %+v: %+v", text, pill, r)
		}
	}
	reload, ok := tt.Find("Reload")
	if !ok || reload.X+reload.W > 1240 {
		t.Fatalf("reload pushed off the toolbar: %+v", reload)
	}
}
