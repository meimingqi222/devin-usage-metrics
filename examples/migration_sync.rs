//! Opt-in migration acceptance helper. Use an isolated HOME and loopback server.
//! Inputs are fixture files, never production login credentials.
use devin_usage_metrics::{data::LoadedData, sync};
use std::{env, fs};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let url = env::var("AUM_INTEROP_URL")?;
    if !url.starts_with("http://127.0.0.1:") {
        return Err("acceptance helper requires a loopback server".into());
    }
    let token = fs::read_to_string(env::var("AUM_INTEROP_TOKEN_FILE")?)?;
    let input: LoadedData = serde_json::from_slice(&fs::read(env::var("AUM_INTEROP_INPUT")?)?)?;
    sync::save_config(&sync::SyncConfig {
        enabled: true,
        api_url: url,
    });
    sync::save_github_sync_session(&sync::GitHubSyncSession {
        sync_token: token,
        login: "migration-fixture".into(),
        expires_at: chrono::Utc::now().timestamp() + 3600,
    })?;
    let (published, (remote, _, import_error)) = sync::sync_cycle(input);
    // Never print raw transport errors: they may contain a server URL or body.
    if published.is_err() || import_error.is_some() {
        return Err(format!(
            "fixture sync failed: publish={}, import={}",
            published.is_err(),
            import_error.is_some()
        )
        .into());
    }
    println!("{}", serde_json::to_string(&remote)?);
    Ok(())
}
