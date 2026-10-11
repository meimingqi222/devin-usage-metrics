//! Historical context replay estimating API-equivalent cost, not task quality
//! or subscription quota. Configuration changes live in compaction_config.
use crate::data::{AgentKind, LoadedData, TurnRec};
use crate::pricing::{Pricing, PricingTable};
use std::collections::BTreeMap;

const STEP: u64 = 10_000;
const MIN_CALLS: usize = 50;
const MIN_COMPACTIONS: usize = 3;
/// Codex's threshold is one global setting, so every model's segments are
/// pooled under this label rather than competing for their own advice.
pub const CODEX_GROUP: &str = "Codex · all models";
/// Where the agents' own models start: Claude Code's 200K (1M when asked for
/// the long context), Codex's 272K. A prompt past it means the model holds
/// more, which is what raises the search above what was observed.
const BASE_WINDOW_CLAUDE: u64 = 200_000;
const BASE_WINDOW_CODEX: u64 = 272_000;

/// What replaying the history at one threshold came to.
#[derive(Debug, Clone, Copy, Default)]
struct Replay {
    /// Weighted cost in dollars.
    cost: f64,
    /// Compactions the replay had to make.
    compactions: usize,
    /// Calls whose context would have passed the threshold.
    over: usize,
}

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
    /// The threshold the advice is measured against: the configured one when
    /// there is one, else `observed`.
    pub current: u64,
    /// The most the narrowest model in the pool can hold, which caps both
    /// the baseline and the search.
    pub window: u64,
    /// Actual prompt threshold, NOT Claude's autoCompactWindow setting.
    pub recommended: u64,
    /// The threshold the replay liked best, before any "keep" override.
    pub candidate: u64,
    pub baseline_cost: f64,
    pub recommended_cost: f64,
    /// Compactions the replay makes at `observed`.
    pub baseline_compactions: usize,
    /// Compactions the replay makes at `candidate`.
    pub candidate_compactions: usize,
    /// Share of replayed calls whose context would pass `candidate`.
    pub over_share: f64,
    /// What `candidate` would have saved, before any "keep" override.
    pub candidate_saving: f64,
    pub candidate_cost: f64,
    /// Candidate saving when estimated rework is doubled (at least +10K).
    pub cautious_saving: f64,
    pub sensitivity_failed: bool,
    pub baseline_inferred: bool,
    pub priced_calls: usize,
    pub curve: Vec<(u64, f64)>,
    /// Streams dropped from the replay because their model has no usable
    /// price. They still count towards the sample statistics.
    pub unpriced: usize,
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
/// `current` maps a group's (device, label) to the threshold it runs with
/// now, as a trigger rather than as the setting; advice is measured against
/// it when one is set, and against the observed compaction point when it is
/// not. Only the local machine fills it in — another's config is not ours.
pub fn analyze(
    data: &LoadedData,
    agent: AgentKind,
    device: Option<&str>,
    start: i64,
    end: i64,
    current: &BTreeMap<(String, String), u64>,
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
            CODEX_GROUP.to_string()
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
        let trigger = current.get(&(device_id.clone(), model.clone())).copied();
        report
            .recommendations
            .push(recommend(agent, device_id, model, &streams, trigger));
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
    current: Option<u64>,
) -> Recommendation {
    let calls = streams.iter().map(Vec::len).sum();
    let stream_count = streams.len();
    let device_name = streams[0][0].device_name.clone();
    let (priced_streams, prices): (Vec<_>, Vec<_>) = streams
        .iter()
        .filter_map(|s| {
            PricingTable::instance()
                .find(&s[0].model)
                .filter(|p| p.i > 0.0)
                .map(|price| (s.clone(), price))
        })
        .unzip();
    let unpriced = stream_count - priced_streams.len();
    // Coverage includes unknown models; simulation calibration excludes them.
    let streams = priced_streams.as_slice();
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
        device_name,
        model,
        calls,
        streams: stream_count,
        compactions: peaks.len(),
        growth: median(growths),
        after: median(afters),
        rework: median(reworks),
        observed: median(peaks),
        current: 0,
        window: 0,
        recommended: 0,
        candidate: 0,
        baseline_cost: 0.0,
        recommended_cost: 0.0,
        baseline_compactions: 0,
        candidate_compactions: 0,
        over_share: 0.0,
        candidate_saving: 0.0,
        candidate_cost: 0.0,
        cautious_saving: 0.0,
        sensitivity_failed: false,
        baseline_inferred: current.is_none(),
        priced_calls: streams.iter().map(Vec::len).sum(),
        curve: Vec::new(),
        unpriced,
        status: Status::NotEnoughCalls,
    };
    if streams.is_empty() {
        r.status = Status::UnknownPricing;
        return r;
    }
    if r.priced_calls < MIN_CALLS {
        return r;
    }
    if r.compactions < MIN_COMPACTIONS {
        r.status = Status::NotEnoughCompactions;
        return r;
    }
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
    // The setting is global, so the narrowest model in the pool binds: a
    // threshold it cannot hold would be ignored by it. A prompt only ever
    // came from a model that served it, so a stream that reached past its
    // catalog window proves the catalog is stale for that model, and what
    // the stream actually reached is the better bound for it.
    let base_window = if agent == AgentKind::Claude {
        BASE_WINDOW_CLAUDE
    } else {
        BASE_WINDOW_CODEX
    };
    let hard_cap = if agent == AgentKind::Claude {
        967_000
    } else {
        1_000_000
    };
    let window = streams
        .iter()
        .map(|s| {
            let reached = s.iter().map(|t| prompt(t)).max().unwrap_or_default();
            PricingTable::instance()
                .context_window(&s[0].model)
                .unwrap_or(base_window)
                .max(reached)
        })
        .min()
        .unwrap_or(base_window)
        .min(hard_cap);
    r.window = window;
    // Measure against what is set when something sets it, and against where
    // compactions happened to land when nothing does. The agent compacts at
    // the smaller of the setting and the model's window either way.
    r.current = current.unwrap_or(r.observed).min(window);
    // Search up to the window, not only as far as compactions were seen: a
    // model that compacts at 200K by default says nothing about 250K.
    let top = window;
    let run_with_rework = |threshold, rework| {
        streams
            .iter()
            .zip(&prices)
            .map(|(stream, price)| {
                replay(
                    std::slice::from_ref(stream),
                    threshold,
                    r.after,
                    base,
                    rework,
                    agent,
                    price,
                )
            })
            .fold(Replay::default(), |sum, part| Replay {
                cost: sum.cost + part.cost,
                compactions: sum.compactions + part.compactions,
                over: sum.over + part.over,
            })
    };
    let run = |threshold| run_with_rework(threshold, r.rework);
    let baseline = run(r.current);
    r.baseline_cost = baseline.cost;
    r.baseline_compactions = baseline.compactions;
    let mut curve = Vec::new();
    for threshold in (floor..=top).step_by(STEP as usize) {
        curve.push((threshold, run(threshold).cost));
    }
    curve.push((r.current, baseline.cost));
    let low = curve
        .iter()
        .map(|(_, cost)| *cost)
        .fold(f64::INFINITY, f64::min);
    let best = curve
        .iter()
        .filter(|(_, cost)| *cost <= low * 1.01)
        .max_by_key(|(threshold, _)| *threshold)
        .unwrap();
    r.candidate = best.0;
    r.candidate_cost = best.1;
    r.recommended = best.0;
    r.recommended_cost = best.1;
    let advised = run(best.0);
    r.candidate_compactions = advised.compactions;
    r.over_share = advised.over as f64 / r.priced_calls.max(1) as f64;
    r.candidate_saving = if r.baseline_cost > 0.0 {
        ((r.baseline_cost - best.1) / r.baseline_cost).max(0.0)
    } else {
        0.0
    };
    r.status = Status::Recommend;
    // Compacting more often throws away context the cost model does not
    // price, so a smaller threshold has to clear a higher bar than a larger
    // one, which only ever compacts less.
    let bar = if r.candidate < r.current { 0.05 } else { 0.03 };
    let cautious_rework = r
        .rework
        .saturating_mul(2)
        .max(r.rework.saturating_add(10_000));
    let cautious_baseline = run_with_rework(r.current, cautious_rework).cost;
    let cautious_candidate = run_with_rework(r.candidate, cautious_rework).cost;
    r.cautious_saving = if cautious_baseline > 0.0 {
        ((cautious_baseline - cautious_candidate) / cautious_baseline).max(0.0)
    } else {
        0.0
    };
    r.sensitivity_failed = r.candidate_saving >= bar && r.cautious_saving < bar;
    if r.savings_fraction() < bar || r.sensitivity_failed {
        r.recommended = r.current;
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
) -> Replay {
    let summary = after.saturating_sub(base) as f64;
    let mut cost = 0.0;
    let mut compactions = 0usize;
    let mut over = 0usize;
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
            // Read the threshold before compacting: the compaction resets
            // ctx, which would hide the call that triggered it.
            let past = ctx + growth > threshold;
            if past && ctx > after {
                cost +=
                    ctx as f64 * price.cr + summary * price.o + (summary + rework as f64) * write;
                ctx = after + rework;
                compactions += 1;
            }
            if past {
                over += 1;
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
    Replay {
        cost: cost / 1_000_000.0,
        compactions,
        over,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unpriced_streams_do_not_change_replay_calibration() {
        let mut data = history(AgentKind::Codex, 320_000, 1000);
        let before = analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new());
        let mut unknown = history(AgentKind::Codex, 150_000, 2000);
        for t in &mut unknown.turns {
            t.session_key = "unpriced".into();
            t.model = "unpriced-model-xyz".into();
        }
        data.turns.extend(unknown.turns);
        let after = analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new());
        let (a, b) = (&before.recommendations[0], &after.recommendations[0]);
        assert_eq!(a.observed, b.observed);
        assert_eq!(a.after, b.after);
        assert_eq!(a.candidate, b.candidate);
        assert_eq!(a.baseline_cost, b.baseline_cost);
        assert_eq!(b.unpriced, 1);
        assert_eq!(b.calls, 3000);
    }

    #[test]
    fn a_wide_model_cannot_raise_another_models_cap() {
        let mut data = history(AgentKind::Codex, 960_000, 1000);
        let mut narrow = history(AgentKind::Codex, 150_000, 200);
        for t in &mut narrow.turns {
            t.session_key = "narrow".into();
            t.model = "gpt-5.1".into();
        }
        data.turns.extend(narrow.turns);
        let r = &analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new())
            .recommendations[0];
        assert_eq!(
            r.window,
            PricingTable::instance().context_window("gpt-5.1").unwrap()
        );
        assert!(r.curve.iter().all(|p| p.0 <= r.window));
    }

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
    fn one_unpriced_stream_does_not_sink_the_whole_pooled_codex_group() {
        // Codex pools every model into one group, so a single stream whose
        // model carries no usable price (e.g. the all-zero placeholder
        // "codex-auto-review" in models.dev) must not turn the entire
        // recommendation into "missing pricing".
        let mut priced = history(AgentKind::Codex, 960_000, 1000);
        for t in &mut priced.turns {
            t.session_key = "priced".into();
        }
        let mut review = history(AgentKind::Codex, 200_000, 120);
        for t in &mut review.turns {
            t.session_key = "review".into();
            t.model = "codex-auto-review".into();
        }
        priced.turns.extend(review.turns);
        let report = analyze(
            &priced,
            AgentKind::Codex,
            None,
            0,
            i64::MAX,
            &BTreeMap::new(),
        );
        let r = &report.recommendations[0];
        assert_ne!(r.status, Status::UnknownPricing);
        assert_eq!(r.status, Status::Recommend);
        assert_eq!(r.observed, 955_000);
        assert_eq!(r.unpriced, 1);
        assert!(r.recommended < r.observed);
        assert!(r.baseline_cost > 0.0);
    }

    #[test]
    fn a_small_saving_that_would_compact_more_often_is_not_advised() {
        // Compacting more often throws away context the cost model does not
        // price, so a threshold below the historical one has to clear a
        // higher bar than a larger one. A few percent is not enough.
        let kept = &analyze(
            &history(AgentKind::Codex, 320_000, 1000),
            AgentKind::Codex,
            None,
            0,
            i64::MAX,
            &BTreeMap::new(),
        )
        .recommendations[0];
        // 4.6% would have cleared the old flat 3% bar, but it costs five
        // more compactions, so it now reads as "keep".
        assert!(kept.candidate < kept.observed);
        assert!(kept.candidate_saving >= 0.03 && kept.candidate_saving < 0.05);
        assert_eq!(kept.status, Status::Keep);
        assert_eq!(kept.recommended, kept.observed);
        assert_eq!(kept.recommended_cost, kept.baseline_cost);
        // The rejected candidate stays visible so the trade-off is shown.
        assert_eq!(kept.candidate_compactions, kept.baseline_compactions + 5);
        assert!(kept.over_share > 0.0);

        // Clearing the nominal bar alone is insufficient when plausible
        // extra rework reverses that advantage.
        let advised = &analyze(
            &history(AgentKind::Codex, 340_000, 1000),
            AgentKind::Codex,
            None,
            0,
            i64::MAX,
            &BTreeMap::new(),
        )
        .recommendations[0];
        assert!(advised.candidate < advised.observed);
        assert!(advised.candidate_saving >= 0.05);
        assert!(advised.cautious_saving < 0.05);
        assert!(advised.sensitivity_failed);
        assert_eq!(advised.status, Status::Keep);
        assert_eq!(advised.recommended, advised.current);
    }

    #[test]
    fn the_trade_off_counts_compactions_and_calls_past_the_threshold() {
        let data = history(AgentKind::Codex, 960_000, 1000);
        let report = analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new());
        let r = &report.recommendations[0];
        assert_eq!(r.status, Status::Recommend);
        // Compacting earlier means more compactions, and the share of calls
        // that would still reach the threshold is a real fraction.
        assert!(r.candidate_compactions > r.baseline_compactions);
        assert!(r.over_share > 0.0 && r.over_share <= 1.0);
        assert_eq!(r.candidate, r.recommended);
    }

    #[test]
    fn long_history_recommends_smaller_but_short_history_does_not_guess() {
        for agent in [AgentKind::Claude, AgentKind::Codex] {
            let data = history(agent, 960_000, 1000);
            let report = analyze(&data, agent, None, 0, i64::MAX, &BTreeMap::new());
            let r = &report.recommendations[0];
            assert_eq!(r.status, Status::Recommend);
            assert_eq!(r.observed, 955_000);
            assert_eq!(r.growth, 5_000);
            assert_eq!(r.after, 60_000);
            assert!(r.recommended >= 160_000 && r.recommended < r.observed);
            assert!(r.savings_fraction() >= 0.2);
            let short = analyze(&data, agent, None, 0, 1_000 + 20 * 30, &BTreeMap::new());
            assert_eq!(short.recommendations[0].status, Status::NotEnoughCalls);
            let no_cuts = analyze(
                &history(agent, 960_000, 100),
                agent,
                None,
                0,
                i64::MAX,
                &BTreeMap::new(),
            );
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
        let all = analyze(
            &data,
            AgentKind::Claude,
            None,
            0,
            i64::MAX,
            &BTreeMap::new(),
        );
        assert_eq!(all.recommendations.len(), 2);
        assert!(all
            .recommendations
            .iter()
            .all(|r| r.compactions == 0 && r.streams == 2));
        assert_eq!(
            analyze(
                &data,
                AgentKind::Claude,
                Some("laptop"),
                0,
                i64::MAX,
                &BTreeMap::new()
            )
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
        assert!(analyze(
            &switched,
            AgentKind::Claude,
            None,
            0,
            i64::MAX,
            &BTreeMap::new()
        )
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
            analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new()).recommendations
                [0]
            .status,
            Status::UnknownPricing
        );
        for t in &mut data.turns {
            t.context_stream.clear();
        }
        let report = analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new());
        assert!(report.recommendations.is_empty());
        assert_eq!(report.skipped, 200);
        assert!(
            analyze(&data, AgentKind::Devin, None, 0, i64::MAX, &BTreeMap::new())
                .recommendations
                .is_empty()
        );
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
        let report = analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new());
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
        let pooled = analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new());
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
        let original = analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new());
        let r = &original.recommendations[0];
        // The search now runs up to the model's window, so a threshold above
        // where compactions happened to land is reachable: compacting less
        // often saves the summary and the rework it costs.
        assert_eq!(r.observed, 145_000);
        assert_eq!(r.window, 400_000);
        assert_eq!(r.status, Status::Recommend);
        assert!(r.candidate > r.observed);
        assert_eq!(r.recommended, r.candidate);
        assert!(r.candidate_compactions < r.baseline_compactions);
        assert!(r.candidate_saving >= 0.03);
        // Hash maps / synced records need not arrive in transcript order.
        for (i, t) in data.turns.iter_mut().enumerate() {
            t.created_at = 1_000;
            t.context_order = i as u64;
        }
        data.turns.reverse();
        let reordered = analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new());
        assert_eq!(reordered.recommendations[0].compactions, r.compactions);
        assert_eq!(reordered.recommendations[0].observed, 145_000);
        assert_eq!(reordered.recommendations[0].candidate, r.candidate);
    }

    #[test]
    fn the_narrowest_model_in_the_pool_caps_the_advice() {
        // A global setting applies to every model pooled with it, so the
        // smallest window binds: Claude Haiku's 200K, not Opus's 1M.
        let mut data = history(AgentKind::Claude, 150_000, 200);
        for t in &mut data.turns {
            t.model = "claude-haiku-4-5".into();
        }
        let r = &analyze(
            &data,
            AgentKind::Claude,
            None,
            0,
            i64::MAX,
            &BTreeMap::new(),
        )
        .recommendations[0];
        assert_eq!(r.window, 200_000);
        assert!(r.candidate <= r.window);
        assert!(r.current <= r.window);
        // Nothing on the curve may sit above the window either.
        assert!(r.curve.iter().all(|(threshold, _)| *threshold <= r.window));
    }

    #[test]
    fn the_configured_threshold_is_what_the_advice_is_measured_against() {
        let data = history(AgentKind::Codex, 390_000, 1000);
        let key = ("desktop".to_string(), CODEX_GROUP.to_string());
        // Nothing set: the observed compaction point stands in for it.
        let inferred = &analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &BTreeMap::new())
            .recommendations[0];
        assert_eq!(inferred.current, inferred.observed);
        // Something set: that is the baseline, and the window still caps it.
        let set = BTreeMap::from([(key.clone(), 300_000)]);
        let measured =
            &analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &set).recommendations[0];
        assert_eq!(measured.current, 300_000);
        assert_eq!(measured.window, 400_000);
        // A setting past the window is clamped to it, as the agent runs at
        // the smaller of the two.
        let beyond = BTreeMap::from([(key, 900_000)]);
        let clamped =
            &analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &beyond).recommendations[0];
        assert_eq!(clamped.current, 400_000);
        // Another device's config is not ours to read, so its groups keep
        // the inferred baseline.
        let other = BTreeMap::from([(("laptop".to_string(), CODEX_GROUP.to_string()), 300_000)]);
        assert!(analyze(&data, AgentKind::Codex, None, 0, i64::MAX, &other)
            .recommendations
            .iter()
            .all(|r| r.current == r.observed));
    }
}
