package desktop

import (
	"fmt"
	"math"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
	"github.com/meimingqi222/devin-usage-metrics/internal/analytics"
	"github.com/meimingqi222/devin-usage-metrics/internal/compaction"
)

var (
	compactionGreen     = ui.Hex("#3ddc97")
	compactionCheckIcon = ui.MustParseSVG([]byte(`<svg viewBox="0 0 20 20" fill="none" xmlns="http://www.w3.org/2000/svg"><path d="M4 10L8 15L16 5" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"/></svg>`))
	compactionBoltIcon  = ui.MustParseSVG([]byte(`<svg viewBox="0 0 20 20" xmlns="http://www.w3.org/2000/svg"><path d="M11 2L4 11H9L8 18L16 8H11Z" fill="currentColor"/></svg>`))
	compactionInfoIcon  = ui.MustParseSVG([]byte(`<svg viewBox="0 0 20 20" fill="none" xmlns="http://www.w3.org/2000/svg"><circle cx="10" cy="10" r="7" stroke="currentColor" stroke-width="1.8"/><path d="M10 9V14M10 6V6.2" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg>`))
)

// compactionKey is what a report depends on. configGen changes when local
// settings may have changed, since thresholds and undo records live on disk.
type compactionKey struct {
	data      dataKey
	period    analytics.Period
	page      int
	device    string
	configGen int
}

type compactionState struct {
	requested, ready compactionKey
	hasReport        bool
	report           compaction.Report
	pending          []string
	locked           bool
	configGen        int
}

// refreshCompaction replays history off the main thread, like the Rust
// version: a replay walks every turn once per curve point.
func (a *App) refreshCompaction() {
	key := compactionKey{a.dataKey(), a.period, a.page, a.device, a.compactionUI.configGen}
	if key == a.compactionUI.requested || a.busy["compaction"] {
		return
	}
	a.compactionUI.requested = key
	d, agent, device, localID := a.allData(), a.agent, a.device, a.localID
	start, end := analytics.Window(a.period, a.page, time.Now())
	a.work("compaction", func() func() {
		pending, _ := compaction.Pending(agent)
		locked := compaction.Locked(agent)
		report := compaction.Analyze(d, agent, device, start.Unix(), end.Unix(), compaction.CurrentThresholds(agent, localID))
		return func() {
			a.compactionUI.ready, a.compactionUI.hasReport = key, true
			a.compactionUI.report, a.compactionUI.pending, a.compactionUI.locked = report, pending, locked
		}
	})
}

// compactionConfigChanged makes the next frame re-read local thresholds.
func (a *App) compactionConfigChanged() { a.compactionUI.configGen++ }

func (a *App) compactionView(c *ui.Context) {
	ui.Text(c, a.t("Compaction", "压缩建议")).FontSize(18).Bold()
	ui.Text(c, a.t("Historical replay, not quality or subscription quota prediction. Apply affects only your local configuration.", "基于历史调用回放，不预测质量或订阅配额；应用建议只修改本机配置。")).FontSize(14).TextColor(c.Theme().TextMuted)
	a.refreshCompaction()
	if !a.compactionUI.hasReport || a.compactionUI.ready != a.compactionUI.requested {
		ui.Text(c, a.t("Replaying history…", "正在回放历史调用…")).FontSize(14).TextColor(c.Theme().TextMuted)
		return
	}
	for _, name := range a.compactionUI.pending {
		if textButton(c.Key("pending/"+name), a.t("Restore previous setting: ", "恢复原设置：")+name, false).Clicked() {
			a.undoCompaction(name)
		}
	}
	report := a.compactionUI.report
	if len(report.Recommendations) == 0 {
		ui.Text(c, a.t("No eligible data. Claude Code and Codex require at least 50 calls with context metadata and 3 observed compactions.", "没有符合条件的数据。Claude Code / Codex 至少需要 50 次带上下文信息的调用与 3 次疑似压缩。")).FontSize(14).TextColor(c.Theme().TextMuted)
	}
	for _, r := range report.Recommendations {
		ui.Column(c.Key(r.DeviceID+r.Model)).Padding(16).Gap(12).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, r.Model).FontSize(14).Bold()
				ui.Text(c, r.DeviceName).FontSize(12).Padding(1, 8).Radius(4).Background(c.Theme().SurfaceHover).TextColor(c.Theme().TextMuted)
				ui.Spacer(c)
				ui.Text(c, fmt.Sprintf(a.t("%d calls · %d streams · %d observed compactions", "%d 次调用 · %d 条上下文 · %d 次实际压缩"), r.Calls, r.Streams, r.Compactions)).FontSize(12).TextColor(c.Theme().TextMuted)
			})
			a.compactionHero(c, r)
			if r.Status == "recommend" || r.Status == "keep" {
				a.compactionStats(c, r)
				a.compactionConfig(c, r)

				if len(r.Curve) > 1 {
					a.compactionCurve(c, r)
				}
				a.compactionDetailsDrawer(c, r)
			}
		})
	}
	if report.Skipped > 0 {
		ui.Text(c, fmt.Sprintf(a.t("Skipped %d calls without safe context metadata.", "跳过 %d 次缺少可靠上下文信息的调用。"), report.Skipped)).FontSize(14).TextColor(c.Theme().Warning)
	}
}

func (a *App) compactionHero(c *ui.Context, r compaction.Recommendation) {
	color := c.Theme().Accent
	icon := compactionBoltIcon
	title := fmt.Sprintf(a.t("Adjust threshold (%s)", "建议调整阈值（%s）"), formatTokens(float64(r.Recommended)))
	badge := a.t("Recommended", "建议调整")
	if r.Status == "keep" {
		color = compactionGreen
		icon = compactionCheckIcon
		title = fmt.Sprintf(a.t("Keep current threshold (%s)", "保持当前阈值（%s）"), formatTokens(float64(r.Current)))
		badge = a.t("Already optimal", "已是最优")
		if r.Uncalibrated {
			color = c.Theme().Warning
			icon = compactionInfoIcon
			badge = a.t("Low confidence", "置信度低")
		}
	}
	if r.Status != "recommend" && r.Status != "keep" {
		// Match the Rust status cards; never show the internal status ID.
		color = c.Theme().TextMuted
		icon = compactionInfoIcon
		title = a.t("Not enough eligible samples", "有效样本不足")
		badge = a.t("Insufficient samples", "样本不足")
		if r.Status == "unknown_pricing" {
			color = c.Theme().Danger
			title = a.t("Missing pricing", "缺少定价")
			badge = title
		}
	}
	description := a.compactionDescription(r)
	ui.Row(c).Padding(12).Gap(12).Radius(6).Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).Children(func() {
		ui.Column(c).Size(32, 32).Shrink(0).AlignItems(ui.Center).Justify(ui.Center).TextColor(color).Background(color.Alpha(.12)).Radius(6).Children(func() {
			ui.Icon(c, icon).Size(18, 18)
		})
		ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
			ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, title).FontSize(14).Bold()
				ui.Text(c, badge).FontSize(12).Padding(1, 8).Radius(4).Background(color.Alpha(.15)).TextColor(color)
			})
			ui.Text(c, description).FontSize(12).TextColor(c.Theme().TextMuted)
			if r.Status == "recommend" && r.BaselineCompactions > 0 && r.RecommendedCompactions > r.BaselineCompactions*3/2 {
				ui.Text(c, fmt.Sprintf(a.t(
					"The risk-adjusted optimum still raises replayed compactions by %.0f%% (%d → %d). More frequent compaction can discard context; quality is not modelled.",
					"风险调整后的最优点仍使回放压缩次数增加 %.0f%%（%d → %d）。更频繁的压缩可能丢失上下文，且不评估质量。"),
					(float64(r.RecommendedCompactions)/float64(r.BaselineCompactions)-1)*100,
					r.BaselineCompactions, r.RecommendedCompactions)).FontSize(12).TextColor(c.Theme().Warning)
			}
		})
	})
}

type compactionAlternative struct {
	threshold   uint64
	cost        float64
	saving      float64
	netSaving   float64
	compactions int
}

// compactionAlternative returns the point a keep verdict was measured against:
// the risk-adjusted optimum, or the cheapest cap-compliant point when the
// optimum is the current threshold itself.
func (a *App) compactionAlternative(r compaction.Recommendation) (compactionAlternative, bool) {
	if r.Status != "keep" {
		return compactionAlternative{}, false
	}
	if r.Optimal != r.Current {
		return compactionAlternative{r.Optimal, r.OptimalCost, r.OptimalSaving, r.OptimalNetSaving, r.OptimalCompactions}, true
	}
	if r.Candidate != r.Current {
		return compactionAlternative{r.Candidate, r.CandidateCost, r.CandidateSaving, r.CandidateNetSaving, r.CandidateCompactions}, true
	}
	return compactionAlternative{}, false
}

func (a *App) compactionDescription(r compaction.Recommendation) string {
	switch r.Status {
	case "recommend":
		action := a.t("Lowering to", "降到")
		if r.Recommended > r.Current {
			action = a.t("Raising to", "提高到")
		}
		return fmt.Sprintf(a.t(
			action+" %s would cut simulated cost by %.1f%% (%s). Replayed compactions: %d → %d. After a %.0f%% drift charge per extra compaction, net saving is %.1f%%. Quality and subscription quotas are not predicted.",
			action+" %s 预计节省 %.1f%%（%s）。回放压缩：%d → %d。按每次额外压缩 %.0f%% 的偏移成本计算，净节省 %.1f%%。不预测质量或订阅配额。"),
			formatTokens(float64(r.Recommended)), r.SavingsFraction()*100, trayMoney(r.BaselineCost*r.SavingsFraction()),
			r.BaselineCompactions, r.RecommendedCompactions, compaction.CompactionDrift*100, r.OptimalNetSaving*100)
	case "keep":
		if r.Uncalibrated {
			return fmt.Sprintf(a.t(
				"Replay at %s makes %d compactions, but %d were observed. The threshold probably changed within this range, or many compactions were manual, so this history cannot support a recommendation.",
				"按 %s 回放会产生 %d 次压缩，但实际只有 %d 次。可能该时间段内阈值有变化，或很多压缩是手动触发，因此这段历史不足以支持建议。"),
				formatTokens(float64(r.Current)), r.BaselineCompactions, r.Compactions)
		}
		alt, ok := a.compactionAlternative(r)
		if r.SensitivityFailed {
			return fmt.Sprintf(a.t(
				"The best alternative %s saves %.1f%% before the sensitivity check, but only %.1f%% when rework is doubled. Not enough to adjust the threshold.",
				"最佳替代点 %s 在敏感性检查前可省 %.1f%%，但返工翻倍后仅省 %.1f%%，不足以调整阈值。"),
				formatTokens(float64(r.Optimal)), r.OptimalSaving*100, r.CautiousSaving*100)
		}
		if !ok {
			return a.t("The current threshold is already the risk-adjusted optimum in this sample.", "当前阈值已是该样本下风险调整后的最优点。")
		}
		if alt.compactions > r.BaselineCompactions {
			return fmt.Sprintf(a.t(
				"The best alternative %s saves %.1f%% raw, but %+d extra compactions at a %.0f%% drift charge leave %.1f%% net, below the %.1f%% bar. Keeping the current threshold.",
				"最佳替代点 %s 原始节省 %.1f%%，但多 %d 次压缩按 %.0f%% 偏移成本计算后净节省仅 %.1f%%，低于 %.1f%% 门槛，因此保持当前阈值。"),
				formatTokens(float64(alt.threshold)), alt.saving*100, alt.compactions-r.BaselineCompactions, compaction.CompactionDrift*100, alt.netSaving*100, r.SavingBar*100)
		}
		return fmt.Sprintf(a.t(
			"The best alternative %s saves only %.1f%% before the drift charge (%.1f%% net), below the %.1f%% bar. Keeping the current threshold.",
			"最佳替代点 %s 扣除偏移成本前仅节省 %.1f%%（净节省 %.1f%%），低于 %.1f%% 门槛，因此保持当前阈值。"),
			formatTokens(float64(alt.threshold)), alt.saving*100, alt.netSaving*100, r.SavingBar*100)
	case "not_enough_calls":
		return a.t("Too little data: at least 50 priced valid calls are required.", "样本不足：至少需要 50 次可定价的有效调用。")
	case "not_enough_compactions":
		return a.t("Too few compactions: at least 3 suspected compactions are required. No default threshold is guessed.", "压缩样本不足：至少需要 3 次疑似压缩，不猜测默认阈值。")
	case "unknown_pricing":
		return a.t("No model pricing available; no threshold recommendation.", "缺少模型定价，暂不推荐阈值。")
	default:
		return a.t("There is not enough eligible data for a recommendation.", "有效数据不足，无法给出建议。")
	}
}

func (a *App) compactionStats(c *ui.Context, r compaction.Recommendation) {
	baselineLabel := a.t("Historical threshold", "历史阈值")
	if r.BaselineInferred {
		baselineLabel = a.t("Inferred threshold", "推断阈值")
	} else {
		baselineLabel = a.t("Configured threshold", "已配置阈值")
	}
	threshold, cost := r.Recommended, r.RecommendedCost
	thresholdLabel := a.t("Recommended threshold", "建议阈值")
	savingLabel := a.t("Estimated saving", "预计节省")
	saved := r.SavingsFraction()
	compactions := r.RecommendedCompactions
	net := r.OptimalNetSaving
	if r.Status == "keep" {
		if alt, ok := a.compactionAlternative(r); ok {
			threshold, cost = alt.threshold, alt.cost
			thresholdLabel = a.t("Best alternative threshold", "最佳替代阈值")
			savingLabel = a.t("Alternative saving", "替代节省")
			saved, compactions, net = alt.saving, alt.compactions, alt.netSaving
		} else {
			threshold, cost = r.Current, r.BaselineCost
			thresholdLabel = a.t("Current threshold", "当前阈值")
			savingLabel = a.t("Saving", "节省")
			saved, compactions, net = 0, r.BaselineCompactions, 0
		}
	}
	diff := max(0, compactions-r.BaselineCompactions)
	impactColor := c.Theme().TextMuted
	if diff > 0 {
		impactColor = c.Theme().Warning
	}
	if r.BaselineCompactions > 0 && compactions > r.BaselineCompactions*2 {
		impactColor = c.Theme().Danger
	}
	impactSub := fmt.Sprintf("%d → %d", r.BaselineCompactions, compactions)
	if r.BaselineCompactions > 0 {
		impactSub += fmt.Sprintf(" (%.1f×)", float64(compactions)/float64(r.BaselineCompactions))
	}
	netColor := c.Theme().TextMuted
	if r.Status == "recommend" && net >= r.SavingBar {
		netColor = compactionGreen
	} else if net < 0 {
		netColor = c.Theme().Warning
	}
	stats := []struct {
		label, value, sub string
		color             ui.Color
	}{
		{baselineLabel, formatTokens(float64(r.Current)), trayMoney(r.BaselineCost), c.Theme().Text},
		{thresholdLabel, formatTokens(float64(threshold)), trayMoney(cost), c.Theme().Accent},
		{savingLabel, fmt.Sprintf("%.1f%%", 100*saved), "~" + trayMoney(r.BaselineCost*saved), compactionGreen},
		{a.t("Net saving", "净节省"), fmt.Sprintf("%.1f%%", 100*net), fmt.Sprintf(a.t("%.0f%% per extra compaction", "每次额外压缩 %.0f%%"), compaction.CompactionDrift*100), netColor},
		{a.t("Compaction impact", "压缩影响"), fmt.Sprintf("+%d", diff), impactSub, impactColor},
	}
	if r.Status == "keep" {
		stats[1].color = c.Theme().TextMuted
	}
	ui.Row(c).Wrap().Gap(8).Children(func() {
		for _, v := range stats {
			ui.Column(c).Grow(1).Basis(130).MinWidth(130).Padding(12).Gap(4).Radius(6).Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).Children(func() {
				ui.Text(c, v.label).FontSize(12).TextColor(c.Theme().TextMuted)
				ui.Text(c, v.value).FontSize(20).Bold().TextColor(v.color)
				ui.Text(c, v.sub).FontSize(12).TextColor(c.Theme().TextMuted)
			})
		}
	})
}

func (a *App) compactionConfig(c *ui.Context, r compaction.Recommendation) {
	detail := ""
	if r.BaselineInferred {
		if r.Status == "recommend" {
			detail = fmt.Sprintf(a.t(
				"No local threshold detected; baseline %s reproduces the %d observed compactions (median compaction point %s). Recommended point: %s.",
				"未检测到本机阈值；基线 %s 可复现实际的 %d 次压缩（压缩点中位数 %s）。建议点：%s。"),
				formatTokens(float64(r.Current)), r.Compactions, formatTokens(float64(r.Observed)), formatTokens(float64(r.Recommended)))
		} else {
			detail = fmt.Sprintf(a.t(
				"No local threshold detected; baseline %s reproduces the %d observed compactions (median compaction point %s). No change recommended.",
				"未检测到本机阈值；基线 %s 可复现实际的 %d 次压缩（压缩点中位数 %s）。建议保持不变。"),
				formatTokens(float64(r.Current)), r.Compactions, formatTokens(float64(r.Observed)))
		}
	} else if r.Status == "recommend" {
		detail = fmt.Sprintf(a.t("Configured %s → recommended %s.", "当前配置 %s → 建议 %s。"),
			formatTokens(float64(r.Current)), formatTokens(float64(r.Recommended)))
	} else {
		detail = fmt.Sprintf(a.t("Configured %s; no change recommended.", "当前配置 %s；建议保持不变。"), formatTokens(float64(r.Current)))
	}
	ui.Row(c).Padding(12).Gap(8).Radius(6).AlignItems(ui.Center).Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
			ui.Text(c, a.t("Local configuration", "本机配置")).FontSize(12).Bold()
			ui.Text(c, detail).FontSize(12).TextColor(c.Theme().TextMuted)
			if r.DeviceID != a.localID {
				ui.Text(c, a.t("Remote device: analysis only. Apply on the source device.", "远程设备：仅展示分析，请在来源设备应用配置。")).FontSize(12).TextColor(c.Theme().TextMuted)
			} else if a.compactionUI.locked {
				ui.Text(c, a.t("An environment variable overrides compaction. Remove it before applying.", "环境变量覆盖了压缩设置。请先移除覆盖，再应用建议。")).FontSize(12).TextColor(c.Theme().Warning)
			}
		})
		if r.DeviceID == a.localID {
			if ui.PrimaryButton(c, a.t("Apply recommendation", "应用建议")).FontSize(12).Disabled(r.Status != "recommend" || a.compactionUI.locked).Clicked() {
				apply, err := compaction.Prepare(a.agent, r.Model, r.Recommended)
				if err != nil {
					a.message = err.Error()
				} else {
					copy := r
					a.pending = &copy
					a.pendingApply = apply
					a.confirm = true
				}
			}
			if textButton(c, a.t("Undo local change", "撤销本机修改"), false).Clicked() {
				a.undoCompaction(r.Model)
			}
		}
	})
}

func (a *App) compactionCurve(c *ui.Context, r compaction.Recommendation) {
	if len(r.Curve) < 2 || r.BaselineCost <= 0 {
		return
	}
	alt, hasAlt := a.compactionAlternative(r)
	first := float64(r.Curve[0].Threshold)
	last := float64(r.Curve[len(r.Curve)-1].Threshold)
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, p := range r.Curve {
		v := (p.Cost/r.BaselineCost - 1) * 100
		lo = math.Min(lo, v)
		hi = math.Max(hi, v)
	}
	lo, hi = math.Min(lo, 0), math.Max(hi, 0)
	padding := math.Max((hi-lo)*.06, .5)
	yLo, yHi := lo-padding, hi+padding
	x := chart.NewLinear(first, last)
	y := chart.NewLinear(yLo, yHi)
	xs, ys := make([]float32, len(r.Curve)), make([]float32, len(r.Curve))
	for i, p := range r.Curve {
		xs[i] = x.At(float64(p.Threshold))
		ys[i] = y.At((p.Cost/r.BaselineCost - 1) * 100)
	}
	ticks := []chart.Tick{
		{At: 1, Value: yHi, Label: fmt.Sprintf("%+.1f%%", yHi)},
		{At: 0, Value: yLo, Label: fmt.Sprintf("%+.1f%%", yLo)},
	}
	// Shade thresholds that would exceed the compaction-growth cap. The
	// boundary is the cheapest curve point that still fits the cap, not the
	// chosen candidate, which may be a larger near-optimal threshold.
	capStart := uint64(0)
	shaded := false
	if r.CompactionLimit > 0 {
		for _, p := range r.Curve {
			if p.Compactions <= r.CompactionLimit {
				capStart, shaded = p.Threshold, float64(p.Threshold) > first
				break
			}
		}
	}
	ui.Column(c).Padding(12).Gap(8).Radius(6).Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).Children(func() {
		ui.Row(c).Wrap().Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, a.t("Cost sensitivity curve", "费用敏感度曲线")).FontSize(12).Bold()
			ui.Text(c, a.t("Lower is cheaper", "越低越节省")).FontSize(12).TextColor(c.Theme().TextMuted)
			ui.Spacer(c)
			compactionLegend(c, compactionGreen, a.t("Recommended / keep", "建议/保持点"))
			if hasAlt {
				compactionLegend(c, c.Theme().TextMuted, a.t("Best alternative", "最佳替代"))
			}
			compactionLegend(c, c.Theme().Accent, a.t("Simulated cost", "模拟费用"))
			if shaded {
				compactionLegend(c, c.Theme().Warning.Alpha(.35), a.t("Over compaction cap", "超过压缩上限"))
			}
		})
		chart.Plot(c, chart.Axes{Left: ticks, Label: func(side chart.Side, tick chart.Tick) {
			label := tick.Label
			if side == chart.Left {
				label = fmt.Sprintf("%+.1f%%", tick.Value)
			}
			ui.Text(c, label).FontSize(12).TextColor(c.Theme().TextMuted).NoWrap()
		}}, func(e ui.Element) {
			e.Background(c.Theme().Background).Draw(func(p *ui.Painter, rect ui.Rect) {
				if shaded {
					if cutoff := chart.X(rect, x.At(float64(capStart))); cutoff > rect.X {
						p.Fill(ui.Rect{X: rect.X, Y: rect.Y, W: cutoff - rect.X, H: rect.H}, c.Theme().Warning.Alpha(.07), 0)
					}
				}
				for _, f := range []float32{0, .5, 1} {
					y := rect.Y + f*rect.H
					p.Line(rect.X, y, rect.X+rect.W, y, 1, c.Theme().Border)
				}
				if yLo < 0 && yHi > 0 {
					y := chart.Y(rect, y.At(0))
					p.Line(rect.X, y, rect.X+rect.W, y, 1, c.Theme().TextMuted.Alpha(.55))
				}
				p.StrokePath(chart.Line(rect, xs, ys, chart.CurveLinear), 2, c.Theme().Accent)
				if hasAlt {
					x := chart.X(rect, x.At(float64(alt.threshold)))
					p.Line(x, rect.Y, x, rect.Y+rect.H, 1, c.Theme().TextMuted)
				}
				x := chart.X(rect, x.At(float64(r.Recommended)))
				p.Line(x, rect.Y, x, rect.Y+rect.H, 2, compactionGreen)
			})
		}).Height(110).Gap(6)
		ui.Row(c).FillWidth().PaddingStart(52).Justify(ui.SpaceBetween).Children(func() {
			ui.Text(c, formatTokens(first)).FontSize(12).TextColor(c.Theme().TextMuted)
			ui.Row(c).Gap(12).Children(func() {
				label := a.t("Keep", "保持")
				if r.Status == "recommend" {
					label = a.t("Recommended", "建议")
				}
				ui.Text(c, fmt.Sprintf("%s %s", label, formatTokens(float64(r.Recommended)))).FontSize(12).TextColor(compactionGreen)
				if hasAlt {
					ui.Text(c, fmt.Sprintf("%s %s", a.t("Best alternative", "最佳替代"), formatTokens(float64(alt.threshold)))).FontSize(12).TextColor(c.Theme().TextMuted)
				}
			})
			ui.Text(c, formatTokens(last)).FontSize(12).TextColor(c.Theme().TextMuted)
		})
		if shaded {
			ui.Text(c, fmt.Sprintf(a.t(
				"The shaded area is cheaper in replay, but exceeds the compaction cap (at most %d additional compactions).",
				"阴影区域费用更低，但压缩次数超过上限（最多增加 %d 次）。"),
				r.CompactionLimit-r.BaselineCompactions)).FontSize(12).TextColor(c.Theme().TextMuted)
		}
	})
}

func compactionLegend(c *ui.Context, color ui.Color, label string) {
	ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(8, 3).Radius(2).Background(color)
		ui.Text(c, label).FontSize(12).TextColor(c.Theme().TextMuted)
	})
}

func (a *App) compactionDetailsDrawer(c *ui.Context, r compaction.Recommendation) {
	open := a.compactionDetails
	ui.Column(c).FillWidth().Radius(6).Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).Children(func() {
		toggle := ui.ButtonBase(c).FillWidth().Height(34).Padding(8, 12).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
				ui.Text(c, a.sectionTitle(open, fmt.Sprintf(a.t("Algorithm and sample details (%d calls)", "算法依据与样本明细（%d 次调用）"), r.Calls))).FontSize(12).TextColor(c.Theme().TextMuted)
				ui.Spacer(c)
				label := a.t("Expand", "展开明细")
				if open {
					label = a.t("Collapse", "收起")
				}
				ui.Text(c, label).FontSize(12).TextColor(c.Theme().TextMuted)
			})
		})
		if toggle.Hovered() {
			toggle.Background(c.Theme().Surface)
		}
		if toggle.Clicked() {
			a.compactionDetails = !open
		}
		if open {
			ui.Box(c).FillWidth().Height(1).Background(c.Theme().Border)
			ui.Column(c).Padding(12).Gap(8).Children(func() {
				ui.Row(c).Wrap().Gap(8).Children(func() {
					for _, v := range []struct{ label, value string }{
						{a.t("Growth per call", "单次调用增长"), formatTokens(float64(r.Growth))},
						{a.t("Kept after compaction", "压缩后保留"), formatTokens(float64(r.After))},
						{a.t("Estimated rework", "估计返工"), formatTokens(float64(r.Rework))},
						{a.t("Observed compactions", "实际压缩事件"), fmt.Sprintf("%d", r.Compactions)},
					} {
						ui.Column(c).Grow(1).Basis(110).MinWidth(100).Padding(8).Gap(2).Radius(4).Background(c.Theme().Background).Children(func() {
							ui.Text(c, v.label).FontSize(12).TextColor(c.Theme().TextMuted)
							ui.Text(c, v.value).FontSize(12).Bold().TextColor(c.Theme().Text)
						})
					}
				})
				ui.Text(c, fmt.Sprintf(a.t(
					"Priced %d/%d calls; %d observed compactions; replay at the baseline threshold makes %d; compaction cap is %d.",
					"已定价 %d/%d 次调用；实际检测到 %d 次压缩；基线阈值回放产生 %d 次压缩；压缩上限为 %d 次。"),
					r.PricedCalls, r.Calls, r.Compactions, r.BaselineCompactions, r.CompactionLimit)).FontSize(12).TextColor(c.Theme().TextMuted)
				if r.Unpriced > 0 {
					ui.Text(c, fmt.Sprintf(a.t("%d streams have no usable price and are excluded from calibration.", "%d 条上下文缺少可用价格，未参与校准。"), r.Unpriced)).FontSize(12).TextColor(c.Theme().Danger)
				}
				if r.BaselineInferred {
					ui.Text(c, a.t("No local threshold was found; the baseline is the threshold whose replay reproduces the observed compaction count.", "未找到本机阈值；基线取回放压缩次数与实际一致的阈值。")).FontSize(12).TextColor(c.Theme().TextMuted)
				}
				ui.Text(c, a.t(
					"A prompt of at least 20K dropping below 60% counts as a suspected compaction. Rework is estimated over the next 10 calls; scarce samples use a 10K minimum. The search steps by 10K. Extra compactions are capped at 50% (minimum 10); within that cap, every extra replayed compaction is charged 1% of baseline simulated cost and the lowest risk-adjusted cost is selected. When the verdict is keep, the grey line marks the best rejected alternative: the risk-adjusted optimum, or the cheapest cap-compliant point when the optimum is the current threshold. Lowering requires at least 5% net saving and a doubled-rework sensitivity check. Thresholds are total input tokens, excluding output. Summary quality is not evaluated; subscription quota is not API cost.",
					"提示词至少 20K 且下降到 60% 以下时视为疑似压缩。返工量按后续 10 次调用估计，样本不足时至少按 10K 计。搜索步长 10K。压缩次数最多增加 50%（至少允许增加 10 次）；在该上限内，每次额外回放压缩按基线模拟费用的 1% 计入偏移成本，选择风险调整后费用最低的点。保持结论下，灰色线标出被否决的最佳替代点：风险调整后的最优点；若该点就是当前阈值，则取上限内费用最低的点。降低阈值至少需要 5% 净节省，并经过返工翻倍的敏感性检查。阈值指总输入 token（不含输出）。不评估摘要质量；订阅配额不等于 API 费用。")).FontSize(12).TextColor(c.Theme().TextMuted)
			})
		}
	})
}

func (a *App) undoCompaction(name string) {
	defer a.compactionConfigChanged()
	if err := compaction.Undo(a.agent, name); err != nil {
		a.message = err.Error()
	} else {
		a.message = a.t("Previous setting restored", "已恢复原设置")
	}
}
