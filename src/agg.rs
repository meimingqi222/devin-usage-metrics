use crate::data::{AgentKind, LoadedData};
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
    pub session_keys: BTreeSet<String>,
    pub by_model: BTreeMap<String, ModelUsage>,
}

impl Bucket {
    pub fn total(&self) -> f64 {
        self.input + self.output + self.cached
    }
}

pub fn bucket_count(kind: PeriodKind) -> usize {
    match kind {
        PeriodKind::Day => 14,
        PeriodKind::Week => 12,
        PeriodKind::Month => 6,
    }
}

/// Earliest timestamp needed to cover the bucket window.
pub fn window_start(kind: PeriodKind) -> i64 {
    let now = Local::now();
    let days_back = match kind {
        PeriodKind::Day => bucket_count(PeriodKind::Day) as i64,
        PeriodKind::Week => (bucket_count(PeriodKind::Week) * 7) as i64,
        PeriodKind::Month => 186,
    };
    (now - TimeDelta::days(days_back + 1)).timestamp()
}

pub fn build_buckets(data: &LoadedData, kind: PeriodKind) -> Vec<Bucket> {
    build_buckets_inner(data, kind, None)
}

pub fn build_buckets_for(data: &LoadedData, kind: PeriodKind, agent: AgentKind) -> Vec<Bucket> {
    build_buckets_inner(data, kind, Some(agent))
}

fn build_buckets_inner(
    data: &LoadedData,
    kind: PeriodKind,
    agent: Option<AgentKind>,
) -> Vec<Bucket> {
    let now = Local::now();
    let today = now.date_naive();
    let mut buckets: Vec<Bucket> = Vec::new();

    match kind {
        PeriodKind::Day => {
            let n = bucket_count(kind);
            for i in (0..n).rev() {
                let d = today - TimeDelta::days(i as i64);
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
            let n = bucket_count(kind);
            for i in (0..n).rev() {
                let mon = this_monday - TimeDelta::weeks(i as i64);
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
            let n = bucket_count(kind);
            let (mut y, mut m) = (today.year(), today.month() as i32);
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
        let model = session_model
            .get(&turn.session_key)
            .cloned()
            .unwrap_or_else(|| "unknown".into());
        let mu = b.by_model.entry(model).or_default();
        mu.input += turn.input_tokens;
        mu.output += turn.output_tokens;
        mu.cached += turn.cache_read_tokens;
        mu.turns += 1;
    }
    buckets
}
