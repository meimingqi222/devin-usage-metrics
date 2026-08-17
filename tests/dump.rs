use chrono::Local;
use devin_usage_metrics::{agg, data, pricing};

#[test]
fn dump_recent_usage() {
    let end = Local::now().timestamp() + 3600;
    let start = end - 30 * 86400;
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
    let mut total_cost = 0.0;
    for b in buckets.iter().rev().take(7) {
        total_cost += b.cost;
        let models: Vec<String> = b
            .by_model
            .iter()
            .map(|(m, u)| {
                let cost_str = if u.priced {
                    pricing::fmt_cost(u.cost)
                } else {
                    "—".into()
                };
                format!("{}={:.1}M/{}", m, u.total() / 1e6, cost_str)
            })
            .collect();
        println!(
            "{} [{}] 总计 {:.1}M 费用 {} | in {:.2}M out {:.2}M cached {:.1}M | {} 轮 {} 会话 | {}",
            b.label,
            b.sub,
            b.total() / 1e6,
            pricing::fmt_cost(b.cost),
            b.input / 1e6,
            b.output / 1e6,
            b.cached / 1e6,
            b.turns,
            b.session_keys.len(),
            models.join(", ")
        );
    }
    println!("最近 7 天总费用: {}", pricing::fmt_cost(total_cost));

    // 验证定价表能覆盖实际数据中的模型
    let table = pricing::PricingTable::instance();
    let mut unpriced: Vec<String> = Vec::new();
    for session in &data.sessions {
        let model = session.display_model();
        if model != "unknown" && table.find(&model).is_none() {
            unpriced.push(format!("{}: {}", session.agent.label(), model));
        }
    }
    if !unpriced.is_empty() {
        // 去重并只显示前 10 个
        unpriced.sort();
        unpriced.dedup();
        println!(
            "WARN 以下模型未找到定价 (共 {}): {}",
            unpriced.len(),
            unpriced.iter().take(10).cloned().collect::<Vec<_>>().join(", ")
        );
    }

    // Verify that the most recent non-empty bucket has reasonable aggregates.
    // Avoid hard-coding a specific date, since the local dataset changes over time.
    if let Some(b) = buckets.iter().rev().find(|b| b.total() > 0.0) {
        println!(
            "最新非空周期 {} 校验: input={:.2}M output={:.2}M cached={:.1}M cost={}",
            b.label,
            b.input / 1e6,
            b.output / 1e6,
            b.cached / 1e6,
            pricing::fmt_cost(b.cost)
        );
        assert!(
            b.input > 0.0 || b.output > 0.0 || b.cached > 0.0,
            "empty bucket"
        );
    }
}

#[test]
fn test_per_agent_lazy_loading() {
    let end = Local::now().timestamp() + 3600;
    let start = end - 30 * 86400;

    for agent in data::AgentKind::ALL {
        let loaded = data::load_agent(agent, start, end);
        println!(
            "Lazy loaded {}: {} sessions, {} turns, {} errors",
            agent.label(),
            loaded.sessions.len(),
            loaded.turns.len(),
            loaded.errors.len()
        );
        // All sessions loaded must belong to this agent
        for session in &loaded.sessions {
            assert_eq!(session.agent, agent);
        }
        for turn in &loaded.turns {
            assert_eq!(turn.agent, agent);
        }
    }
}
