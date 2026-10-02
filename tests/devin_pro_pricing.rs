use devin_usage_metrics::pricing::{refresh_devin_pro, PricingTable};
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
    let document = format!(
        "export const modelCostData = {};\nexport const Component = null;",
        serde_json::to_string(&vec![
            row("TEAMS_TIER_ENTERPRISE_SAAS", 0.75),
            row("TEAMS_TIER_PRO", 0.)
        ])
        .unwrap()
    );
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

    // A fresh cache must avoid the network altogether.
    std::env::set_var(
        "DEVIN_USAGE_PRO_PRICING_URL",
        "http://127.0.0.1:1/unavailable",
    );
    assert!(!refresh_devin_pro(false));
    let (url, task) = serve_once("export const modelCostData = [];".into());
    std::env::set_var("DEVIN_USAGE_PRO_PRICING_URL", url);
    assert!(!refresh_devin_pro(true));
    task.join().unwrap();
    assert_eq!(std::fs::read_to_string(&cache).unwrap(), document);
    assert_eq!(
        PricingTable::instance()
            .find_devin_pro("swe-2-high")
            .unwrap()
            .i,
        0.
    );
    assert!(cache.with_file_name("models.md.retry").exists());
    std::env::remove_var("DEVIN_USAGE_PRO_PRICING_PATH");
    std::env::remove_var("DEVIN_USAGE_PRO_PRICING_URL");
    std::fs::remove_file(&cache).unwrap();
    std::fs::remove_file(cache.with_file_name("models.md.retry")).unwrap();
    std::fs::remove_dir(folder).unwrap();
}
