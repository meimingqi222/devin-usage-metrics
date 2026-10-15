//! 托盘业务数据、加载和刷新；不持有窗口或原生托盘。
use chrono::{Local, TimeZone};
use devin_usage_metrics::{agg::cost_for_turn, data, data::AgentKind, i18n, quota};
use gpui::Context;
use gpui_app_host::PopoverLifetime;
use std::time::Duration;

pub fn agent_color(agent: AgentKind) -> u32 {
    match agent {
        AgentKind::Claude => 0xd97757,
        AgentKind::Codex => 0x3ddc97,
        AgentKind::Devin => 0x4cc2ff,
        AgentKind::Antigravity => 0x8b5cf6,
        AgentKind::OpenCode => 0xfbbf24,
        AgentKind::Grok => 0xe6e6ee,
        AgentKind::Amp => 0xec4899,
        AgentKind::ZCode => 0x60a5fa,
        AgentKind::Pi => 0xa78bfa,
        AgentKind::MimoCode => 0x34d399,
        AgentKind::WorkBuddy => 0x38bdf8,
    }
}

/// 概览周期切换
#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
pub enum OverviewPeriod {
    Today,
    Yesterday,
    #[default]
    ThirtyDays,
}

impl OverviewPeriod {
    pub fn label(&self) -> &'static str {
        match self {
            OverviewPeriod::Today => i18n::t(i18n::Key::TrayToday),
            OverviewPeriod::Yesterday => i18n::t(i18n::Key::TrayYesterday),
            OverviewPeriod::ThirtyDays => i18n::t(i18n::Key::TrayThirtyDays),
        }
    }
}

/// 单个模型的消费统计
#[derive(Clone, Debug, Default)]
pub struct ModelCostItem {
    pub name: String,
    pub color: u32,
    pub cost: f64,
    pub tokens: f64,
    pub percentage: f64,
}

/// 顶部整体概览数据
#[derive(Clone, Debug, Default)]
pub struct CostOverview {
    pub total_cost: f64,
    pub total_tokens: f64,
    pub items: Vec<ModelCostItem>,
}

/// 托盘卡片数据结构
#[derive(Clone, Debug)]
#[allow(dead_code)]
pub struct TrayQuotaCard {
    pub key: String,
    pub provider: quota::Provider,
    pub label: String,
    pub result: Result<quota::QuotaResult, String>,
}

pub fn compute_cost_overview(period: OverviewPeriod) -> CostOverview {
    let now = Local::now().timestamp();
    let (start, end) = match period {
        OverviewPeriod::Today => {
            let today_start = Local::now()
                .date_naive()
                .and_hms_opt(0, 0, 0)
                .map(|t| Local.from_local_datetime(&t).unwrap().timestamp())
                .unwrap_or(now - 86400);
            (today_start, now)
        }
        OverviewPeriod::Yesterday => {
            let today_start = Local::now()
                .date_naive()
                .and_hms_opt(0, 0, 0)
                .map(|t| Local.from_local_datetime(&t).unwrap().timestamp())
                .unwrap_or(now - 86400);
            (today_start - 86400, today_start)
        }
        OverviewPeriod::ThirtyDays => (now - 30 * 86400, now),
    };

    let agents = [
        AgentKind::Claude,
        AgentKind::Codex,
        AgentKind::Devin,
        AgentKind::Antigravity,
        AgentKind::OpenCode,
    ];

    let mut items: Vec<ModelCostItem> = Vec::new();
    let mut total_cost = 0.0;
    let mut total_tokens = 0.0;

    for agent in agents {
        // 优先从缓存极速读取
        let loaded = data::load_agent(agent, start, end);
        let mut agent_cost = 0.0;
        let mut agent_tokens = 0.0;

        for turn in &loaded.turns {
            if turn.created_at >= start && turn.created_at <= end {
                let cost = cost_for_turn(turn, &turn.model).unwrap_or(0.0);
                agent_cost += cost;
                agent_tokens += turn.input_tokens
                    + turn.output_tokens
                    + turn.cache_read_tokens
                    + turn.cache_creation_tokens;
            }
        }

        if agent_cost > 0.0 || agent_tokens > 0.0 {
            total_cost += agent_cost;
            total_tokens += agent_tokens;
            items.push(ModelCostItem {
                name: agent.label().to_string(),
                color: agent_color(agent),
                cost: agent_cost,
                tokens: agent_tokens,
                percentage: 0.0,
            });
        }
    }

    if total_cost > 0.0 {
        for item in &mut items {
            item.percentage = (item.cost / total_cost) * 100.0;
        }
        items.sort_by(|a, b| {
            b.cost
                .partial_cmp(&a.cost)
                .unwrap_or(std::cmp::Ordering::Equal)
        });
    } else if total_tokens > 0.0 {
        for item in &mut items {
            item.percentage = (item.tokens / total_tokens) * 100.0;
        }
        items.sort_by(|a, b| {
            b.tokens
                .partial_cmp(&a.tokens)
                .unwrap_or(std::cmp::Ordering::Equal)
        });
    }

    CostOverview {
        total_cost,
        total_tokens,
        items,
    }
}

pub struct TrayModel {
    pub cards: Vec<TrayQuotaCard>,
    pub loading: bool,
    pub updated_at: String,
    pub message: Option<String>,
    pub load_id: usize,
    pub period: OverviewPeriod,
    pub overview: Option<CostOverview>,
    pub overview_loading: bool,
    overview_load_id: usize,
    overview_task: Option<gpui::Task<()>>,
    pub other_accounts_expanded: bool,
    pub task: Option<gpui::Task<()>>,
    pub tick_task: Option<gpui::Task<()>>,
    lifetime: PopoverLifetime,
}
impl TrayModel {
    pub fn new(lifetime: PopoverLifetime, cx: &mut Context<Self>) -> Self {
        let mut s = Self {
            cards: Vec::new(),
            loading: false,
            updated_at: String::new(),
            message: None,
            load_id: 0,
            period: OverviewPeriod::ThirtyDays,
            overview: None,
            overview_loading: false,
            overview_load_id: 0,
            overview_task: None,
            other_accounts_expanded: false,
            task: None,
            tick_task: None,
            lifetime,
        };
        s.start_load(false, cx);
        s
    }
    pub fn start_load(&mut self, force: bool, cx: &mut Context<Self>) {
        if self.loading && !force {
            return;
        }
        self.load_id += 1;
        let load_id = self.load_id;
        self.loading = true;
        self.message = None;
        let allow_keychain = quota::keychain_allowed();
        self.start_overview_load(cx);
        cx.notify();

        let (updates, progress) = smol::channel::unbounded();
        let load = cx.background_executor().spawn(async move {
            let started = std::time::Instant::now();
            let accounts = quota::discover_accounts(allow_keychain);
            data::log_event(format!(
                "quota discovery elapsed_ms={} accounts={}",
                started.elapsed().as_millis(),
                accounts.len()
            ));
            let results =
                quota::fetch_accounts_with_progress(&accounts, force, |index, account, result| {
                    let _ = updates.try_send((
                        index,
                        TrayQuotaCard {
                            key: account.key.clone(),
                            provider: account.provider,
                            label: account.label.clone(),
                            result: result.clone(),
                        },
                    ));
                });
            accounts
                .iter()
                .zip(results)
                .map(|(account, result)| TrayQuotaCard {
                    key: account.key.clone(),
                    provider: account.provider,
                    label: account.label.clone(),
                    result,
                })
                .collect::<Vec<_>>()
        });

        self.task = Some(cx.spawn(async move |this, cx| {
            let mut completed = std::collections::BTreeMap::new();
            while let Ok((index, card)) = progress.recv().await {
                completed.insert(index, card);
                if this
                    .update(cx, |this, cx| {
                        if this.load_id != load_id {
                            return;
                        }
                        // 已完成账号按发现顺序展示；刷新时其余账号保留上次结果。
                        let mut cards: Vec<TrayQuotaCard> = completed.values().cloned().collect();
                        cards.extend(
                            this.cards
                                .iter()
                                .filter(|old| !completed.values().any(|new| new.key == old.key))
                                .cloned(),
                        );
                        this.cards = cards;
                        cx.notify();
                    })
                    .is_err()
                {
                    return;
                }
            }
            let cards = load.await;
            this.update(cx, |this, cx| {
                if this.load_id != load_id {
                    return;
                }
                this.loading = false;
                this.cards = cards;
                this.updated_at = chrono::Local::now().format("%H:%M:%S").to_string();
                this.start_tick(cx);
                cx.notify();
            })
            .ok();
        }));
    }

    pub fn switch_period(&mut self, period: OverviewPeriod, cx: &mut Context<Self>) {
        if self.period == period {
            return;
        }
        self.period = period;
        self.overview = None;
        self.start_overview_load(cx);
    }

    fn start_overview_load(&mut self, cx: &mut Context<Self>) {
        self.overview_load_id += 1;
        let load_id = self.overview_load_id;
        self.overview_loading = true;
        let p = self.period;
        cx.notify();
        let task = cx
            .background_executor()
            .spawn(async move { compute_cost_overview(p) });
        self.overview_task = Some(cx.spawn(async move |this, cx| {
            let overview = task.await;
            this.update(cx, |this, cx| {
                if this.overview_load_id != load_id {
                    return;
                }
                this.overview = Some(overview);
                this.overview_loading = false;
                cx.notify();
            })
            .ok();
        }));
    }

    fn start_tick(&mut self, cx: &mut Context<Self>) {
        if self.tick_task.is_some() {
            return;
        }
        self.tick_task = Some(cx.spawn(async move |this, cx| loop {
            cx.background_executor()
                .timer(Duration::from_secs(30))
                .await;
            let active = this
                .update(cx, |this, cx| {
                    if this.lifetime.is_open(cx) && !this.cards.is_empty() {
                        cx.notify();
                        true
                    } else {
                        false
                    }
                })
                .unwrap_or(false);
            if !active {
                break;
            }
        }));
    }

    pub fn toggle_accounts(&mut self, cx: &mut Context<Self>) {
        self.other_accounts_expanded = !self.other_accounts_expanded;
        cx.notify();
    }
}
