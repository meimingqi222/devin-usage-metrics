#![allow(unexpected_cfgs)]
// On Windows, hide the console window in release builds. In debug builds we
// keep the console so println!/eprintln! output is still visible while developing.
#![cfg_attr(
    all(target_os = "windows", not(debug_assertions)),
    windows_subsystem = "windows"
)]

use agg::{build_buckets_for, window_for, Bucket, PeriodKind};
use chrono::TimeZone;
use data::{AgentKind, LoadedData};
use devin_usage_metrics::{agg, data, pricing};
use gpui::{
    actions, div, prelude::*, px, rgb, size, Animation, AnimationExt as _, App, Application,
    Bounds, Context, KeyBinding, Render, SharedString, Task, Window, WindowBounds, WindowOptions,
};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

actions!(main, [Quit]);

const BG: u32 = 0x101014;
const PANEL: u32 = 0x1a1a21;
const PANEL2: u32 = 0x22222b;
const BORDER: u32 = 0x2c2c38;
const TEXT: u32 = 0xe6e6ee;
const MUTED: u32 = 0x8f8fa3;
const ACCENT: u32 = 0x4cc2ff;
const C_IN: u32 = 0x4cc2ff;
const C_OUT: u32 = 0x3ddc97;
const C_CACHED: u32 = 0x8b7cf6;
const C_COST: u32 = 0xfbbf24;

const MODEL_COLORS: [u32; 10] = [
    0xf472b6, 0xfbbf24, 0x60a5fa, 0x34d399, 0xa78bfa, 0xfb923c, 0xf87171, 0x4ade80, 0x22d3ee,
    0xfacc15,
];

fn fmt_tokens(n: f64) -> String {
    if n >= 1e9 {
        format!("{:.2}B", n / 1e9)
    } else if n >= 1e6 {
        format!("{:.1}M", n / 1e6)
    } else if n >= 1e3 {
        format!("{:.1}K", n / 1e3)
    } else {
        format!("{:.0}", n)
    }
}

fn fmt_ts(sec: i64) -> String {
    chrono::Local
        .timestamp_opt(sec, 0)
        .single()
        .map(|t| t.format("%m-%d %H:%M").to_string())
        .unwrap_or_else(|| "-".into())
}

fn rgba(hex: u32, alpha: f32) -> gpui::Hsla {
    let mut c = gpui::Hsla::from(rgb(hex));
    c.a = alpha;
    c
}

fn model_color(name: &str) -> u32 {
    let mut h: usize = 0;
    for b in name.bytes() {
        h = h.wrapping_mul(31).wrapping_add(b as usize);
    }
    MODEL_COLORS[h % MODEL_COLORS.len()]
}

#[derive(Clone, Copy, PartialEq)]
enum Tab {
    Usage,
    Sessions,
}

/// 会话列表的一行，在数据变化时预计算好展示所需的字符串，
/// 避免每次 render 都重新排序、查定价表、截断标题。
struct SessionRow {
    session: data::SessionRec,
    time_str: String,
    id_short: String,
    title_short: String,
    model_text: String,
    cost_str: String,
    total: f64,
}

struct Root {
    data: Arc<LoadedData>,
    loaded_agents: HashMap<AgentKind, Arc<LoadedData>>,
    agent: AgentKind,
    agent_menu_open: bool,
    tab: Tab,
    period: PeriodKind,
    page: usize,
    buckets: Vec<Bucket>,
    sessions_rows: Vec<SessionRow>,
    selected: Option<String>,
    loaded_at: String,
    loading: bool,
    has_loaded: bool,
    load_task: Option<Task<()>>,
    load_id: u64,
}

impl Root {
    fn switch_agent(&mut self, target_agent: AgentKind, cx: &mut Context<Self>) {
        if self.agent == target_agent && self.has_loaded {
            self.agent_menu_open = false;
            cx.notify();
            return;
        }

        self.agent = target_agent;
        self.agent_menu_open = false;
        self.selected = None;
        self.page = 0;

        if let Some(cached_data) = self.loaded_agents.get(&target_agent) {
            self.data = cached_data.clone();
            self.rebuild_buckets();
            self.rebuild_sessions();
            cx.notify();
            return;
        }

        self.start_load(false, cx);
    }

    fn start_load(&mut self, force: bool, cx: &mut Context<Self>) {
        // 序号递增后，上一个加载任务完成时会因 load_id 不匹配而丢弃结果，
        // 不会覆盖当前 Agent 的界面数据（GPUI 0.2 的 Task 没有 cancel 接口）
        self.load_id += 1;
        let load_id = self.load_id;
        self.loading = true;
        self.selected = None;
        cx.notify();

        let (start, end) = window_for(self.period, self.page);
        let agent = self.agent;
        let previous = self.data.clone();
        let load = cx.background_executor().spawn(async move {
            if force {
                data::reload_agent_from(previous, start, end, agent)
            } else {
                data::load_agent(agent, start, end)
            }
        });
        self.load_task = Some(cx.spawn(async move |this, cx| {
            let data = load.await;
            this.update(cx, |this, cx| {
                // 已有更新的加载任务时，直接丢弃本次结果
                if this.load_id != load_id {
                    return;
                }
                let arc_data = Arc::new(data);
                this.loaded_agents.insert(agent, arc_data.clone());
                this.loading = false;
                // 加载期间用户可能已切换到其他 Agent，只缓存结果，不覆盖当前界面
                if this.agent == agent {
                    this.data = arc_data;
                    this.has_loaded = true;
                    this.loaded_at = chrono::Local::now().format("%H:%M:%S").to_string();
                    this.rebuild_buckets();
                    this.rebuild_sessions();
                    cx.notify();
                }
            })
            .ok();
        }));
    }

    fn rebuild_buckets(&mut self) {
        self.buckets = build_buckets_for(&self.data, self.period, self.agent, self.page);
    }

    /// 按当前 Agent 预计算会话列表（排序、截断、费用、模型展示名）。
    fn rebuild_sessions(&mut self) {
        let mut rows: Vec<SessionRow> = self
            .data
            .sessions
            .iter()
            .filter(|s| s.agent == self.agent)
            .map(|s| {
                let total = s.input_tokens + s.output_tokens + s.cached_tokens;
                let cost_str = if s.agent == data::AgentKind::Claude
                    && (s.cache_creation_5m_tokens > 0.0 || s.cache_creation_1h_tokens > 0.0)
                {
                    pricing::turn_cost_claude(
                        &s.display_model(),
                        s.input_tokens,
                        s.output_tokens,
                        s.cached_tokens,
                        s.cache_creation_5m_tokens,
                        s.cache_creation_1h_tokens,
                    )
                } else {
                    pricing::turn_cost(
                        &s.display_model(),
                        s.input_tokens,
                        s.output_tokens,
                        s.cached_tokens,
                        s.cache_creation_tokens,
                    )
                }
                .map(pricing::fmt_cost)
                .unwrap_or_else(|| "—".into());
                let adaptive = s.selected_model == "adaptive";
                let model_text = if adaptive {
                    format!("adaptive → {}", s.display_model())
                } else if s.selected_model.is_empty() {
                    s.display_model()
                } else {
                    s.selected_model.clone()
                };
                SessionRow {
                    time_str: fmt_ts(s.last_activity_at),
                    id_short: truncate(&s.id, 18),
                    title_short: truncate(&s.title, 48),
                    model_text: truncate(&model_text, 30),
                    cost_str,
                    total,
                    session: s.clone(),
                }
            })
            .collect();
        rows.sort_by_key(|row| std::cmp::Reverse(row.session.last_activity_at));
        rows.truncate(500);
        self.sessions_rows = rows;
    }

    /// 判断当前窗口内是否已经有目标 Agent 的数据。
    /// 不仅检查总范围是否覆盖，还要检查该 Agent 在 [start, end) 内是否有会话重叠。
    fn has_data_for(&self, start: i64, end: i64, agent: AgentKind) -> bool {
        if self.data.turns_start == 0 && self.data.turns_end == 0 {
            return false;
        }
        if start < self.data.turns_start || end > self.data.turns_end {
            return false;
        }
        self.data
            .sessions
            .iter()
            .any(|s| s.agent == agent && s.created_at < end && s.last_activity_at >= start)
    }

    fn page_needs_load(&self) -> bool {
        let (start, end) = window_for(self.period, self.page);
        start < self.data.turns_start
            || end > self.data.turns_end
            || !self.has_data_for(start, end, self.agent)
    }

    fn agent_dropdown_overlay(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let current_agent = self.agent;
        let items = AgentKind::ALL.into_iter().map(|kind| {
            let is_selected = kind == current_agent;
            let count = self
                .loaded_agents
                .get(&kind)
                .map(|d| d.sessions.iter().filter(|s| s.agent == kind).count());

            div()
                .id(SharedString::from(format!("agent-opt-{}", kind.label())))
                .flex()
                .items_center()
                .justify_between()
                .px_3()
                .py_2()
                .rounded_md()
                .cursor_pointer()
                .when(is_selected, |d| {
                    d.bg(rgba(ACCENT, 0.18)).text_color(rgb(ACCENT))
                })
                .when(!is_selected, |d| {
                    d.text_color(rgb(TEXT)).hover(|h| h.bg(rgb(PANEL2)))
                })
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap_2()
                        .child(div().text_sm().child(kind.badge()))
                        .child(
                            div()
                                .text_sm()
                                .font_weight(if is_selected {
                                    gpui::FontWeight::BOLD
                                } else {
                                    gpui::FontWeight::MEDIUM
                                })
                                .child(kind.label()),
                        ),
                )
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap_2()
                        .when_some(count, |d, c| {
                            d.child(
                                div()
                                    .text_xs()
                                    .text_color(rgb(MUTED))
                                    .child(format!("{c} 会话")),
                            )
                        })
                        .when(is_selected, |d| {
                            d.child(
                                div()
                                    .text_xs()
                                    .font_weight(gpui::FontWeight::BOLD)
                                    .text_color(rgb(ACCENT))
                                    .child("✓"),
                            )
                        }),
                )
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.switch_agent(kind, cx);
                }))
        });

        div()
            .id("agent-dropdown-overlay")
            .absolute()
            .inset_0()
            .child(
                div()
                    .id("agent-dropdown-backdrop")
                    .absolute()
                    .inset_0()
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.agent_menu_open = false;
                        cx.notify();
                    })),
            )
            .child(
                div()
                    .id("agent-dropdown-menu")
                    .absolute()
                    .top(px(46.))
                    .left(px(160.))
                    .w(px(250.))
                    .p_1()
                    .rounded_lg()
                    .bg(rgb(PANEL))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .shadow_lg()
                    .flex()
                    .flex_col()
                    .gap_1()
                    .children(items),
            )
    }

    fn top_bar(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let tab = self.tab;
        let period = self.period;
        let page = self.page;
        let agent = self.agent;
        let show_period = tab == Tab::Usage;
        let loaded_at = self.loaded_at.clone();

        let tab_btn = |kind: Tab, name: &'static str| {
            let active = tab == kind;
            div()
                .id(SharedString::from(format!("tab-{name}")))
                .px_3()
                .py_1()
                .rounded_md()
                .text_sm()
                .cursor_pointer()
                .when(active, |d| d.bg(rgb(ACCENT)).text_color(rgb(0x0a0a0c)))
                .when(!active, |d| {
                    d.text_color(rgb(MUTED)).hover(|h| h.bg(rgb(PANEL2)))
                })
                .child(name)
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.tab = kind;
                    cx.notify();
                }))
        };

        let period_btn = |kind: PeriodKind| {
            let active = period == kind;
            div()
                .id(SharedString::from(format!("period-{}", kind.label())))
                .px_2()
                .py(px(2.))
                .rounded_sm()
                .text_xs()
                .cursor_pointer()
                .when(active, |d| d.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                .when(!active, |d| {
                    d.text_color(rgb(MUTED)).hover(|h| h.bg(rgb(PANEL2)))
                })
                .child(kind.label())
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.period = kind;
                    this.page = 0;
                    if this.page_needs_load() {
                        this.start_load(true, cx);
                    } else {
                        this.rebuild_buckets();
                        cx.notify();
                    }
                }))
        };

        let prev_page = div()
            .id("prev-page")
            .px_2()
            .py(px(2.))
            .rounded_sm()
            .text_xs()
            .when(page > 0, |d| {
                d.cursor_pointer()
                    .text_color(rgb(MUTED))
                    .hover(|h| h.bg(rgb(PANEL2)))
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.page = this.page.saturating_sub(1);
                        if this.page_needs_load() {
                            this.start_load(true, cx);
                        } else {
                            this.rebuild_buckets();
                            cx.notify();
                        }
                    }))
            })
            .when(page == 0, |d| {
                d.text_color(rgba(MUTED, 0.3)).cursor_default()
            })
            .child("◀ 上一页");

        let next_page = div()
            .id("next-page")
            .px_2()
            .py(px(2.))
            .rounded_sm()
            .text_xs()
            .cursor_pointer()
            .text_color(rgb(MUTED))
            .hover(|h| h.bg(rgb(PANEL2)))
            .child("下一页 ▶")
            .on_click(cx.listener(|this, _, _, cx| {
                this.page = this.page.saturating_add(1);
                if this.page_needs_load() {
                    this.start_load(true, cx);
                } else {
                    this.rebuild_buckets();
                    cx.notify();
                }
            }));

        let active_count = self
            .data
            .sessions
            .iter()
            .filter(|s| s.agent == agent)
            .count();
        let count_str = if active_count > 0 {
            format!(" ({active_count})")
        } else {
            String::new()
        };

        let agent_selector = div()
            .id("agent-selector-trigger")
            .flex()
            .items_center()
            .gap_2()
            .px_3()
            .py_1()
            .rounded_md()
            .border_1()
            .border_color(if self.agent_menu_open {
                rgb(ACCENT)
            } else {
                rgb(BORDER)
            })
            .bg(if self.agent_menu_open {
                rgb(PANEL2)
            } else {
                rgb(PANEL)
            })
            .text_sm()
            .cursor_pointer()
            .hover(|h| h.bg(rgb(PANEL2)).border_color(rgb(ACCENT)))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_2()
                    .child(agent.badge())
                    .child(
                        div()
                            .font_weight(gpui::FontWeight::MEDIUM)
                            .text_color(rgb(TEXT))
                            .child(format!("{}{}", agent.label(), count_str)),
                    ),
            )
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child(if self.agent_menu_open { "▲" } else { "▼" }),
            )
            .on_click(cx.listener(|this, _, _, cx| {
                this.agent_menu_open = !this.agent_menu_open;
                cx.notify();
            }));

        div()
            .flex()
            .w_full()
            .min_w(px(0.))
            .items_center()
            .gap_3()
            .px_4()
            .h(px(48.))
            .border_b_1()
            .border_color(rgb(BORDER))
            .child(
                div()
                    .text_sm()
                    .font_weight(gpui::FontWeight::BOLD)
                    .text_color(rgb(TEXT))
                    .child("Agent Usage Metrics"),
            )
            .child(
                div()
                    .flex()
                    .pl_2()
                    .border_l_1()
                    .border_color(rgb(BORDER))
                    .child(agent_selector),
            )
            .child(tab_btn(Tab::Usage, "用量"))
            .child(tab_btn(Tab::Sessions, "会话"))
            .when(show_period, |d| {
                d.child(
                    div()
                        .flex()
                        .gap_1()
                        .pl_3()
                        .border_l_1()
                        .border_color(rgb(BORDER))
                        .child(period_btn(PeriodKind::Day))
                        .child(period_btn(PeriodKind::Week))
                        .child(period_btn(PeriodKind::Month)),
                )
                .child(
                    div()
                        .flex()
                        .gap_1()
                        .pl_3()
                        .border_l_1()
                        .border_color(rgb(BORDER))
                        .child(prev_page)
                        .child(next_page),
                )
            })
            .child(div().flex_1())
            .child(
                div()
                    .id("reload")
                    .px_2()
                    .py(px(2.))
                    .rounded_sm()
                    .text_xs()
                    .flex()
                    .items_center()
                    .gap_2()
                    .when(!self.loading, |d| {
                        d.cursor_pointer()
                            .text_color(rgb(MUTED))
                            .hover(|h| h.bg(rgb(PANEL2)))
                    })
                    .when(self.loading, |d| d.text_color(rgba(MUTED, 0.55)))
                    .child(if self.loading {
                        loading_dots(5., 1.).into_any_element()
                    } else {
                        div().into_any_element()
                    })
                    .child(if self.loading {
                        "正在刷新…"
                    } else {
                        "重新加载"
                    })
                    .on_click(cx.listener(|this, _, _, cx| {
                        if !this.loading {
                            this.start_load(true, cx);
                        }
                    })),
            )
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child(format!("数据截至 {}", loaded_at)),
            )
    }

    fn loading_view(&self) -> impl IntoElement {
        div()
            .size_full()
            .bg(rgb(BG))
            .flex()
            .flex_col()
            .items_center()
            .justify_center()
            .gap_4()
            .child(loading_dots(10., 2.))
            .child(
                div()
                    .text_lg()
                    .font_weight(gpui::FontWeight::SEMIBOLD)
                    .text_color(rgb(TEXT))
                    .child("正在读取本地 Agent 用量数据"),
            )
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child("首次读取完成后，后续启动会使用 5 分钟缓存"),
            )
    }

    fn stat_card(label: &'static str, value: String, color: u32) -> impl IntoElement {
        div()
            .flex_1()
            .min_w(px(140.))
            .flex()
            .flex_col()
            .gap_1()
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child(label.to_string()),
            )
            .child(
                div()
                    .text_lg()
                    .font_weight(gpui::FontWeight::BOLD)
                    .text_color(rgb(color))
                    .child(value),
            )
    }

    fn usage_view(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let buckets = &self.buckets;
        let (mut ti, mut to, mut tc, mut turns, mut cost) = (0.0, 0.0, 0.0, 0u32, 0.0);
        let mut sessions = std::collections::BTreeSet::new();
        for b in buckets {
            ti += b.input;
            to += b.output;
            tc += b.cached;
            turns += b.turns;
            cost += b.cost;
            sessions.extend(b.session_keys.iter().cloned());
        }
        let total = ti + to + tc;

        let cards = div()
            .flex()
            .w_full()
            .min_w(px(0.))
            .gap_2()
            .child(Self::stat_card("总 Tokens", fmt_tokens(total), TEXT))
            .child(Self::stat_card("输入（新）", fmt_tokens(ti), C_IN))
            .child(Self::stat_card("输出", fmt_tokens(to), C_OUT))
            .child(Self::stat_card("缓存读取", fmt_tokens(tc), C_CACHED))
            .child(Self::stat_card(
                "费用 (USD)",
                pricing::fmt_cost(cost),
                C_COST,
            ))
            .child(Self::stat_card(
                "轮次 / 会话",
                format!("{turns} / {}", sessions.len()),
                MUTED,
            ));

        let latest_activity = self
            .data
            .sessions
            .iter()
            .filter(|session| session.agent == self.agent)
            .map(|session| session.last_activity_at)
            .max()
            .map(fmt_ts)
            .unwrap_or_else(|| "无记录".into());
        let empty_message = format!(
            "当前{}窗口没有{}用量；最近记录：{}。可点击“下一页 ▶”查看更早历史。",
            self.period.label(),
            self.agent.label(),
            latest_activity
        );
        let chart = self.chart();
        let table = self.bucket_table(cx);

        div()
            .id("usage-scroll")
            .size_full()
            .min_w(px(0.))
            .overflow_scroll()
            .p_4()
            .flex()
            .flex_col()
            .gap_3()
            .child(cards)
            .when(turns == 0, |d| {
                d.child(
                    div()
                        .w_full()
                        .min_w(px(0.))
                        .px_3()
                        .py_2()
                        .rounded_md()
                        .bg(rgb(PANEL))
                        .border_1()
                        .border_color(rgb(BORDER))
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child(empty_message.clone()),
                )
            })
            .child(chart)
            .child(table)
    }

    fn chart(&self) -> impl IntoElement {
        let max = self
            .buckets
            .iter()
            .map(|b| b.total())
            .fold(1.0f64, f64::max);
        let bar_area = px(140.);
        let bar_area_f: f32 = bar_area.into();
        let mut cols: Vec<gpui::Div> = Vec::new();
        for b in &self.buckets {
            let total = b.total();
            let scale = |v: f64| {
                if v <= 0.0 {
                    px(0.)
                } else {
                    px(((v / max) as f32 * bar_area_f).max(2.0))
                }
            };
            let h_in = scale(b.input);
            let h_out = scale(b.output);
            let h_ca = scale(b.cached);
            let label_color = if total <= 0.0 { MUTED } else { TEXT };
            cols.push(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .flex()
                    .flex_col()
                    .items_center()
                    .gap_1()
                    .child(
                        div()
                            .w_full()
                            .min_w(px(0.))
                            .text_center()
                            .whitespace_nowrap()
                            .text_xs()
                            .text_color(rgb(label_color))
                            .child(if total > 0.0 {
                                fmt_tokens(total)
                            } else {
                                "·".into()
                            }),
                    )
                    .child(
                        div()
                            .w(px(26.))
                            .h(bar_area)
                            .flex()
                            .flex_col()
                            .justify_end()
                            .child(div().w_full().h(h_ca).bg(rgba(C_CACHED, 0.9)))
                            .child(div().w_full().h(h_in).bg(rgba(C_IN, 0.9)))
                            .child(div().w_full().h(h_out).bg(rgba(C_OUT, 0.9))),
                    )
                    .child(
                        div()
                            .w_full()
                            .min_w(px(0.))
                            .text_center()
                            .whitespace_nowrap()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .child(b.label.clone()),
                    ),
            );
        }
        div()
            .w_full()
            .min_w(px(0.))
            .p_3()
            .rounded_md()
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
                    .gap_3()
                    .child(
                        div()
                            .text_sm()
                            .font_weight(gpui::FontWeight::SEMIBOLD)
                            .text_color(rgb(TEXT))
                            .child("Token 用量趋势"),
                    )
                    .child(legend("缓存", C_CACHED))
                    .child(legend("输入", C_IN))
                    .child(legend("输出", C_OUT)),
            )
            .child(
                div()
                    .w_full()
                    .min_w(px(0.))
                    .flex()
                    .items_end()
                    .gap_1()
                    .children(cols),
            )
    }

    fn bucket_table(&self, _cx: &mut Context<Self>) -> impl IntoElement {
        let header = div()
            .flex()
            .w_full()
            .min_w(px(0.))
            .gap_2()
            .px_2()
            .py_1()
            .text_xs()
            .text_color(rgb(MUTED))
            .border_b_1()
            .border_color(rgb(BORDER))
            .child(cell("周期", 90.))
            .child(cell_r("会话", 44.))
            .child(cell_r("轮次", 52.))
            .child(cell_r("输入", 70.))
            .child(cell_r("输出", 70.))
            .child(cell_r("缓存", 76.))
            .child(cell_r("总计", 76.))
            .child(cell_r("费用", 70.))
            .child(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .overflow_hidden()
                    .whitespace_nowrap()
                    .text_ellipsis()
                    .child("模型分布"),
            );
        let mut rows: Vec<gpui::Div> = Vec::new();
        for b in self.buckets.iter().rev() {
            let mut models: Vec<(String, f64)> = b
                .by_model
                .iter()
                .map(|(m, u)| (m.clone(), u.total()))
                .collect();
            models.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));
            let model_bits: Vec<gpui::Div> = models
                .iter()
                .filter(|(_, t)| *t > 0.0)
                .map(|(m, t)| {
                    div()
                        .flex()
                        .items_center()
                        .gap_1()
                        .mr_2()
                        .child(
                            div()
                                .w(px(7.))
                                .h(px(7.))
                                .rounded_full()
                                .bg(rgb(model_color(m))),
                        )
                        .child(format!("{} {}", m, fmt_tokens(*t)))
                })
                .collect();
            let empty = b.total() <= 0.0;
            rows.push(
                div()
                    .flex()
                    .w_full()
                    .min_w(px(0.))
                    .gap_2()
                    .px_2()
                    .py_1()
                    .text_xs()
                    .text_color(if empty {
                        rgba(MUTED, 0.5)
                    } else {
                        rgba(TEXT, 1.0)
                    })
                    .border_b_1()
                    .border_color(rgba(BORDER, 0.5))
                    .child(div().w(px(90.)).child(format!("{} {}", b.label, b.sub)))
                    .child(cell_r(&format!("{}", b.session_keys.len()), 44.))
                    .child(cell_r(&format!("{}", b.turns), 52.))
                    .child(cell_r(&fmt_tokens(b.input), 70.))
                    .child(cell_r(&fmt_tokens(b.output), 70.))
                    .child(cell_r(&fmt_tokens(b.cached), 76.))
                    .child(cell_r(&fmt_tokens(b.total()), 76.))
                    .child(cell_r(&pricing::fmt_cost(b.cost), 70.))
                    .child(
                        div()
                            .flex_1()
                            .min_w(px(0.))
                            .overflow_hidden()
                            .flex()
                            .flex_wrap()
                            .children(model_bits),
                    ),
            );
        }
        div()
            .w_full()
            .min_w(px(0.))
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .flex()
            .flex_col()
            .child(header)
            .children(rows)
    }

    fn sessions_view(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let detail = self
            .selected
            .as_ref()
            .map(|key| self.session_detail(key, cx));
        let header = div()
            .flex()
            .gap_2()
            .px_2()
            .py_1()
            .text_xs()
            .text_color(rgb(MUTED))
            .border_b_1()
            .border_color(rgb(BORDER))
            .child(cell("最后活跃", 84.))
            .child(cell("会话", 130.))
            .child(div().flex_1().child("标题"))
            .child(cell("模式", 70.))
            .child(cell("模型", 190.))
            .child(cell_r("消息", 48.))
            .child(cell_r("Tokens", 72.))
            .child(cell_r("费用", 64.));
        let mut rows: Vec<gpui::AnyElement> = Vec::new();
        for r in &self.sessions_rows {
            let s = &r.session;
            let key = s.key.clone();
            let is_sel = self.selected.as_deref() == Some(key.as_str());
            let adaptive = s.selected_model == "adaptive";
            let mut row = div()
                .id(SharedString::from(format!("srow-{}", key)))
                .flex()
                .gap_2()
                .px_2()
                .py_1()
                .text_xs()
                .cursor_pointer()
                .items_center()
                .hover(|h| h.bg(rgb(PANEL2)))
                .when(is_sel, |d| d.bg(rgb(PANEL2)))
                .border_b_1()
                .border_color(rgba(BORDER, 0.4))
                .child(cell(&r.time_str, 84.))
                .child(
                    div()
                        .w(px(130.))
                        .flex()
                        .gap_1()
                        .items_center()
                        .child(
                            div()
                                .w(px(6.))
                                .h(px(6.))
                                .rounded_full()
                                .bg(rgb(model_color(&s.display_model()))),
                        )
                        .child(r.id_short.clone()),
                )
                .child(div().flex_1().child(r.title_short.clone()))
                .child(cell(&s.agent_mode, 70.))
                .child(
                    div()
                        .w(px(190.))
                        .text_color(if adaptive { rgb(ACCENT) } else { rgb(TEXT) })
                        .child(r.model_text.clone()),
                )
                .child(cell_r(&format!("{:.0}", s.agent_messages), 48.))
                .child(cell_r(&fmt_tokens(r.total), 72.))
                .child(cell_r(&r.cost_str, 64.))
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.selected = Some(key.clone());
                    cx.notify();
                }));
            if self.agent == AgentKind::Devin && s.source == "cli-next" {
                row = row.child(div().text_xs().text_color(rgba(MUTED, 0.7)).child("next"));
            }
            rows.push(row.into_any_element());
        }
        let mut body = div()
            .id("sessions-scroll")
            .size_full()
            .min_w(px(0.))
            .overflow_scroll()
            .p_4()
            .flex()
            .flex_col()
            .gap_3();
        if let Some(d) = detail {
            body = body.child(d);
        }
        let empty = rows.is_empty();
        body.child(
            div()
                .w_full()
                .min_w(px(0.))
                .p_3()
                .rounded_md()
                .bg(rgb(PANEL))
                .border_1()
                .border_color(rgb(BORDER))
                .flex()
                .flex_col()
                .child(header)
                .when(empty, |panel| {
                    panel.child(
                        div()
                            .p_4()
                            .text_sm()
                            .text_color(rgb(MUTED))
                            .child(format!("未找到 {} 本地会话数据", self.agent.label())),
                    )
                })
                .children(rows),
        )
    }

    fn session_detail(&self, key: &str, cx: &mut Context<Self>) -> gpui::Div {
        let Some(s) = self.data.sessions.iter().find(|s| s.key == key) else {
            return div();
        };
        let turns: Vec<&data::TurnRec> = self
            .data
            .turns
            .iter()
            .filter(|t| t.session_key == key)
            .collect();
        let mut ttfts: Vec<f64> = turns
            .iter()
            .map(|t| t.ttft_ms)
            .filter(|ttft| *ttft > 0.0)
            .collect();
        ttfts.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
        let ttft_med = ttfts.get(ttfts.len() / 2).copied().unwrap_or(0.0);
        let total = s.input_tokens + s.output_tokens + s.cached_tokens;
        // 计算会话级费用：用会话的 display_model 查定价表
        let session_cost = if s.agent == data::AgentKind::Claude
            && (s.cache_creation_5m_tokens > 0.0 || s.cache_creation_1h_tokens > 0.0)
        {
            pricing::turn_cost_claude(
                &s.display_model(),
                s.input_tokens,
                s.output_tokens,
                s.cached_tokens,
                s.cache_creation_5m_tokens,
                s.cache_creation_1h_tokens,
            )
        } else {
            pricing::turn_cost(
                &s.display_model(),
                s.input_tokens,
                s.output_tokens,
                s.cached_tokens,
                s.cache_creation_tokens,
            )
        };
        let adaptive = s.selected_model == "adaptive";
        let model_text = if adaptive {
            format!("adaptive → {}（服务端路由）", s.display_model())
        } else {
            s.display_model()
        };
        let model_detail = if adaptive {
            format!("{}（配置值：{}）", model_text, s.selected_model)
        } else {
            model_text
        };

        let kv = |k: &str, v: String| {
            div()
                .flex()
                .gap_2()
                .text_xs()
                .child(div().w(px(84.)).text_color(rgb(MUTED)).child(k.to_string()))
                .child(div().flex_1().text_color(rgb(TEXT)).child(v))
        };

        div()
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgba(ACCENT, 0.4))
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
                            .text_sm()
                            .font_weight(gpui::FontWeight::SEMIBOLD)
                            .child(if s.title.is_empty() {
                                s.id.clone()
                            } else {
                                s.title.clone()
                            }),
                    )
                    .child(div().flex_1())
                    .child(
                        div()
                            .id("close-detail")
                            .px_2()
                            .text_xs()
                            .cursor_pointer()
                            .text_color(rgb(MUTED))
                            .hover(|h| h.text_color(rgb(TEXT)))
                            .child("✕")
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.selected = None;
                                cx.notify();
                            })),
                    ),
            )
            .child(kv(
                "会话",
                format!("{} · {} · {}", s.id, s.source, s.agent_mode),
            ))
            .child(kv("工作目录", s.working_directory.clone()))
            .child(kv("模型", model_detail))
            .child(kv(
                "时间",
                format!(
                    "{} 创建，最后活跃 {}",
                    fmt_ts(s.created_at),
                    fmt_ts(s.last_activity_at)
                ),
            ))
            .child(kv(
                "Tokens",
                format!(
                    "输入 {} · 输出 {} · 缓存 {} · 共 {}",
                    fmt_tokens(s.input_tokens),
                    fmt_tokens(s.output_tokens),
                    fmt_tokens(s.cached_tokens),
                    fmt_tokens(total)
                ),
            ))
            .child(kv(
                "费用",
                match session_cost {
                    Some(c) => format!("{}（按 {} 定价）", pricing::fmt_cost(c), s.display_model()),
                    None => "未知（无匹配定价）".into(),
                },
            ))
            .child(kv(
                "活动",
                if ttfts.is_empty() {
                    format!(
                        "{:.0} 条 agent 消息 · 窗口内 {} 轮",
                        s.agent_messages,
                        turns.len()
                    )
                } else {
                    format!(
                        "{:.0} 条 agent 消息 · 窗口内 {} 轮 · TTFT 中位 {:.0} ms",
                        s.agent_messages,
                        turns.len(),
                        ttft_med
                    )
                },
            ))
    }
}

fn legend(name: &'static str, color: u32) -> impl IntoElement {
    div()
        .flex()
        .items_center()
        .gap_1()
        .text_xs()
        .text_color(rgb(MUTED))
        .child(div().w(px(8.)).h(px(8.)).rounded_sm().bg(rgb(color)))
        .child(name.to_string())
}

fn cell(text: &str, w: f32) -> gpui::Div {
    div().w(px(w)).child(text.to_string())
}

fn cell_r(text: &str, w: f32) -> gpui::Div {
    div().w(px(w)).text_right().child(text.to_string())
}

fn truncate(s: &str, n: usize) -> String {
    let mut out = String::new();
    for (i, c) in s.chars().enumerate() {
        if i >= n {
            out.push('…');
            break;
        }
        out.push(c);
    }
    out
}

fn loading_dots(size: f32, gap: f32) -> impl IntoElement {
    let dots = (0..3).map(|index| {
        let offset = index as f32 / 3.0;
        div()
            .w(px(size))
            .h(px(size))
            .rounded_full()
            .bg(rgba(ACCENT, 0.25))
            .with_animation(
                SharedString::from(format!("loading-dot-{size}-{index}")),
                Animation::new(Duration::from_millis(900)).repeat(),
                move |dot, delta| {
                    let phase = ((delta + offset) % 1.0) * std::f32::consts::TAU;
                    let opacity = 0.25 + 0.75 * (phase.sin() * 0.5 + 0.5);
                    dot.bg(rgba(ACCENT, opacity))
                },
            )
    });
    div().flex().items_center().gap(px(gap)).children(dots)
}

impl Render for Root {
    fn render(&mut self, _window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        if self.loading && !self.has_loaded {
            return self.loading_view().into_any_element();
        }

        let content = match self.tab {
            Tab::Usage => self.usage_view(cx).into_any_element(),
            Tab::Sessions => self.sessions_view(cx).into_any_element(),
        };
        let errors: Vec<String> = self
            .data
            .errors
            .iter()
            .filter(|error| error.agent == self.agent)
            .map(|error| error.message.clone())
            .collect();
        let mut root = div()
            .size_full()
            .min_w(px(0.))
            .overflow_hidden()
            .bg(rgb(BG))
            .text_color(rgb(TEXT))
            .flex()
            .flex_col()
            .relative()
            .child(self.top_bar(cx));
        for e in errors {
            root = root.child(
                div()
                    .px_4()
                    .py_1()
                    .text_xs()
                    .text_color(rgb(0xf87171))
                    .child(format!("数据源警告：{e}")),
            );
        }
        root = root.child(content);

        if self.agent_menu_open {
            root = root.child(self.agent_dropdown_overlay(cx));
        }

        root.into_any_element()
    }
}

#[cfg(target_os = "macos")]
fn set_macos_dock_icon() {
    use objc::{class, msg_send, sel, sel_impl};
    const ICON_PNG: &[u8] = include_bytes!("../assets/icon.png");

    unsafe {
        let app: *mut objc::runtime::Object = msg_send![class!(NSApplication), sharedApplication];
        if app.is_null() {
            return;
        }
        let data: *mut objc::runtime::Object = msg_send![
            class!(NSData),
            dataWithBytes: ICON_PNG.as_ptr() as *const std::ffi::c_void
            length: ICON_PNG.len()
        ];
        if data.is_null() {
            return;
        }
        let image: *mut objc::runtime::Object = msg_send![class!(NSImage), alloc];
        let image: *mut objc::runtime::Object = msg_send![image, initWithData: data];
        if !image.is_null() {
            let _: () = msg_send![app, setApplicationIconImage: image];
            let _: () = msg_send![image, release];
        }
    }
}

fn main() {
    Application::new().run(move |cx: &mut App| {
        #[cfg(target_os = "macos")]
        set_macos_dock_icon();

        // Bind platform-specific quit shortcut
        #[cfg(target_os = "macos")]
        cx.bind_keys([KeyBinding::new("cmd-q", Quit, None)]);
        #[cfg(not(target_os = "macos"))]
        cx.bind_keys([KeyBinding::new("ctrl-q", Quit, None)]);
        cx.on_action(|_: &Quit, cx| cx.quit());

        let bounds = Bounds::centered(None, size(px(1180.), px(760.)), cx);
        cx.open_window(
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(bounds)),
                ..Default::default()
            },
            |window, cx| {
                // Quit app when window is closed (clicking the red X button)
                window.on_window_should_close(cx, |_, cx| {
                    cx.quit();
                    true // allow the window to close
                });

                cx.new(|cx| {
                    let mut root = Root {
                        data: Arc::new(LoadedData::default()),
                        loaded_agents: HashMap::new(),
                        agent: AgentKind::Devin,
                        agent_menu_open: false,
                        tab: Tab::Usage,
                        period: PeriodKind::Day,
                        page: 0,
                        buckets: Vec::new(),
                        sessions_rows: Vec::new(),
                        selected: None,
                        loaded_at: "加载中...".into(),
                        loading: false,
                        has_loaded: false,
                        load_task: None,
                        load_id: 0,
                    };
                    root.rebuild_buckets();
                    root.rebuild_sessions();
                    root.start_load(false, cx);
                    root
                })
            },
        )
        .unwrap();

        cx.activate(true);
    });
}
