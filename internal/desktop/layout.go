package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/syncclient"
)

// The chrome follows the telemetry redesign: a 200px sidebar, a 46px
// toolbar and blue-black surfaces; type stays at GPUI xs/sm sizes.
const (
	sidebarWidth  = 200
	toolbarHeight = 46
	// compactToolbarRoom is the toolbar width, less the caption buttons,
	// that fits every control and the load time at the default window width.
	compactToolbarRoom = 900
)

func (a *App) theme(c *ui.Context) {
	t := ui.DarkTheme()
	t.Background = ui.Hex("#0c0d12")
	t.Surface = ui.Hex("#151821")
	t.SurfaceHover = ui.Hex("#191d29")
	t.Border = ui.Hex("#202533")
	t.Text = ui.Hex("#f3f5f9")
	t.TextMuted = ui.Hex("#9098aa")
	t.Accent = ui.Hex("#38bdf8")
	t.AccentText = ui.Hex("#0a0a0c")
	t.Scrollbar = ui.Hex("#2c3345")
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
			for _, e := range a.loaded.Errors {
				ui.Text(c, e.Message).FontSize(12).TextColor(c.Theme().Danger).Padding(4, 16)
			}
			if a.switching() {
				ui.Column(c).Grow(1).MinWidth(0).AlignItems(ui.Center).Justify(ui.Center).Children(func() { ui.Spinner(c) })
				return
			}
			if a.screen == "quota" {
				ui.Scroll(c.Key("quota")).Grow(1).Padding(16).Gap(12).Children(func() { a.quotaView(c) })
				return
			}
			if a.screen == "ext" {
				ui.Scroll(c.Key("ext")).Grow(1).Padding(16).Gap(12).Children(func() { a.extView(c) })
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
	a.extDialogs(c)
}

func (a *App) loadingView(c *ui.Context) {
	ui.Column(c).Fill().AlignItems(ui.Center).Justify(ui.Center).Gap(16).Background(c.Theme().Background).Children(func() {
		ui.Spinner(c)
		ui.Text(c, a.t("Reading local agent usage data", "正在读取本地 Agent 用量数据")).FontSize(18).Bold()
		ui.Text(c, a.t("After the first read, subsequent launches use a 5-minute cache", "首次读取完成后，后续启动会使用 5 分钟缓存")).FontSize(12).TextColor(c.Theme().TextMuted)
	})
}

// Chrome colors beyond the theme: the sidebar and toolbar sit a shade off
// the content, and controls use one recessed fill.
var (
	sidebarColor  = ui.Hex("#11131a")
	footerColor   = ui.Hex("#0f1118")
	toolbarColor  = ui.Hex("#0f1117")
	controlColor  = ui.Hex("#171b25")
	strongBorder  = ui.Hex("#2c3345")
	tabActive     = ui.Hex("#2563eb")
	periodActive  = ui.Hex("#232a39")
	syncOnColor   = ui.Hex("#10b981")
	syncFailColor = ui.Hex("#f87171")
)

const (
	gearSVG    = `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>`
	monitorSVG = `<svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="2" y="3" width="20" height="14" rx="2" ry="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg>`
)

var gearIcon, monitorIcon = svgIcon(gearSVG), svgIcon(monitorSVG)

// faint is the third text level, below the theme's muted text.
func faint(t *ui.Theme) ui.Color { return t.TextMuted.Alpha(.65) }

func (a *App) sidebar(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Width(sidebarWidth).Shrink(0).FillHeight().Gap(0).Background(sidebarColor).BorderWidth(0, 1, 0, 0).BorderColor(t.Border).Label("Sidebar").Children(func() {
		brand := ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(0, 16, 14)
		if runtime.GOOS == "darwin" {
			// Leave room for the window's traffic lights.
			brand.Margin(40, 0, 0, 0)
		} else {
			brand.Height(toolbarHeight).Padding(0, 16)
		}
		brand.Children(func() {
			ui.Box(c).Size(22, 22).Shrink(0).Radius(6).Shadow(0, 2, 6, 0, tabActive.Alpha(.4)).Draw(func(p *ui.Painter, r ui.Rect) {
				p.FillGradient(r, ui.LinearGradient{From: ui.Hex("#0284c7"), To: tabActive, Angle: 135}, 6)
			}).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
				ui.Text(c, "A").FontSize(11).FixedLineHeight(14).FontWeight(800).TextColor(ui.Hex("#ffffff"))
			})
			ui.Text(c, "Agent Usage Metrics").FontSize(13).FixedLineHeight(20).Bold().SingleLine().Ellipsis("…").MinWidth(0)
		})
		a.quotaEntry(c)
		a.extEntry(c)
		ui.Scroll(c).Grow(1).Padding(0, 10).Gap(2).Children(func() {
			var shown, other []model.Agent
			for _, agent := range model.Agents {
				if a.installed[agent] || a.counts[agent] > 0 || agent == a.agent {
					shown = append(shown, agent)
				} else {
					other = append(other, agent)
				}
			}
			if a.sectionHeader(c, !a.agentsClosed, a.t("Agents", "代理"), len(shown)) {
				a.agentsClosed = !a.agentsClosed
			}
			if a.agentsClosed {
				return
			}
			for _, agent := range shown {
				a.agentItem(c, agent)
			}
			if len(other) == 0 {
				return
			}
			if a.sectionHeader(c, a.otherAgents, a.t("More agents", "更多代理"), len(other)) {
				a.otherAgents = !a.otherAgents
			}
			if a.otherAgents {
				for _, agent := range other {
					a.agentItem(c, agent)
				}
			}
		})
		ui.Column(c).Padding(10, 14).Gap(8).Background(footerColor).BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
			a.syncStatus(c)
			ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
				settings := ui.ButtonBase(c).Label(a.t("Settings", "设置")).Padding(2, 4).Margin(0, 0, 0, -4).Radius(4).Border(0, ui.Color{}).Background(ui.Color{}).TextColor(t.TextMuted)
				if settings.Hovered() {
					settings.TextColor(t.Text)
				}
				settings.Children(func() {
					ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, gearIcon).Size(12, 12)
						ui.Text(c, a.t("Settings", "设置")).FontSize(11).NoWrap()
					})
				})
				if settings.Clicked() {
					a.settings = true
				}
				ui.Spacer(c)
				a.updateVersionRow(c)
			})
		})
	})
}

func (a *App) quotaEntry(c *ui.Context) {
	t := c.Theme()
	r := ui.Row(c).Margin(0, 10, 4).Padding(7, 10).Gap(7).Radius(6).Background(controlColor).Border(1, t.Border).AlignItems(ui.Center).Label(a.t("Live quotas", "实时配额")).Focusable()
	switch {
	case a.screen == "quota":
		r.Background(t.Accent.Alpha(.12)).Border(1, t.Accent.Alpha(.35))
	case r.Hovered():
		r.Background(t.SurfaceHover).Border(1, strongBorder)
	}
	if r.Clicked() {
		a.screen = "quota"
		a.refreshQuota(false)
	}
	r.Children(func() {
		ui.Box(c).Size(6, 6).Radius(3).Background(syncOnColor).Shadow(0, 0, 6, 0, syncOnColor.Alpha(.5))
		ui.Text(c, a.t("Live quotas", "实时配额")).FontSize(12).FontWeight(500).Grow(1)
		if len(a.accounts) > 0 {
			ui.Text(c, fmt.Sprint(len(a.accounts))).FontSize(11).FontWeight(600).Padding(1, 6).Radius(10).Background(ui.Hex("#202737")).TextColor(t.TextMuted)
		}
	})
}

// sectionHeader draws a small caps list title with its count, and reports
// a click that opens or closes the section.
func (a *App) sectionHeader(c *ui.Context, open bool, label string, count int) bool {
	t := c.Theme()
	r := ui.Row(c.Key("section/"+label)).Padding(10, 8, 4).Gap(6).AlignItems(ui.Center).Label(a.sectionTitle(open, label)).Focusable()
	color := faint(t)
	if r.Hovered() {
		color = t.TextMuted
	}
	r.Children(func() {
		ui.Text(c, strings.ToUpper(label)).FontSize(10).FixedLineHeight(14).Bold().TextColor(color).Grow(1).NoWrap()
		ui.Text(c, fmt.Sprint(count)).FontSize(10).FixedLineHeight(14).Bold().TextColor(color)
		chevron := "▸"
		if open {
			chevron = "▾"
		}
		ui.Text(c, chevron).FontSize(10).FixedLineHeight(14).TextColor(color)
	})
	return r.Clicked()
}

func (a *App) sectionTitle(open bool, label string) string {
	if open {
		return "▾  " + label
	}
	return "▸  " + label
}

// monogram is an agent's one-letter mark; Codex takes X beside Claude Code.
func monogram(agent model.Agent) string {
	if agent == model.Codex {
		return "X"
	}
	return strings.ToUpper(agent.Label()[:1])
}

func (a *App) agentItem(c *ui.Context, agent model.Agent) {
	t := c.Theme()
	active := a.screen == "history" && a.agent == agent
	installed := a.installed[agent] || a.counts[agent] > 0
	r := ui.Row(c.Key(string(agent))).Height(30).Shrink(0).Padding(0, 8).Gap(8).Radius(6).AlignItems(ui.Center).Label(agent.Label()).Focusable()
	hovered := r.Hovered()
	switch {
	case active:
		r.Background(t.Accent.Alpha(.12)).Draw(func(p *ui.Painter, rect ui.Rect) {
			p.Fill(ui.Rect{X: rect.X, Y: rect.Y + 6, W: 3, H: rect.H - 12}, t.Accent, 1.5)
		})
	case hovered:
		r.Background(t.Text.Alpha(.04))
	}
	if r.Clicked() {
		a.switchAgent(agent)
	}
	text, mark, markFill, markBorder := t.TextMuted, t.TextMuted, ui.Hex("#1c2230"), ui.Hex("#283244")
	switch {
	case active:
		text, mark, markFill, markBorder = t.Accent, t.Accent, t.Accent.Alpha(.18), t.Accent.Alpha(.35)
	case !installed:
		text, mark = t.TextMuted.Alpha(.45), t.TextMuted.Alpha(.45)
	case hovered:
		text = t.Text
	}
	r.Children(func() {
		ui.Column(c).Size(17, 17).Shrink(0).Radius(4).Background(markFill).Border(1, markBorder).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
			ui.Text(c, monogram(agent)).FontSize(10).FixedLineHeight(12).Bold().TextColor(mark)
		})
		name := ui.Text(c, agent.Label()).FontSize(12).Grow(1).MinWidth(0).SingleLine().Ellipsis("…").TextColor(text)
		if active {
			name.FontWeight(600)
		}
		if n := a.counts[agent]; n > 0 {
			count := faint(t)
			if active {
				count = t.Accent.Alpha(.85)
			}
			ui.Text(c, fmt.Sprint(n)).FontSize(10.5).FontWeight(500).FontFeatures("tnum").TextColor(count)
		}
	})
}

// syncStatus shows the sync state and last time; a click toggles sync, or
// opens settings when it is not configured.
func (a *App) syncStatus(c *ui.Context) {
	t := c.Theme()
	cfg := syncclient.ReadConfig()
	label, color := a.t("Sync off", "同步已关"), t.TextMuted
	if cfg.Enabled {
		label, color = a.t("Sync on", "同步已开"), syncOnColor
		if a.busy["sync"] {
			label = a.t("Syncing…", "正在同步…")
		} else if a.syncUI.err != "" {
			label, color = a.t("Sync failed", "同步失败"), syncFailColor
		} else if a.syncUI.lastAt != "" {
			label = a.t("Synced", "已同步")
		}
	}
	r := ui.Row(c).Gap(7).Padding(2, 4).Margin(0, -4).Radius(4).AlignItems(ui.Center).Label(label).Focusable()
	if r.Hovered() && !a.busy["sync"] {
		r.Background(t.Text.Alpha(.04))
	}
	if r.Clicked() && !a.busy["sync"] {
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
	r.Children(func() {
		ui.Box(c).Size(6, 6).Shrink(0).Radius(3).Background(color)
		ui.Text(c, label).FontSize(11).FontWeight(500).TextColor(t.TextMuted).Grow(1).NoWrap()
		if cfg.Enabled && !a.busy["sync"] && a.syncUI.err == "" && a.syncUI.lastAt != "" {
			ui.Text(c, a.syncUI.lastAt).FontSize(10).FontFeatures("tnum").TextColor(faint(t)).Label(a.t("Last sync ", "上次同步 ") + a.syncUI.lastAt)
		}
	})
	if cfg.Enabled && !a.busy["sync"] && a.syncUI.err != "" {
		ui.Text(c, a.syncUI.err).FontSize(11).TextColor(syncFailColor).Margin(-4, 0, 0, 0)
	}
}

// segment is one option of a recessed segmented control.
func segment(c *ui.Context, label string, active bool, fontSize float32, padX, padY float32, activeFill, activeText ui.Color) ui.Element {
	t := c.Theme()
	b := ui.ButtonBase(c).Label(label).FontSize(fontSize).Padding(padY, padX).Radius(4).Border(0, ui.Color{}).Background(ui.Color{}).TextColor(t.TextMuted)
	switch {
	case active:
		b.Background(activeFill).TextColor(activeText).FontWeight(600)
	case b.Hovered():
		b.TextColor(t.Text)
	}
	b.Children(func() { ui.Text(c, label).NoWrap() })
	return b
}

func control(c *ui.Context, radius float32) ui.Element {
	return ui.Row(c).Gap(0).Padding(2).Radius(radius).Background(controlColor).Border(1, c.Theme().Border).AlignItems(ui.Center)
}

func (a *App) header(c *ui.Context) {
	t := c.Theme()
	bar := ui.Row(c).Height(toolbarHeight).Shrink(0).Padding(0, max(20, c.TitleBar().Right), 0, 20).Gap(16).AlignItems(ui.Center).Background(toolbarColor).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Label("Toolbar").DragWindow()
	// Windows' caption buttons take 138px of the toolbar; below this width
	// the load time moves into the reload button's tooltip.
	room := bar.Bounds().W - max(20, c.TitleBar().Right)
	compact := room > 0 && room < compactToolbarRoom
	bar.Children(func() {
		// The quota page titles itself; its toolbar keeps only reload.
		if a.screen == "ext" {
			a.extToolbar(c)
			return
		}
		if a.screen != "quota" {
			control(c, 6).Children(func() {
				for _, tab := range []struct{ key, en, zh string }{{"usage", "Usage", "用量"}, {"sessions", "Sessions", "会话"}, {"compaction", "Compaction", "压缩建议"}} {
					if tab.key == "compaction" && a.agent != model.Claude && a.agent != model.Codex {
						continue
					}
					b := segment(c, a.t(tab.en, tab.zh), a.tab == tab.key, 12, 12, 4, tabActive, ui.Hex("#ffffff"))
					if a.tab == tab.key {
						b.Shadow(0, 1, 3, 0, ui.Hex("#000000").Alpha(.3))
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
			})
			if a.tab != "sessions" {
				a.periodView(c)
			}
		}
		ui.Spacer(c)
		if a.screen == "quota" {
			// The quota page has its own query button and time.
			if a.busy["quota"] {
				ui.Text(c, a.t("Querying…", "查询中…")).FontSize(11).TextColor(faint(t)).NoWrap()
			}
			return
		}
		a.deviceScope(c)
		reload := ui.ButtonBase(c).Label(a.t("Reload", "重新加载")).FontSize(11).Padding(4, 8).Radius(5).Background(ui.Hex("#161b26")).Border(1, t.Border).TextColor(t.TextMuted).Disabled(a.busy[string(a.agent)])
		if reload.Hovered() {
			reload.TextColor(t.Text).Border(1, strongBorder)
		}
		reload.Shrink(0).Children(func() { ui.Text(c, a.t("Reload", "重新加载")).NoWrap() })
		if reload.Clicked() {
			a.load(a.agent, true)
		}
		if a.busy[string(a.agent)] {
			ui.Text(c, a.t("Loading…", "加载中…")).FontSize(11).TextColor(faint(t)).NoWrap()
		} else if a.loadedAt != "" && compact {
			reload.Tooltip(a.t("Data as of ", "数据截至 ") + a.loadedAt)
		} else if a.loadedAt != "" {
			ui.Text(c, a.t("Data as of ", "数据截至 ")+a.loadedAt).FontSize(11).FontFeatures("tnum").TextColor(faint(t)).NoWrap().Shrink(1).MinWidth(0).Ellipsis("…")
		}
	})
}

// deviceScope is the toolbar's device filter: a pill naming the current
// scope that opens a menu of every known device.
func (a *App) deviceScope(c *ui.Context) {
	t := c.Theme()
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
	name := func(id string) string {
		switch id {
		case "":
			return a.t("All devices", "全部设备")
		case a.localID:
			return a.t("This device", "本机") + " (" + strings.TrimSuffix(a.deviceName, ".local") + ")"
		}
		return devices[id]
	}
	// A row, not a button: the name must shrink to its ellipsis inside the
	// pill's maximum width while the icon and arrow keep their size.
	pill := ui.Row(c).MaxWidth(260).Shrink(1).MinWidth(0).Gap(7).Padding(4, 10).Radius(6).AlignItems(ui.Center).Background(ui.Hex("#161b26")).Border(1, ui.Hex("#283347")).Label(a.t("Device scope", "设备范围")).Focusable()
	if pill.Hovered() || a.deviceMenu {
		pill.Background(ui.Hex("#1c2333")).Border(1, ui.Hex("#3b82f6"))
	}
	pill.Children(func() {
		ui.Icon(c, monitorIcon).Size(13, 13).Shrink(0).TextColor(t.Accent)
		ui.Text(c, name(a.device)).FontSize(12).FontWeight(500).Shrink(1).MinWidth(0).SingleLine().Ellipsis("…")
		ui.Text(c, "▾").FontSize(9).Shrink(0).TextColor(faint(t))
	})
	if pill.Clicked() {
		a.deviceMenu = !a.deviceMenu
	}
	ui.PopoverBase(c, pill, &a.deviceMenu, func(panel ui.Element) {
		panel.MinWidth(220).Padding(4).Gap(2).Radius(6).Background(t.Surface).Border(1, strongBorder).Shadow(0, 8, 24, 0, ui.Hex("#000000").Alpha(.55))
		panel.Children(func() {
			a.deviceItem(c, "", name(""), t.TextMuted)
			for _, id := range ids {
				color := t.Accent
				if id == a.localID {
					color = syncOnColor
				}
				a.deviceItem(c, id, name(id), color)
			}
		})
	})
}

func (a *App) deviceItem(c *ui.Context, id, name string, color ui.Color) {
	t := c.Theme()
	selected := a.device == id
	r := ui.Row(c.Key("device/"+id)).Height(28).Shrink(0).Padding(0, 8).Gap(8).Radius(4).AlignItems(ui.Center).Label(name).Focusable()
	switch {
	case selected:
		r.Background(t.Accent.Alpha(.12))
	case r.Hovered():
		r.Background(t.SurfaceHover)
	}
	if r.Clicked() {
		a.device = id
		a.deviceMenu = false
	}
	r.Children(func() {
		ui.Box(c).Size(6, 6).Shrink(0).Radius(3).Background(color)
		text := ui.Text(c, name).FontSize(12).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
		if selected {
			text.FontWeight(600).TextColor(t.Accent)
			ui.Icon(c, checkIcon).Size(11, 11).TextColor(t.Accent)
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
	paths := map[model.Agent][]string{model.Devin: {filepath.Join(devin, "cli/sessions.db"), filepath.Join(devin, "cli-next/sessions.db")}, model.Claude: {p(".claude/projects")}, model.Codex: {filepath.Join(codex, "sessions"), filepath.Join(codex, "archived_sessions")}, model.Antigravity: {p(".gemini/antigravity/conversations"), p(".gemini/antigravity-cli/conversations")}, model.Grok: {p(".grok/sessions")}, model.ZCode: {p(".zcode/cli/db/db.sqlite"), p("Library/Application Support/zcode/cli/db/db.sqlite")}, model.OpenCode: {filepath.Join(base, "opencode/opencode.db"), p(".local/share/opencode/opencode.db")}, model.Pi: {p(".pi/agent/sessions")}, model.Omp: {p(".omp/agent/sessions")}, model.MimoCode: {filepath.Join(base, "mimocode/mimocode.db"), p(".local/share/mimocode/mimocode.db")}, model.WorkBuddy: {p(".workbuddy/projects"), p(".workbuddy-ai/projects"), p(".codebuddy/projects")}}
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
	t := c.Theme()
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		control(c, 5).Children(func() {
			for _, p := range []analytics.Period{analytics.Day, analytics.Week, analytics.Month} {
				label := map[analytics.Period][2]string{analytics.Day: {"Day", "日"}, analytics.Week: {"Week", "周"}, analytics.Month: {"Month", "月"}}[p]
				if segment(c, a.t(label[0], label[1]), a.period == p, 11, 8, 3, periodActive, t.Text).Radius(3).Clicked() {
					a.period = p
					a.page = 0
				}
			}
		})
		arrow := func(text, label string, disabled bool) bool {
			b := ui.ButtonBase(c).Label(label).FontSize(11).Padding(0, 2).Border(0, ui.Color{}).Background(ui.Color{}).TextColor(faint(t)).Disabled(disabled)
			if b.Hovered() && !disabled {
				b.TextColor(t.Text)
			}
			b.Children(func() { ui.Text(c, text) })
			return b.Clicked()
		}
		ui.Row(c).Gap(6).Padding(3, 8).Radius(5).Background(ui.Hex("#141721")).Border(1, t.Border).AlignItems(ui.Center).Children(func() {
			if arrow("◀", a.t("Previous period", "上一周期"), false) {
				a.page++
			}
			start, end := analytics.Window(a.period, a.page, time.Now())
			ui.Text(c, start.Format("01-02")+" ~ "+end.AddDate(0, 0, -1).Format("01-02")).FontSize(12).FontWeight(500).FontFeatures("tnum").TextColor(t.TextMuted).NoWrap()
			if arrow("▶", a.t("Next period", "下一周期"), a.page == 0) {
				a.page--
			}
		})
		if a.page > 0 && textButton(c, a.t("Today", "今天"), false).Height(22).Clicked() {
			a.page = 0
		}
	})
}
