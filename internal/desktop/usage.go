package desktop

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/model"
)

func (a *App) usageView(c *ui.Context) {
	bs := analytics.Build(a.allData(), a.period, a.page, a.agent, a.device, time.Now())
	var total analytics.Bucket
	for _, b := range bs {
		total.Input += b.Input
		total.Output += b.Output
		total.Cached += b.Cached
		total.CacheWrite += b.CacheWrite
		total.Cost += b.Cost
		total.Turns += b.Turns
	}
	start, end := analytics.Window(a.period, a.page, time.Now())
	sessions := map[string]bool{}
	for _, t := range a.allData().Turns {
		if t.Agent == a.agent && (a.device == "" || a.device == t.DeviceID) && t.CreatedAt >= start.Unix() && t.CreatedAt < end.Unix() {
			sessions[t.DeviceID+"/"+t.SessionKey] = true
		}
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(6, 6).Radius(3).Background(c.Theme().Accent)
		ui.Text(c, a.t("Period: ", "统计周期: ")+start.Format("01-02")+" ~ "+end.AddDate(0, 0, -1).Format("01-02")).FontSize(12).TextColor(c.Theme().TextMuted)
	})
	ui.Row(c).Wrap().Gap(8).AlignItems(ui.Stretch).Children(func() {
		costLabel := a.t("Cost (USD)", "费用 (USD)")
		if a.agent == model.Devin {
			costLabel = a.t("Period Cost (USD · Pro)", "本期费用 (USD · Pro)")
		}
		for _, v := range []struct{ label, value, color string }{{a.t("Total Tokens", "总 Tokens"), formatTokens(total.Total()), "#e6e6ee"}, {a.t("Input (new)", "输入（新）"), formatTokens(total.Input), "#4cc2ff"}, {a.t("Output", "输出"), formatTokens(total.Output), "#3ddc97"}, {a.t("Cache Write", "缓存写入"), formatTokens(total.CacheWrite), "#c4b5fd"}, {a.t("Cache Read", "缓存读取"), formatTokens(total.Cached), "#8b7cf6"}, {costLabel, usageMoney(total.Cost), "#fbbf24"}, {a.t("Turns / Sessions", "轮次 / 会话"), fmt.Sprintf("%d / %d", total.Turns, len(sessions)), "#8f8fa3"}} {
			ui.Column(c).Grow(1).Basis(120).MinWidth(120).Padding(12).Gap(4).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border).Children(func() {
				ui.Text(c, v.label).FontSize(12).FixedLineHeight(20).TextColor(c.Theme().TextMuted)
				ui.Text(c, v.value).FontSize(18).FixedLineHeight(28).Bold().TextColor(ui.Hex(v.color))
			})
		}
	})
	if total.Turns == 0 {
		ui.Text(c, a.t("No usage in this period. Select another agent or previous period.", "此周期没有用量，请切换 Agent 或查看上一周期。")).FontSize(12).TextColor(c.Theme().TextMuted).Padding(8, 12).Background(c.Theme().Surface).Border(1, c.Theme().Border).Radius(6)
	}
	a.usageChart(c, bs)
	ui.Scroll(c.Key("period-details")).Grow(1).MinHeight(0).Gap(0).Label(a.t("Period details", "周期明细")).Children(func() {
		ui.Column(c).Padding(12).Gap(0).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border).Children(func() {
			ui.Row(c).AlignItems(ui.Center).Padding(0, 0, 8, 0).Children(func() {
				ui.Text(c, a.t("Period", "周期")).FontSize(12).Bold().Grow(1)
				mark := "☐"
				if a.hideEmptyBuckets {
					mark = "☑"
				}
				b := textButton(c, mark+"  "+a.t("Hide inactive dates", "隐藏无活动日期"), false).Label(a.t("Hide inactive dates", "隐藏无活动日期")).Height(24).Padding(2, 8)
				if a.hideEmptyBuckets {
					b.TextColor(c.Theme().Accent).Background(c.Theme().Accent.Alpha(.15))
				}
				if b.Clicked() {
					a.hideEmptyBuckets = !a.hideEmptyBuckets
				}
			})
			head := []string{a.t("Period", "周期"), a.t("Sessions", "会话"), a.t("Turns", "轮次"), a.t("Input", "输入"), a.t("Output", "输出"), a.t("Cache Write", "缓存写入"), a.t("Cache Read", "缓存读取"), a.t("Total", "总计"), a.t("Cost", "费用"), a.t("Models", "模型分布")}
			a.usageRow(c, head, true, false, nil, nil)
			visible := 0
			for i := len(bs) - 1; i >= 0; i-- {
				b := bs[i]
				if a.hideEmptyBuckets && b.Total() == 0 && b.Turns == 0 {
					continue
				}
				visible++
				names := sortedModels(b.ByModel)
				label := b.Label
				if a.period == analytics.Day {
					label += " " + time.Unix(b.Start, 0).Format("Mon")
				}
				if len(names) > 1 {
					label = a.sectionTitle(a.expandedBuckets[b.Start], label)
				} else {
					label = "    " + label
				}
				values := []string{label, fmt.Sprint(b.Sessions), fmt.Sprint(b.Turns), formatTokens(b.Input), formatTokens(b.Output), formatTokens(b.CacheWrite), formatTokens(b.Cached), formatTokens(b.Total()), usageMoney(b.Cost), ""}
				a.usageRow(c.Key(fmt.Sprint(b.Start)), values, false, b.Total() == 0, func() {
					if len(names) > 1 {
						a.expandedBuckets[b.Start] = !a.expandedBuckets[b.Start]
					}
				}, b.ByModel)
				if a.expandedBuckets[b.Start] && len(names) > 1 {
					a.modelBreakdown(c, b, names)
				}
			}
			if visible == 0 {
				ui.Text(c, a.t("No activity in this period", "当前周期无活动记录")).FontSize(12).TextColor(c.Theme().TextMuted).Padding(24, 0).FillWidth().TextAlign(ui.Center)
			}
		})
	})
}

func usageMoney(cost float64) string {
	if cost <= 0 {
		return "—"
	}
	return trayMoney(cost)
}

func (a *App) usageChart(c *ui.Context, bs []analytics.Bucket) {
	hi := 1.0
	for _, b := range bs {
		hi = math.Max(hi, b.Total())
	}
	ui.Column(c).Padding(12).Gap(8).Radius(6).Shrink(0).Background(c.Theme().Surface).Border(1, c.Theme().Border).Label(a.t("Stacked token usage chart", "Token 用量堆叠图")).Children(func() {
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.t("Token Usage Trend", "Token 用量趋势")).FontSize(14).FixedLineHeight(24).FontWeight(600)
			for _, v := range []struct{ en, zh, color string }{{"Cache Write", "缓存写入", "#c4b5fd"}, {"Cache Read", "缓存读取", "#8b7cf6"}, {"In", "输入", "#4cc2ff"}, {"Out", "输出", "#3ddc97"}} {
				ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
					ui.Box(c).Size(7, 7).Radius(4).Background(ui.Hex(v.color))
					ui.Text(c, a.t(v.en, v.zh)).FontSize(12).TextColor(c.Theme().TextMuted)
				})
			}
		})
		ui.Row(c).Gap(4).Children(func() {
			for _, b := range bs {
				ui.Column(c).Grow(1).Basis(0).MinWidth(0).Gap(4).AlignItems(ui.Center).Children(func() {
					label := "·"
					if b.Total() > 0 {
						label = formatTokens(b.Total())
					}
					color := c.Theme().Text
					if b.Total() == 0 {
						color = c.Theme().TextMuted
					}
					ui.Text(c, label).FontSize(12).FixedLineHeight(20).TextColor(color).TextAlign(ui.Center).FillWidth().NoWrap()
					ui.Column(c).Width(26).Height(140).Gap(0).Justify(ui.End).Children(func() {
						for j, v := range []float64{b.CacheWrite, b.Cached, b.Input, b.Output} {
							height := float32(0)
							if v > 0 {
								height = float32(math.Max(2, v/hi*140))
							}
							colors := []string{"#c4b5fd", "#8b7cf6", "#4cc2ff", "#3ddc97"}
							ui.Box(c).FillWidth().Height(height).Shrink(0).Background(ui.Hex(colors[j]).Alpha(.9))
						}
					})
					ui.Text(c, b.Label).FontSize(12).TextColor(c.Theme().TextMuted).FillWidth().TextAlign(ui.Center).NoWrap()
				})
			}
		})
	})
}

func sortedModels(models map[string]analytics.ModelUsage) []string {
	names := make([]string, 0, len(models))
	for n, m := range models {
		if m.Total() > 0 {
			names = append(names, n)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if models[names[i]].Total() == models[names[j]].Total() {
			return names[i] < names[j]
		}
		return models[names[i]].Total() > models[names[j]].Total()
	})
	return names
}

func modelColor(name string) ui.Color {
	var hash uint64
	for _, b := range []byte(name) {
		hash = hash*31 + uint64(b)
	}
	colors := [...]string{"#f472b6", "#fbbf24", "#60a5fa", "#34d399", "#a78bfa", "#fb923c", "#f87171", "#4ade80", "#22d3ee", "#facc15"}
	return ui.Hex(colors[hash%uint64(len(colors))])
}

func (a *App) usageRow(c *ui.Context, values []string, heading, inactive bool, onClick func(), models map[string]analytics.ModelUsage) {
	r := ui.Row(c).Padding(4, 8).Gap(8).FontSize(12).FixedLineHeight(20).AlignItems(ui.Center).BorderWidth(0, 0, 1, 0).BorderColor(c.Theme().Border)
	if inactive {
		r.TextColor(c.Theme().TextMuted.Alpha(.5))
	}
	if onClick != nil {
		r.Focusable().Label(values[0])
		if r.Hovered() {
			r.Background(c.Theme().SurfaceHover)
		}
		if r.Clicked() {
			onClick()
		}
	}
	r.Children(func() {
		widths := []float32{90, 60, 52, 70, 70, 76, 76, 76, 70}
		for i, v := range values {
			if i == 0 && !heading {
				ui.Row(c).Width(90).Shrink(0).Gap(4).AlignItems(ui.Center).Children(func() {
					arrow := ""
					if strings.HasPrefix(v, "▸") {
						arrow = "▸"
					} else if strings.HasPrefix(v, "▾") {
						arrow = "▾"
					}
					ui.Text(c, arrow).Width(10).Shrink(0).TextAlign(ui.Center)
					ui.Text(c, strings.TrimLeft(v, "▸▾ ")).Grow(1).MinWidth(0).NoWrap()
				})
				continue
			}
			if i == 9 && !heading {
				a.modelSummary(c, models)
				continue
			}
			e := ui.Text(c, v).FontSize(12).NoWrap().Ellipsis("…")
			if i < len(widths) {
				e.Width(widths[i]).Shrink(0)
				if i > 0 {
					e.TextAlign(ui.End)
				}
			} else {
				e.Grow(1).MinWidth(0)
			}
			if heading {
				e.TextColor(c.Theme().TextMuted)
			}
		}
	})
}

func (a *App) modelSummary(c *ui.Context, models map[string]analytics.ModelUsage) {
	names := sortedModels(models)
	ui.Row(c).Grow(1).MinWidth(0).Gap(4).AlignItems(ui.Center).Clip().Children(func() {
		if len(names) == 0 {
			ui.Text(c, "-").FontSize(12).TextColor(c.Theme().TextMuted)
			return
		}
		total := 0.0
		for _, name := range names {
			total += models[name].Total()
		}
		if len(names) > 1 {
			ui.Row(c).Size(48, 5).Shrink(0).Gap(0).Radius(2.5).Clip().Children(func() {
				for _, name := range names {
					ui.Box(c).Width(float32(math.Max(2, math.Round(models[name].Total()/total*48)))).FillHeight().Shrink(0).Background(modelColor(name))
				}
			})
		}
		ui.Box(c).Size(6, 6).Shrink(0).Radius(3).Background(modelColor(names[0]))
		label := names[0]
		if len(names) == 1 {
			label += " " + formatTokens(models[names[0]].Total())
		}
		ui.Text(c, label).MinWidth(0).FontSize(12).NoWrap().Ellipsis("…")
		if len(names) > 1 {
			ui.Text(c, fmt.Sprintf("%.0f%%", models[names[0]].Total()/total*100)).Shrink(0).FontSize(12).TextColor(c.Theme().TextMuted)
			ui.Text(c, fmt.Sprintf("+%d", len(names)-1)).Shrink(0).FontSize(12).Padding(.5, 4).Radius(2).Background(ui.Hex("#272733")).TextColor(c.Theme().TextMuted)
		}
	})
}

func (a *App) modelBreakdown(c *ui.Context, b analytics.Bucket, names []string) {
	ui.Column(c).Padding(8, 12).Gap(0).Background(ui.Hex("#131318")).BorderWidth(0, 0, 1, 0).BorderColor(c.Theme().Border).Label(a.t("Model breakdown", "模型明细")).Children(func() {
		ui.Column(c).Padding(0, 0, 0, 12).Gap(4).BorderWidth(0, 0, 0, 2).BorderColor(c.Theme().Accent).Children(func() {
			ui.Row(c).Gap(12).AlignItems(ui.Center).Padding(0, 0, 4, 0).BorderWidth(0, 0, 1, 0).BorderColor(ui.Hex("#242430")).Children(func() {
				ui.Text(c, b.Label+" "+a.t("Model details", "模型明细")).FontSize(12).FontWeight(500)
				ui.Text(c, fmt.Sprintf(a.t("%d models", "共 %d 个模型"), len(names))).FontSize(12).TextColor(c.Theme().TextMuted)
			})
			headers := []string{a.t("Model", "模型"), a.t("Share", "占比"), a.t("Turns", "轮次"), a.t("Input", "输入"), a.t("Output", "输出"), a.t("Cache Write", "缓存写入"), a.t("Cache Read", "缓存读取"), a.t("Total", "总计"), a.t("Cost", "费用")}
			widths := []float32{220, 56, 52, 70, 70, 76, 76, 76, 70}
			ui.Row(c).Gap(8).Padding(4, 0).BorderWidth(0, 0, 1, 0).BorderColor(ui.Hex("#242430")).Children(func() {
				for i, v := range headers {
					e := ui.Text(c, v).Width(widths[i]).Shrink(0).FontSize(12).TextColor(c.Theme().TextMuted)
					if i > 0 {
						e.TextAlign(ui.End)
					}
				}
			})
			for _, name := range names {
				m := b.ByModel[name]
				ui.Row(c).Gap(8).Padding(4, 0).AlignItems(ui.Center).Children(func() {
					ui.Row(c).Width(220).Shrink(0).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Box(c).Size(6, 6).Shrink(0).Radius(3).Background(modelColor(name))
						ui.Text(c, name).MinWidth(0).FontSize(12).NoWrap().Ellipsis("…")
					})
					cost := usageMoney(m.Cost)
					if m.Cost == 0 && m.Priced {
						cost = "$0.00"
					}
					share := fmt.Sprintf("%.1f%%", 100*m.Total()/math.Max(1, b.Total()))
					if m.Total()/math.Max(1, b.Total()) < .001 {
						share = "<0.1%"
					}
					values := []string{share, fmt.Sprint(m.Turns), formatTokens(m.Input), formatTokens(m.Output), formatTokens(m.CacheWrite), formatTokens(m.Cached), formatTokens(m.Total()), cost}
					for i, v := range values {
						e := ui.Text(c, v).Width(widths[i+1]).Shrink(0).FontSize(12).TextAlign(ui.End)
						if i == 0 {
							e.TextColor(c.Theme().TextMuted)
						} else if i == 7 {
							e.TextColor(ui.Hex("#3ddc97"))
						}
					}
				})
			}
		})
	})
}
