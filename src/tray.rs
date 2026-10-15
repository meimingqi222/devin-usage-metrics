//! 托盘内容视图；业务状态与窗口事件由外部注入。
use crate::tray_controller::TrayAction;
use crate::tray_model::*;
use devin_usage_metrics::{i18n, pricing, quota};
use gpui::{
    div, point, prelude::*, px, relative, rgb, Animation, AnimationExt as _, Context, Entity,
    IntoElement, MouseButton, PathBuilder, Render, SharedString, Window,
};
use gpui_app_host::TraySession;
use std::borrow::Cow;
use std::time::Duration;

fn loading_spinner(id: &'static str) -> impl IntoElement {
    gpui::svg()
        .path("icons/refresh.svg")
        .size(px(13.))
        .text_color(rgb(MUTED))
        .with_animation(
            id,
            Animation::new(Duration::from_millis(900)).repeat(),
            |icon, delta| {
                icon.with_transformation(gpui::Transformation::rotate(gpui::percentage(delta)))
            },
        )
}

const BG: u32 = 0x101014;
const PANEL: u32 = 0x181820;
const PANEL2: u32 = 0x22222b;
const BORDER: u32 = 0x2c2c38;
const TEXT: u32 = 0xe6e6ee;
const MUTED: u32 = 0x8f8fa3;
const ACCENT: u32 = 0x4cc2ff;
const GREEN: u32 = 0x3ddc97;
const YELLOW: u32 = 0xfbbf24;
const RED: u32 = 0xf87171;

/// 嵌入式矢量资产源（用于渲染 SVG 图标）
pub struct EmbeddedAssets;

impl gpui::AssetSource for EmbeddedAssets {
    fn load(&self, path: &str) -> gpui::Result<Option<Cow<'static, [u8]>>> {
        let bytes: Option<&'static [u8]> = match path {
            "icons/refresh.svg" => Some(include_bytes!("../assets/icons/refresh.svg")),
            "icons/external.svg" => Some(include_bytes!("../assets/icons/external.svg")),
            "icons/close.svg" => Some(include_bytes!("../assets/icons/close.svg")),
            _ => None,
        };
        Ok(bytes.map(Cow::Borrowed))
    }

    fn list(&self, _path: &str) -> gpui::Result<Vec<SharedString>> {
        Ok(vec![
            "icons/refresh.svg".into(),
            "icons/external.svg".into(),
            "icons/close.svg".into(),
        ])
    }
}

pub fn provider_color(provider: quota::Provider) -> u32 {
    match provider {
        quota::Provider::Claude => 0xd97757,
        quota::Provider::Codex => 0x3ddc97,
        quota::Provider::Grok => 0xe6e6ee,
        quota::Provider::Devin => 0x4cc2ff,
        quota::Provider::Antigravity => 0x8b5cf6,
    }
}

pub fn remaining_color(remaining_pct: f64) -> u32 {
    if remaining_pct > 50.0 {
        GREEN
    } else if remaining_pct > 20.0 {
        YELLOW
    } else {
        RED
    }
}

pub fn window_label_text(label: &quota::WindowLabel) -> String {
    match label {
        quota::WindowLabel::FiveHour => i18n::t(i18n::Key::QuotaWindowFiveHour).into(),
        quota::WindowLabel::SevenDay => i18n::t(i18n::Key::QuotaWindowSevenDay).into(),
        quota::WindowLabel::SevenDayOauthApps => i18n::t(i18n::Key::QuotaWindowSevenDayApps).into(),
        quota::WindowLabel::SevenDayOpus => i18n::t(i18n::Key::QuotaWindowSevenDayOpus).into(),
        quota::WindowLabel::SevenDaySonnet => i18n::t(i18n::Key::QuotaWindowSevenDaySonnet).into(),
        quota::WindowLabel::Daily => i18n::t(i18n::Key::QuotaWindowDaily).into(),
        quota::WindowLabel::Weekly => i18n::t(i18n::Key::QuotaWindowWeekly).into(),
        quota::WindowLabel::Monthly => i18n::t(i18n::Key::QuotaWindowMonthly).into(),
        quota::WindowLabel::Custom(name) => name.clone(),
    }
}

/// 快速聚合指定周期的历史消费数据
pub struct TrayPopover {
    model: Entity<TrayModel>,
    session: TraySession<TrayAction>,
}
impl TrayPopover {
    pub fn new(
        model: Entity<TrayModel>,
        session: TraySession<TrayAction>,
        cx: &mut Context<Self>,
    ) -> Self {
        cx.observe(&model, |_, _, cx| cx.notify()).detach();
        Self { model, session }
    }
    /// 渲染顶部 Donut Chart 与消费分布列表
    fn render_overview_section(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let overview = self.model.read(cx).overview.clone().unwrap_or_default();
        let total_val = if overview.total_cost > 0.0 {
            pricing::fmt_cost(overview.total_cost)
        } else if overview.total_tokens > 0.0 {
            format!("{:.1}M", overview.total_tokens / 1e6)
        } else {
            "$0.00".into()
        };

        let sub_label = if overview.total_cost > 0.0 {
            i18n::t(i18n::Key::TrayDollars)
        } else {
            i18n::t(i18n::Key::TrayTokens)
        };

        let items_for_chart = overview.items.clone();
        let total_cost_chart = overview.total_cost;

        // 环形图 Canvas (72x72px)
        let donut_chart = div()
            .w(px(76.))
            .h(px(76.))
            .flex_shrink_0()
            .relative()
            .child(
                gpui::canvas(
                    |_, _, _| (),
                    move |bounds, _, window, _| {
                        let cx = bounds.left() + px(38.);
                        let cy = bounds.top() + px(38.);
                        let radius: f32 = 33.;

                        if items_for_chart.is_empty() || total_cost_chart <= 0.0 {
                            let mut base = PathBuilder::stroke(px(5.));
                            for i in 0..=32 {
                                let a = (i as f32 / 32.) * std::f32::consts::TAU;
                                let pt =
                                    point(cx + px(radius * a.cos()), cy + px(radius * a.sin()));
                                if i == 0 {
                                    base.move_to(pt);
                                } else {
                                    base.line_to(pt);
                                }
                            }
                            if let Ok(p) = base.build() {
                                window.paint_path(p, rgb(0x282834));
                            }
                            return;
                        }

                        let mut cur_angle = -std::f32::consts::FRAC_PI_2;
                        let gap = 0.06; // 扇区微缝

                        for item in &items_for_chart {
                            let sweep = ((item.percentage as f32) / 100.) * std::f32::consts::TAU;
                            if sweep < 0.08 {
                                continue;
                            }
                            let a_start = cur_angle + gap;
                            let a_end = cur_angle + sweep - gap;
                            cur_angle += sweep;

                            if a_end <= a_start {
                                continue;
                            }

                            let steps = 16;
                            let mut builder = PathBuilder::stroke(px(5.));
                            for i in 0..=steps {
                                let t = i as f32 / steps as f32;
                                let a = a_start + (a_end - a_start) * t;
                                let pt =
                                    point(cx + px(radius * a.cos()), cy + px(radius * a.sin()));
                                if i == 0 {
                                    builder.move_to(pt);
                                } else {
                                    builder.line_to(pt);
                                }
                            }
                            if let Ok(path) = builder.build() {
                                window.paint_path(path, rgb(item.color));
                            }
                        }
                    },
                )
                .size_full(),
            )
            .child(
                div()
                    .absolute()
                    .left(px(13.))
                    .top(px(25.))
                    .w(px(50.))
                    .h(px(26.))
                    .flex()
                    .flex_col()
                    .items_center()
                    .justify_center()
                    .gap(px(0.))
                    .child(
                        div()
                            .w_full()
                            .text_center()
                            .whitespace_nowrap()
                            .overflow_hidden()
                            .text_ellipsis()
                            .text_size(px(11.))
                            .line_height(px(14.))
                            .font_weight(gpui::FontWeight::BOLD)
                            .text_color(rgb(TEXT))
                            .child(total_val),
                    )
                    .child(
                        div()
                            .text_size(px(9.))
                            .line_height(px(12.))
                            .text_color(rgb(MUTED))
                            .child(sub_label),
                    ),
            );

        // 右侧列表项（最多前 4 个）
        let mut list_col = div().flex_1().flex().flex_col().gap(px(3.5));
        if overview.items.is_empty() {
            list_col = list_col.child(
                div()
                    .py_4()
                    .text_size(px(11.))
                    .text_color(rgb(MUTED))
                    .child(i18n::t(i18n::Key::TrayNoRecords)),
            );
        } else {
            for item in overview.items.iter().take(4) {
                let cost_str = if item.cost > 0.0 {
                    pricing::fmt_cost(item.cost)
                } else {
                    format!("{:.1}K", item.tokens / 1e3)
                };
                list_col = list_col.child(
                    div()
                        .flex()
                        .items_center()
                        .justify_between()
                        .text_size(px(11.))
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap(px(6.))
                                .child(
                                    div()
                                        .w(px(6.5))
                                        .h(px(6.5))
                                        .rounded_full()
                                        .bg(rgb(item.color)),
                                )
                                .child(div().text_color(rgb(TEXT)).child(item.name.clone())),
                        )
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap(px(4.))
                                .child(
                                    div()
                                        .font_weight(gpui::FontWeight::SEMIBOLD)
                                        .text_color(rgb(TEXT))
                                        .child(cost_str),
                                )
                                .child(
                                    div()
                                        .text_size(px(10.))
                                        .text_color(rgb(MUTED))
                                        .child(format!("({:.0}%)", item.percentage)),
                                ),
                        ),
                );
            }
        }

        // 周期切换胶囊
        let period_capsule = div()
            .flex()
            .items_center()
            .bg(rgb(PANEL2))
            .p(px(2.))
            .rounded_md()
            .child(self.period_pill(OverviewPeriod::Today, cx))
            .child(self.period_pill(OverviewPeriod::Yesterday, cx))
            .child(self.period_pill(OverviewPeriod::ThirtyDays, cx));

        div()
            .p_2p5()
            .rounded_lg()
            .bg(rgb(PANEL))
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
                            .text_size(px(12.))
                            .font_weight(gpui::FontWeight::SEMIBOLD)
                            .text_color(rgb(TEXT))
                            .child(i18n::t(i18n::Key::TrayCostUsage)),
                    )
                    .child(period_capsule),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_3()
                    .when(
                        self.model.read(cx).overview_loading
                            && self.model.read(cx).overview.is_none(),
                        |row| {
                            row.h(px(76.))
                                .justify_center()
                                .text_size(px(11.))
                                .text_color(rgb(MUTED))
                                .child(loading_spinner("overview-spinner"))
                                .child(i18n::t(i18n::Key::TrayLoadingUsage))
                        },
                    )
                    .when(
                        !(self.model.read(cx).overview_loading
                            && self.model.read(cx).overview.is_none()),
                        |row| row.child(donut_chart).child(list_col),
                    ),
            )
    }

    fn period_pill(&self, p: OverviewPeriod, cx: &mut Context<Self>) -> impl IntoElement {
        let is_selected = self.model.read(cx).period == p;
        div()
            .id(SharedString::from(format!("pill-{p:?}")))
            .px_2()
            .py(px(1.5))
            .rounded_sm()
            .text_size(px(10.))
            .cursor_pointer()
            .when(is_selected, |d| {
                d.bg(rgb(0x353542))
                    .text_color(rgb(TEXT))
                    .font_weight(gpui::FontWeight::SEMIBOLD)
            })
            .when(!is_selected, |d| {
                d.text_color(rgb(MUTED)).hover(|h| h.text_color(rgb(TEXT)))
            })
            .child(p.label())
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .on_click(cx.listener(move |this, _, _, cx| {
                this.session.emit(TrayAction::Period(p), cx);
            }))
    }

    /// 渲染极高密度的单个配额卡片
    fn render_dense_card(
        &self,
        card: &TrayQuotaCard,
        _cx: &mut Context<Self>,
    ) -> Option<gpui::Div> {
        let result = card.result.as_ref().ok()?;
        if result.windows.is_empty() {
            return None; // 无配额窗口的移到其它分类折叠展示
        }

        let provider_name = card.provider.label();
        let display_title = if card.label.trim().is_empty() || card.label == provider_name {
            provider_name.to_string()
        } else if card.label.contains('@') {
            card.label.clone()
        } else {
            format!("{} ({})", provider_name, card.label)
        };

        let mut card_div = div()
            .w_full()
            .p_2p5()
            .rounded_lg()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .flex()
            .flex_col()
            .gap(px(7.));

        // 单行 Header: 圆点 + 名称 + 计划徽章
        let mut header = div().flex().items_center().justify_between().gap_2().child(
            div()
                .flex()
                .items_center()
                .gap_1p5()
                .min_w(px(0.))
                .flex_1()
                .child(
                    div()
                        .w(px(6.5))
                        .h(px(6.5))
                        .rounded_full()
                        .flex_shrink_0()
                        .bg(rgb(provider_color(card.provider))),
                )
                .child(
                    div()
                        .text_size(px(11.5))
                        .font_weight(gpui::FontWeight::SEMIBOLD)
                        .text_color(rgb(TEXT))
                        .overflow_hidden()
                        .whitespace_nowrap()
                        .text_ellipsis()
                        .child(display_title),
                ),
        );

        if let Some(plan) = &result.plan {
            let plan_color = if plan == "API MODE" { 0x34d399 } else { ACCENT };
            header = header.child(
                div()
                    .px_1p5()
                    .py(px(1.))
                    .rounded_sm()
                    .bg(rgb(PANEL2))
                    .text_size(px(10.))
                    .text_color(rgb(plan_color))
                    .child(if plan == "API MODE" {
                        i18n::t(i18n::Key::TrayApiMode).to_string()
                    } else {
                        plan.clone()
                    }),
            );
        }
        card_div = card_div.child(header);

        // 紧凑配额行
        for w in &result.windows {
            let remaining_pct = (100.0 - w.used_percent).clamp(0.0, 100.0);
            let bar_pct = (remaining_pct / 100.0) as f32;
            let status_color = remaining_color(remaining_pct);

            let mut row1 = div()
                .flex()
                .items_center()
                .justify_between()
                .text_size(px(10.5))
                .child(
                    div()
                        .font_weight(gpui::FontWeight::MEDIUM)
                        .text_color(rgb(TEXT))
                        .child(window_label_text(&w.label)),
                );

            // 濒临限流预警标注
            if remaining_pct < 20.0 {
                row1 = row1.child(
                    div()
                        .text_size(px(10.))
                        .font_weight(gpui::FontWeight::SEMIBOLD)
                        .text_color(rgb(RED))
                        .child(i18n::tf(
                            i18n::Key::TrayLimitAlert,
                            &[&format!("{remaining_pct:.0}")],
                        )),
                );
            } else {
                row1 = row1.child(
                    div()
                        .font_weight(gpui::FontWeight::SEMIBOLD)
                        .text_color(rgb(status_color))
                        .child(i18n::tf(
                            i18n::Key::TrayRemaining,
                            &[&format!("{remaining_pct:.0}")],
                        )),
                );
            }

            // 3px 极细进度条
            let progress_bar = div()
                .w_full()
                .h(px(3.))
                .rounded_full()
                .bg(rgb(PANEL2))
                .child(
                    div()
                        .h_full()
                        .w(relative(bar_pct))
                        .rounded_full()
                        .bg(rgb(status_color)),
                );

            let mut row3 = div()
                .flex()
                .items_center()
                .justify_between()
                .text_size(px(10.))
                .text_color(rgb(MUTED))
                .child(div().child(i18n::tf(
                    i18n::Key::TrayUsed,
                    &[&format!("{:.1}", w.used_percent)],
                )));

            if let Some(ts) = w.resets_at {
                let countdown = quota::fmt_countdown(ts);
                if !countdown.is_empty() {
                    row3 =
                        row3.child(div().child(i18n::tf(i18n::Key::QuotaResetsIn, &[&countdown])));
                }
            }

            card_div = card_div.child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(3.))
                    .child(row1)
                    .child(progress_bar)
                    .child(row3),
            );
        }

        Some(card_div)
    }
}

impl Render for TrayPopover {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        // 顶栏 Header
        let header = div()
            .flex()
            .items_center()
            .justify_between()
            .pb_1p5()
            .border_b_1()
            .border_color(rgb(BORDER))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_2()
                    .child(
                        div()
                            .text_size(px(12.))
                            .font_weight(gpui::FontWeight::BOLD)
                            .text_color(rgb(TEXT))
                            .child(i18n::t(i18n::Key::LiveQuotaTitle)),
                    )
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap_1()
                            .text_size(px(10.))
                            .text_color(rgb(MUTED))
                            .when(
                                self.model.read(cx).loading || self.model.read(cx).overview_loading,
                                |status| status.child(loading_spinner("header-spinner")),
                            )
                            .child(
                                if self.model.read(cx).loading
                                    || self.model.read(cx).overview_loading
                                {
                                    i18n::t(i18n::Key::TrayUpdating).into()
                                } else if !self.model.read(cx).updated_at.is_empty() {
                                    i18n::tf(
                                        i18n::Key::QuotaUpdated,
                                        &[&self.model.read(cx).updated_at],
                                    )
                                } else {
                                    String::new()
                                },
                            ),
                    ),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_1()
                    // 刷新按钮 (SVG)
                    .child(
                        div()
                            .id("popover-refresh")
                            .p_1()
                            .rounded_sm()
                            .cursor_pointer()
                            .text_color(rgb(MUTED))
                            .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                            .child(
                                gpui::svg()
                                    .path("icons/refresh.svg")
                                    .size(px(13.))
                                    .text_color(rgb(MUTED)),
                            )
                            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.session.emit(TrayAction::Refresh, cx);
                            })),
                    )
                    // 关闭浮窗按钮 (SVG)
                    .child(
                        div()
                            .id("popover-close")
                            .p_1()
                            .rounded_sm()
                            .cursor_pointer()
                            .text_color(rgb(MUTED))
                            .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                            .child(
                                gpui::svg()
                                    .path("icons/close.svg")
                                    .size(px(13.))
                                    .text_color(rgb(MUTED)),
                            )
                            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                            .on_click(cx.listener(move |_this, _, _, cx| {
                                _this.session.dismiss(cx);
                            })),
                    ),
            );

        // 中部内容区（可滚动）
        let mut scroll_content = div()
            .flex_1()
            .min_h(px(0.))
            .flex()
            .flex_col()
            .gap(px(8.))
            .py_1();

        // 1. 顶部 Donut Chart 消费统计图谱
        scroll_content = scroll_content.child(self.render_overview_section(cx));

        // 2. 分隔标签
        scroll_content = scroll_content.child(
            div()
                .pt_1()
                .text_size(px(10.))
                .font_weight(gpui::FontWeight::BOLD)
                .text_color(rgb(MUTED))
                .child(i18n::t(i18n::Key::TrayQuotaLimits)),
        );

        // 3. 配额卡片列表与收纳折叠
        let mut active_cards = Vec::new();
        let mut idle_accounts = Vec::new();

        let cards = self.model.read(cx).cards.clone();
        for card in &cards {
            if let Some(card_elem) = self.render_dense_card(card, cx) {
                active_cards.push(card_elem);
            } else {
                let plan_str = card
                    .result
                    .as_ref()
                    .ok()
                    .and_then(|r| r.plan.clone())
                    .map(|plan| {
                        if plan == "API MODE" {
                            i18n::t(i18n::Key::TrayApiMode).into()
                        } else {
                            plan
                        }
                    })
                    .unwrap_or_else(|| i18n::t(i18n::Key::TrayLocal).into());
                idle_accounts.push(format!("{} ({})", card.provider.label(), plan_str));
            }
        }

        for card in active_cards {
            scroll_content = scroll_content.child(card);
        }
        if self.model.read(cx).loading {
            scroll_content = scroll_content.child(
                div()
                    .py_4()
                    .flex()
                    .items_center()
                    .justify_center()
                    .gap_2()
                    .text_size(px(11.))
                    .text_color(rgb(MUTED))
                    .child(loading_spinner("quota-spinner"))
                    .child(i18n::t(i18n::Key::QuotaLoading)),
            );
        }

        // 4. 收纳无配额窗口的账号（API Mode / Local Source）
        if !idle_accounts.is_empty() {
            let expanded = self.model.read(cx).other_accounts_expanded;
            let arrow = if expanded { "▾" } else { "▸" };
            let count = idle_accounts.len();

            let mut idle_sec =
                div()
                    .p_2()
                    .rounded_md()
                    .bg(rgb(PANEL))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .flex()
                    .flex_col()
                    .gap(px(4.))
                    .child(
                        div()
                            .id("toggle-idle-accounts")
                            .flex()
                            .items_center()
                            .justify_between()
                            .cursor_pointer()
                            .text_size(px(10.5))
                            .text_color(rgb(MUTED))
                            .hover(|h| h.text_color(rgb(TEXT)))
                            .child(div().flex().items_center().gap_1().child(arrow).child(
                                i18n::tf(i18n::Key::TrayOtherSources, &[&count.to_string()]),
                            ))
                            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.session.emit(TrayAction::ToggleAccounts, cx);
                            })),
                    );

            if expanded {
                idle_sec = idle_sec.child(div().flex().flex_wrap().gap_1().pt_1().children(
                    idle_accounts.into_iter().map(|acc| {
                        div()
                            .px_2()
                            .py(px(1.))
                            .rounded_sm()
                            .bg(rgb(PANEL2))
                            .text_size(px(9.5))
                            .text_color(rgb(MUTED))
                            .child(acc)
                    }),
                ));
            }

            scroll_content = scroll_content.child(idle_sec);
        }

        // 底栏 Footer

        let footer = div()
            .pt_1p5()
            .border_t_1()
            .border_color(rgb(BORDER))
            .flex()
            .items_center()
            .justify_between()
            .child(
                div()
                    .text_size(px(10.))
                    .text_color(rgb(MUTED))
                    .child("Agent Metrics v0.2.10"),
            )
            .child(
                div()
                    .id("popover-view-full")
                    .text_size(px(10.5))
                    .font_weight(gpui::FontWeight::MEDIUM)
                    .text_color(rgb(ACCENT))
                    .cursor_pointer()
                    .hover(|h| h.text_color(rgb(0x6ad4ff)))
                    .child(format!("{} →", i18n::t(i18n::Key::TrayOpenDashboard)))
                    .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                    .on_click(cx.listener(move |_this, _, _, cx| {
                        _this.session.emit(TrayAction::OpenDashboard, cx);
                    })),
            );

        div()
            .size_full()
            .bg(rgb(BG))
            .border_1()
            .border_color(rgb(BORDER))
            .rounded_xl()
            .p_3()
            .flex()
            .flex_col()
            .gap(px(6.))
            .child(header)
            .child(gpui_app_widgets::scroll_area::ScrollView::new(
                "popover-scroll",
                gpui_app_widgets::scroll::ScrollbarStyle {
                    track: rgb(PANEL).into(),
                    thumb: rgb(BORDER).into(),
                },
                scroll_content,
            ))
            .child(footer)
    }
}
