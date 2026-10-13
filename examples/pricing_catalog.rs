//! Audit every provider quote and optionally write the offline pricing snapshot.
//! cargo run --example pricing_catalog -- <api.json> [<snapshot.json>]
use devin_usage_metrics::pricing::extract_models_dev_pricing;
use std::collections::{BTreeMap, BTreeSet};

fn main() {
    let args: Vec<_> = std::env::args().collect();
    let catalog: serde_json::Value = serde_json::from_str(
        &std::fs::read_to_string(args.get(1).expect("catalog path required")).unwrap(),
    )
    .unwrap();
    let prices = extract_models_dev_pricing(&catalog);
    let mut quotes = 0;
    let mut rejected = Vec::new();
    let mut mismatches = Vec::new();
    let mut bare_models = BTreeSet::new();
    for (provider, data) in catalog.as_object().unwrap() {
        for (id, model) in data["models"].as_object().unwrap() {
            let Some(cost) = model.get("cost") else {
                continue;
            };
            if cost.get("input").and_then(|v| v.as_f64()).is_none()
                || cost.get("output").and_then(|v| v.as_f64()).is_none()
            {
                continue;
            }
            quotes += 1;
            let scoped = format!("{provider}/{id}").to_ascii_lowercase();
            let bare = id.rsplit('/').next().unwrap().to_ascii_lowercase();
            bare_models.insert(bare);
            let Some(p) = prices.get(&scoped) else {
                rejected.push(scoped);
                continue;
            };
            let rates = [p.i, p.o, p.cr, p.cw];
            let expected = ["input", "output", "cache_read", "cache_write"]
                .map(|k| cost[k].as_f64().unwrap_or(0.0));
            if rates != expected || p.source.as_deref() != Some(provider.as_str()) {
                mismatches.push(scoped);
            }
            for tier in cost
                .get("tiers")
                .and_then(|v| v.as_array())
                .into_iter()
                .flatten()
            {
                let threshold = tier["tier"]["size"].as_u64().unwrap();
                let actual = p.for_prompt((threshold + 1) as f64);
                let expected = ["input", "output", "cache_read", "cache_write"]
                    .iter()
                    .zip(rates)
                    .map(|(key, base)| tier[key].as_f64().unwrap_or(base))
                    .collect::<Vec<_>>();
                if [actual.i, actual.o, actual.cr, actual.cw].as_slice() != expected {
                    mismatches.push(format!("{provider}/{id}: tier {threshold}"));
                }
            }
        }
    }
    let defaults = bare_models
        .iter()
        .filter(|id| prices.contains_key(*id))
        .count();
    println!("providers={} quotes={} scoped_and_aliases={} bare_defaults={} unavailable_defaults={} rejected_quotes={} mismatches={}",
        catalog.as_object().unwrap().len(), quotes, prices.len(), defaults, bare_models.len() - defaults, rejected.len(), mismatches.len());
    for id in rejected.iter().take(12) {
        println!("rejected: {id}");
    }
    for id in mismatches.iter().take(12) {
        println!("mismatch: {id}");
    }
    for id in [
        "claude-sonnet-5-5",
        "claude-opus-5-5",
        "gpt-6.1-sol",
        "gemini-3.8-flash",
        "deepseek-v4-pro",
        "kimi-k3",
        "glm-5.2",
        "minimax-m3",
    ] {
        if let Some(p) = prices.get(id) {
            println!(
                "{id}: provider={} input={} output={} cache_read={} cache_write={} tiers={}",
                p.source.as_deref().unwrap_or("unknown"),
                p.i,
                p.o,
                p.cr,
                p.cw,
                p.tiers.len()
            );
        } else {
            println!("{id}: unknown");
        }
    }
    assert!(
        mismatches.is_empty(),
        "provider quotes or tiers changed during extraction"
    );
    if let Some(path) = args.get(2) {
        let sorted: BTreeMap<_, _> = prices.into_iter().collect();
        // One model per line keeps a complete provider snapshot reviewable.
        let lines: Vec<_> = sorted
            .into_iter()
            .map(|(id, p)| {
                format!(
                    "  {}: {}",
                    serde_json::to_string(&id).unwrap(),
                    serde_json::to_string(&p).unwrap()
                )
            })
            .collect();
        std::fs::write(path, format!("{{\n{}\n}}\n", lines.join(",\n"))).unwrap();
    }
}
