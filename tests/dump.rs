use chrono::Local;
use devin_usage_metrics::{agg, data};

#[test]
fn dump_recent_usage() {
    let end = Local::now().timestamp() + 3600;
    let start = end - 200 * 86400;
    let data = data::load_all(start, end);
    println!(
        "sessions: {}, turns: {}, errors: {:?}",
        data.sessions.len(),
        data.turns.len(),
        data.errors
    );
    assert!(!data.sessions.is_empty(), "no sessions loaded");
    assert!(!data.turns.is_empty(), "no turns loaded");

    for agent in data::AgentKind::ALL {
        let sessions = data
            .sessions
            .iter()
            .filter(|session| session.agent == agent)
            .count();
        let turns: Vec<_> = data
            .turns
            .iter()
            .filter(|turn| turn.agent == agent)
            .collect();
        let tokens: f64 = turns
            .iter()
            .map(|turn| turn.input_tokens + turn.output_tokens + turn.cache_read_tokens)
            .sum();
        println!(
            "{}: {} sessions, {} turns, {} tokens",
            agent.label(),
            sessions,
            turns.len(),
            tokens
        );
    }

    let buckets = agg::build_buckets(&data, agg::PeriodKind::Day);
    for b in buckets.iter().rev().take(7) {
        let models: Vec<String> = b
            .by_model
            .iter()
            .map(|(m, u)| format!("{}={:.1}M", m, u.total() / 1e6))
            .collect();
        println!(
            "{} [{}] 总计 {:.1}M | in {:.2}M out {:.2}M cached {:.1}M | {} 轮 {} 会话 | {}",
            b.label,
            b.sub,
            b.total() / 1e6,
            b.input / 1e6,
            b.output / 1e6,
            b.cached / 1e6,
            b.turns,
            b.session_keys.len(),
            models.join(", ")
        );
    }

    // Verify that the most recent non-empty bucket has reasonable aggregates.
    // Avoid hard-coding a specific date, since the local dataset changes over time.
    if let Some(b) = buckets.iter().rev().find(|b| b.total() > 0.0) {
        println!(
            "最新非空周期 {} 校验: input={:.2}M output={:.2}M cached={:.1}M",
            b.label,
            b.input / 1e6,
            b.output / 1e6,
            b.cached / 1e6
        );
        assert!(
            b.input > 0.0 || b.output > 0.0 || b.cached > 0.0,
            "empty bucket"
        );
    }
}
