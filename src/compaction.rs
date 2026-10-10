//! Historical context replay estimating API-equivalent cost, not task quality
//! or subscription quota. Configuration changes live in compaction_config.
use crate::data::{AgentKind, LoadedData, TurnRec};
use crate::pricing::{Pricing, PricingTable};
use std::collections::BTreeMap;

const STEP: u64 = 10_000;
const MIN_CALLS: usize = 50;
const MIN_COMPACTIONS: usize = 3;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Status {
    NotEnoughCalls,
    NotEnoughCompactions,
    UnknownPricing,
    Keep,
    Recommend,
}

#[derive(Debug, Clone)]
pub struct Recommendation {
    pub device_id: String,
    pub device_name: String,
    pub model: String,
    pub calls: usize,
    pub streams: usize,
    pub compactions: usize,
    pub growth: u64,
    pub after: u64,
    pub rework: u64,
    /// Median observed pre-compaction prompt, not the current config setting.
    pub observed: u64,
    /// Actual prompt threshold, NOT Claude's autoCompactWindow setting.
    pub recommended: u64,
    pub baseline_cost: f64,
    pub recommended_cost: f64,
    pub curve: Vec<(u64, f64)>,
    pub status: Status,
}

impl Recommendation {
    pub fn savings_fraction(&self) -> f64 {
        if self.baseline_cost > 0.0 {
            ((self.baseline_cost - self.recommended_cost) / self.baseline_cost).max(0.0)
        } else {
            0.0
        }
    }
}

#[derive(Debug, Default)]
pub struct Report {
    pub recommendations: Vec<Recommendation>,
    /// Older synced snapshots and invalid usage cannot safely be replayed.
    pub skipped: usize,
}

fn prompt(t: &TurnRec) -> u64 {
    (t.input_tokens + t.cache_read_tokens + t.cache_creation_tokens) as u64
}

fn compacted(prev: u64, next: u64) -> bool {
    prev >= 20_000 && (next as f64) < prev as f64 * 0.6
}

fn median(mut values: Vec<u64>) -> u64 {
    if values.is_empty() {
        return 0;
    }
    values.sort_unstable();
    values[values.len() / 2]
}

/// Uses the selected window, device and agent. Streams are isolated BEFORE
/// grouping by model: switching A → B → A must not join the two A fragments.
pub fn analyze(
    data: &LoadedData,
    agent: AgentKind,
    device: Option<&str>,
    start: i64,
    end: i64,
) -> Report {
    let mut report = Report::default();
    if !matches!(agent, AgentKind::Claude | AgentKind::Codex) {
        return report;
    }
    let mut streams: BTreeMap<(&str, &str, &str), Vec<&TurnRec>> = BTreeMap::new();
    for t in &data.turns {
        if t.agent != agent
            || device.is_some_and(|d| d != t.device_id)
            || t.created_at < start
            || t.created_at >= end
        {
            continue;
        }
        // Keep unknown/invalid calls as boundaries below, not merely filtered
        // gaps that could look like a compaction on either side of them.
        streams
            .entry((&t.device_id, &t.session_key, &t.context_stream))
            .or_default()
            .push(t);
    }
    let valid = |t: &TurnRec| {
        !t.context_stream.is_empty()
            && !t.model.trim().is_empty()
            && t.created_at > 0
            && [
                t.input_tokens,
                t.cache_read_tokens,
                t.cache_creation_tokens,
                t.output_tokens,
            ]
            .iter()
            .all(|v| v.is_finite() && *v >= 0.0)
            && prompt(t) > 0
    };
    let mut groups: BTreeMap<(String, String), Vec<Vec<&TurnRec>>> = BTreeMap::new();
    let group_model = |t: &TurnRec| {
        if agent == AgentKind::Codex {
            "Codex · all models".to_string()
        } else {
            t.model.trim().to_ascii_lowercase()
        }
    };
    for (_, mut calls) in streams {
        calls.sort_by_key(|t| (t.created_at, t.context_order));
        let mut segment: Vec<&TurnRec> = Vec::new();
        for t in calls {
            let model = t.model.trim().to_ascii_lowercase();
            let boundary = segment.last().is_some_and(|prev| {
                prev.model.trim().to_ascii_lowercase() != model
                    || (prompt(t) < prompt(prev) && !compacted(prompt(prev), prompt(t)))
            });
            if boundary || !valid(t) {
                if let Some(first) = segment.first() {
                    groups
                        .entry((first.device_id.clone(), group_model(first)))
                        .or_default()
                        .push(std::mem::take(&mut segment));
                }
            }
            if valid(t) {
                segment.push(t);
            } else {
                report.skipped += 1;
            }
        }
        if let Some(first) = segment.first() {
            groups
                .entry((first.device_id.clone(), group_model(first)))
                .or_default()
                .push(segment);
        }
    }
    for ((device_id, model), streams) in groups {
        report
            .recommendations
            .push(recommend(agent, device_id, model, &streams));
    }
    report.recommendations.sort_by_key(|r| {
        (
            match r.status {
                Status::Recommend => 0,
                Status::Keep => 1,
                _ => 2,
            },
            std::cmp::Reverse(r.calls),
        )
    });
    report
}

fn recommend(
    agent: AgentKind,
    device_id: String,
    model: String,
    streams: &[Vec<&TurnRec>],
) -> Recommendation {
    let mut peaks = Vec::new();
    let mut afters = Vec::new();
    let mut growths = Vec::new();
    let mut reworks = Vec::new();
    for s in streams {
        let growth = median(
            s.windows(2)
                .filter_map(|p| {
                    let (prev, next) = (prompt(p[0]), prompt(p[1]));
                    (!compacted(prev, next)).then(|| next.saturating_sub(prev))
                })
                .collect(),
        );
        for (i, p) in s.windows(2).enumerate() {
            let (prev, next) = (prompt(p[0]), prompt(p[1]));
            if compacted(prev, next) {
                peaks.push(prev);
                afters.push(next);
                let at = i + 1;
                if at + 10 < s.len()
                    && s[at..=at + 10]
                        .windows(2)
                        .all(|p| !compacted(prompt(p[0]), prompt(p[1])))
                {
                    reworks.push(prompt(s[at + 10]).saturating_sub(next + 10 * growth));
                }
            } else {
                growths.push(next.saturating_sub(prev));
            }
        }
    }
    let has_rework_samples = !reworks.is_empty();
    let mut r = Recommendation {
        device_id,
        device_name: streams[0][0].device_name.clone(),
        model,
        calls: streams.iter().map(Vec::len).sum(),
        streams: streams.len(),
        compactions: peaks.len(),
        growth: median(growths),
        after: median(afters),
        rework: median(reworks),
        observed: median(peaks),
        recommended: 0,
        baseline_cost: 0.0,
        recommended_cost: 0.0,
        curve: Vec::new(),
        status: Status::NotEnoughCalls,
    };
    if r.calls < MIN_CALLS {
        return r;
    }
    if r.compactions < MIN_COMPACTIONS {
        r.status = Status::NotEnoughCompactions;
        return r;
    }
    // Codex has a global setting: pool isolated model segments, retaining
    // each segment's own pricing instead of offering competing settings.
    let prices: Option<Vec<_>> = streams
        .iter()
        .map(|s| {
            PricingTable::instance()
                .find(&s[0].model)
                .filter(|p| p.i > 0.0)
        })
        .collect();
    let Some(prices) = prices else {
        r.status = Status::UnknownPricing;
        return r;
    };
    let base = median(streams.iter().map(|s| prompt(s[0])).collect());
    if r.compactions < 5 || !has_rework_samples {
        r.rework = r.rework.max(10_000);
    }
    // Leave room for twenty normal calls or half a summary after compaction.
    let min = if agent == AgentKind::Claude {
        67_000
    } else {
        50_000
    };
    let floor = (r.after + r.rework + (r.after / 2).max(20 * r.growth))
        .max(min)
        .div_ceil(STEP)
        * STEP;
    // Never advise expanding beyond a historical prompt we actually observed.
    let top = r.observed.min(if agent == AgentKind::Claude {
        967_000
    } else {
        1_000_000
    });
    let run = |threshold| {
        streams
            .iter()
            .zip(&prices)
            .map(|(stream, price)| {
                replay(
                    std::slice::from_ref(stream),
                    threshold,
                    r.after,
                    base,
                    r.rework,
                    agent,
                    price,
                )
            })
            .sum()
    };
    r.baseline_cost = run(r.observed);
    let mut curve = Vec::new();
    for threshold in (floor..=top).step_by(STEP as usize) {
        curve.push((threshold, run(threshold)));
    }
    curve.push((r.observed, r.baseline_cost));
    let low = curve
        .iter()
        .map(|(_, cost)| *cost)
        .fold(f64::INFINITY, f64::min);
    let best = curve
        .iter()
        .filter(|(_, cost)| *cost <= low * 1.01)
        .max_by_key(|(threshold, _)| *threshold)
        .unwrap();
    r.recommended = best.0;
    r.recommended_cost = best.1;
    r.status = Status::Recommend;
    if r.savings_fraction() < 0.03 {
        r.recommended = r.observed;
        r.recommended_cost = r.baseline_cost;
        r.status = Status::Keep;
    }
    curve.sort_by_key(|(threshold, _)| *threshold);
    curve.dedup_by_key(|(threshold, _)| *threshold);
    r.curve = curve;
    r
}

fn replay(
    streams: &[Vec<&TurnRec>],
    threshold: u64,
    after: u64,
    base: u64,
    rework: u64,
    agent: AgentKind,
    price: &Pricing,
) -> f64 {
    let summary = after.saturating_sub(base) as f64;
    let mut cost = 0.0;
    for s in streams {
        let write_rate = |t: &TurnRec| {
            if agent != AgentKind::Claude {
                return price.i;
            }
            let hour_share = if t.cache_creation_tokens > 0.0 {
                (t.cache_creation_1h_tokens / t.cache_creation_tokens).clamp(0.0, 1.0)
            } else {
                0.0
            };
            price.cw * (1.0 - hour_share) + price.i * 2.0 * hour_share
        };
        let mut ctx = prompt(s[0]);
        cost += ctx as f64 * write_rate(s[0]) + s[0].output_tokens * price.o;
        for p in s.windows(2) {
            let (prev, t) = (p[0], p[1]);
            let growth = if compacted(prompt(prev), prompt(t)) {
                0
            } else {
                prompt(t).saturating_sub(prompt(prev))
            };
            let write = write_rate(t);
            if ctx + growth > threshold && ctx > after {
                cost +=
                    ctx as f64 * price.cr + summary * price.o + (summary + rework as f64) * write;
                ctx = after + rework;
            }
            // Unlike an always-hit replay, account for observed Claude pauses.
            let ttl = if t.cache_creation_1h_tokens > t.cache_creation_5m_tokens {
                3600
            } else {
                300
            };
            let read = if agent == AgentKind::Claude && t.created_at - prev.created_at > ttl {
                write
            } else {
                price.cr
            };
            cost += ctx as f64 * read + growth as f64 * write + t.output_tokens * price.o;
            ctx += growth;
        }
    }
    cost / 1_000_000.0
}

#[cfg(test)]
mod tests {
    use super::*;

    fn history(agent: AgentKind, compact_at: u64, count: usize) -> LoadedData {
        let mut data = LoadedData::default();
        let mut ctx = 30_000;
        for i in 0..count {
            data.turns.push(TurnRec {
                agent,
                session_key: "session".into(),
                device_id: "desktop".into(),
                device_name: "Desktop".into(),
                context_stream: "main".into(),
                created_at: 1_000 + i as i64 * 30,
                model: if agent == AgentKind::Claude {
                    "claude-opus-4-6"
                } else {
                    "gpt-5.2"
                }
                .into(),
                input_tokens: 500.0,
                cache_read_tokens: (ctx - 5_000) as f64,
                cache_creation_tokens: 4_500.0,
                cache_creation_5m_tokens: 4_500.0,
                output_tokens: 800.0,
                ..Default::default()
            });
            ctx += 5_000;
            if ctx >= compact_at {
                ctx = 60_000;
            }
        }
        data
    }

    #[test]
    fn long_history_recommends_smaller_but_short_history_does_not_guess() {
        for agent in [AgentKind::Claude, AgentKind::Codex] {
            let data = history(agent, 960_000, 1000);
            let report = analyze(&data, agent, None, 0, i64::MAX);
            let r = &report.recommendations[0];
            assert_eq!(r.status, Status::Recommend);
            assert_eq!(r.observed, 955_000);
            assert_eq!(r.growth, 5_000);
            assert_eq!(r.after, 60_000);
            assert!(r.recommended >= 160_000 && r.recommended < r.observed);
            assert!(r.savings_fraction() >= 0.2);
            let short = analyze(&data, agent, None, 0, 1_000 + 20 * 30);
            assert_eq!(short.recommendations[0].status, Status::NotEnoughCalls);
            let no_cuts = analyze(&history(agent, 960_000, 100), agent, None, 0, i64::MAX);
            assert_eq!(
                no_cuts.recommendations[0].status,
                Status::NotEnoughCompactions
            );
        }
    }

    #[test]
    fn devices_subagents_and_model_switches_do_not_create_false_compactions() {
        let mut data = history(AgentKind::Claude, 960_000, 120);
        // Interleave a tiny child context with the large parent, same session.
        let mut child = data.turns.clone();
        for t in &mut child {
            t.context_stream = "subagent/child".into();
            t.created_at += 1;
            t.cache_read_tokens = 10_000.0;
        }
        data.turns.extend(child);
        let mut remote = data.turns.clone();
        for t in &mut remote {
            t.device_id = "laptop".into();
        }
        data.turns.extend(remote);
        let all = analyze(&data, AgentKind::Claude, None, 0, i64::MAX);
        assert_eq!(all.recommendations.len(), 2);
        assert!(all
            .recommendations
            .iter()
            .all(|r| r.compactions == 0 && r.streams == 2));
        assert_eq!(
            analyze(&data, AgentKind::Claude, Some("laptop"), 0, i64::MAX)
                .recommendations
                .len(),
            1
        );
        // Same model on both sides of a model change must remain separate.
        let mut switched = history(AgentKind::Claude, 960_000, 120);
        for (i, t) in switched.turns.iter_mut().enumerate() {
            if i % 3 == 1 {
                t.model = "claude-sonnet-4-6".into();
            }
            if i % 3 == 2 {
                t.cache_read_tokens = 10_000.0;
            }
        }
        assert!(analyze(&switched, AgentKind::Claude, None, 0, i64::MAX)
            .recommendations
            .iter()
            .all(|r| r.compactions == 0));
    }

    #[test]
    fn missing_metadata_and_unknown_prices_never_produce_advice() {
        let mut data = history(AgentKind::Codex, 200_000, 200);
        for t in &mut data.turns {
            t.model = "unpriced-model-xyz".into();
        }
        assert_eq!(
            analyze(&data, AgentKind::Codex, None, 0, i64::MAX).recommendations[0].status,
            Status::UnknownPricing
        );
        for t in &mut data.turns {
            t.context_stream.clear();
        }
        let report = analyze(&data, AgentKind::Codex, None, 0, i64::MAX);
        assert!(report.recommendations.is_empty());
        assert_eq!(report.skipped, 200);
        assert!(analyze(&data, AgentKind::Devin, None, 0, i64::MAX)
            .recommendations
            .is_empty());
    }

    #[test]
    fn replay_cost_includes_summary_generation_and_cache_rebuild() {
        let mut data = history(AgentKind::Codex, 960_000, 50);
        for (i, t) in data.turns.iter_mut().enumerate() {
            let tokens = match i {
                0 => 30_000,
                1 => 100_000,
                2 => 200_000,
                _ => [60_000, 130_000, 200_000][(i - 3) % 3],
            };
            t.cache_read_tokens = (tokens - 5_000) as f64;
        }
        let report = analyze(&data, AgentKind::Codex, None, 0, i64::MAX);
        let r = &report.recommendations[0];
        assert_eq!(r.compactions, 16);
        assert_eq!(r.observed, 200_000);
        // Cuts every three calls leave no uninterrupted ten-call rework
        // sample, so the conservative fallback is 10K. Replaying this
        // pathological cadence produces 31 summaries (30K each).
        assert_eq!(r.rework, 10_000);
        // Hand-counted: 2.34M growth + 30K first prompt + 31 × 40K
        // rebuilds; 4.60M ordinary reads + 4.40M compaction reads;
        // 50 × 800 original output + 31 × 30K generated summaries.
        let price = PricingTable::instance().find("gpt-5.2").unwrap();
        let expected = 3.61 * price.i + 9.0 * price.cr + 0.97 * price.o;
        assert!((r.baseline_cost - expected).abs() < 1e-9);
        // One global Codex recommendation, but independently priced model
        // histories. A first-model-only implementation gets this cost wrong.
        let mut second = data.turns.clone();
        for t in &mut second {
            t.session_key = "second".into();
            t.model = "gpt-5.1".into();
        }
        data.turns.extend(second);
        let pooled = analyze(&data, AgentKind::Codex, None, 0, i64::MAX);
        assert_eq!(pooled.recommendations.len(), 1);
        assert_eq!(pooled.recommendations[0].calls, 100);
        assert_eq!(pooled.recommendations[0].streams, 2);
        let second_price = PricingTable::instance().find("gpt-5.1").unwrap();
        let pooled_expected =
            expected + 3.61 * second_price.i + 9.0 * second_price.cr + 0.97 * second_price.o;
        assert!((pooled.recommendations[0].baseline_cost - pooled_expected).abs() < 1e-9);
    }

    #[test]
    fn keeps_a_threshold_with_no_safe_smaller_candidate_and_preserves_transcript_order() {
        let mut data = history(AgentKind::Codex, 150_000, 200);
        let original = analyze(&data, AgentKind::Codex, None, 0, i64::MAX);
        let r = &original.recommendations[0];
        assert_eq!(r.status, Status::Keep);
        assert_eq!(r.observed, 145_000);
        assert_eq!(r.recommended, r.observed);
        assert_eq!(r.baseline_cost, r.recommended_cost);
        // Hash maps / synced records need not arrive in transcript order.
        for (i, t) in data.turns.iter_mut().enumerate() {
            t.created_at = 1_000;
            t.context_order = i as u64;
        }
        data.turns.reverse();
        let reordered = analyze(&data, AgentKind::Codex, None, 0, i64::MAX);
        assert_eq!(reordered.recommendations[0].compactions, r.compactions);
        assert_eq!(reordered.recommendations[0].observed, 145_000);
    }
}
