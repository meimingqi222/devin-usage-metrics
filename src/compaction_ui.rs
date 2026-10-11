use super::*;
use devin_usage_metrics::compaction_config::Target;
use i18n::{t, Key};

/// 规范化指标卡片，对齐主界面 stat_card 风格
pub fn metric_card(
    label: impl Into<SharedString>,
    value: impl Into<SharedString>,
    sub: Option<impl Into<SharedString>>,
    color: u32,
) -> gpui::Div {
    div()
        .flex_1()
        .min_w(px(130.))
        .flex()
        .flex_col()
        .justify_between()
        .p_3()
        .rounded_md()
        .bg(rgb(PANEL2))
        .border_1()
        .border_color(rgb(BORDER))
        .child(div().text_xs().text_color(rgb(MUTED)).child(label.into()))
        .child(
            div()
                .py_1()
                .text_xl()
                .font_weight(gpui::FontWeight::BOLD)
                .text_color(rgb(color))
                .child(value.into()),
        )
        .child(
            div()
                .text_xs()
                .text_color(rgb(MUTED))
                .child(sub.map(|s| s.into()).unwrap_or_else(|| "".into())),
        )
}

pub fn curve(r: &compaction::Recommendation) -> impl IntoElement {
    let values: Vec<_> = r
        .curve
        .iter()
        .map(|(threshold, cost)| (*threshold, (cost / r.baseline_cost.max(1e-9) - 1.0) * 100.0))
        .collect();
    let first = values.first().map(|p| p.0).unwrap_or(0);
    let last = values.last().map(|p| p.0).unwrap_or(0);
    let lo = values.iter().map(|p| p.1).fold(f64::INFINITY, f64::min);
    let hi = values.iter().map(|p| p.1).fold(f64::NEG_INFINITY, f64::max);
    let recommended = r.recommended;
    let candidate = r.candidate;
    let label = if r.status == compaction::Status::Keep {
        Key::CompactKeepTitle
    } else {
        Key::CompactionRecommended
    };

    div()
        .w_full()
        .p_3()
        .rounded_md()
        .bg(rgb(PANEL2))
        .border_1()
        .border_color(rgb(BORDER))
        .flex()
        .flex_col()
        .gap_2()
        .child(
            div()
                .flex()
                .items_center()
                .justify_between()
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap_2()
                        .child(
                            div()
                                .text_xs()
                                .font_weight(gpui::FontWeight::BOLD)
                                .text_color(rgb(TEXT))
                                .child("费用敏感度曲线"),
                        )
                        .child(div().text_xs().text_color(rgb(MUTED)).child("越低越节省")),
                )
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap_3()
                        .text_xs()
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap_1()
                                .child(div().w(px(8.)).h(px(3.)).rounded_sm().bg(rgb(C_OUT)))
                                .child(div().text_color(rgb(MUTED)).child("建议/保持点")),
                        )
                        .when(candidate != recommended, |d| {
                            d.child(
                                div()
                                    .flex()
                                    .items_center()
                                    .gap_1()
                                    .child(div().w(px(8.)).h(px(3.)).rounded_sm().bg(rgb(MUTED)))
                                    .child(div().text_color(rgb(MUTED)).child("对比候选")),
                            )
                        })
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap_1()
                                .child(div().w(px(8.)).h(px(3.)).rounded_sm().bg(rgb(ACCENT)))
                                .child(div().text_color(rgb(MUTED)).child("模拟费用")),
                        ),
                ),
        )
        .child(
            div()
                .w_full()
                .p_2()
                .rounded_md()
                .bg(rgb(BG))
                .border_1()
                .border_color(rgb(BORDER))
                .flex()
                .gap_3()
                .child(
                    div()
                        .w(px(60.))
                        .h(px(90.))
                        .flex()
                        .flex_col()
                        .justify_between()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child(format!("{hi:+.1}%"))
                        .child(format!("{lo:+.1}%")),
                )
                .child(
                    gpui::canvas(
                        |_, _, _| (),
                        move |bounds, _, window, _| {
                            let coord = |threshold: u64, cost: f64| {
                                point(
                                    bounds.left()
                                        + bounds.size.width
                                            * ((threshold - first) as f32
                                                / (last - first).max(1) as f32),
                                    bounds.top()
                                        + px(5.)
                                        + (bounds.size.height - px(10.))
                                            * (1. - ((cost - lo) / (hi - lo).max(1e-9)) as f32),
                                )
                            };
                            let mut grid = gpui::PathBuilder::stroke(px(1.));
                            for fraction in [0., 0.5, 1.] {
                                let y = bounds.top() + bounds.size.height * fraction;
                                grid.move_to(point(bounds.left(), y));
                                grid.line_to(point(bounds.right(), y));
                            }
                            if let Ok(path) = grid.build() {
                                window.paint_path(path, rgb(BORDER));
                            }
                            let y = coord(first, 0.0).y;
                            let mut baseline = gpui::PathBuilder::stroke(px(1.));
                            baseline.move_to(point(bounds.left(), y));
                            baseline.line_to(point(bounds.right(), y));
                            if let Ok(path) = baseline.build() {
                                window.paint_path(path, rgb(MUTED));
                            }
                            let mut line = gpui::PathBuilder::stroke(px(2.));
                            for (i, (threshold, cost)) in values.iter().enumerate() {
                                if i == 0 {
                                    line.move_to(coord(*threshold, *cost));
                                } else {
                                    line.line_to(coord(*threshold, *cost));
                                }
                            }
                            if let Ok(path) = line.build() {
                                window.paint_path(path, rgb(ACCENT));
                            }
                            if candidate != recommended {
                                let x = coord(candidate, lo).x;
                                let mut alt = gpui::PathBuilder::stroke(px(1.));
                                alt.move_to(point(x, bounds.top()));
                                alt.line_to(point(x, bounds.bottom()));
                                if let Ok(path) = alt.build() {
                                    window.paint_path(path, rgb(MUTED));
                                }
                            }
                            let x = coord(recommended, lo).x;
                            let mut marker = gpui::PathBuilder::stroke(px(2.));
                            marker.move_to(point(x, bounds.top()));
                            marker.line_to(point(x, bounds.bottom()));
                            if let Ok(path) = marker.build() {
                                window.paint_path(path, rgb(C_OUT));
                            }
                        },
                    )
                    .flex_1()
                    .h(px(90.)),
                ),
        )
        .child(
            div()
                .pl(px(72.))
                .flex()
                .justify_between()
                .text_xs()
                .text_color(rgb(MUTED))
                .child(fmt_tokens(first as f64))
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap_2()
                        .child(div().text_color(rgb(C_OUT)).child(format!(
                            "{} {}",
                            t(label),
                            fmt_tokens(recommended as f64)
                        )))
                        .when(candidate != recommended, |d| {
                            d.child(div().text_color(rgb(MUTED)).child(format!(
                                "{} {}",
                                t(Key::CompactCandidate),
                                fmt_tokens(candidate as f64)
                            )))
                        }),
                )
                .child(fmt_tokens(last as f64)),
        )
}

impl Root {
    pub(super) fn compaction_config_panel(
        &self,
        device: &str,
        model: &str,
        trigger: Option<u64>,
        cx: &mut Context<Self>,
    ) -> gpui::Div {
        let panel = div()
            .w_full()
            .min_w(px(0.))
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL2))
            .border_1()
            .border_color(rgb(BORDER))
            .flex()
            .flex_col()
            .gap_2();
        if !device.is_empty() && device != data::device_id() {
            return panel.child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child(t(Key::CompactRemote)),
            );
        }
        let target = match Target::local(self.agent, model) {
            Ok(target) => target,
            Err(error) => return panel.child(div().text_xs().text_color(rgb(C_COST)).child(error)),
        };
        let setting = match target.read() {
            Ok(setting) => setting,
            Err(error) => return panel.child(div().text_xs().text_color(rgb(C_COST)).child(error)),
        };
        let proposal = trigger.and_then(|n| target.proposed(n).ok());
        let current = setting
            .effective
            .as_ref()
            .map(|v| v.to_string())
            .unwrap_or_else(|| t(Key::CompactDefault).into());

        let busy = self.compaction_config_task.is_some();
        let mut actions = div().flex().items_center().gap_2();
        if proposal
            .as_ref()
            .is_some_and(|p| setting.value.as_ref() != Some(p))
        {
            let apply_target = target.clone();
            let expected = setting.value.clone();
            let threshold = trigger.unwrap();
            let enabled = !busy && !setting.locked;
            actions = actions.child(
                div()
                    .id(SharedString::from(format!("compact-apply-{model}")))
                    .px_3()
                    .py_1()
                    .rounded_md()
                    .bg(rgb(if enabled { ACCENT } else { PANEL }))
                    .text_xs()
                    .font_weight(gpui::FontWeight::BOLD)
                    .text_color(rgb(if enabled { 0x0a0a0c } else { MUTED }))
                    .child(t(Key::CompactApply))
                    .when(enabled, |d| {
                        d.cursor_pointer()
                            .hover(|h| h.bg(rgb(0x6ad4ff)))
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.change_compaction(
                                    apply_target.clone(),
                                    expected.clone(),
                                    Some(threshold),
                                    cx,
                                );
                            }))
                    }),
            );
        }
        if setting.has_undo {
            let undo_target = target.clone();
            let expected = setting.value.clone();
            actions = actions.child(
                div()
                    .id(SharedString::from(format!("compact-undo-{model}")))
                    .px_3()
                    .py_1()
                    .rounded_md()
                    .border_1()
                    .border_color(rgb(BORDER))
                    .text_xs()
                    .text_color(rgb(TEXT))
                    .child(t(Key::CompactUndo))
                    .when(!busy, |d| {
                        d.cursor_pointer()
                            .hover(|h| h.bg(rgb(PANEL)))
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.change_compaction(
                                    undo_target.clone(),
                                    expected.clone(),
                                    None,
                                    cx,
                                );
                            }))
                    }),
            );
        }

        let row = div()
            .flex()
            .flex_wrap()
            .items_center()
            .justify_between()
            .gap_2()
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap_1()
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap_2()
                            .child(
                                div()
                                    .text_xs()
                                    .font_weight(gpui::FontWeight::BOLD)
                                    .text_color(rgb(TEXT))
                                    .child(format!("⚙ {}", t(Key::CompactConfig))),
                            )
                            .child(
                                div()
                                    .text_xs()
                                    .text_color(rgb(MUTED))
                                    .child(target.path.display().to_string()),
                            )
                            .when(setting.ours, |d| {
                                d.child(
                                    div()
                                        .text_xs()
                                        .font_weight(gpui::FontWeight::BOLD)
                                        .text_color(rgb(C_OUT))
                                        .child(format!("✓ {}", t(Key::CompactApplied))),
                                )
                            }),
                    )
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap_1()
                            .text_xs()
                            .child(
                                div()
                                    .text_color(rgb(MUTED))
                                    .child(format!("{}: ", target.key)),
                            )
                            .child(div().text_color(rgb(TEXT)).child(current))
                            .when_some(
                                proposal
                                    .as_ref()
                                    .filter(|v| setting.value.as_ref() != Some(*v)),
                                |d, p| {
                                    d.child(div().text_color(rgb(MUTED)).child("→")).child(
                                        div()
                                            .text_color(rgb(ACCENT))
                                            .font_weight(gpui::FontWeight::BOLD)
                                            .child(p.to_string()),
                                    )
                                },
                            ),
                    ),
            )
            .child(actions);

        panel.child(row).child(
            div()
                .text_xs()
                .text_color(rgb(MUTED))
                .whitespace_normal()
                .child(t(if setting.locked {
                    Key::CompactLocked
                } else {
                    Key::CompactScope
                })),
        )
    }

    fn change_compaction(
        &mut self,
        target: Target,
        expected: Option<serde_json::Value>,
        trigger: Option<u64>,
        cx: &mut Context<Self>,
    ) {
        if self.compaction_config_task.is_some() {
            return;
        }
        let work = cx
            .background_executor()
            .spawn(async move { target.change(expected, trigger) });
        self.compaction_config_task = Some(cx.spawn(async move |this, cx| {
            let result = work.await;
            this.update(cx, |this, cx| {
                this.compaction_notice = Some(match result {
                    Ok(()) => (
                        true,
                        t(if trigger.is_some() {
                            Key::CompactSaved
                        } else {
                            Key::CompactRestored
                        })
                        .into(),
                    ),
                    Err(error) => (false, error),
                });
                this.compaction_config_task = None;
                cx.notify();
            })
            .ok();
        }));
        cx.notify();
    }
}
