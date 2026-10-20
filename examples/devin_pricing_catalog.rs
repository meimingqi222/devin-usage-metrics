//! Inspect the official Pro document and optionally regenerate its offline snapshot.
//! cargo run --example devin_pricing_catalog -- <models.md> [<snapshot.json>]
use devin_usage_metrics::pricing::extract_devin_pro_pricing;
use std::collections::BTreeMap;

fn main() {
    let args: Vec<_> = std::env::args().collect();
    let text = std::fs::read_to_string(args.get(1).expect("models.md path required")).unwrap();
    let prices = extract_devin_pro_pricing(&text).expect("invalid official Pro pricing document");
    let tiered = prices
        .values()
        .filter(|price| !price.tiers.is_empty())
        .count();
    println!("pro_models={} tiered_models={tiered}", prices.len());
    if let Some(path) = args.get(2) {
        let sorted: BTreeMap<_, _> = prices.into_iter().collect();
        std::fs::write(path, serde_json::to_string_pretty(&sorted).unwrap() + "\n").unwrap();
    }
}
