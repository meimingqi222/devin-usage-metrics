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
}
