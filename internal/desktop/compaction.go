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

func (a *App) compactionView(c *ui.Context) {
	ui.Text(c, a.t("Compaction", "压缩建议")).FontSize(18).Bold()
	ui.Text(c, a.t("Historical replay, not quality or subscription quota prediction. Apply affects only your local configuration.", "基于历史调用回放，不预测质量或订阅配额；应用建议只修改本机配置。")).FontSize(14).TextColor(c.Theme().TextMuted)
	if names, err := compaction.Pending(a.agent); err == nil {
		for _, name := range names {
			if textButton(c.Key("pending/"+name), a.t("Restore previous setting: ", "恢复原设置：")+name, false).Clicked() {
				a.undoCompaction(name)
			}
		}
	}
	start, end := analytics.Window(a.period, a.page, time.Now())
	report := compaction.Analyze(a.allData(), a.agent, a.device, start.Unix(), end.Unix(), compaction.CurrentThresholds(a.agent, a.localID))
	if len(report.Recommendations) == 0 {
		ui.Text(c, a.t("No eligible data. Claude Code and Codex require at least 50 calls with context metadata and 3 observed compactions.", "没有符合条件的数据。Claude Code / Codex 至少需要 50 次带上下文信息的调用与 3 次疑似压缩。")).FontSize(14).TextColor(c.Theme().TextMuted)
	}
	for _, r := range report.Recommendations {
		ui.Column(c.Key(r.DeviceID+r.Model)).Padding(16).Gap(12).Radius(6).Background(c.Theme().Surface).Border(1, c.Theme().Border).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, r.Model).FontSize(14).Bold()
				ui.Text(c, r.DeviceName).FontSize(12).Padding(1, 8).Radius(4).Background(c.Theme().SurfaceHover).TextColor(c.Theme().TextMuted)
				ui.Spacer(c)
				ui.Text(c, fmt.Sprintf(a.t("%d calls · %d streams · %d compactions", "%d 次调用 · %d 条上下文 · %d 次压缩"), r.Calls, r.Streams, r.Compactions)).FontSize(12).TextColor(c.Theme().TextMuted)
			})
			color := c.Theme().Accent
			icon := "⚡"
			title := fmt.Sprintf(a.t("Adjust threshold (%s)", "建议调整阈值（%s）"), formatTokens(float64(r.Recommended)))
			badge := a.t("Recommended", "建议调整")
			if r.Status == "keep" {
				color = ui.Hex("#3ddc97")
				icon = "✓"
				title = fmt.Sprintf(a.t("Keep current threshold (%s)", "保持当前阈值（%s）"), formatTokens(float64(r.Current)))
				badge = a.t("Already optimal", "已是最优")
			}
			if r.Status != "recommend" && r.Status != "keep" {
				color = c.Theme().TextMuted
				icon = "ℹ"
				title = a.t("Not enough eligible samples", "有效样本不足")
				badge = r.Status
			}
			ui.Row(c).Padding(12).Gap(12).Radius(6).Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).Children(func() {
				ui.Text(c, icon).Size(32, 32).FontSize(18).Bold().TextAlign(ui.Center).TextColor(color).Background(color.Alpha(.12)).Radius(6)
				ui.Column(c).Grow(1).Gap(4).Children(func() {
					ui.Row(c).Gap(8).Children(func() {
						ui.Text(c, title).FontSize(14).Bold()
						ui.Text(c, badge).FontSize(12).Padding(1, 8).Radius(4).Background(color.Alpha(.15)).TextColor(color)
					})
					ui.Text(c, fmt.Sprintf(a.t("Estimated saving %.1f%% against observed history; quality and subscription quotas are not predicted.", "相对历史回放预计节省 %.1f%%；不预测质量或订阅配额。"), r.CandidateSaving*100)).FontSize(12).TextColor(c.Theme().TextMuted)
				})
			})
			if r.Status == "recommend" || r.Status == "keep" {
				ui.Row(c).Wrap().Gap(8).Children(func() {
					for _, v := range []struct{ label, value, sub, color string }{{a.t("Historical threshold", "历史阈值"), formatTokens(float64(r.Current)), trayMoney(r.BaselineCost), "#e6e6ee"}, {a.t("Recommended threshold", "建议阈值"), formatTokens(float64(r.Recommended)), trayMoney(r.RecommendedCost), "#4cc2ff"}, {a.t("Estimated saving", "预计节省"), fmt.Sprintf("%.1f%%", 100*r.CandidateSaving), trayMoney(r.BaselineCost * r.CandidateSaving), "#3ddc97"}, {a.t("Compaction impact", "压缩影响"), fmt.Sprintf("+%d", max(0, r.CandidateCompactions-r.BaselineCompactions)), fmt.Sprintf("%d → %d", r.BaselineCompactions, r.CandidateCompactions), "#8f8fa3"}} {
						ui.Column(c).Grow(1).Basis(130).MinWidth(130).Padding(12).Gap(4).Radius(6).Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).Children(func() {
							ui.Text(c, v.label).FontSize(12).TextColor(c.Theme().TextMuted)
							ui.Text(c, v.value).FontSize(20).Bold().TextColor(ui.Hex(v.color))
							ui.Text(c, v.sub).FontSize(12).TextColor(c.Theme().TextMuted)
						})
					}
				})
				ui.Row(c).Padding(12).Gap(8).Radius(6).AlignItems(ui.Center).Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).Children(func() {
					ui.Column(c).Grow(1).Gap(4).Children(func() {
						ui.Text(c, a.t("Local configuration", "本机配置")).FontSize(12).Bold()
						ui.Text(c, fmt.Sprintf(a.t("Observed %s → Recommended %s", "历史阈值 %s → 建议阈值 %s"), formatTokens(float64(r.Observed)), formatTokens(float64(r.Recommended)))).FontSize(12).TextColor(c.Theme().TextMuted)
					})
					if r.DeviceID == a.localID {
						if ui.PrimaryButton(c, a.t("Apply recommendation", "应用建议")).FontSize(12).Disabled(r.Status != "recommend").Clicked() {
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
				if len(r.Curve) > 1 {
					a.compactionCurve(c, r)
				}
				if textButton(c, a.sectionTitle(a.compactionDetails, a.t("Algorithm and sample details", "算法依据与样本明细")), false).FillWidth().TextAlign(ui.Start).Background(c.Theme().SurfaceHover).Padding(8, 12).Height(32).Clicked() {
					a.compactionDetails = !a.compactionDetails
				}
				if a.compactionDetails {
					ui.Text(c, fmt.Sprintf(a.t("Growth %s · After compaction %s · Rework %s · Priced calls %d/%d", "上下文增长 %s · 压缩后 %s · 返工 %s · 已定价调用 %d/%d"), formatTokens(float64(r.Growth)), formatTokens(float64(r.After)), formatTokens(float64(r.Rework)), r.PricedCalls, r.Calls)).FontSize(12).Padding(12).Background(c.Theme().SurfaceHover)
				}
			}
		})
	}
	if report.Skipped > 0 {
		ui.Text(c, fmt.Sprintf(a.t("Skipped %d calls without safe context metadata.", "跳过 %d 次缺少可靠上下文信息的调用。"), report.Skipped)).FontSize(14).TextColor(c.Theme().Warning)
	}
}

func (a *App) undoCompaction(name string) {
	if err := compaction.Undo(a.agent, name); err != nil {
		a.message = err.Error()
	} else {
		a.message = a.t("Previous setting restored", "已恢复原设置")
	}
}

func (a *App) compactionCurve(c *ui.Context, r compaction.Recommendation) {
	ui.Column(c).Padding(12).Gap(8).Radius(6).Background(c.Theme().SurfaceHover).Border(1, c.Theme().Border).Children(func() {
		ui.Row(c).Gap(8).Children(func() {
			ui.Text(c, a.t("Cost sensitivity curve", "费用敏感度曲线")).FontSize(12).Bold()
			ui.Text(c, a.t("Lower is cheaper", "越低越节省")).FontSize(12).TextColor(c.Theme().TextMuted)
			ui.Spacer(c)
			ui.Text(c, a.t("━ Recommended / keep", "━ 建议/保持点")).FontSize(12).TextColor(ui.Hex("#3ddc97"))
			ui.Text(c, a.t("━ Simulated cost", "━ 模拟费用")).FontSize(12).TextColor(c.Theme().Accent)
		})
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, p := range r.Curve {
			v := (p.Cost/math.Max(r.BaselineCost, 1e-9) - 1) * 100
			lo = math.Min(lo, v)
			hi = math.Max(hi, v)
		}
		x := chart.NewLinear(float64(r.Curve[0].Threshold), float64(r.Curve[len(r.Curve)-1].Threshold))
		y := chart.NewLinear(lo, math.Max(hi, lo+.001))
		xs, ys := make([]float32, len(r.Curve)), make([]float32, len(r.Curve))
		for i, p := range r.Curve {
			xs[i] = x.At(float64(p.Threshold))
			ys[i] = y.At((p.Cost/math.Max(r.BaselineCost, 1e-9) - 1) * 100)
		}
		chart.Plot(c, chart.Axes{Left: y.Ticks(3), Bottom: x.Ticks(4), Label: func(side chart.Side, tick chart.Tick) {
			label := tick.Label
			if side == chart.Left {
				label = fmt.Sprintf("%+.1f%%", float64(tick.Value))
			}
			ui.Text(c, label).FontSize(12).TextColor(c.Theme().TextMuted)
		}}, func(e ui.Element) {
			e.Background(c.Theme().Background).Draw(func(p *ui.Painter, rect ui.Rect) {
				p.StrokePath(chart.Line(rect, xs, ys, chart.CurveLinear), 2, c.Theme().Accent)
				at := chart.X(rect, x.At(float64(r.Recommended)))
				p.Line(at, rect.Y, at, rect.Y+rect.H, 2, ui.Hex("#3ddc97"))
			})
		}).Height(130)
	})
}
