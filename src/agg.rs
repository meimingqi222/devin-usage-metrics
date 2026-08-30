use crate::data::{AgentKind, LoadedData};
use crate::pricing::PricingTable;
use chrono::{Datelike, Local, TimeDelta, TimeZone};
use std::collections::{BTreeMap, BTreeSet};

#[derive(Clone, Copy, PartialEq, Debug)]
pub enum PeriodKind {
    Day,
    Week,
    Month,
}

impl PeriodKind {
    pub fn label(&self) -> &'static str {
        match self {
            PeriodKind::Day => crate::i18n::t(crate::i18n::Key::PeriodDay),
            PeriodKind::Week => crate::i18n::t(crate::i18n::Key::PeriodWeek),
            PeriodKind::Month => crate::i18n::t(crate::i18n::Key::PeriodMonth),
        }
    }
}

#[derive(Debug, Default, Clone)]
pub struct ModelUsage {
    pub input: f64,
    pub output: f64,
    pub cached: f64,
    pub turns: u32,
    pub cost: f64,
    /// 该模型是否有已知定价（false 表示费用为估算或缺失）
    pub priced: bool,
}

impl ModelUsage {
    pub fn total(&self) -> f64 {
        self.input + self.output + self.cached
    }
}

#[derive(Debug, Default)]
pub struct Bucket {
    pub label: String,
    pub sub: String,
    pub start: i64,
    pub end: i64,
    pub input: f64,
    pub output: f64,
    pub cached: f64,
    pub turns: u32,
    pub cost: f64,
    pub session_keys: BTreeSet<String>,
    pub by_model: BTreeMap<String, ModelUsage>,
}

impl Bucket {
    pub fn total(&self) -> f64 {
        self.input + self.output + self.cached
    }
}

/// 每种周期显示的 bucket 数量。
pub fn bucket_count(kind: PeriodKind) -> usize {
    match kind {
        PeriodKind::Day => 15,
        PeriodKind::Week => 12,
        PeriodKind::Month => 12,
    }
}

fn local_midnight(date: chrono::NaiveDate) -> i64 {
    Local
        .from_local_datetime(&date.and_hms_opt(0, 0, 0).unwrap())
        .single()
        .map(|time| time.timestamp())
        .unwrap_or(0)
}

fn month_start(year: i32, month: u32) -> i64 {
    local_midnight(chrono::NaiveDate::from_ymd_opt(year, month, 1).unwrap())
}

fn shifted_month(year: i32, month: u32, offset: i32) -> (i32, u32) {
    let index = year * 12 + month as i32 - 1 + offset;
    (index.div_euclid(12), index.rem_euclid(12) as u32 + 1)
}

/// 返回指定页的完整自然时间窗口。
/// `page = 0` 是最新页，`page = 1` 是紧邻的上一页。
pub fn window_for(kind: PeriodKind, page: usize) -> (i64, i64) {
    let today = Local::now().date_naive();
    let n = bucket_count(kind) as i64;
    let page_offset = page as i64 * n;
    match kind {
        PeriodKind::Day => {
            let start_date = today - TimeDelta::days(n - 1 + page_offset);
            let end_date = today + TimeDelta::days(1 - page_offset);
            (local_midnight(start_date), local_midnight(end_date))
        }
        PeriodKind::Week => {
            let weekday = today.weekday().num_days_from_monday();
            let this_monday = today - TimeDelta::days(weekday as i64);
            let start = this_monday - TimeDelta::weeks(n - 1 + page_offset);
            let end = this_monday + TimeDelta::days(7) - TimeDelta::weeks(page_offset);
            (local_midnight(start), local_midnight(end))
        }
        PeriodKind::Month => {
            let current_month = (today.year(), today.month());
            let (start_year, start_month) = shifted_month(
                current_month.0,
                current_month.1,
                -(n as i32 - 1) - page_offset as i32,
            );
            let (end_year, end_month) =
                shifted_month(current_month.0, current_month.1, 1 - page_offset as i32);
            (
                month_start(start_year, start_month),
                month_start(end_year, end_month),
            )
        }
    }
}

/// 计算指定页的数据加载起始时间。
pub fn window_start(kind: PeriodKind, page: usize) -> i64 {
    window_for(kind, page).0
}

pub fn build_buckets(data: &LoadedData, kind: PeriodKind) -> Vec<Bucket> {
    build_buckets_inner(data, kind, None, 0)
}

pub fn build_buckets_for(
    data: &LoadedData,
    kind: PeriodKind,
    agent: AgentKind,
    page: usize,
) -> Vec<Bucket> {
    build_buckets_inner(data, kind, Some(agent), page)
}

/// 使用与图表聚合完全相同的口径计算单轮费用。
pub fn cost_for_turn(turn: &crate::data::TurnRec, model: &str) -> Option<f64> {
    if let Some(cost) = turn.recorded_cost {
        return Some(cost);
    }
    let pricing = PricingTable::instance();
    if turn.agent == AgentKind::Claude
        && (turn.cache_creation_5m_tokens > 0.0 || turn.cache_creation_1h_tokens > 0.0)
    {
        return pricing.find(model).map(|p| {
            p.cost_claude_cache(
                turn.input_tokens,
                turn.output_tokens,
                turn.cache_read_tokens,
                turn.cache_creation_5m_tokens,
                turn.cache_creation_1h_tokens,
            )
        });
    }
    if turn.agent == AgentKind::Devin {
        crate::pricing::single_turn_cost(
            model,
            turn.input_tokens,
            turn.output_tokens,
            turn.cache_read_tokens,
            turn.cache_creation_tokens,
        )
    } else {
        crate::pricing::turn_cost(
            model,
            turn.input_tokens,
            turn.output_tokens,
            turn.cache_read_tokens,
            turn.cache_creation_tokens,
        )
    }
}

/// 当前已加载窗口内，一个会话对聚合费用的贡献。
/// 每轮使用自己的实际模型和阶梯作用域；至少一轮可定价时返回累计金额。
pub fn cost_for_session_turns(data: &LoadedData, session: &crate::data::SessionRec) -> Option<f64> {
    let mut total = 0.0;
    let mut has_priced_turn = false;
    for turn in data
        .turns
        .iter()
        .filter(|turn| turn.agent == session.agent && turn.session_key == session.key)
    {
        let model = if turn.model.is_empty() {
            session.display_model()
        } else {
            turn.model.clone()
        };
        if let Some(cost) = cost_for_turn(turn, &model) {
            total += cost;
            has_priced_turn = true;
        }
    }
    has_priced_turn.then_some(total)
}

fn build_buckets_inner(
    data: &LoadedData,
    kind: PeriodKind,
    agent: Option<AgentKind>,
    page: usize,
) -> Vec<Bucket> {
    let today = Local::now().date_naive();
    let n = bucket_count(kind);
    let page_offset = page as i64 * n as i64;
    let mut buckets: Vec<Bucket> = Vec::new();

    match kind {
        PeriodKind::Day => {
            for i in (0..n).rev() {
                let d = today - TimeDelta::days(i as i64 + page_offset);
                let start = Local
                    .from_local_datetime(&d.and_hms_opt(0, 0, 0).unwrap())
                    .single()
                    .map(|t| t.timestamp())
                    .unwrap_or(0);
                buckets.push(Bucket {
                    label: d.format("%m-%d").to_string(),
                    sub: d.format("%a").to_string(),
                    start,
                    end: start + 86400,
                    ..Default::default()
                });
            }
        }
        PeriodKind::Week => {
            let weekday = today.weekday().num_days_from_monday();
            let this_monday = today - TimeDelta::days(weekday as i64);
            for i in (0..n).rev() {
                let mon = this_monday - TimeDelta::weeks(i as i64 + page_offset);
                let sun = mon + TimeDelta::days(6);
                let start = Local
                    .from_local_datetime(&mon.and_hms_opt(0, 0, 0).unwrap())
                    .single()
                    .map(|t| t.timestamp())
                    .unwrap_or(0);
                buckets.push(Bucket {
                    label: format!("W{:02}", mon.iso_week().week()),
                    sub: format!("{}~{}", mon.format("%m-%d"), sun.format("%m-%d")),
                    start,
                    end: start + 7 * 86400,
                    ..Default::default()
                });
            }
        }
        PeriodKind::Month => {
            let (mut y, mut m) = (today.year(), today.month() as i32);
            for _ in 0..page_offset {
                m -= 1;
                if m == 0 {
                    m = 12;
                    y -= 1;
                }
            }
            let mut months = Vec::new();
            for _ in 0..n {
                months.push((y, m));
                m -= 1;
                if m == 0 {
                    m = 12;
                    y -= 1;
                }
            }
            months.reverse();
            for (y, m) in months {
                let first = chrono::NaiveDate::from_ymd_opt(y, m as u32, 1).unwrap();
                let (ny, nm) = if m == 12 { (y + 1, 1) } else { (y, m + 1) };
                let next = chrono::NaiveDate::from_ymd_opt(ny, nm as u32, 1).unwrap();
                let start = Local
                    .from_local_datetime(&first.and_hms_opt(0, 0, 0).unwrap())
                    .single()
                    .map(|t| t.timestamp())
                    .unwrap_or(0);
                let end = Local
                    .from_local_datetime(&next.and_hms_opt(0, 0, 0).unwrap())
                    .single()
                    .map(|t| t.timestamp())
                    .unwrap_or(0);
                buckets.push(Bucket {
                    label: format!("{y}-{m:02}"),
                    sub: String::new(),
                    start,
                    end,
                    ..Default::default()
                });
            }
        }
    }

    let session_model: std::collections::HashMap<String, String> = data
        .sessions
        .iter()
        .filter(|session| agent.is_none_or(|agent| session.agent == agent))
        .map(|s| (s.key.clone(), s.display_model()))
        .collect();

    // adaptive 会话的 key 集合：其流量是服务端路由的，在模型分布里单独标注
    let session_adaptive: std::collections::HashSet<String> = data
        .sessions
        .iter()
        .filter(|session| agent.is_none_or(|agent| session.agent == agent))
        .filter(|s| s.selected_model == "adaptive")
        .map(|s| s.key.clone())
        .collect();

    for turn in &data.turns {
        if agent.is_some_and(|agent| turn.agent != agent) {
            continue;
        }
        let Some(b) = buckets
            .iter_mut()
            .find(|b| turn.created_at >= b.start && turn.created_at < b.end)
        else {
            continue;
        };
        b.input += turn.input_tokens;
        b.output += turn.output_tokens;
        b.cached += turn.cache_read_tokens;
        b.turns += 1;
        b.session_keys.insert(turn.session_key.clone());
        // 优先使用 turn 自己的模型（Devin 的 generation_model），否则回退到会话 display_model
        let model: &str = if !turn.model.is_empty() {
            &turn.model
        } else {
            session_model
                .get(&turn.session_key)
                .map(String::as_str)
                .unwrap_or("unknown")
        };
        // adaptive 会话路由到的模型加 (adaptive) 后缀，与主动选型的流量区分；
        // compactor / unknown 是内部流程标记，不是路由结果，不加后缀
        let label = if session_adaptive.contains(&turn.session_key)
            && model != "compactor"
            && model != "unknown"
        {
            format!("{model}(adaptive)")
        } else {
            model.to_string()
        };
        // 按模型名查找定价并计算费用；agent 自身记录过费用（如 Grok）则直接采用
        // （定价按原始模型名查，label 只是展示键）
        let turn_cost = cost_for_turn(turn, model);
        if let Some(c) = turn_cost {
            b.cost += c;
        }
        // BTreeMap::entry 要求拥有 key，用 get_mut 避免每个 turn 都克隆模型名
        match b.by_model.get_mut(&label) {
            Some(mu) => {
                mu.input += turn.input_tokens;
                mu.output += turn.output_tokens;
                mu.cached += turn.cache_read_tokens;
                mu.turns += 1;
                if let Some(c) = turn_cost {
                    mu.cost += c;
                    mu.priced = true;
                }
            }
            None => {
                b.by_model.insert(
                    label,
                    ModelUsage {
                        input: turn.input_tokens,
                        output: turn.output_tokens,
                        cached: turn.cache_read_tokens,
                        turns: 1,
                        cost: turn_cost.unwrap_or(0.0),
                        priced: turn_cost.is_some(),
                    },
                );
            }
        }
    }
    buckets
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn paged_windows_are_adjacent() {
        for kind in [PeriodKind::Day, PeriodKind::Week, PeriodKind::Month] {
            let (current_start, current_end) = window_for(kind, 0);
            let (previous_start, previous_end) = window_for(kind, 1);
            assert!(current_start < current_end);
            assert!(previous_start < previous_end);
            assert_eq!(previous_end, current_start);

            let current_buckets =
                build_buckets_for(&LoadedData::default(), kind, AgentKind::Devin, 0);
            let previous_buckets =
                build_buckets_for(&LoadedData::default(), kind, AgentKind::Devin, 1);
            assert_eq!(current_buckets.len(), bucket_count(kind));
            assert_eq!(previous_buckets.len(), bucket_count(kind));
            assert_eq!(current_buckets.first().unwrap().start, current_start);
            assert_eq!(current_buckets.last().unwrap().end, current_end);
            assert_eq!(previous_buckets.first().unwrap().start, previous_start);
            assert_eq!(previous_buckets.last().unwrap().end, previous_end);
        }
    }

    #[test]
    fn adaptive_sessions_are_labeled_in_model_breakdown() {
        let start = window_start(PeriodKind::Day, 0) + 3600;
        let mk_turn = |model: &str| crate::data::TurnRec {
            agent: AgentKind::Devin,
            session_key: "cli/s1".into(),
            created_at: start,
            input_tokens: 1000.0,
            output_tokens: 100.0,
            cache_read_tokens: 0.0,
            model: model.into(),
            ..Default::default()
        };
        let data = LoadedData {
            sessions: vec![crate::data::SessionRec {
                agent: AgentKind::Devin,
                key: "cli/s1".into(),
                source: "cli".into(),
                id: "s1".into(),
                title: String::new(),
                working_directory: String::new(),
                selected_model: "adaptive".into(),
                real_model: Some("glm-5-2".into()),
                agent_mode: String::new(),
                created_at: start,
                last_activity_at: start,
                ..Default::default()
            }],
            turns: vec![mk_turn("glm-5-2"), mk_turn("compactor")],
            ..Default::default()
        };

        let buckets = build_buckets_for(&data, PeriodKind::Day, AgentKind::Devin, 0);
        let by_model = &buckets.first().unwrap().by_model;
        // 路由流量带 (adaptive) 后缀，compactor 不加
        assert!(by_model.contains_key("glm-5-2(adaptive)"));
        assert!(by_model.contains_key("compactor"));
        assert!(!by_model.contains_key("glm-5-2"));
        // 定价仍按原始模型名计算（glm-5-2: input $1.4/M + output $4.4/M）
        let mu = &by_model["glm-5-2(adaptive)"];
        assert!(mu.priced);
        let expected = 1000.0 / 1e6 * 1.4 + 100.0 / 1e6 * 4.4;
        assert!((mu.cost - expected).abs() < 1e-9);
    }

    #[test]
    fn long_context_tier_is_devin_only() {
        let make_turn = |agent| crate::data::TurnRec {
            agent,
            model: "gpt-5.6-sol".into(),
            input_tokens: 300_000.0,
            ..Default::default()
        };
        let devin = cost_for_turn(&make_turn(AgentKind::Devin), "gpt-5.6-sol").unwrap();
        let codex = cost_for_turn(&make_turn(AgentKind::Codex), "gpt-5.6-sol").unwrap();
        assert!((devin - 3.0).abs() < 1e-12);
        assert!((codex - 1.5).abs() < 1e-12);
    }

    #[test]
    fn session_cost_sums_actual_turn_models_and_tiers() {
        let session = crate::data::SessionRec {
            agent: AgentKind::Devin,
            key: "cli/mixed".into(),
            selected_model: "adaptive".into(),
            ..Default::default()
        };
        let make_turn = |model: &str, input: f64| crate::data::TurnRec {
            agent: AgentKind::Devin,
            session_key: session.key.clone(),
            model: model.into(),
            input_tokens: input,
            ..Default::default()
        };
        let data = LoadedData {
            sessions: vec![session.clone()],
            turns: vec![
                make_turn("gpt-5-6-luna-high", 300_000.0),
                make_turn("gpt-5-6-luna-high", 100_000.0),
                crate::data::TurnRec {
                    agent: AgentKind::Codex,
                    session_key: session.key.clone(),
                    model: "gpt-5.6-sol".into(),
                    input_tokens: 1_000_000.0,
                    ..Default::default()
                },
            ],
            ..Default::default()
        };
        let cost = cost_for_session_turns(&data, &session).unwrap();
        let expected = 300_000.0 * 0.4 / 1e6 + 100_000.0 * 0.2 / 1e6;
        assert!((cost - expected).abs() < 1e-12);
    }
}
