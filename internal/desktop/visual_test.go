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
	a.loadedAt, a.syncLastAt = "10:52:53", "11:53:16"
	a.remote = model.Data{Sessions: []model.Session{{DeviceID: "laptop", DeviceName: "LAPTOP-9MA1G6"}}}
	if err := syncclient.SaveConfig(syncclient.Config{Enabled: true, APIURL: "https://sync.example.test"}); err != nil {
		t.Fatal(err)
	}
	for _, agent := range model.Agents {
		a.installed[agent] = true
		delete(a.loaded, agent)
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
	a.loaded[model.Devin] = d
	return a
}

func TestOriginalDenseVisualStates(t *testing.T) {
	a := denseFixture(t)
	tt := ui.NewTester(a.View, 1240, 780)
	tt.SetScale(2)
	if !tt.HasText("隐藏无活动日期") || !tt.HasText("上次同步 11:53:16") {
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

// Long hostnames must end with an ellipsis inside the 160 DIP sidebar
// instead of running past the row into the window edge.
func TestDeviceNamesFitSidebar(t *testing.T) {
	a := fixture(t)
	a.lang, a.agentsClosed = "en", true
	a.device = a.localID
	a.remote = model.Data{Sessions: []model.Session{{DeviceID: "laptop", DeviceName: "LAPTOP-9MA1G6D"}}}
	tt := ui.NewTester(func(c *ui.Context) {
		a.theme(c)
		ui.Row(c).Fill().Children(func() { a.sidebar(c) })
	}, 160, 600)
	row, ok := tt.Find("LAPTOP-9MA1G6D")
	if !ok {
		t.Fatalf("no device row: %v", tt.Texts())
	}
	// The row's 8 DIP right padding must stay clear of glyphs.
	img := tt.Image()
	for y := int(row.Y + 4); y < int(row.Y+row.H-4); y++ {
		for x := int(row.X + row.W - 6); x < int(row.X+row.W-1); x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if r>>8 > 100 || g>>8 > 100 || b>>8 > 100 {
				t.Fatalf("device name overflows into the right padding at %d,%d", x, y)
			}
		}
	}
}
