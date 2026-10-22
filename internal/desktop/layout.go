package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
)

// Match src/main.rs: 160px sidebar, 48px toolbar, GPUI xs/sm typography.
func (a *App) theme(c *ui.Context) {
	t := ui.DarkTheme()
	t.Background = ui.Hex("#101014")
	t.Surface = ui.Hex("#1a1a21")
	t.SurfaceHover = ui.Hex("#22222b")
	t.Border = ui.Hex("#2c2c38")
	t.Text = ui.Hex("#e6e6ee")
	t.TextMuted = ui.Hex("#8f8fa3")
	t.Accent = ui.Hex("#4cc2ff")
	t.AccentText = ui.Hex("#0a0a0c")
	t.Scrollbar = ui.Hex("#2c2c38")
	c.SetTheme(t)
}

// Rust uses div-based controls, not the toolkit's default raised buttons.
func textButton(c *ui.Context, label string, active bool) ui.Element {
	b := ui.ButtonBase(c).Label(label).FontSize(12).FixedLineHeight(20).Height(28).Padding(4, 8).Radius(4).Border(0, ui.Color{}).Background(ui.Color{}).TextColor(c.Theme().TextMuted)
	b.Children(func() { ui.Text(c, label).NoWrap() })
	if b.Hovered() {
		b.Background(c.Theme().SurfaceHover)
	}
	if active {
		b.Background(c.Theme().SurfaceHover).TextColor(c.Theme().Text)
	}
	return b
}

func (a *App) View(c *ui.Context) {
	a.drain()
	a.theme(c)
	// Match Rust: until the first read finishes, an empty dashboard would
	// claim there is no usage.
	if !a.hasLoaded && a.busy[string(a.agent)] {
		a.loadingView(c)
		return
	}
	ui.Row(c).Fill().Gap(0).FontSize(12).FixedLineHeight(20).Children(func() {
		a.sidebar(c)
		ui.Column(c).Grow(1).MinWidth(0).FillHeight().Gap(0).Children(func() {
			a.header(c)
			if a.message != "" {
				ui.Text(c, a.message).FontSize(12).TextColor(c.Theme().Accent).Padding(4, 16)
			}
			for _, e := range a.loaded[a.agent].Errors {
				ui.Text(c, e.Message).FontSize(12).TextColor(c.Theme().Danger).Padding(4, 16)
			}
			if a.screen == "quota" {
				ui.Scroll(c.Key("quota")).Grow(1).Padding(16).Gap(12).Children(func() { a.quotaView(c) })
				return
			}
			if a.tab == "usage" {
				ui.Column(c).Grow(1).MinHeight(0).MinWidth(0).Padding(16).Gap(12).Children(func() { a.usageView(c) })
				return
			}
			ui.Scroll(c.Key(a.tab)).Grow(1).Padding(16).Gap(12).Children(func() {
				if a.tab == "sessions" {
					a.sessionsView(c)
				} else {
					a.compactionView(c)
				}
			})
		})
	})
	a.settingsView(c)
	if a.pending != nil {
		r := *a.pending
		text := fmt.Sprintf(a.t("Apply %s threshold %s? Only this device's user configuration changes. Start a new agent session afterwards.", "为 %s 应用 %s 阈值？仅修改本机用户配置，应用后请开启新的 Agent 会话。"), r.Model, formatTokens(float64(r.Recommended)))
		if ui.AlertDialog(c, &a.confirm, a.t("Apply recommendation", "应用建议"), text, a.t("Cancel", "取消"), a.t("Apply", "应用")) == 1 {
			err := a.pendingApply()
			a.compactionConfigChanged()
			if err != nil {
				a.message = err.Error()
			} else {
				a.message = a.t("Applied. Restart your agent session.", "已应用，请开启新的 Agent 会话。")
			}
			a.pending = nil
		}
		if !a.confirm {
			a.pending = nil
		}
	}
}

func (a *App) loadingView(c *ui.Context) {
	ui.Column(c).Fill().AlignItems(ui.Center).Justify(ui.Center).Gap(16).Background(c.Theme().Background).Children(func() {
		ui.Spinner(c)
		ui.Text(c, a.t("Reading local agent usage data", "正在读取本地 Agent 用量数据")).FontSize(18).Bold()
		ui.Text(c, a.t("After the first read, subsequent launches use a 5-minute cache", "首次读取完成后，后续启动会使用 5 分钟缓存")).FontSize(12).TextColor(c.Theme().TextMuted)
	})
}

func (a *App) sidebar(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Width(160).Shrink(0).FillHeight().Gap(0).Background(t.Surface).Label("Sidebar").Children(func() {
		if runtime.GOOS == "darwin" {
			ui.Box(c).Height(40)
			ui.Column(c).Padding(0, 16, 8).Children(func() { ui.Text(c, "Agent Usage\nMetrics").FontSize(14).FixedLineHeight(24).Bold() })
		} else {
			ui.Row(c).Height(48).Padding(0, 16).AlignItems(ui.Center).Children(func() { ui.Text(c, "Agent Usage\nMetrics").FontSize(12).FixedLineHeight(20).Bold() })
		}
		r := ui.Row(c).Margin(0, 8, 8).Padding(6, 8).Gap(8).Radius(6).AlignItems(ui.Center).Label(a.t("Live quotas", "实时配额")).Focusable()
		if a.screen == "quota" {
			r.Background(t.Accent.Alpha(.18))
		} else if r.Hovered() {
			r.Background(t.SurfaceHover)
		}
		if r.Clicked() {
			a.screen = "quota"
			a.refreshQuota(false)
		}
		r.Children(func() {
			ui.Box(c).Size(7, 7).Radius(4).Background(ui.Hex("#3ddc97"))
			ui.Text(c, a.t("Live quotas", "实时配额")).FontSize(14).FixedLineHeight(24).Grow(1)
			if len(a.accounts) > 0 {
				ui.Text(c, fmt.Sprint(len(a.accounts))).FontSize(12).TextColor(t.TextMuted)
			}
		})
		ui.Divider(c).Margin(0, 12, 4)
		ui.Scroll(c).Grow(1).Gap(0).Children(func() {
			if textButton(c, a.sectionTitle(!a.agentsClosed, a.t("Agents", "代理")), false).Height(36).Padding(12, 12, 4).FillWidth().Justify(ui.Start).Clicked() {
				a.agentsClosed = !a.agentsClosed
			}
			if !a.agentsClosed {
				var other []model.Agent
				for _, agent := range model.Agents {
					d, loaded := a.loaded[agent]
					if a.installed[agent] || (loaded && len(d.Sessions) > 0) || agent == a.agent {
						a.agentItem(c, agent)
					} else {
						other = append(other, agent)
					}
				}
				if len(other) > 0 {
					if textButton(c, a.sectionTitle(a.otherAgents, fmt.Sprintf(a.t("More agents (%d)", "更多代理（%d）"), len(other))), false).FillWidth().Justify(ui.Start).Margin(0, 12).Clicked() {
						a.otherAgents = !a.otherAgents
					}
					if a.otherAgents {
						for _, agent := range other {
							a.agentItem(c, agent)
						}
					}
				}
			}
			if textButton(c, a.sectionTitle(!a.devicesClosed, a.t("Devices", "设备")), false).Height(36).Padding(12, 12, 4).FillWidth().Justify(ui.Start).Clicked() {
				a.devicesClosed = !a.devicesClosed
			}
			if !a.devicesClosed {
				a.deviceItem(c, "", a.t("All devices", "全部"), ui.Color{})
				devices := map[string]string{a.localID: a.deviceName}
				for _, s := range a.remote.Sessions {
					devices[s.DeviceID] = s.DeviceName
				}
				ids := []string{a.localID}
				for id := range devices {
					if id != a.localID {
						ids = append(ids, id)
					}
				}
				sort.Strings(ids[1:])
				for _, id := range ids {
					color := ui.Hex("#4cc2ff")
					if id == a.localID {
						color = ui.Hex("#3ddc97")
					}
					name := devices[id]
					if id == a.localID {
						name = "This Device"
					}
					a.deviceItem(c, id, name, color)
				}
			}
		})
		cfg := syncclient.ReadConfig()
		label, color := a.t("Sync off", "同步已关"), t.TextMuted
		if cfg.Enabled {
			label, color = a.t("Sync on", "同步已开"), ui.Hex("#3ddc97")
			if a.busy["sync"] {
				label = a.t("Syncing…", "正在同步…")
			} else if a.syncError != "" {
				label, color = a.t("Sync failed", "同步失败"), ui.Hex("#f87171")
			} else if a.syncLastAt != "" {
				label = a.t("Synced", "已同步")
			}
		}
		if textButton(c, "●  "+label, false).Label(label).FillWidth().Height(36).Padding(8, 12).Justify(ui.Start).TextColor(color).Margin(0, 8, 8).Disabled(a.busy["sync"]).Clicked() {
			if cfg.APIURL == "" || syncclient.SessionLabel() == "" {
				a.settings = true
			} else {
				cfg.Enabled = !cfg.Enabled
				if err := syncclient.SaveConfig(cfg); err != nil {
					a.message = err.Error()
				} else if cfg.Enabled {
					a.sync()
				}
			}
		}
		if cfg.Enabled && !a.busy["sync"] {
			detail := a.syncError
			if detail == "" && a.syncLastAt != "" {
				detail = a.t("Last sync ", "上次同步 ") + a.syncLastAt
			}
			if detail != "" {
				ui.Text(c, detail).FontSize(12).TextColor(t.TextMuted).Padding(1, 12, 0).Margin(0, 8, 8)
			}
		}
		ui.Row(c).Margin(0, 8, 8).AlignItems(ui.Center).Children(func() {
			if textButton(c, "⚙ "+a.t("Settings", "设置"), false).Label(a.t("Settings", "设置")).Height(24).Padding(2, 4).Clicked() {
				a.settings = true
			}
			ui.Spacer(c)
			a.updateVersionRow(c)
		})
	})
}

func (a *App) sectionTitle(open bool, label string) string {
	if open {
		return "▾  " + label
	}
	return "▸  " + label
}

func (a *App) agentItem(c *ui.Context, agent model.Agent) {
	active := a.screen == "history" && a.agent == agent
	d, loaded := a.loaded[agent]
	installed := a.installed[agent] || len(d.Sessions) > 0
	r := ui.Row(c.Key(string(agent))).Margin(0, 8, 0, 12).Height(40).Shrink(0).Padding(8).Gap(8).Radius(6).AlignItems(ui.Center).Label(agent.Label()).Focusable()
	if active {
		r.Background(c.Theme().Accent.Alpha(.18))
	} else if r.Hovered() {
		r.Background(c.Theme().SurfaceHover)
	}
	if r.Clicked() {
		a.agent = agent
		a.screen = "history"
		a.page = 0
		a.selected = nil
		if a.tab == "compaction" && agent != model.Claude && agent != model.Codex {
			a.tab = "usage"
		}
		if !loaded {
			a.load(agent, false)
		}
	}
	r.Children(func() {
		color := modelColor(agent.Label())
		if !installed {
			color = c.Theme().TextMuted.Alpha(.45)
		}
		ui.Box(c).Size(7, 7).Radius(4).Background(color)
		text := ui.Text(c, agent.Label()).FontSize(14).FixedLineHeight(24).Grow(1).MinWidth(0).NoWrap()
		if active {
			text.Bold().TextColor(c.Theme().Accent)
		} else if !installed {
			text.TextColor(c.Theme().TextMuted.Alpha(.45))
		}
		if loaded {
			ui.Text(c, fmt.Sprint(len(d.Sessions))).FontSize(12).TextColor(c.Theme().TextMuted)
		}
	})
}

func (a *App) deviceItem(c *ui.Context, id, name string, color ui.Color) {
	r := ui.Row(c.Key("device/"+id)).Margin(0, 8, 0, 12).Height(32).Shrink(0).Padding(4, 8).Gap(8).Radius(6).AlignItems(ui.Center).Label(name).Focusable()
	if a.device == id {
		r.Background(c.Theme().Accent.Alpha(.18))
	} else if r.Hovered() {
		r.Background(c.Theme().SurfaceHover)
	}
	if r.Clicked() {
		a.device = id
	}
	r.Children(func() {
		ui.Box(c).Size(7, 7).Radius(4).Background(color)
		text := ui.Text(c, name).FontSize(14).FixedLineHeight(24).Grow(1).MinWidth(0).SingleLine()
		if a.device == id {
			text.Bold().TextColor(c.Theme().Accent)
		}
	})
}

func (a *App) header(c *ui.Context) {
	ui.Row(c).Height(48).Shrink(0).Padding(0, max(16, c.TitleBar().Right), 0, 16).Gap(8).AlignItems(ui.Center).Label("Toolbar").DragWindow().Children(func() {
		if a.screen == "quota" {
			ui.Text(c, a.t("Live quotas", "实时配额")).FontSize(14).Bold()
			if textButton(c, "+ "+a.t("Add Devin account", "添加 Devin 账号"), false).Clicked() {
				a.openAccountModal()
			}
		} else {
			for _, tab := range []struct{ key, en, zh string }{{"usage", "Usage", "用量"}, {"sessions", "Sessions", "会话"}, {"compaction", "Compaction", "压缩建议"}} {
				if tab.key == "compaction" && a.agent != model.Claude && a.agent != model.Codex {
					continue
				}
				b := textButton(c, a.t(tab.en, tab.zh), false).FontSize(14).FixedLineHeight(24).Height(32).Padding(4, 12).Radius(6)
				if a.tab == tab.key {
					b.Background(c.Theme().Accent).TextColor(c.Theme().AccentText)
				}
				if b.Clicked() {
					if tab.key == "compaction" {
						// Pick up settings edited outside the app.
						a.compactionConfigChanged()
					}
					a.tab = tab.key
					a.selected = nil
				}
			}
			if a.tab != "sessions" {
				a.periodView(c)
			}
		}
		ui.Spacer(c)
		if textButton(c, a.t("Reload", "重新加载"), false).Height(24).Padding(2, 8).Disabled(a.busy[string(a.agent)] || a.busy["quota"]).Clicked() {
			if a.screen == "quota" {
				a.refreshQuota(true)
			} else {
				a.load(a.agent, true)
			}
			a.sync()
		}
		if a.busy[string(a.agent)] || a.busy["quota"] {
			ui.Text(c, a.t("Loading…", "加载中…")).FontSize(12).TextColor(c.Theme().TextMuted)
		} else if a.loadedAt != "" {
			ui.Text(c, a.t("Data as of ", "数据截至 ")+a.loadedAt).FontSize(12).TextColor(c.Theme().TextMuted)
		}
	})
}

// Rust's sidebar checks for existing data directories without parsing histories.
// Snapshot these at startup, rather than touching the filesystem every frame.
func installedAgents() map[model.Agent]bool {
	home, _ := os.UserHomeDir()
	p := func(s string) string { return filepath.Join(home, s) }
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		base = p(".local/share")
	}
	if runtime.GOOS == "windows" {
		base = os.Getenv("APPDATA")
	} else if runtime.GOOS == "darwin" {
		base = p("Library/Application Support")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = p(".codex")
	}
	devin := p(".local/share/devin")
	if runtime.GOOS == "windows" {
		devin = filepath.Join(os.Getenv("APPDATA"), "devin")
	}
	paths := map[model.Agent][]string{model.Devin: {filepath.Join(devin, "cli/sessions.db"), filepath.Join(devin, "cli-next/sessions.db")}, model.Claude: {p(".claude/projects")}, model.Codex: {filepath.Join(codex, "sessions"), filepath.Join(codex, "archived_sessions")}, model.Antigravity: {p(".gemini/antigravity/conversations")}, model.Grok: {p(".grok/sessions")}, model.ZCode: {p(".zcode/cli/db/db.sqlite"), p("Library/Application Support/zcode/cli/db/db.sqlite")}, model.OpenCode: {filepath.Join(base, "opencode/opencode.db"), p(".local/share/opencode/opencode.db")}, model.Pi: {p(".pi/agent/sessions")}, model.MimoCode: {filepath.Join(base, "mimocode/mimocode.db"), p(".local/share/mimocode/mimocode.db")}, model.WorkBuddy: {p(".workbuddy/projects"), p(".workbuddy-ai/projects"), p(".codebuddy/projects")}}
	for _, key := range []string{"WORKBUDDY_CONFIG_DIR", "CODEBUDDY_CONFIG_DIR", "WORKBUDDY_DIR"} {
		if value := os.Getenv(key); value != "" {
			paths[model.WorkBuddy] = append(paths[model.WorkBuddy], filepath.Join(value, "projects"))
		}
	}
	installed := map[model.Agent]bool{}
	for agent, items := range paths {
		for _, path := range items {
			if _, err := os.Stat(path); err == nil {
				installed[agent] = true
				break
			}
		}
	}
	return installed
}

func (a *App) periodView(c *ui.Context) {
	ui.Row(c).Gap(4).Padding(0, 0, 0, 8).BorderWidth(0, 0, 0, 1).BorderColor(c.Theme().Border).Children(func() {
		for _, p := range []analytics.Period{analytics.Day, analytics.Week, analytics.Month} {
			label := map[analytics.Period][2]string{analytics.Day: {"Day", "日"}, analytics.Week: {"Week", "周"}, analytics.Month: {"Month", "月"}}[p]
			if textButton(c, a.t(label[0], label[1]), a.period == p).Height(24).Padding(2, 8).Clicked() {
				a.period = p
				a.page = 0
			}
		}
	})
	ui.Row(c).Gap(4).Padding(0, 0, 0, 8).BorderWidth(0, 0, 0, 1).BorderColor(c.Theme().Border).Children(func() {
		if textButton(c, "◀", false).Label(a.t("Previous period", "上一周期")).Height(24).Padding(2, 8).Clicked() {
			a.page++
		}
		start, end := analytics.Window(a.period, a.page, time.Now())
		ui.Text(c, start.Format("01-02")+" ~ "+end.AddDate(0, 0, -1).Format("01-02")).Padding(2, 8).FontSize(12).NoWrap()
		if textButton(c, "▶", false).Label(a.t("Next period", "下一周期")).Height(24).Padding(2, 8).Disabled(a.page == 0).Clicked() {
			a.page--
		}
		if a.page > 0 && textButton(c, a.t("Today", "今天"), false).Height(22).Clicked() {
			a.page = 0
		}
	})
}
