use super::*;
use devin_usage_metrics::compaction_config::Target;
use i18n::{t, Key};

pub fn curve(r: &compaction::Recommendation) -> impl IntoElement {
    let values = r.curve.clone();
    let first = values.first().map(|p| p.0).unwrap_or(0);
    let last = values.last().map(|p| p.0).unwrap_or(0);
    let lo = values.iter().map(|p| p.1).fold(f64::INFINITY, f64::min);
    let hi = values.iter().map(|p| p.1).fold(0.0, f64::max);
    let recommended = r.recommended;
    div()
        .w_full()
        .p_3()
        .rounded_md()
        .bg(rgb(BG))
        .flex()
        .flex_col()
        .gap_1()
        .child(
            div()
                .text_xs()
                .text_color(rgb(MUTED))
                .child(t(Key::CompactCurve)),
        )
        .child(
            div()
                .flex()
                .gap_3()
                .child(
                    div()
                        .w(px(70.))
                        .h(px(90.))
                        .flex()
                        .flex_col()
                        .justify_between()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child(pricing::fmt_cost(hi))
                        .child(pricing::fmt_cost(lo)),
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
                .pl(px(82.))
                .flex()
                .justify_between()
                .text_xs()
                .text_color(rgb(MUTED))
                .child(fmt_tokens(first as f64))
                .child(div().text_color(rgb(C_OUT)).child(format!(
                    "{} {}",
                    t(Key::CompactionRecommended),
                    fmt_tokens(recommended as f64)
                )))
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
        let mut panel = div()
            .w_full()
            .min_w(px(0.))
            .pt_3()
            .mt_1()
            .border_t_1()
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
        panel = panel
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .child(
                        div()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .child(t(Key::CompactConfig)),
                    )
                    .when(setting.ours, |d| {
                        d.child(
                            div()
                                .text_xs()
                                .text_color(rgb(C_OUT))
                                .child(format!("✓ {}", t(Key::CompactApplied))),
                        )
                    }),
            )
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .whitespace_normal()
                    .child(target.path.display().to_string()),
            )
            .child(
                div()
                    .p_2()
                    .rounded_md()
                    .bg(rgb(BG))
                    .text_sm()
                    .whitespace_normal()
                    .child(format!(
                        "{}: {}{}",
                        target.key,
                        current,
                        proposal
                            .as_ref()
                            .filter(|v| setting.value.as_ref() != Some(*v))
                            .map(|v| format!("  →  {v}"))
                            .unwrap_or_default()
                    )),
            );
        let busy = self.compaction_config_task.is_some();
        let mut actions = div().flex().items_center().gap_2();
        if proposal.is_some_and(|p| setting.value.as_ref() != Some(&p)) {
            let apply_target = target.clone();
            let expected = setting.value.clone();
            let threshold = trigger.unwrap();
            let enabled = !busy && !setting.locked;
            actions = actions.child(
                div()
                    .id(SharedString::from(format!("compact-apply-{model}")))
                    .px_3()
                    .py_2()
                    .rounded_md()
                    .bg(rgb(if enabled { ACCENT } else { PANEL2 }))
                    .text_sm()
                    .text_color(rgb(if enabled { BG } else { MUTED }))
                    .child(t(Key::CompactApply))
                    .when(enabled, |d| {
                        d.cursor_pointer()
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
            let expected = setting.value.clone();
            actions = actions.child(
                div()
                    .id(SharedString::from(format!("compact-undo-{model}")))
                    .px_3()
                    .py_2()
                    .rounded_md()
                    .border_1()
                    .border_color(rgb(BORDER))
                    .text_sm()
                    .child(t(Key::CompactUndo))
                    .when(!busy, |d| {
                        d.cursor_pointer()
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.change_compaction(target.clone(), expected.clone(), None, cx);
                            }))
                    }),
            );
        }
        panel.child(actions).child(
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
