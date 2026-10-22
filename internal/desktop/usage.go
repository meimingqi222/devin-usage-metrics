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
	d := a.allData()
	now := time.Now()
	start, end := analytics.Window(a.period, a.page, now)
	cache := &a.usageCache
	if cache.buckets == nil || cache.period != a.period || cache.page != a.page || cache.device != a.device || cache.start != start.Unix() {
		cache.buckets = analytics.Build(d, a.period, a.page, a.agent, a.device, now)
		cache.period, cache.page, cache.device, cache.start = a.period, a.page, a.device, start.Unix()
	}
	bs := cache.buckets
	var total analytics.Bucket
	for _, b := range bs {
		total.Input += b.Input
		total.Output += b.Output
		total.Cached += b.Cached
		total.CacheWrite += b.CacheWrite
		total.Cost += b.Cost
		total.Turns += b.Turns
	}
	sessions := map[string]bool{}
	for _, t := range d.Turns {
		if t.Agent == a.agent && (a.device == "" || a.device == t.DeviceID) && t.CreatedAt >= start.Unix() && t.CreatedAt < end.Unix() {
			sessions[t.DeviceID+"/"+t.SessionKey] = true
		}
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(6, 6).Radius(3).Background(c.Theme().Accent)
		ui.Text(c, a.t("Period: ", "统计周期: ")+start.Format("01-02")+" ~ "+end.AddDate(0, 0, -1).Format("01-02")).FontSize(12).TextColor(c.Theme().TextMuted)
	})
	a.usageSummary(c, total, len(sessions))
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

// Series colors shared by the summary cards and the chart. Cache writes are
// pink so they stand apart from cache reads on the stacked bars.
var (
	inputColor      = ui.Hex("#4cc2ff")
	outputColor     = ui.Hex("#3ddc97")
	cacheWriteColor = ui.Hex("#f472b6")
	cacheReadColor  = ui.Hex("#8b7cf6")
	costColor       = ui.Hex("#fbbf24")
)

type summaryCard struct {
	label, value, note string
	color              ui.Color
	primary            bool
}

// usageSummary leads with cost and volume, the figures people act on; the
// token breakdown follows in smaller cards whose notes give each number a
// meaning (its share, the cache hit rate, turns per session).
func (a *App) usageSummary(c *ui.Context, total analytics.Bucket, sessions int) {
	t := c.Theme()
	share := func(v float64) string {
		if total.Total() <= 0 {
			return ""
		}
		return fmt.Sprintf(a.t("%.1f%% of total", "占总量 %.1f%%"), v/total.Total()*100)
	}
	hitRate := ""
	if prompt := total.Input + total.Cached + total.CacheWrite; prompt > 0 {
		hitRate = fmt.Sprintf(a.t("%.0f%% hit rate", "命中率 %.0f%%"), total.Cached/prompt*100)
	}
	costNote := a.t("USD · API estimate", "USD · 按 API 价格估算")
	if a.agent == model.Devin {
		costNote = a.t("USD · Pro quote", "USD · 按 Pro 报价估算")
	}
	perSession := ""
	if sessions > 0 {
		perSession = fmt.Sprintf(a.t("%.0f turns per session", "平均每会话 %.0f 轮"), float64(total.Turns)/float64(sessions))
	}
	cards := []summaryCard{
		{a.t("Cost", "费用"), usageMoney(total.Cost), costNote, costColor, true},
		{a.t("Total Tokens", "总 Tokens"), formatTokens(total.Total()), fmt.Sprintf(a.t("%s without cache reads", "不含缓存读取 %s"), formatTokens(total.Total()-total.Cached)), t.Text, true},
		{a.t("Input (new)", "输入（新）"), formatTokens(total.Input), share(total.Input), inputColor, false},
		{a.t("Output", "输出"), formatTokens(total.Output), share(total.Output), outputColor, false},
		{a.t("Cache Write", "缓存写入"), formatTokens(total.CacheWrite), share(total.CacheWrite), cacheWriteColor, false},
		{a.t("Cache Read", "缓存读取"), formatTokens(total.Cached), hitRate, cacheReadColor, false},
		{a.t("Turns / Sessions", "轮次 / 会话"), fmt.Sprintf("%d / %d", total.Turns, sessions), perSession, t.Text, false},
	}
	ui.Row(c).Wrap().Gap(8).AlignItems(ui.Stretch).Label(a.t("Usage summary", "用量概览")).Children(func() {
		for _, v := range cards {
			grow, basis, size := float32(1), float32(112), float32(18)
			if v.primary {
				grow, basis, size = 1.5, 150, 24
			}
			ui.Column(c).Grow(grow).Basis(basis).MinWidth(basis).Padding(10, 12).Gap(2).Radius(6).Background(t.Surface).Border(1, t.Border).Children(func() {
				ui.Text(c, v.label).FontSize(12).FixedLineHeight(18).TextColor(t.TextMuted).NoWrap().Ellipsis("…")
				ui.Text(c, v.value).FontSize(size).FixedLineHeight(size + 8).Bold().TextColor(v.color).NoWrap()
				if v.note != "" {
					ui.Text(c, v.note).FontSize(11).FixedLineHeight(16).TextColor(t.TextMuted).NoWrap().Ellipsis("…")
				}
			})
		}
	})
}

// Chart modes. Cost is the default: cache reads are most of the tokens, so a
// token chart mostly shows them, while cost is what changes behaviour.
const (
	chartCost = iota
	chartTokens
	chartNewTokens
)

type chartSeries struct {
	en, zh string
	color  ui.Color
	value  func(analytics.Bucket) float64
}

func (a *App) usageChart(c *ui.Context, bs []analytics.Bucket) {
	t := c.Theme()
	// Listed top to bottom, the order the segments stack in each bar.
	var series []chartSeries
	switch a.chartMode {
	case chartCost:
		series = []chartSeries{{"Cost", "费用", costColor, func(b analytics.Bucket) float64 { return b.Cost }}}
	default:
		series = []chartSeries{{"Cache Write", "缓存写入", cacheWriteColor, func(b analytics.Bucket) float64 { return b.CacheWrite }}}
		if a.chartMode == chartTokens {
			series = append(series, chartSeries{"Cache Read", "缓存读取", cacheReadColor, func(b analytics.Bucket) float64 { return b.Cached }})
		}
		series = append(series,
			chartSeries{"Input", "输入", inputColor, func(b analytics.Bucket) float64 { return b.Input }},
			chartSeries{"Output", "输出", outputColor, func(b analytics.Bucket) float64 { return b.Output }})
	}
	sum := func(b analytics.Bucket) float64 {
		v := 0.0
		for _, s := range series {
			v += s.value(b)
		}
		return v
	}
	label := formatTokens
	if a.chartMode == chartCost {
		label = usageMoney
	}
	hi := 0.0
	for _, b := range bs {
		hi = math.Max(hi, sum(b))
	}
	const barHeight = 140
	ui.Column(c).Padding(12).Gap(8).Radius(6).Shrink(0).Background(t.Surface).Border(1, t.Border).Label(a.t("Stacked token usage chart", "Token 用量堆叠图")).Children(func() {
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			title := a.t("Daily Cost", "费用趋势")
			if a.chartMode != chartCost {
				title = a.t("Token Usage Trend", "Token 用量趋势")
			}
			ui.Text(c, title).FontSize(14).FixedLineHeight(24).FontWeight(600)
			if len(series) > 1 {
				for _, s := range series {
					ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
						ui.Box(c).Size(7, 7).Radius(4).Background(s.color)
						ui.Text(c, a.t(s.en, s.zh)).FontSize(12).TextColor(t.TextMuted)
					})
				}
			}
			ui.Spacer(c)
			ui.Segmented(c, &a.chartMode, a.t("Cost", "费用"), "Tokens", a.t("Without cache reads", "不含缓存读取")).FontSize(12).Label(a.t("Chart metric", "图表指标"))
		})
		if hi <= 0 {
			msg := a.t("No usage in this period", "此周期没有用量")
			if a.chartMode == chartCost {
				msg = a.t("No priced usage in this period; switch to Tokens", "此周期没有可计价的用量，可切换到 Tokens 查看")
			}
			ui.Text(c, msg).FontSize(12).TextColor(t.TextMuted).FillWidth().TextAlign(ui.Center).Padding(barHeight/2-10, 0)
			return
		}
		ui.Row(c).Gap(4).Children(func() {
			for _, b := range bs {
				total := sum(b)
				ui.Column(c).Grow(1).Basis(0).MinWidth(0).Gap(4).AlignItems(ui.Center).Children(func() {
					// The value sits on top of its own bar, not on a shared row.
					ui.Column(c).Height(barHeight + 24).FillWidth().Gap(4).AlignItems(ui.Center).Justify(ui.End).Children(func() {
						if total <= 0 {
							return
						}
						ui.Text(c, label(total)).FontSize(11).FixedLineHeight(16).TextColor(t.Text).TextAlign(ui.Center).FillWidth().NoWrap()
						ui.Column(c).Width(26).Gap(0).Children(func() {
							for _, s := range series {
								if v := s.value(b); v > 0 {
									ui.Box(c).FillWidth().Height(float32(math.Max(2, v/hi*barHeight))).Shrink(0).Background(s.color.Alpha(.9))
								}
							}
						})
					})
					ui.Text(c, b.Label).FontSize(12).TextColor(t.TextMuted).FillWidth().TextAlign(ui.Center).NoWrap()
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
