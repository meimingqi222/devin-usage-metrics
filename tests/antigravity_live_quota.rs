#![cfg(target_os = "windows")]
use devin_usage_metrics::quota::{discover_accounts, fetch_account, Provider};
#[test]
#[ignore = "Requires installed Antigravity, an active login and network access"]
fn current_windows_login_can_fetch_quota() {
    let account = discover_accounts(false)
        .into_iter()
        .find(|account| account.provider == Provider::Antigravity)
        .expect("Antigravity account should be discovered");
    let result = fetch_account(&account).expect("quota query should succeed");
    assert!(
        !result.windows.is_empty(),
        "official quota windows should be present"
    );
}
