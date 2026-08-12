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

    // 2026-08-05 full-day totals independently verified via Python/SQL:
    // input=6.07M output=0.75M cached=316.8M (2337 turns, 3 sessions)
    if let Some(b) = buckets.iter().find(|b| b.label == "08-05") {
        println!(
            "08-05 校验: input={:.2}M output={:.2}M cached={:.1}M",
            b.input / 1e6,
            b.output / 1e6,
            b.cached / 1e6
        );
        assert!((b.input / 1e6 - 6.07).abs() < 0.5, "08-05 input mismatch");
        assert!((b.output / 1e6 - 0.75).abs() < 0.2, "08-05 output mismatch");
        assert!(
            (b.cached / 1e6 - 316.8).abs() < 20.0,
            "08-05 cached mismatch"
        );
    }
}
