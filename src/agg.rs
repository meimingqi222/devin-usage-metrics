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
            PeriodKind::Day => "日",
            PeriodKind::Week => "周",
            PeriodKind::Month => "月",
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
            let (start_year, start_month) =
                shifted_month(current_month.0, current_month.1, -(n as i32 - 1) - page_offset as i32);
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

    let pricing = PricingTable::instance();

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
        let model = if !turn.model.is_empty() {
            turn.model.clone()
        } else {
            session_model
                .get(&turn.session_key)
                .cloned()
                .unwrap_or_else(|| "unknown".into())
        };
        // 按模型名查找定价并计算费用
        let pricing_entry = pricing.find(&model);
        if let Some(ref p) = pricing_entry {
            // Claude 区分 5m/1h cache creation，1h 费率 = input × 2
            let c = if turn.agent == AgentKind::Claude
                && (turn.cache_creation_5m_tokens > 0.0 || turn.cache_creation_1h_tokens > 0.0)
            {
                p.cost_claude_cache(
                    turn.input_tokens,
                    turn.output_tokens,
                    turn.cache_read_tokens,
                    turn.cache_creation_5m_tokens,
                    turn.cache_creation_1h_tokens,
                )
            } else {
                p.cost_with_cache_write(
                    turn.input_tokens,
                    turn.output_tokens,
                    turn.cache_read_tokens,
                    turn.cache_creation_tokens,
                )
            };
            b.cost += c;
        }
        let mu = b.by_model.entry(model).or_default();
        mu.input += turn.input_tokens;
        mu.output += turn.output_tokens;
        mu.cached += turn.cache_read_tokens;
        mu.turns += 1;
        if let Some(ref p) = pricing_entry {
            let c = if turn.agent == AgentKind::Claude
                && (turn.cache_creation_5m_tokens > 0.0 || turn.cache_creation_1h_tokens > 0.0)
            {
                p.cost_claude_cache(
                    turn.input_tokens,
                    turn.output_tokens,
                    turn.cache_read_tokens,
                    turn.cache_creation_5m_tokens,
                    turn.cache_creation_1h_tokens,
                )
            } else {
                p.cost_with_cache_write(
                    turn.input_tokens,
                    turn.output_tokens,
                    turn.cache_read_tokens,
                    turn.cache_creation_tokens,
                )
            };
            mu.cost += c;
            mu.priced = true;
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

            let current_buckets = build_buckets_for(
                &LoadedData::default(),
                kind,
                AgentKind::Devin,
                0,
            );
            let previous_buckets = build_buckets_for(
                &LoadedData::default(),
                kind,
                AgentKind::Devin,
                1,
            );
            assert_eq!(current_buckets.len(), bucket_count(kind));
            assert_eq!(previous_buckets.len(), bucket_count(kind));
            assert_eq!(current_buckets.first().unwrap().start, current_start);
            assert_eq!(current_buckets.last().unwrap().end, current_end);
            assert_eq!(previous_buckets.first().unwrap().start, previous_start);
            assert_eq!(previous_buckets.last().unwrap().end, previous_end);
        }
    }
}
