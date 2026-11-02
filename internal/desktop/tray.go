package desktop

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
	"github.com/meimingqi222/devin-usage-metrics/internal/pricing"
	"github.com/meimingqi222/devin-usage-metrics/internal/updater"
)

// The host supplies actions, not window handles. Zero state matches the Rust
// popover: the last 30 days and collapsed non-quota sources.
type trayUIState struct {
	period                          int // 0: 30 days, 1: today, 2: yesterday
	otherAccountsExpanded           bool
	openDashboard, closePopup, quit func()
	updatedAt                       string
	wasLoading                      bool
}

// trayWidth is the popover's width; the host sizes its window to match.
const trayWidth = 380

var (
	trayBackground = ui.Hex("#11131a")
	trayBar        = ui.Hex("#141720")
	trayCard       = ui.Hex("#161922")
	trayPill       = ui.Hex("#1f2533")
	trayBorder     = ui.Hex("#232938")
	trayText       = ui.Hex("#f1f5f9")
	traySecondary  = ui.Hex("#94a3b8")
	trayValue      = ui.Hex("#cbd5e1")
	trayMuted      = ui.Hex("#5e6b82")
	// Same vector paths as assets/icons/{refresh,close}.svg; glyphs vary by font.
	refreshIcon = ui.MustParseSVG([]byte(`<svg viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M13.5 8A5.5 5.5 0 0 1 3.2 10.5M2.5 8A5.5 5.5 0 0 1 12.8 5.5" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/><path d="M13.5 4.5V8H10M2.5 11.5V8H6" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/></svg>`))
	closeIcon   = ui.MustParseSVG([]byte(`<svg viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M4 4L12 12M12 4L4 12" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>`))
)

// Rust's controls are plain divs: no native button padding, minimum size,
// shadow, or raised hover treatment. Text inherits the control typography.
func trayButton(c *ui.Context, label string) ui.Element {
	b := ui.ButtonBase(c).Label(label).Radius(3).TextColor(traySecondary)
	if b.Hovered() {
		b.Background(trayPill).TextColor(trayText)
	}
	b.Children(func() { ui.Text(c, label).NoWrap() })
	return b
}

func trayIconButton(c *ui.Context, key, label string, icon *ui.SVG) ui.Element {
	b := ui.ButtonBase(c.Key(key)).Label(label).Size(22, 22).Radius(4).Border(0, ui.Color{}).Background(ui.Color{}).TextColor(traySecondary).AlignItems(ui.Center).Justify(ui.Center)
	if b.Hovered() {
		b.Background(trayPill).TextColor(trayText)
	}
	b.Children(func() { ui.Icon(c, icon).Size(13, 13) })
	return b
}

func trayCardBox(c *ui.Context) ui.Element {
	return ui.Column(c).Padding(10, 12).Gap(8).Radius(7).Background(trayCard).Border(1, trayBorder)
}

type trayCostItem struct {
	name                     string
	color                    ui.Color
	cost, tokens, percentage float64
}

func trayAgentColor(agent model.Agent) ui.Color {
	colors := map[model.Agent]string{model.Claude: "#d97757", model.Codex: "#10b981", model.Devin: "#0284c7", model.Antigravity: "#3b82f6", model.OpenCode: "#fbbf24"}
	if color := colors[agent]; color != "" {
		return ui.Hex(color)
	}
	return trayText
}

func trayMoney(cost float64) string {
	if cost > 0 && cost < .01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	if cost >= 1000 {
		return fmt.Sprintf("$%.0f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
}

// trayEntry is one turn's cost/token contribution to the menu overview,
// precomputed at load time because only the current agent's turns are
// kept in memory.
type trayEntry struct {
	at           int64
	cost, tokens float64
}

func trayEntriesOf(turns []model.Turn, localID string) []trayEntry {
	var out []trayEntry
	for _, turn := range turns {
		if turn.DeviceID != "" && localID != "" && turn.DeviceID != localID {
			continue
		}
		v, _ := pricing.Cost(turn, turn.Model)
		out = append(out, trayEntry{at: turn.CreatedAt, cost: v, tokens: turn.InputTokens + turn.OutputTokens + turn.CacheReadTokens + turn.CacheCreationTokens})
	}
	return out
}

// localUsage sums an agent's cost and tokens on this device between start
// and end, from the precomputed per-agent entries.
func (a *App) localUsage(agent model.Agent, start, end time.Time) (cost, tokens float64) {
	for _, e := range a.trayEntries[agent] {
		if e.at < start.Unix() || e.at > end.Unix() {
			continue
		}
		cost += e.cost
		tokens += e.tokens
	}
	return cost, tokens
}

func (a *App) trayOverview(now time.Time) ([]trayCostItem, float64, float64) {
	start, end := now.Add(-30*24*time.Hour), now
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if a.trayUI.period == 1 {
		start = midnight
	}
	if a.trayUI.period == 2 {
		start, end = midnight.Add(-24*time.Hour), midnight
	}
	var items []trayCostItem
	var cost, tokens float64
	for _, agent := range []model.Agent{model.Claude, model.Codex, model.Devin, model.Antigravity, model.OpenCode} {
		item := trayCostItem{name: agent.Label(), color: trayAgentColor(agent)}
		item.cost, item.tokens = a.localUsage(agent, start, end)
		if item.cost > 0 || item.tokens > 0 {
			items = append(items, item)
			cost += item.cost
			tokens += item.tokens
		}
	}
	for i := range items {
		if cost > 0 {
			items[i].percentage = items[i].cost / cost * 100
		} else if tokens > 0 {
			items[i].percentage = items[i].tokens / tokens * 100
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if cost > 0 {
			return items[i].cost > items[j].cost
		}
		return items[i].tokens > items[j].tokens
	})
	return items, cost, tokens
}

func (a *App) trayView(c *ui.Context) {
	a.drain()
	a.theme(c)
	loading := a.busy["quota"]
	overviewLoading := false
	for _, agent := range []model.Agent{model.Claude, model.Codex, model.Devin, model.Antigravity, model.OpenCode} {
		overviewLoading = overviewLoading || a.busy[string(agent)]
	}
	if a.trayUI.wasLoading && !loading {
		a.trayUI.updatedAt = time.Now().Format("15:04:05")
	}
	a.trayUI.wasLoading = loading
	ui.Column(c).Fill().Gap(0).LineHeight(1.5).Background(trayBackground).Border(1, ui.Hex("#2d3548")).Radius(10).Clip().Children(func() {
		ui.Row(c).Height(38).Shrink(0).Padding(0, 12).Gap(6).AlignItems(ui.Center).Background(trayBar).BorderWidth(0, 0, 1, 0).BorderColor(trayBorder).Children(func() {
			ui.Text(c, a.t("Subscription Quotas", "订阅配额监控")).FontSize(12).Bold().TextColor(trayText).NoWrap()
			status := ""
			if loading || overviewLoading {
				status = a.t("Updating…", "正在更新…")
			} else if a.trayUI.updatedAt != "" {
				status = a.t("Updated ", "更新于 ") + a.trayUI.updatedAt
			}
			ui.Text(c, status).FontSize(10).FontFeatures("tnum").TextColor(trayMuted).Grow(1).NoWrap()
			if trayIconButton(c, "tray-refresh", a.t("Refresh Quotas", "刷新配额"), refreshIcon).Clicked() {
				a.refreshQuota(true)
			}
			if trayIconButton(c, "tray-close", a.t("Close", "关闭"), closeIcon).Clicked() && a.trayUI.closePopup != nil {
				a.trayUI.closePopup()
			}
		})
		ui.Scroll(c.Key("tray-scroll")).Grow(1).MinHeight(0).Gap(10).Padding(10, 12).Children(func() {
			a.trayOverviewView(c, overviewLoading)
			ui.Text(c, a.t("LIVE QUOTAS & LIMITS", "实时配额与限额")).FontSize(10).Bold().TextColor(trayMuted).Padding(2, 4, 0)
			var idle []string
			for _, ar := range a.accounts {
				if ar.err == nil && len(ar.result.Windows) > 0 {
					a.trayDenseCard(c, ar)
					continue
				}
				plan := ar.result.Plan
				if ar.err != nil || plan == "" {
					plan = a.t("Local", "本地")
				} else if plan == "API MODE" {
					plan = a.t("API Mode", "API 模式")
				}
				idle = append(idle, providerName(ar.account.Provider)+" ("+plan+")")
			}
			if loading {
				ui.Text(c, "↻ "+a.t("Querying subscription quotas", "正在查询订阅配额")).FontSize(11).TextColor(trayMuted).Padding(16, 0).AlignSelf(ui.Center)
			}
			if len(idle) > 0 {
				trayCardBox(c).Gap(4).Children(func() {
					arrow := "▸ "
					if a.trayUI.otherAccountsExpanded {
						arrow = "▾ "
					}
					label := fmt.Sprintf(a.t("Other Sources (%d idle / API Mode)", "其他来源（%d 个无配额 / API 模式）"), len(idle))
					if trayButton(c.Key("tray-other"), arrow+label).FontSize(10.5).FillWidth().Justify(ui.Start).Clicked() {
						a.trayUI.otherAccountsExpanded = !a.trayUI.otherAccountsExpanded
					}
					if a.trayUI.otherAccountsExpanded {
						ui.Row(c).Wrap().Gap(4).Padding(4, 0, 0).Children(func() {
							for _, label := range idle {
								ui.Text(c, label).FontSize(9.5).TextColor(traySecondary).Background(trayPill).Radius(3).Padding(1, 8)
							}
						})
					}
				})
			}
		})
		ui.Row(c).Height(34).Shrink(0).Padding(0, 12).AlignItems(ui.Center).Background(trayBar).BorderWidth(1, 0, 0, 0).BorderColor(trayBorder).Children(func() {
			ui.Text(c, "Agent Metrics v"+updater.CurrentVersion).FontSize(10.5).TextColor(trayMuted).Grow(1)
			open := trayButton(c.Key("tray-dashboard"), a.t("Open Dashboard", "打开控制台")+" →").FontSize(10.5).FontWeight(500).Padding(2, 4).TextColor(c.Theme().Accent)
			if open.Clicked() && a.trayUI.openDashboard != nil {
				a.trayUI.openDashboard()
			}
		})
	})
	if a.quotaUI.resetFromTray {
		a.claudeResetDialog(c)
	}
}

func (a *App) trayOverviewView(c *ui.Context, loading bool) {
	items, cost, tokens := a.trayOverview(time.Now())
	trayCardBox(c).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.t("Cost & Usage", "费用与用量")).FontSize(11.5).FontWeight(600).TextColor(trayText).Grow(1)
			ui.Row(c).Background(ui.Hex("#1a1e2a")).Border(1, trayBorder).Padding(1).Radius(4).Children(func() {
				for _, p := range []struct {
					id     int
					en, zh string
				}{{1, "Today", "今天"}, {2, "Yesterday", "昨天"}, {0, "30 Days", "30 天"}} {
					b := trayButton(c.Key(fmt.Sprint("tray-period-", p.id)), a.t(p.en, p.zh)).FontSize(9.5).Padding(2, 6).Radius(3).TextColor(trayMuted)
					if a.trayUI.period == p.id {
						b.Background(ui.Hex("#283244")).TextColor(trayText).FontWeight(600)
					}
					if b.Clicked() {
						a.trayUI.period = p.id
					}
				}
			})
		})
		ui.Row(c).Gap(12).AlignItems(ui.Center).Padding(2, 0).Children(func() {
			if loading && len(items) == 0 {
				ui.Text(c, "↻ "+a.t("Loading usage records…", "正在加载用量记录…")).Height(68).FontSize(11).TextColor(trayMuted)
				return
			}
			const size, outer, inner = 68, 33.5, 28.5
			ui.Column(c).Width(size).Height(size).Shrink(0).Justify(ui.Center).AlignItems(ui.Center).Draw(func(p *ui.Painter, r ui.Rect) {
				cx, cy := r.X+size/2, r.Y+size/2
				draw := func(start, end float64, color ui.Color) {
					path := &ui.Path{}
					// A filled annular segment preserves GPUI's flat stroke ends.
					for i := 0; i <= 16; i++ {
						angle := start + (end-start)*float64(i)/16
						x, y := cx+outer*float32(math.Cos(angle)), cy+outer*float32(math.Sin(angle))
						if i == 0 {
							path.MoveTo(x, y)
						} else {
							path.LineTo(x, y)
						}
					}
					for i := 16; i >= 0; i-- {
						angle := start + (end-start)*float64(i)/16
						path.LineTo(cx+inner*float32(math.Cos(angle)), cy+inner*float32(math.Sin(angle)))
					}
					path.Close()
					p.FillPath(path, color)
				}
				if cost <= 0 || len(items) == 0 {
					draw(0, 2*math.Pi, trayPill)
					return
				}
				angle := -math.Pi / 2
				for _, item := range items {
					sweep := item.percentage / 100 * 2 * math.Pi
					if sweep < .08 {
						continue
					}
					start, end := angle+.05, angle+sweep-.05
					angle += sweep
					if end > start {
						draw(start, end, item.color)
					}
				}
			}).Children(func() {
				value, label := "$0.00", a.t("tokens", "Token")
				if cost > 0 {
					value, label = trayMoney(cost), a.t("total cost", "总费用")
				} else if tokens > 0 {
					value = fmt.Sprintf("%.1fM", tokens/1e6)
				}
				ui.Text(c, value).Width(50).NoWrap().TextAlign(ui.Center).FontSize(11).FixedLineHeight(14).Bold().FontFeatures("tnum").TextColor(trayText)
				ui.Text(c, label).FontSize(9).FixedLineHeight(12).TextColor(trayMuted)
			})
			ui.Column(c).Grow(1).MinWidth(0).Gap(3).Children(func() {
				if len(items) == 0 {
					ui.Text(c, a.t("No session records in this period", "此周期内暂无会话记录")).FontSize(10.5).TextColor(trayMuted)
				}
				for _, item := range items[:min(4, len(items))] {
					ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
						ui.Box(c).Size(5, 5).Shrink(0).Radius(2.5).Background(item.color)
						ui.Text(c, item.name).FontSize(10.5).TextColor(traySecondary).Grow(1).MinWidth(0).NoWrap()
						// An agent without priced usage (Antigravity's free tier)
						// shows its tokens rather than a 0% share of the cost.
						value, share, valueColor := trayMoney(item.cost), fmt.Sprintf("(%.0f%%)", item.percentage), trayValue
						if item.cost <= 0 {
							value, valueColor = fmt.Sprintf("%.1fK T", item.tokens/1e3), trayMuted
							if item.tokens >= 1e6 {
								value = fmt.Sprintf("%.1fM T", item.tokens/1e6)
							}
							if cost > 0 {
								share = a.t("(free)", "(免费)")
							}
						}
						ui.Text(c, value).FontSize(10.5).FontWeight(600).FontFeatures("tnum").TextColor(valueColor).NoWrap()
						ui.Text(c, share).FontSize(9.5).TextColor(trayMuted).NoWrap()
					})
				}
			})
		})
	})
}

// trayCountdown is the compact time to a reset: 3d3h, 2h44m or 12m.
func trayCountdown(seconds int64) string {
	switch {
	case seconds >= 86400:
		return fmt.Sprintf("%dd%dh", seconds/86400, seconds%86400/3600)
	case seconds >= 3600:
		return fmt.Sprintf("%dh%dm", seconds/3600, seconds%3600/60)
	}
	return fmt.Sprintf("%dm", max(1, seconds/60))
}

func (a *App) trayDenseCard(c *ui.Context, ar accountResult) {
	provider := providerName(ar.account.Provider)
	if ar.account.Provider == "claude" {
		provider = "Claude Code"
	}
	card := trayCardBox(c.Key("tray-card/" + ar.account.Key))
	if quotaAlert(ar.result) {
		card.Border(1, quotaRose.Alpha(.35))
	}
	card.Children(func() {
		ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Size(18, 18).Shrink(0).Radius(4).Background(ui.Hex("#1c2230")).Border(1, ui.Hex("#283244")).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
				if logo := providerLogos[providerName(ar.account.Provider)]; logo != nil {
					ui.Image(c, logo).Size(12, 12)
				} else {
					ui.Box(c).Size(5, 5).Radius(2.5).Background(trayAgentColor(model.Agent(ar.account.Provider)))
				}
			})
			ui.Text(c, provider).FontSize(12).FontWeight(600).TextColor(trayText).NoWrap()
			if label := strings.TrimSpace(ar.account.Label); label != "" && label != provider && label != providerName(ar.account.Provider) {
				ui.Text(c, label).FontSize(10).TextColor(trayMuted).MinWidth(0).MaxWidth(150).SingleLine()
			}
			ui.Spacer(c)
			if plan := ar.result.Plan; plan != "" {
				tag := ui.Text(c, strings.ToUpper(plan)).FontSize(9).FontWeight(600).Padding(1, 5).Radius(3).Background(ui.Hex("#1d2331")).Border(1, ui.Hex("#293448")).TextColor(trayMuted).NoWrap()
				if plan == "API MODE" {
					tag.TextColor(quotaGreen)
				} else {
					tag.TextColor(c.Theme().Accent).Border(1, c.Theme().Accent.Alpha(.3))
				}
			}
		})
		ui.Column(c).Gap(6).Children(func() {
			for _, w := range ar.result.Windows {
				remaining := math.Max(0, math.Min(100, 100-w.UsedPercent))
				color := quotaColor(w.UsedPercent)
				ui.Column(c).Gap(2).Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Text(c, a.windowLabel(w.Label)).FontSize(10).FontWeight(500).TextColor(trayValue).Grow(1).MinWidth(0).SingleLine()
						ui.Text(c, fmt.Sprintf(a.t("%.0f%% left", "剩余 %.0f%%"), remaining)).FontSize(10).FontWeight(600).FontFeatures("tnum").TextColor(color).NoWrap()
						switch seconds := w.ResetsAt - time.Now().Unix(); {
						case w.UsedPercent <= 0:
							ui.Text(c, a.t("full", "满额")).FontSize(10).TextColor(trayMuted).NoWrap()
						case w.ResetsAt > 0 && seconds > 0:
							ui.Text(c, fmt.Sprintf(a.t("resets %s", "%s 重置"), trayCountdown(seconds))).FontSize(10).FontFeatures("tnum").TextColor(trayMuted).NoWrap()
						}
					})
					ui.Box(c).Height(3).FillWidth().Radius(1.5).Background(ui.Hex("#202737")).Draw(func(p *ui.Painter, r ui.Rect) {
						r.W *= float32(math.Min(1, w.UsedPercent/100))
						p.Fill(r, color, 1.5)
					})
				})
			}
		})
		if ar.result.Reset != nil {
			ui.Row(c).Gap(8).Padding(6, 0, 0).AlignItems(ui.Center).BorderWidth(1, 0, 0, 0).BorderColor(ui.Hex("#1f2535")).Children(func() {
				a.claudeResetControls(c, ar.account, *ar.result.Reset, ar.result.Windows, true)
			})
			if a.quotaUI.resetResult != "" {
				ui.Text(c, a.quotaUI.resetResult).FontSize(10).TextColor(traySecondary)
			}
		}
	})
}
