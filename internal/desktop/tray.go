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

var (
	trayPanel  = ui.Hex("#181820")
	trayPanel2 = ui.Hex("#22222b")
	trayBorder = ui.Hex("#2c2c38")
	trayText   = ui.Hex("#e6e6ee")
	trayMuted  = ui.Hex("#8f8fa3")
	// Same vector paths as assets/icons/{refresh,close}.svg; glyphs vary by font.
	refreshIcon = ui.MustParseSVG([]byte(`<svg viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M13.5 8A5.5 5.5 0 0 1 3.2 10.5M2.5 8A5.5 5.5 0 0 1 12.8 5.5" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/><path d="M13.5 4.5V8H10M2.5 11.5V8H6" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/></svg>`))
	closeIcon   = ui.MustParseSVG([]byte(`<svg viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M4 4L12 12M12 4L4 12" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>`))
)

// Rust's controls are plain divs: no native button padding, minimum size,
// shadow, or raised hover treatment. Text inherits the control typography.
func trayButton(c *ui.Context, label string) ui.Element {
	b := ui.ButtonBase(c).Label(label).Radius(3).TextColor(trayMuted)
	if b.Hovered() {
		b.Background(trayPanel2).TextColor(trayText)
	}
	b.Children(func() { ui.Text(c, label).NoWrap() })
	return b
}

type trayCostItem struct {
	name                     string
	color                    ui.Color
	cost, tokens, percentage float64
}

func trayAgentColor(agent model.Agent) ui.Color {
	colors := map[model.Agent]string{model.Claude: "#d97757", model.Codex: "#3ddc97", model.Devin: "#4cc2ff", model.Antigravity: "#8b5cf6", model.OpenCode: "#fbbf24"}
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

// Like compute_cost_overview, aggregate local turns by agent, not by model,
// dashboard selection, or synced device. Never perform disk/network IO in a view.
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
		for _, turn := range a.loaded[agent].Turns {
			if turn.CreatedAt < start.Unix() || turn.CreatedAt > end.Unix() || (turn.DeviceID != "" && a.localID != "" && turn.DeviceID != a.localID) {
				continue
			}
			v, _ := pricing.Cost(turn, turn.Model)
			item.cost += v
			item.tokens += turn.InputTokens + turn.OutputTokens + turn.CacheReadTokens + turn.CacheCreationTokens
		}
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
	ui.Column(c).Fill().LineHeight(1.5).Background(ui.Hex("#101014")).Border(1, trayBorder).Radius(12).Padding(12).Gap(6).Children(func() {
		ui.Row(c).Shrink(0).Gap(8).Padding(0, 0, 6).BorderWidth(0, 0, 1, 0).BorderColor(trayBorder).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.t("Subscription Quotas", "订阅配额监控")).FontSize(12).Bold().TextColor(trayText)
			status := ""
			if loading || overviewLoading {
				status = a.t("Updating…", "正在更新…")
			} else if a.trayUI.updatedAt != "" {
				status = a.t("Updated ", "更新于 ") + a.trayUI.updatedAt
			}
			ui.Text(c, status).FontSize(10).TextColor(trayMuted).Grow(1)
			refresh := ui.ButtonBase(c.Key("tray-refresh")).Label(a.t("Refresh Quotas", "刷新配额")).Padding(4).TextColor(trayMuted).Children(func() { ui.Icon(c, refreshIcon).Size(13, 13) })
			if refresh.Hovered() {
				refresh.Background(trayPanel2).TextColor(trayText)
			}
			if refresh.Clicked() {
				a.refreshQuota(true)
			}
			close := ui.ButtonBase(c.Key("tray-close")).Label(a.t("Close", "关闭")).Padding(4).TextColor(trayMuted).Children(func() { ui.Icon(c, closeIcon).Size(13, 13) })
			if close.Hovered() {
				close.Background(trayPanel2).TextColor(trayText)
			}
			if close.Clicked() && a.trayUI.closePopup != nil {
				a.trayUI.closePopup()
			}
		})
		ui.Scroll(c.Key("tray-scroll")).Grow(1).MinHeight(0).Gap(8).Padding(4, 0).Children(func() {
			a.trayOverviewView(c, overviewLoading)
			ui.Text(c, a.t("LIVE QUOTAS & LIMITS", "实时配额与限额")).FontSize(10).Bold().TextColor(trayMuted).Margin(4, 0, 0)
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
				ui.Column(c).Padding(8).Gap(4).Radius(6).Background(trayPanel).Border(1, trayBorder).Children(func() {
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
								ui.Text(c, label).FontSize(9.5).TextColor(trayMuted).Background(trayPanel2).Radius(3).Padding(1, 8)
							}
						})
					}
				})
			}
		})
		ui.Row(c).Shrink(0).AlignItems(ui.Center).Padding(6, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(trayBorder).Children(func() {
			ui.Text(c, "Agent Metrics v"+updater.CurrentVersion).FontSize(10).TextColor(trayMuted).Grow(1)
			if trayButton(c.Key("tray-dashboard"), a.t("Open Dashboard", "打开控制台")+" →").FontSize(10.5).FontWeight(500).TextColor(ui.Hex("#4cc2ff")).Clicked() && a.trayUI.openDashboard != nil {
				a.trayUI.openDashboard()
			}
		})
	})
}

func (a *App) trayOverviewView(c *ui.Context, loading bool) {
	items, cost, tokens := a.trayOverview(time.Now())
	ui.Column(c).Padding(10).Gap(8).Radius(8).Background(trayPanel).Border(1, trayBorder).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.t("Cost & Usage", "费用与用量")).FontSize(12).FontWeight(600).Grow(1)
			ui.Row(c).Background(trayPanel2).Padding(2).Radius(6).Children(func() {
				for _, p := range []struct {
					id     int
					en, zh string
				}{{1, "Today", "今天"}, {2, "Yesterday", "昨天"}, {0, "30 Days", "30 天"}} {
					b := trayButton(c.Key(fmt.Sprint("tray-period-", p.id)), a.t(p.en, p.zh)).FontSize(10).Padding(1.5, 8).Radius(2)
					if a.trayUI.period == p.id {
						b.Background(ui.Hex("#353542")).TextColor(trayText).FontWeight(600)
					}
					if b.Clicked() {
						a.trayUI.period = p.id
					}
				}
			})
		})
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			if loading && len(items) == 0 {
				ui.Text(c, "↻ "+a.t("Loading usage records…", "正在加载用量记录…")).Height(76).FontSize(11).TextColor(trayMuted)
				return
			}
			ui.Column(c).Width(76).Height(76).Shrink(0).Justify(ui.Center).AlignItems(ui.Center).Draw(func(p *ui.Painter, r ui.Rect) {
				draw := func(start, end float64, color ui.Color) {
					path := &ui.Path{}
					// A filled annular segment preserves GPUI's flat stroke ends.
					for i := 0; i <= 16; i++ {
						angle := start + (end-start)*float64(i)/16
						x, y := r.X+38+35.5*float32(math.Cos(angle)), r.Y+38+35.5*float32(math.Sin(angle))
						if i == 0 {
							path.MoveTo(x, y)
						} else {
							path.LineTo(x, y)
						}
					}
					for i := 16; i >= 0; i-- {
						angle := start + (end-start)*float64(i)/16
						path.LineTo(r.X+38+30.5*float32(math.Cos(angle)), r.Y+38+30.5*float32(math.Sin(angle)))
					}
					path.Close()
					p.FillPath(path, color)
				}
				if cost <= 0 || len(items) == 0 {
					draw(0, 2*math.Pi, ui.Hex("#282834"))
					return
				}
				angle := -math.Pi / 2
				for _, item := range items {
					sweep := item.percentage / 100 * 2 * math.Pi
					if sweep < .08 {
						continue
					}
					start, end := angle+.06, angle+sweep-.06
					angle += sweep
					if end > start {
						draw(start, end, item.color)
					}
				}
			}).Children(func() {
				value, label := "$0.00", a.t("tokens", "Token")
				if cost > 0 {
					value, label = trayMoney(cost), a.t("dollars", "美元")
				} else if tokens > 0 {
					value = fmt.Sprintf("%.1fM", tokens/1e6)
				}
				ui.Text(c, value).Width(50).NoWrap().TextAlign(ui.Center).FontSize(11).FixedLineHeight(14).Bold()
				ui.Text(c, label).FontSize(9).FixedLineHeight(12).TextColor(trayMuted)
			})
			ui.Column(c).Grow(1).MinWidth(0).Gap(3.5).Children(func() {
				if len(items) == 0 {
					ui.Text(c, a.t("No session records in this period", "此周期内暂无会话记录")).FontSize(11).TextColor(trayMuted)
				}
				for _, item := range items[:min(4, len(items))] {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Box(c).Width(6.5).Height(6.5).Radius(4).Background(item.color)
						ui.Text(c, item.name).FontSize(11).Grow(1)
						value := fmt.Sprintf("%.1fK", item.tokens/1e3)
						if item.cost > 0 {
							value = trayMoney(item.cost)
						}
						ui.Text(c, value).FontSize(11).FontWeight(600)
						ui.Text(c, fmt.Sprintf("(%.0f%%)", item.percentage)).FontSize(10).TextColor(trayMuted)
					})
				}
			})
		})
	})
}

func (a *App) trayDenseCard(c *ui.Context, ar accountResult) {
	provider := providerName(ar.account.Provider)
	if ar.account.Provider == "claude" {
		provider = "Claude Code"
	}
	title := provider
	if strings.TrimSpace(ar.account.Label) != "" && ar.account.Label != provider {
		if strings.Contains(ar.account.Label, "@") {
			title = ar.account.Label
		} else {
			title = provider + " (" + ar.account.Label + ")"
		}
	}
	ui.Column(c.Key("tray-card/"+ar.account.Key)).Padding(10).Gap(7).Radius(8).Background(trayPanel).Border(1, trayBorder).Children(func() {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Width(6.5).Height(6.5).Shrink(0).Radius(4).Background(trayAgentColor(model.Agent(ar.account.Provider)))
			ui.Text(c, title).FontSize(11.5).FontWeight(600).Grow(1).MinWidth(0).NoWrap()
			if ar.result.Plan != "" {
				plan, color := ar.result.Plan, ui.Hex("#4cc2ff")
				if plan == "API MODE" {
					plan, color = a.t("API Mode", "API 模式"), ui.Hex("#34d399")
				}
				ui.Text(c, plan).FontSize(10).TextColor(color).Background(trayPanel2).Radius(3).Padding(1, 6)
			}
		})
		for _, w := range ar.result.Windows {
			remaining := math.Max(0, math.Min(100, 100-w.UsedPercent))
			color := ui.Hex("#f87171")
			if remaining > 50 {
				color = ui.Hex("#3ddc97")
			} else if remaining > 20 {
				color = ui.Hex("#fbbf24")
			}
			ui.Column(c).Gap(3).Children(func() {
				ui.Row(c).Children(func() {
					ui.Text(c, a.trayWindowLabel(w.Label)).FontSize(10.5).Grow(1)
					label := fmt.Sprintf(a.t("%.0f%% left", "剩余 %.0f%%"), remaining)
					if remaining < 20 {
						label = fmt.Sprintf(a.t("Limit alert · %.0f%% left", "额度预警 · 剩余 %.0f%%"), remaining)
					}
					size := float32(10.5)
					if remaining < 20 {
						size = 10
					}
					ui.Text(c, label).FontSize(size).FontWeight(600).TextColor(color)
				})
				ui.Box(c).Height(3).FillWidth().Radius(2).Background(trayPanel2).Children(func() { ui.Box(c).Height(3).WidthPercent(float32(remaining)).Radius(2).Background(color) })
				ui.Row(c).Children(func() {
					ui.Text(c, fmt.Sprintf(a.t("Used: %.1f%%", "已用 %.1f%%"), w.UsedPercent)).FontSize(10).TextColor(trayMuted).Grow(1)
					seconds := w.ResetsAt - time.Now().Unix()
					if w.ResetsAt > 0 && seconds > 0 {
						countdown := fmt.Sprintf(a.t("%dm", "%d分钟"), seconds/60)
						if seconds >= 86400 {
							countdown = fmt.Sprintf(a.t("%dd %dh", "%d天%d小时"), seconds/86400, seconds%86400/3600)
						} else if seconds >= 3600 {
							countdown = fmt.Sprintf(a.t("%dh %dm", "%d小时%d分"), seconds/3600, seconds%3600/60)
						}
						ui.Text(c, fmt.Sprintf(a.t("resets in %s", "%s 后重置"), countdown)).FontSize(10).TextColor(trayMuted)
					}
				})
			})
		}
	})
}

func (a *App) trayWindowLabel(label string) string {
	switch label {
	case "5h":
		label = "5-hour"
	case "7d":
		label = "7-day"
	case "7d OAuth apps":
		label = "7-day OAuth apps"
	case "7d Opus":
		label = "7-day Opus"
	case "7d Sonnet":
		label = "7-day Sonnet"
	}
	labels := map[string][2]string{"5-hour": {"5-hour window", "5 小时窗口"}, "7-day": {"7-day window", "7 天窗口"}, "7-day OAuth apps": {"7-day · OAuth apps", "7 天 · OAuth 应用"}, "7-day Opus": {"7-day · Opus", "7 天 · Opus"}, "7-day Sonnet": {"7-day · Sonnet", "7 天 · Sonnet"}, "Daily": {"Daily quota", "日配额"}, "Weekly": {"Weekly quota", "周配额"}, "Monthly": {"Monthly quota", "月配额"}}
	if text, ok := labels[label]; ok {
		return a.t(text[0], text[1])
	}
	return label
}
