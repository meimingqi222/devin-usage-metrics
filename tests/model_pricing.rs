use devin_usage_metrics::pricing::{refresh_models_dev, PricingTable};
use std::{
    io::{Read, Write},
    net::TcpListener,
    thread,
};

fn serve_once(body: String) -> (String, thread::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}/models.json", listener.local_addr().unwrap());
    let task = thread::spawn(move || {
        let (mut stream, _) = listener.accept().unwrap();
        let mut request = [0; 4096];
        assert!(stream.read(&mut request).unwrap() > 0);
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
fn refresh_removes_stale_defaults_and_preserves_scoped_zero_prices() {
    let folder = tempfile::tempdir().unwrap();
    let cache = folder.path().join("models.json");
    std::env::set_var("DEVIN_USAGE_MODELS_PATH", &cache);
    std::env::remove_var("DEVIN_USAGE_DISABLE_MODELS_FETCH");
    let quote = |input| serde_json::json!({"cost": {"input": input, "output": 2, "cache_read": 0.1, "cache_write": 1.25}});
    let catalog = |second| {
        serde_json::json!({
        "channel-a": {"models": {"conflict-model": quote(1), "free-model": {"cost": {"input": 0, "output": 0}}}},
        "channel-b": {"models": {"conflict-model": quote(second)}}
    }).to_string()
    };
    let (url, task) = serve_once(catalog(1));
    std::env::set_var("DEVIN_USAGE_MODELS_URL", url);
    assert!(refresh_models_dev(true));
    task.join().unwrap();
    let table = PricingTable::instance();
    assert_eq!(table.find_api("conflict-model").unwrap().i, 1.0);
    assert_eq!(table.find_api("channel-a/free-model").unwrap().i, 0.0);
    // A successful complete catalog replaces defaults from the previous snapshot.
    assert!(table.find_api("gpt-5.1").is_none());
    let (url, task) = serve_once(catalog(3));
    std::env::set_var("DEVIN_USAGE_MODELS_URL", url);
    assert!(refresh_models_dev(true));
    task.join().unwrap();
    assert!(table.find_api("conflict-model").is_none());
    assert_eq!(table.find_api("channel-a/conflict-model").unwrap().i, 1.0);
    assert_eq!(table.find_api("channel-b/conflict-model").unwrap().i, 3.0);
    let saved = std::fs::read_to_string(&cache).unwrap();
    let (url, task) = serve_once("{}".into());
    std::env::set_var("DEVIN_USAGE_MODELS_URL", url);
    assert!(!refresh_models_dev(true));
    task.join().unwrap();
    assert_eq!(std::fs::read_to_string(&cache).unwrap(), saved);
    assert_eq!(table.find_api("channel-b/conflict-model").unwrap().i, 3.0);

    let mut catalog = serde_json::json!({
        "google": {"models": {
            "gemini-3.8-flash": {"cost": {"input": 2, "output": 5, "cache_read": 0.1}},
            "gemini-3.8-flash-n": {"cost": {"input": 9, "output": 11, "cache_read": 0.9, "cache_write": 7}}
        }},
        "reseller": {"models": {
            "gemini-3.8-flash": {"cost": {"input": 4, "output": 8, "cache_read": 0.4}}
        }}
    });
    let turn = devin_usage_metrics::data::TurnRec {
        agent: devin_usage_metrics::data::AgentKind::Antigravity,
        input_tokens: 1_000_000.0,
        output_tokens: 2_000_000.0,
        cache_read_tokens: 3_000_000.0,
        cache_creation_tokens: 4_000_000.0,
        ..Default::default()
    };
    let cost = |model| devin_usage_metrics::agg::cost_for_turn(&turn, model);
    let (url, task) = serve_once(catalog.to_string());
    std::env::set_var("DEVIN_USAGE_MODELS_URL", url);
    assert!(refresh_models_dev(true));
    task.join().unwrap();
    // Exact variant quotes win: 1*9 + 2*11 + 3*0.9 + 4*7 = $61.7.
    assert!((cost("gemini-3.8-flash-n").unwrap() - 61.7).abs() < 1e-9);
    // Unknown internal suffixes use the newly refreshed base, not embedded rates.
    assert!((cost("gemini-3.8-flash-exp-a").unwrap() - 20.3).abs() < 1e-9);
    assert!(cost("reseller/gemini-3.8-flash").is_none());
    // A real zero write quote is not a missing quote and must not be overridden.
    catalog["google"]["models"]["gemini-3.8-flash"]["cost"]["cache_write"] = serde_json::json!(0);
    let (url, task) = serve_once(catalog.to_string());
    std::env::set_var("DEVIN_USAGE_MODELS_URL", url);
    assert!(refresh_models_dev(true));
    task.join().unwrap();
    assert!((cost("gemini-3.8-flash-exp-a").unwrap() - 12.3).abs() < 1e-9);
}
