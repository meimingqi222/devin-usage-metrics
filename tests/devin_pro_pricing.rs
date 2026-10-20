use devin_usage_metrics::pricing::{devin_pro_turn_cost, refresh_devin_pro, PricingTable};
use std::{
    io::{Read, Write},
    net::TcpListener,
    thread,
};

fn serve_once(body: String) -> (String, thread::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}/models.md", listener.local_addr().unwrap());
    let task = thread::spawn(move || {
        let (mut stream, _) = listener.accept().unwrap();
        let mut request = [0; 4096];
        let received = stream.read(&mut request).unwrap();
        assert!(received > 0);
        write!(
            stream,
            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
            body.len(),
            body
        )
        .unwrap();
    });
    (url, task)
}

#[test]
fn pro_refresh_preserves_free_prices_and_keeps_cache_on_bad_response() {
    let folder = std::env::temp_dir().join(format!("devin-pro-pricing-{}", std::process::id()));
    std::fs::create_dir_all(&folder).unwrap();
    let cache = folder.join("models.md");
    std::env::set_var("DEVIN_USAGE_PRO_PRICING_PATH", &cache);
    std::env::remove_var("DEVIN_USAGE_DISABLE_MODELS_FETCH");
    let row = |tier, rate| {
        serde_json::json!({
            "tier": tier, "model_uid": "swe-2-high", "label": "SWE-2 High",
            "input_cost_per_million_usd": rate, "output_cost_per_million_usd": rate,
            "cache_write_cost_per_million_usd": 0, "cache_read_cost_per_million_usd": 0
        })
    };
    let mut lightning = row("TEAMS_TIER_PRO", 3.);
    lightning["model_uid"] = "swe-2-lightning-high".into();
    lightning["label"] = "SWE-2 Lightning High".into();
    let mut long_context = row("TEAMS_TIER_PRO", 2.);
    long_context["model_uid"] = "gpt-5-6-sol-high".into();
    long_context["label"] = "GPT-5.6 Sol High Thinking".into();
    let document_for = |swe_rate, threshold: &str, long_input| {
        let rows = serde_json::json!([
            row("TEAMS_TIER_ENTERPRISE_SAAS", 0.75),
            row("TEAMS_TIER_PRO", swe_rate),
            lightning.clone(),
            long_context.clone(),
        ]);
        let families = serde_json::json!({"Online family": {
            "threshold": threshold,
            "input": format!("${long_input}"),
            "output": "$9",
            "cacheInput": "$0.7",
            "cacheWrite": "$3",
        }});
        let models = serde_json::json!({"gpt-5-6-sol-high": "Online family"});
        format!(
            "export const modelCostData = {rows};\nconst longContextFamilies = {families};\nconst longContextModelFamilies = {models};"
        )
    };
    let document = document_for(0., "123K", 7.);
    let (url, task) = serve_once(document.clone());
    std::env::set_var("DEVIN_USAGE_PRO_PRICING_URL", url);
    assert!(refresh_devin_pro(true));
    task.join().unwrap();
    assert_eq!(
        PricingTable::instance()
            .find_devin_pro("swe-2-high")
            .unwrap()
            .i,
        0.
    );
    assert_eq!(std::fs::read_to_string(&cache).unwrap(), document);

    let during_extension = chrono::NaiveDate::from_ymd_opt(2026, 10, 20)
        .unwrap()
        .and_hms_opt(12, 0, 0)
        .unwrap()
        .and_utc()
        .timestamp();
    assert_eq!(
        devin_pro_turn_cost("swe-2-high", 1e6, 1e6, 1e6, 0., during_extension),
        Some(0.)
    );
    assert_eq!(
        devin_pro_turn_cost("swe-2-lightning-high", 1e6, 1e6, 0., 0., during_extension),
        Some(6.)
    );

    assert_eq!(
        devin_pro_turn_cost("gpt-5-6-sol-high", 1e6, 0., 0., 0., during_extension),
        Some(7.)
    );
    // Neither old free dates nor the old GPT-5.6 constants override refreshed quotes.
    let paid_document = document_for(5., "400K", 13.);
    let (url, task) = serve_once(paid_document.clone());
    std::env::set_var("DEVIN_USAGE_PRO_PRICING_URL", url);
    assert!(refresh_devin_pro(true));
    task.join().unwrap();
    for created_at in [0, during_extension, 2_000_000_000] {
        assert_eq!(
            devin_pro_turn_cost("swe-2-high", 1e6, 1e6, 0., 0., created_at),
            Some(10.)
        );
    }
    assert_eq!(
        devin_pro_turn_cost("gpt-5-6-sol-high", 300_000., 0., 0., 0., during_extension),
        Some(0.6)
    );
    assert_eq!(
        devin_pro_turn_cost("gpt-5-6-sol-high", 500_000., 0., 0., 0., during_extension),
        Some(6.5)
    );
    assert_eq!(
        devin_pro_turn_cost(
            "GPT-5.6 Sol High Thinking",
            500_000.,
            0.,
            0.,
            0.,
            during_extension
        ),
        Some(6.5)
    );

    // A fresh cache must avoid the network altogether.
    std::env::set_var(
        "DEVIN_USAGE_PRO_PRICING_URL",
        "http://127.0.0.1:1/unavailable",
    );
    assert!(!refresh_devin_pro(false));
    let (url, task) = serve_once(paid_document.replace("400K", "unknown"));
    std::env::set_var("DEVIN_USAGE_PRO_PRICING_URL", url);
    assert!(!refresh_devin_pro(true));
    task.join().unwrap();
    assert_eq!(std::fs::read_to_string(&cache).unwrap(), paid_document);
    assert_eq!(
        PricingTable::instance()
            .find_devin_pro("swe-2-high")
            .unwrap()
            .i,
        5.
    );
    assert!(cache.with_file_name("models.md.retry").exists());
    // A freshly written but invalid cache must not suppress a real refresh.
    std::fs::write(&cache, "broken document").unwrap();
    std::fs::remove_file(cache.with_file_name("models.md.retry")).unwrap();
    let (url, task) = serve_once(document.clone());
    std::env::set_var("DEVIN_USAGE_PRO_PRICING_URL", url);
    assert!(refresh_devin_pro(false));
    task.join().unwrap();
    assert_eq!(
        devin_pro_turn_cost("swe-2-high", 1e6, 1e6, 0., 0., 2_000_000_000),
        Some(0.)
    );
    assert!(!cache.with_file_name("models.md.retry").exists());
    std::env::remove_var("DEVIN_USAGE_PRO_PRICING_PATH");
    std::env::remove_var("DEVIN_USAGE_PRO_PRICING_URL");
    std::fs::remove_file(&cache).unwrap();
    std::fs::remove_dir(folder).unwrap();
}
