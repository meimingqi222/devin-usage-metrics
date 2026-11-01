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

// usageData caches all overview aggregates with the bucket data. Mouse hover
// and chart-mode redraws must not rescan the full turn history.
func (a *App) usageData(now time.Time) (time.Time, time.Time) {
	d := a.allData()
	start, end := analytics.Window(a.period, a.page, now)
	cache := &a.usageUI.cache
	if cache.buckets != nil && cache.period == a.period && cache.page == a.page && cache.device == a.device && cache.start == start.Unix() {
		return start, end
	}
	cache.buckets = analytics.Build(d, a.period, a.page, a.agent, a.device, now)
	cache.period, cache.page, cache.device, cache.start = a.period, a.page, a.device, start.Unix()
	var total analytics.Bucket
	for _, b := range cache.buckets {
		total.Input += b.Input
		total.Output += b.Output
		total.Cached += b.Cached
		total.CacheWrite += b.CacheWrite
		total.Cost += b.Cost
		total.TokenCost += b.TokenCost
		total.UncachedCost += b.UncachedCost
		total.Turns += b.Turns
	}
	sessions := map[sessionIdentity]bool{}
	for _, t := range d.Turns {
		if t.Agent == a.agent && (a.device == "" || a.device == t.DeviceID) && t.CreatedAt >= start.Unix() && t.CreatedAt < end.Unix() {
			sessions[sessionIdentity{t.Agent, t.DeviceID, t.SessionKey}] = true
		}
	}
	cache.total, cache.sessions = total, len(sessions)
	return start, end
}

// usageUIState is the usage page's own state and its derived-data cache.
type usageUIState struct {
	chartMode        int
	hideEmptyBuckets bool
	expandedBuckets  map[int64]bool
	cache            struct {
		period   analytics.Period
		page     int
		device   string
		start    int64
		buckets  []analytics.Bucket
		total    analytics.Bucket
		sessions int
	}
}

func (a *App) usageView(c *ui.Context) {
	start, end := a.usageData(time.Now())
	bs, total := a.usageUI.cache.buckets, a.usageUI.cache.total
	a.usageHeader(c, start, end)
	a.usageSummary(c, total, a.usageUI.cache.sessions)
	if total.Turns == 0 {
		ui.Text(c, a.t("No usage in this period. Select another agent or previous period.", "此周期没有用量，请切换 Agent 或查看上一周期。")).FontSize(12).TextColor(c.Theme().TextMuted).Padding(8, 12).Background(c.Theme().Surface).Border(1, c.Theme().Border).Radius(6)
	}
	a.usageChart(c, bs)
	ui.Scroll(c.Key("period-details")).Grow(1).MinHeight(0).Gap(0).Label(a.t("Period details", "周期明细")).Children(func() {
		ui.Column(c).Padding(12).Gap(0).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border).Children(func() {
			ui.Row(c).AlignItems(ui.Center).Padding(0, 0, 8, 0).Children(func() {
				ui.Text(c, a.t("Period", "周期")).FontSize(12).Bold().Grow(1)
				mark := "☐"
				if a.usageUI.hideEmptyBuckets {
					mark = "☑"
				}
				b := textButton(c, mark+"  "+a.t("Hide inactive dates", "隐藏无活动日期"), false).Label(a.t("Hide inactive dates", "隐藏无活动日期")).Height(24).Padding(2, 8)
				if a.usageUI.hideEmptyBuckets {
					b.TextColor(c.Theme().Accent).Background(c.Theme().Accent.Alpha(.15))
				}
				if b.Clicked() {
					a.usageUI.hideEmptyBuckets = !a.usageUI.hideEmptyBuckets
				}
			})
			head := []string{a.t("Period", "周期"), a.t("Sessions", "会话"), a.t("Turns", "轮次"), a.t("Input", "输入"), a.t("Output", "输出"), a.t("Cache Write", "缓存写入"), a.t("Cache Read", "缓存读取"), a.t("Total", "总计"), a.t("Cost", "费用"), a.t("Models", "模型分布")}
			a.usageRow(c, head, true, false, nil, nil)
			visible := 0
			for i := len(bs) - 1; i >= 0; i-- {
				b := bs[i]
				if a.usageUI.hideEmptyBuckets && b.Total() == 0 && b.Turns == 0 {
					continue
				}
				visible++
				names := sortedModels(b.ByModel)
				label := b.Label
				if a.period == analytics.Day {
					label += " " + time.Unix(b.Start, 0).Format("Mon")
				}
				if len(names) > 1 {
					label = a.sectionTitle(a.usageUI.expandedBuckets[b.Start], label)
				} else {
					label = "    " + label
				}
				values := []string{label, fmt.Sprint(b.Sessions), fmt.Sprint(b.Turns), formatTokens(b.Input), formatTokens(b.Output), formatTokens(b.CacheWrite), formatTokens(b.Cached), formatTokens(b.Total()), usageMoney(b.Cost), ""}
				a.usageRow(c.Key(fmt.Sprint(b.Start)), values, false, b.Total() == 0, func() {
					if len(names) > 1 {
						a.usageUI.expandedBuckets[b.Start] = !a.usageUI.expandedBuckets[b.Start]
					}
				}, b.ByModel)
				if a.usageUI.expandedBuckets[b.Start] && len(names) > 1 {
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

// A telemetry palette without purple, shared by the cards and the chart:
// cache reads deep blue, cache writes slate, new input cyan, output green.
var (
	cacheReadColor  = ui.Hex("#2563eb")
	cacheWriteColor = ui.Hex("#94a3b8")
	inputColor      = ui.Hex("#06b6d4")
	outputColor     = ui.Hex("#10b981")
	costColor       = ui.Hex("#f59e0b")
	highlightColor  = ui.Hex("#38bdf8")
	tooltipBorder   = ui.Hex("#334155")
)

func (a *App) usageHeader(c *ui.Context, start, end time.Time) {
	t := c.Theme()
	days := int(math.Round(end.Sub(start).Hours() / 24))
	span := fmt.Sprintf(a.t("%d days", "%d 天"), days)
	if a.page == 0 {
		span = fmt.Sprintf(a.t("last %d days", "最近 %d 天"), days)
	}
	pricing := a.t("API list pricing", "API 定价估算")
	if a.agent == model.Devin {
		pricing = a.t("Devin Pro pricing", "Devin Pro 定价模型")
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(6, 6).Radius(3).Background(highlightColor)
		ui.Text(c, fmt.Sprintf(a.t("Period: %s ~ %s (%s)", "统计周期：%s ~ %s（%s）"), start.Format("01-02"), end.AddDate(0, 0, -1).Format("01-02"), span)).FontSize(13).FontWeight(500).TextColor(t.TextMuted)
		ui.Spacer(c)
		ui.Text(c, pricing).FontSize(11).FontWeight(500).Padding(3, 8).Radius(4).Background(t.Background).Border(1, t.Border).TextColor(t.TextMuted).NoWrap()
	})
}

// tokenParts splits formatTokens into the number and its unit.
func tokenParts(n float64) (string, string) {
	s := formatTokens(n)
	if i := strings.IndexAny(s, "KMB"); i > 0 {
		return s[:i], s[i:] + " Tokens"
	}
	return s, "Tokens"
}

// grouped writes n with thousands separators.
func grouped(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

type statPair struct {
	label, value string
	color        ui.Color
}

type summaryCard struct {
	label, badge, value, unit string
	badgeColor, valueColor    ui.Color
	weight                    float32
	highlight                 bool
	// progress is a 0–1 bar under the value; negative draws none.
	progress float64
	// One pair spreads its label and value; several show as "label: value".
	footer []statPair
}

// usageSummary shows four cards: cost, volume, cache efficiency and the
// tokens the model itself read and wrote. Each footer explains its figure.
func (a *App) usageSummary(c *ui.Context, total analytics.Bucket, sessions int) {
	t := c.Theme()
	badge := "USD · API"
	if a.agent == model.Devin {
		badge = "USD · Pro"
	}
	perSession := "—"
	if sessions > 0 && total.Cost > 0 {
		perSession = trayMoney(total.Cost / float64(sessions))
	}
	volume, volumeUnit := tokenParts(total.Total())
	hit, hitRate := "—", -1.0
	if prompt := total.Input + total.Cached + total.CacheWrite; prompt > 0 {
		hitRate = total.Cached / prompt
		hit = fmt.Sprintf("%.1f%%", hitRate*100)
	}
	saved := ""
	if s := total.CacheSavings(); s > 0 {
		saved = fmt.Sprintf(a.t("Saved ~%.0f%%", "节省 ~%.0f%%"), s*100)
	}
	io, ioUnit := tokenParts(total.Input + total.Output)
	cards := []summaryCard{
		{label: a.t("Total cost", "本期总费用"), badge: badge, badgeColor: costColor, value: usageMoney(total.Cost), valueColor: costColor, weight: 1, highlight: true, progress: -1,
			footer: []statPair{{a.t("Per session", "平均单会话"), perSession, t.Text}}},
		{label: a.t("Token throughput", "Token 吞吐总量"), value: volume, unit: volumeUnit, valueColor: t.Text, weight: 1, progress: -1,
			footer: []statPair{{a.t("Sessions / turns", "会话 / 轮次"), fmt.Sprintf(a.t("%d sessions · %s turns", "%d 会话 · %s 轮"), sessions, grouped(total.Turns)), t.Text}}},
		{label: a.t("Cache hits & savings", "缓存命中与节省"), badge: saved, badgeColor: highlightColor, value: hit, unit: a.t("hit rate", "命中率"), valueColor: t.Text, weight: 1.25, progress: hitRate,
			footer: []statPair{{a.t("Read", "读取"), formatTokens(total.Cached), ui.Hex("#60a5fa")}, {a.t("Write", "写入"), formatTokens(total.CacheWrite), cacheWriteColor}}},
		{label: a.t("Model I/O", "原生模型 I/O"), value: io, unit: ioUnit, valueColor: t.Text, weight: 1.1, progress: -1,
			footer: []statPair{{a.t("New input", "新输入"), formatTokens(total.Input), inputColor}, {a.t("Output", "输出"), formatTokens(total.Output), outputColor}}},
	}
	ui.Row(c).Wrap().Gap(12).AlignItems(ui.Stretch).Label(a.t("Usage summary", "用量概览")).Children(func() {
		for _, v := range cards {
			a.summaryCard(c, v)
		}
	})
}

func (a *App) summaryCard(c *ui.Context, v summaryCard) {
	t := c.Theme()
	card := ui.Column(c).Grow(v.weight).Basis(180*v.weight).MinWidth(180).Padding(14, 16).Gap(0).Radius(6).Background(t.Surface).Border(1, t.Border).Shadow(0, 1, 3, 0, ui.Hex("#000000").Alpha(.3))
	if v.highlight {
		card.Background(t.Surface.Mix(t.Text, .03)).Border(1, t.Border.Mix(t.Text, .08))
	}
	card.Children(func() {
		ui.Row(c).Height(22).Gap(8).AlignItems(ui.Center).Margin(0, 0, 6, 0).Children(func() {
			ui.Text(c, v.label).FontSize(12).FixedLineHeight(18).FontWeight(500).TextColor(t.TextMuted).Grow(1).MinWidth(0).SingleLine().Ellipsis("…")
			if v.badge != "" {
				ui.Text(c, v.badge).FontSize(10).FontWeight(600).Padding(2, 6).Radius(3).Background(v.badgeColor.Alpha(.1)).Border(1, v.badgeColor.Alpha(.25)).TextColor(v.badgeColor).NoWrap()
			}
		})
		ui.Row(c).Gap(6).AlignItems(ui.Baseline).Children(func() {
			ui.Text(c, v.value).FontSize(24).FixedLineHeight(32).Bold().FontFeatures("tnum").TextColor(v.valueColor).NoWrap()
			if v.unit != "" {
				ui.Text(c, v.unit).FontSize(13).FontWeight(500).TextColor(t.TextMuted).NoWrap()
			}
		})
		if v.progress >= 0 {
			ui.Box(c).FillWidth().Height(4).Margin(8, 0, 0, 0).Radius(2).Background(t.Border).Draw(func(p *ui.Painter, r ui.Rect) {
				r.W *= float32(math.Min(1, v.progress))
				p.Fill(r, ui.Hex("#0284c7"), 2)
			})
		}
		ui.Spacer(c)
		ui.Row(c).Margin(10, 0, 0, 0).Padding(8, 0, 0, 0).BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Gap(8).AlignItems(ui.Center).Justify(ui.SpaceBetween).Children(func() {
			if len(v.footer) == 1 {
				ui.Text(c, v.footer[0].label).FontSize(11).TextColor(t.TextMuted).NoWrap()
				ui.Text(c, v.footer[0].value).FontSize(11).FontWeight(600).FontFeatures("tnum").TextColor(v.footer[0].color).NoWrap()
				return
			}
			for _, f := range v.footer {
				ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
					ui.Text(c, f.label+":").FontSize(11).TextColor(t.TextMuted).NoWrap()
					ui.Text(c, f.value).FontSize(11).FontWeight(600).FontFeatures("tnum").TextColor(f.color).NoWrap()
				})
			}
		})
	})
}

// Chart modes. All tokens is the default; without cache reads shows the
// tokens that move most with behaviour, and cost what they are worth.
const (
	chartTokens = iota
	chartNewTokens
	chartCost
)

type chartSeries struct {
	en, zh string
	color  ui.Color
	value  func(analytics.Bucket) float64
}

// tokenSeries lists the token series bottom to top, as they stack.
var tokenSeries = []chartSeries{
	{"Cache read", "缓存读取", cacheReadColor, func(b analytics.Bucket) float64 { return b.Cached }},
	{"Cache write", "缓存写入", cacheWriteColor, func(b analytics.Bucket) float64 { return b.CacheWrite }},
	{"New input", "新输入", inputColor, func(b analytics.Bucket) float64 { return b.Input }},
	{"Output", "输出", outputColor, func(b analytics.Bucket) float64 { return b.Output }},
}

// niceStep rounds x up to 1, 2, 2.5 or 5 times a power of ten.
func niceStep(x float64) float64 {
	if x <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(x)))
	for _, f := range []float64{1, 2, 2.5, 5} {
		if x <= f*exp {
			return f * exp
		}
	}
	return 10 * exp
}

// axisLabel drops the decimal of round axis values: 250M, not 250.0M.
func axisLabel(v float64, cost bool) string {
	if cost {
		if v == 0 {
			return "0"
		}
		return strings.TrimSuffix(trayMoney(v), ".00")
	}
	return strings.Replace(formatTokens(v), ".0", "", 1)
}

func (a *App) weekday(d time.Weekday) string {
	return a.t(d.String()[:3], [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}[d])
}

// Chart geometry: headroom above the tallest bar holds its value label.
const (
	headroom  = 18
	barHeight = 150
	barWidth  = 24
	axisWidth = 48
)

func (a *App) usageChart(c *ui.Context, bs []analytics.Bucket) {
	t := c.Theme()
	cost := a.usageUI.chartMode == chartCost
	var series []chartSeries
	switch a.usageUI.chartMode {
	case chartCost:
		series = []chartSeries{{"Cost", "费用", costColor, func(b analytics.Bucket) float64 { return b.Cost }}}
	case chartNewTokens:
		series = tokenSeries[1:]
	default:
		series = tokenSeries
	}
	sum := func(b analytics.Bucket) float64 {
		v := 0.0
		for _, s := range series {
			v += s.value(b)
		}
		return v
	}
	label := formatTokens
	if cost {
		label = usageMoney
	}
	hi, all, peak := 0.0, 0.0, -1
	for i, b := range bs {
		v := sum(b)
		all += v
		if v > hi {
			hi, peak = v, i
		}
	}
	step := niceStep(hi / 3)
	top := 3 * step
	ui.Column(c).Padding(16, 20, 20, 20).Gap(16).Radius(6).Shrink(0).Background(t.Surface).Border(1, t.Border).Shadow(0, 1, 3, 0, ui.Hex("#000000").Alpha(.3)).Label(a.t("Stacked token usage chart", "Token 用量堆叠图")).Children(func() {
		ui.Row(c).Gap(16).AlignItems(ui.Center).Wrap().Children(func() {
			ui.Row(c).Gap(12).AlignItems(ui.Baseline).Children(func() {
				title := a.t("Token Usage Trend", "Token 用量趋势")
				if cost {
					title = a.t("Cost Trend", "费用趋势")
				}
				ui.Text(c, title).FontSize(14).FixedLineHeight(24).FontWeight(600).NoWrap()
				if len(bs) > 0 && all > 0 {
					avg := label(all / float64(len(bs)))
					if !cost {
						avg += " Tokens"
					}
					per := map[analytics.Period][2]string{analytics.Day: {"Daily avg %s", "日均 %s"}, analytics.Week: {"Weekly avg %s", "周均 %s"}, analytics.Month: {"Monthly avg %s", "月均 %s"}}[a.period]
					ui.Text(c, fmt.Sprintf(a.t(per[0], per[1]), avg)).FontSize(12).TextColor(t.TextMuted).NoWrap()
				}
			})
			ui.Spacer(c)
			if len(series) > 1 {
				for _, s := range series {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Box(c).Size(8, 8).Radius(2).Background(s.color)
						ui.Text(c, a.t(s.en, s.zh)).FontSize(12).TextColor(t.TextMuted).NoWrap()
					})
				}
			}
			ui.Segmented(c, &a.usageUI.chartMode, "Tokens", a.t("Without cache reads", "不含缓存读取"), a.t("Cost", "费用")).FontSize(12).Label(a.t("Chart metric", "图表指标"))
		})
		if hi <= 0 {
			msg := a.t("No usage in this period", "此周期没有用量")
			if cost {
				msg = a.t("No priced usage in this period; switch to Tokens", "此周期没有可计价的用量，可切换到 Tokens 查看")
			}
			ui.Text(c, msg).FontSize(12).TextColor(t.TextMuted).FillWidth().TextAlign(ui.Center).Padding(barHeight/2-10, 0)
			return
		}
		// Gridlines and their labels sit behind the bars; the baseline is brighter.
		canvas := ui.Row(c).Gap(4).Padding(0, 0, 0, axisWidth).Draw(func(p *ui.Painter, r ui.Rect) {
			for k := range 4 {
				y := r.Y + headroom + barHeight*(1-float32(k)/3)
				line := t.Border.Alpha(.55)
				if k == 0 {
					line = t.Border
				}
				p.Line(r.X+axisWidth, y, r.X+r.W, y, 1, line)
				tick := ui.Span{Text: axisLabel(step*float64(k), cost), Size: 10, Color: t.TextMuted.Alpha(.7)}
				w, h := p.MeasureText(0, tick)
				p.RichText(r.X+axisWidth-8-w, y-h/2, 0, tick)
			}
		})
		canvas.Children(func() {
			for i, b := range bs {
				a.chartBar(c.Key(fmt.Sprint(b.Start)), b, series, label(sum(b)), float32(sum(b)/top*barHeight), i == peak, top)
			}
		})
	})
}

// chartBar draws one bucket: its stack on a faint track, the value above
// it, and a tooltip with the breakdown and the bucket's estimated cost.
func (a *App) chartBar(c *ui.Context, b analytics.Bucket, series []chartSeries, value string, height float32, peak bool, top float64) {
	t := c.Theme()
	col := ui.Column(c).Grow(1).Basis(0).MinWidth(0).Gap(0).AlignItems(ui.Center)
	hovered := col.Hovered()
	col.Children(func() {
		track := t.Text.Alpha(.015)
		if hovered {
			track = t.Text.Alpha(.06)
		}
		ui.Column(c).Height(headroom + barHeight).FillWidth().Gap(2).AlignItems(ui.Center).Justify(ui.End).Draw(func(p *ui.Painter, r ui.Rect) {
			p.Fill(ui.Rect{X: r.X + (r.W-barWidth)/2, Y: r.Y, W: barWidth, H: r.H}, track, 3)
		}).Children(func() {
			if height <= 0 {
				ui.Box(c).Size(4, 4).Radius(2).Margin(0, 0, 6, 0).Background(t.Border)
				return
			}
			tag := ui.Text(c, value).FontSize(10).FixedLineHeight(14).FontWeight(500).FontFeatures("tnum").TextColor(t.TextMuted).NoWrap()
			switch {
			case hovered:
				tag.TextColor(t.Text).Bold()
			case peak:
				tag.TextColor(highlightColor).Bold()
			}
			ui.Column(c).Width(barWidth).Gap(0).Radius(3, 3, 0, 0).Clip().Children(func() {
				for i := len(series) - 1; i >= 0; i-- {
					if v := series[i].value(b); v > 0 {
						ui.Box(c).FillWidth().Height(float32(math.Max(1, v/top*barHeight))).Shrink(0).Background(series[i].color)
					}
				}
			})
		})
		date := ui.Text(c, b.Label).FontSize(11).FixedLineHeight(24).TextColor(t.TextMuted).FillWidth().TextAlign(ui.Center).NoWrap()
		switch {
		case hovered || peak:
			date.TextColor(t.Text).FontWeight(600)
		case height <= 0:
			date.TextColor(t.TextMuted.Alpha(.5))
		}
	})
	if b.Total() <= 0 && b.Cost <= 0 {
		return
	}
	ui.TooltipBase(c, col, func(tip ui.Element) {
		tip.Width(160).Padding(10, 12).Gap(6).Radius(6).Background(t.Surface.Mix(t.Text, .03)).Border(1, tooltipBorder).Shadow(0, 8, 24, 0, ui.Hex("#000000").Alpha(.55))
		tip.Children(func() {
			heading := b.Label
			if a.period == analytics.Day {
				heading += " · " + a.weekday(time.Unix(b.Start, 0).Weekday())
			}
			ui.Text(c, heading).FontSize(11).FontWeight(600).Padding(0, 0, 4, 0).BorderWidth(0, 0, 1, 0).BorderColor(tooltipBorder).FillWidth()
			row := func(name, value string, color ui.Color) {
				ui.Row(c).Gap(8).Children(func() {
					ui.Text(c, name).FontSize(10).TextColor(color).Grow(1).MinWidth(0).NoWrap()
					ui.Text(c, value).FontSize(10).FontWeight(600).FontFeatures("tnum").NoWrap()
				})
			}
			row(a.t("Total", "总用量"), formatTokens(b.Total()), t.TextMuted)
			for i := len(tokenSeries) - 1; i >= 0; i-- {
				if s := tokenSeries[i]; s.value(b) > 0 {
					row("• "+a.t(s.en, s.zh), formatTokens(s.value(b)), s.color)
				}
			}
			ui.Row(c).Gap(8).Padding(4, 0, 0, 0).BorderWidth(1, 0, 0, 0).BorderStyle(ui.BorderDashed).BorderColor(tooltipBorder).Children(func() {
				ui.Text(c, a.t("Estimated cost", "估算费用")).FontSize(10).TextColor(costColor).Grow(1).NoWrap()
				ui.Text(c, usageMoney(b.Cost)).FontSize(10).FontWeight(600).TextColor(costColor).NoWrap()
			})
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
			e := ui.Text(c, v).FontSize(12).SingleLine().Ellipsis("…")
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
		ui.Text(c, label).MinWidth(0).FontSize(12).SingleLine().Ellipsis("…")
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
						ui.Text(c, name).MinWidth(0).FontSize(12).SingleLine().Ellipsis("…")
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
