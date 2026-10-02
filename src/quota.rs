//! 订阅用量查询：Claude Code / Codex / Grok / Devin 的配额窗口。
//!
//! - 凭证默认读本机各 agent 的登录文件；刷新令牌后原子写回原文件，
//!   与 CLI 自身行为一致（这些 OAuth 的 refresh_token 是一次性轮换的，
//!   只刷不写回会让 CLI 下次刷新失败）。
//! - Devin 走浏览器 session cookie（手动粘贴，存本应用配置目录）。
//! - Devin CLI 走本地 credentials.toml 中的 session token 自动发现，
//!   配额数据从本地缓存的 user_status.bin (protobuf) 读取，辅以
//!   billing/status JSON API。
//! - 全部接口为各家客户端使用的内部接口，无公开文档；解析一律防御式，
//!   字段缺失时降级而不是崩溃。

use chrono::{DateTime, TimeZone};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::path::{Path, PathBuf};
use std::time::Duration;

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum Provider {
    Claude,
    Codex,
    Grok,
    Devin,
    Antigravity,
}

impl Provider {
    pub fn label(self) -> &'static str {
        match self {
            Provider::Claude => "Claude Code",
            Provider::Codex => "Codex",
            Provider::Grok => "Grok",
            Provider::Devin => "Devin",
            Provider::Antigravity => "Antigravity",
        }
    }
}

/// 窗口标签。Server 直接给出的名字（如 Codex additional_rate_limits 的
/// limit_name）用 `Custom` 原样展示。
#[derive(Clone, Debug, PartialEq)]
pub enum WindowLabel {
    FiveHour,
    SevenDay,
    SevenDayOauthApps,
    SevenDayOpus,
    SevenDaySonnet,
    Daily,
    Weekly,
    Monthly,
    Custom(String),
}

#[derive(Clone, Debug)]
pub struct QuotaWindow {
    pub label: WindowLabel,
    /// 已用百分比（0-100，可能略超 100）
    pub used_percent: f64,
    /// 重置时间（unix 秒）
    pub resets_at: Option<i64>,
}

#[derive(Clone, Debug, Default)]
pub struct QuotaResult {
    /// 套餐/计划名（如 pro、max、team），可空
    pub plan: Option<String>,
    pub windows: Vec<QuotaWindow>,
    /// 附加说明行（超额余额等），可空
    pub extra: Option<String>,
}

pub enum AccountKind {
    ClaudeLocal {
        path: PathBuf,
    },
    /// macOS Keychain 中的 Claude Code 凭证（`Claude Code-credentials`）。
    /// Linux/Windows 上不会出现此变体。
    ClaudeKeychain,
    CodexLocal {
        path: PathBuf,
    },
    GrokLocal {
        path: PathBuf,
    },
    Devin {
        cookie: String,
        org_id: Option<String>,
    },
    /// Devin CLI 本地自动发现：session token 来自 credentials.toml，
    /// org_id 来自 config.json。不可手动删除（关闭 Devin CLI 后自动消失）。
    DevinCli {
        token: String,
        org_id: String,
    },
    /// Antigravity (Google Gemini) 本地 OAuth 凭证。
    /// `from_keychain=true` 表示来源是 macOS Keychain（仅在用户显式同意后发现）。
    AntigravityLocal {
        path: PathBuf,
        from_keychain: bool,
    },
}

pub struct Account {
    pub key: String,
    pub provider: Provider,
    pub label: String,
    pub kind: AccountKind,
}

// ---------------------------------------------------------------------------
// 凭证存储（手动添加的账号，目前只有 Devin cookie）
// ---------------------------------------------------------------------------

#[derive(Serialize, Deserialize)]
struct StoredDevin {
    label: String,
    cookie: String,
    org_id: Option<String>,
}

#[derive(Serialize, Deserialize, Default)]
struct StoreFile {
    #[serde(default)]
    devin: Vec<StoredDevin>,
}

fn store_path() -> Option<PathBuf> {
    dirs::config_dir().map(|base| base.join("devin-usage-metrics/quota-accounts.json"))
}

fn prefs_path() -> Option<PathBuf> {
    dirs::config_dir().map(|base| base.join("devin-usage-metrics/quota-prefs.json"))
}

#[derive(Serialize, Deserialize, Default)]
struct QuotaPrefs {
    #[serde(default)]
    keychain_allowed: bool,
}

fn load_prefs() -> QuotaPrefs {
    prefs_path()
        .and_then(|p| std::fs::read_to_string(p).ok())
        .and_then(|text| serde_json::from_str(&text).ok())
        .unwrap_or_default()
}

/// 用户是否已显式同意读取 macOS Keychain。默认 false。
pub fn keychain_allowed() -> bool {
    load_prefs().keychain_allowed
}

pub fn set_keychain_allowed(allowed: bool) {
    let Some(path) = prefs_path() else { return };
    let mut prefs = load_prefs();
    prefs.keychain_allowed = allowed;
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    let Ok(text) = serde_json::to_string_pretty(&prefs) else {
        return;
    };
    atomic_write(&path, text.as_bytes());
}

/// 是否可能还有 Keychain 里的账号可读。**不访问钥匙串**，只看安装痕迹，
/// 用于决定是否展示「允许读取钥匙串」提示。
pub fn macos_may_have_keychain_accounts() -> bool {
    if !cfg!(target_os = "macos") {
        return false;
    }
    let Some(h) = home() else {
        return false;
    };
    // Claude Code 在 macOS 默认把 OAuth 放 Keychain，而不是 .credentials.json
    let claude_keychain_likely =
        h.join(".claude").exists() && !h.join(".claude").join(".credentials.json").exists();
    // Antigravity / Gemini Code Assist
    let antigravity_installed =
        h.join(".gemini").join("antigravity").exists() || h.join(".gemini").join("config").exists();
    claude_keychain_likely || antigravity_installed
}

fn load_store() -> StoreFile {
    store_path()
        .and_then(|p| std::fs::read_to_string(p).ok())
        .and_then(|text| serde_json::from_str(&text).ok())
        .unwrap_or_default()
}

pub fn save_devin_account(label: &str, cookie: &str, org_id: Option<&str>) {
    let Some(path) = store_path() else { return };
    let mut store = load_store();
    store.devin.push(StoredDevin {
        label: label.to_string(),
        cookie: cookie.to_string(),
        org_id: org_id.map(|s| s.to_string()),
    });
    write_store(&path, &store);
}

pub fn remove_devin_account(index: usize) {
    let Some(path) = store_path() else { return };
    let mut store = load_store();
    if index < store.devin.len() {
        store.devin.remove(index);
        write_store(&path, &store);
    }
}

fn write_store(path: &Path, store: &StoreFile) {
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    let Ok(text) = serde_json::to_string_pretty(store) else {
        return;
    };
    atomic_write(path, text.as_bytes());
}

/// 先写临时文件再替换，避免半截文件（Windows 上 rename 不覆盖，需先删旧文件）。
fn atomic_write(path: &Path, data: &[u8]) {
    let temp = path.with_extension(format!("tmp-{}", std::process::id()));
    if std::fs::write(&temp, data).is_err() {
        return;
    }
    if std::fs::rename(&temp, path).is_err() {
        let _ = std::fs::remove_file(path);
        if std::fs::rename(&temp, path).is_err() {
            let _ = std::fs::remove_file(&temp);
        }
    }
}

// ---------------------------------------------------------------------------
// 账号发现：本机登录文件 + 已保存的粘贴账号
// ---------------------------------------------------------------------------

fn home() -> Option<PathBuf> {
    dirs::home_dir()
}

/// 发现本机已登录账号。
///
/// `allow_keychain=false`（默认）时**绝不读取 Keychain 密文**，也不发起任何
/// OAuth refresh——避免启动/进页就弹系统钥匙串授权。用户在 UI 显式同意后
/// 再以 `allow_keychain=true` 调用，此时才会检查/读取 Keychain。
pub fn discover_accounts(allow_keychain: bool) -> Vec<Account> {
    let mut out = Vec::new();

    if let Some(h) = home() {
        let claude = h.join(".claude").join(".credentials.json");
        if claude.exists() {
            let label = read_claude_email(&claude).unwrap_or_else(|| "Claude".into());
            out.push(Account {
                key: "claude-local".into(),
                provider: Provider::Claude,
                label,
                kind: AccountKind::ClaudeLocal { path: claude },
            });
        } else if allow_keychain
            && cfg!(target_os = "macos")
            && keychain_item_exists("Claude Code-credentials")
        {
            // 只探测条目是否存在；真正读密文发生在 fetch 阶段
            out.push(Account {
                key: "claude-keychain".into(),
                provider: Provider::Claude,
                label: "Claude Code".into(),
                kind: AccountKind::ClaudeKeychain,
            });
        }

        let codex = h.join(".codex").join("auth.json");
        if codex.exists() {
            let label = read_codex_email(&codex).unwrap_or_else(|| "Codex".into());
            out.push(Account {
                key: "codex-local".into(),
                provider: Provider::Codex,
                label,
                kind: AccountKind::CodexLocal { path: codex },
            });
        }

        let grok = h.join(".grok").join("auth.json");
        if grok.exists() {
            let label = read_grok_email(&grok).unwrap_or_else(|| "Grok".into());
            out.push(Account {
                key: "grok-local".into(),
                provider: Provider::Grok,
                label,
                kind: AccountKind::GrokLocal { path: grok },
            });
        }

        // Antigravity: macOS 上 Keychain 是当前源；未同意时只读文件，绝不碰钥匙串
        let antigravity = antigravity_state_path()
            .filter(|path| read_antigravity_state_token(path).is_some())
            .unwrap_or_else(|| h.join(".gemini").join("oauth_creds.json"));
        if allow_keychain && cfg!(target_os = "macos") && keychain_item_exists("gemini") {
            out.push(Account {
                key: "antigravity-keychain".into(),
                provider: Provider::Antigravity,
                label: "Antigravity".into(),
                kind: AccountKind::AntigravityLocal {
                    path: antigravity,
                    from_keychain: true,
                },
            });
        } else if antigravity.exists() {
            let label =
                read_antigravity_email(&antigravity).unwrap_or_else(|| "Antigravity".into());
            out.push(Account {
                key: "antigravity-local".into(),
                provider: Provider::Antigravity,
                label,
                kind: AccountKind::AntigravityLocal {
                    path: antigravity,
                    from_keychain: false,
                },
            });
        }
    }

    // Devin CLI 自动发现：读取 credentials.toml + config.json
    if let Some((token, org_id)) = read_devin_cli_credentials() {
        out.push(Account {
            key: "devin-cli".into(),
            provider: Provider::Devin,
            label: "Devin CLI".into(),
            kind: AccountKind::DevinCli { token, org_id },
        });
    }

    for (i, d) in load_store().devin.into_iter().enumerate() {
        out.push(Account {
            key: format!("devin-{i}"),
            provider: Provider::Devin,
            label: if d.label.is_empty() {
                "Devin".into()
            } else {
                d.label
            },
            kind: AccountKind::Devin {
                cookie: d.cookie,
                org_id: d.org_id,
            },
        });
    }
    out
}

fn json_field<'a>(v: &'a Value, path: &[&str]) -> Option<&'a Value> {
    let mut cur = v;
    for key in path {
        cur = cur.get(key)?;
    }
    Some(cur)
}

fn read_claude_email(path: &Path) -> Option<String> {
    let v: Value = serde_json::from_str(&std::fs::read_to_string(path).ok()?).ok()?;
    json_field(&v, &["claudeAiOauth", "emailAddress"])
        .and_then(Value::as_str)
        .map(str::to_string)
}

/// macOS Keychain：只探测条目是否存在，**不读密文**。
/// `security find-generic-password -s <service>`（无 `-w`）通常不会触发授权弹窗。
#[cfg(target_os = "macos")]
fn keychain_item_exists(service: &str) -> bool {
    std::process::Command::new("security")
        .args(["find-generic-password", "-s", service])
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}

#[cfg(not(target_os = "macos"))]
fn keychain_item_exists(_service: &str) -> bool {
    false
}

/// 进程内缓存已成功读到的 Keychain 密文，避免每次刷新都再次触发系统授权。
#[cfg(target_os = "macos")]
fn keychain_secret_cache() -> &'static std::sync::Mutex<std::collections::HashMap<String, String>> {
    use std::collections::HashMap;
    use std::sync::{Mutex, OnceLock};
    static CACHE: OnceLock<Mutex<HashMap<String, String>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

/// macOS Keychain 读取：`security find-generic-password -s <service> -w`
/// 仅在用户显式同意后、真正查询配额时调用。
#[cfg(target_os = "macos")]
fn keychain_read(service: &str) -> Option<String> {
    if let Ok(map) = keychain_secret_cache().lock() {
        if let Some(v) = map.get(service) {
            return Some(v.clone());
        }
    }
    let output = std::process::Command::new("security")
        .args(["find-generic-password", "-s", service, "-w"])
        .output()
        .ok()?;
    if !output.status.success() {
        return None;
    }
    let text = String::from_utf8_lossy(&output.stdout);
    let trimmed = text.trim();
    if trimmed.is_empty() {
        return None;
    }
    let value = trimmed.to_string();
    if let Ok(mut map) = keychain_secret_cache().lock() {
        map.insert(service.to_string(), value.clone());
    }
    Some(value)
}

/// macOS Keychain 写入：先删除旧条目再添加，避免重复。
#[cfg(target_os = "macos")]
fn keychain_write(service: &str, account: &str, value: &str) {
    let _ = std::process::Command::new("security")
        .args(["delete-generic-password", "-s", service])
        .output();
    let _ = std::process::Command::new("security")
        .args([
            "add-generic-password",
            "-s",
            service,
            "-a",
            account,
            "-w",
            value,
        ])
        .output();
    if let Ok(mut map) = keychain_secret_cache().lock() {
        map.insert(service.to_string(), value.to_string());
    }
}

/// 简易 base64 解码器（避免引入 base64 crate 依赖）。
fn base64_decode(input: &str) -> Option<Vec<u8>> {
    let table: [i16; 256] = {
        let mut t = [-1i16; 256];
        for (i, c) in b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
            .iter()
            .enumerate()
        {
            t[*c as usize] = i as i16;
        }
        t
    };
    let mut out = Vec::new();
    let mut buf = 0u32;
    let mut bits = 0;
    for ch in input.bytes() {
        if ch == b'=' {
            break;
        }
        let val = table[ch as usize];
        if val < 0 {
            continue; // 跳过空白等非 base64 字符
        }
        buf = (buf << 6) | (val as u32);
        bits += 6;
        if bits >= 8 {
            bits -= 8;
            out.push((buf >> bits) as u8);
            buf &= (1 << bits) - 1;
        }
    }
    Some(out)
}

fn read_antigravity_email(path: &Path) -> Option<String> {
    if path.extension().and_then(|ext| ext.to_str()) == Some("vscdb") {
        return read_antigravity_state_email(path);
    }
    let v: Value = serde_json::from_str(&std::fs::read_to_string(path).ok()?).ok()?;
    // 从 id_token JWT 中提取 email
    let id_token = v.get("id_token").and_then(Value::as_str)?;
    let parts: Vec<&str> = id_token.split('.').collect();
    if parts.len() < 2 {
        return None;
    }
    let payload = base64_decode(parts[1])?;
    let payload_str = String::from_utf8(payload).ok()?;
    let claims: Value = serde_json::from_str(&payload_str).ok()?;
    claims
        .get("email")
        .and_then(Value::as_str)
        .map(str::to_string)
}

fn read_codex_email(path: &Path) -> Option<String> {
    let v: Value = serde_json::from_str(&std::fs::read_to_string(path).ok()?).ok()?;
    v.get("email").and_then(Value::as_str).map(str::to_string)
}

fn read_grok_email(path: &Path) -> Option<String> {
    let v: Value = serde_json::from_str(&std::fs::read_to_string(path).ok()?).ok()?;
    grok_entry(&v)
        .and_then(|entry| entry.get("email"))
        .and_then(Value::as_str)
        .map(str::to_string)
}

/// ~/.grok/auth.json 顶层键是 "https://auth.x.ai::<client_id>"，取第一个含
/// refresh_token 的对象。
fn grok_entry(v: &Value) -> Option<&Value> {
    let obj = v.as_object()?;
    obj.values()
        .find(|entry| entry.is_object() && entry.get("refresh_token").is_some())
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

fn http_agent() -> ureq::Agent {
    ureq::Agent::config_builder()
        .timeout_global(Some(Duration::from_secs(20)))
        .build()
        .new_agent()
}

fn get_json(agent: &ureq::Agent, url: &str, headers: &[(&str, &str)]) -> Result<Value, String> {
    let mut req = agent.get(url);
    for &(k, v) in headers {
        req = req.header(k, v);
    }
    let resp = req.call().map_err(http_err)?;
    read_json(resp)
}

fn post_form(
    agent: &ureq::Agent,
    url: &str,
    form: &str,
    headers: &[(&str, &str)],
) -> Result<Value, String> {
    let mut req = agent
        .post(url)
        .header("content-type", "application/x-www-form-urlencoded")
        .header("accept", "application/json");
    for &(k, v) in headers {
        req = req.header(k, v);
    }
    let resp = req.send(form.as_bytes()).map_err(http_err)?;
    read_json(resp)
}

fn post_json(
    agent: &ureq::Agent,
    url: &str,
    payload: Value,
    headers: &[(&str, &str)],
) -> Result<Value, String> {
    let mut req = agent.post(url).header("accept", "application/json");
    for &(k, v) in headers {
        req = req.header(k, v);
    }
    let resp = req.send_json(payload).map_err(http_err)?;
    read_json(resp)
}

fn read_json(resp: ureq::http::Response<ureq::Body>) -> Result<Value, String> {
    let mut body = resp.into_body();
    let text = body
        .read_to_string()
        .map_err(|e: ureq::Error| e.to_string())?;
    serde_json::from_str(&text).map_err(|e| format!("JSON: {e}"))
}

fn http_err(e: ureq::Error) -> String {
    match e {
        ureq::Error::StatusCode(code) => format!("HTTP {code}"),
        other => other.to_string(),
    }
}

/// RFC3339 / 秒 / 毫秒 三种形态的时间 → unix 秒。
fn parse_when(v: &Value) -> Option<i64> {
    if let Some(s) = v.as_str() {
        if let Ok(dt) = DateTime::parse_from_rfc3339(s) {
            return Some(dt.timestamp());
        }
        if let Ok(dt) = chrono::NaiveDateTime::parse_from_str(s, "%Y-%m-%dT%H:%M:%S") {
            return Some(dt.and_utc().timestamp());
        }
    }
    if let Some(n) = v.as_f64() {
        if n > 1e12 {
            return Some((n / 1000.0) as i64);
        }
        return Some(n as i64);
    }
    None
}

// ---------------------------------------------------------------------------
// 查询入口
// ---------------------------------------------------------------------------

pub fn fetch_account(account: &Account) -> Result<QuotaResult, String> {
    match &account.kind {
        AccountKind::ClaudeLocal { path } => fetch_claude(path),
        AccountKind::ClaudeKeychain => fetch_claude_keychain(),
        AccountKind::CodexLocal { path } => fetch_codex(path),
        AccountKind::GrokLocal { path } => fetch_grok(path),
        AccountKind::Devin { cookie, org_id } => fetch_devin(cookie, org_id.as_deref()),
        AccountKind::DevinCli { token, org_id } => fetch_devin_cli(token, org_id),
        AccountKind::AntigravityLocal {
            path,
            from_keychain,
        } => fetch_antigravity(path, *from_keychain),
    }
}

// ---------------------------------------------------------------------------
// Claude Code
// ---------------------------------------------------------------------------

const CLAUDE_CLIENT_ID: &str = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";
const CLAUDE_SCOPES: &str =
    "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload";

struct ClaudeCred {
    access_token: String,
    refresh_token: String,
    /// unix 秒；None = 未知，先试直接查询
    expires_at: Option<i64>,
}

fn read_claude_cred(path: &Path) -> Result<ClaudeCred, String> {
    let text = std::fs::read_to_string(path).map_err(|e| e.to_string())?;
    let v: Value = serde_json::from_str(&text).map_err(|e| e.to_string())?;
    let oauth = json_field(&v, &["claudeAiOauth"]).ok_or_else(|| {
        "credentials.json missing claudeAiOauth (macOS Keychain not supported yet)".to_string()
    })?;
    let access_token = json_field(oauth, &["accessToken"])
        .and_then(Value::as_str)
        .ok_or_else(|| "missing accessToken".to_string())?
        .to_string();
    let refresh_token = json_field(oauth, &["refreshToken"])
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();
    let expires_at = json_field(oauth, &["expiresAt"]).and_then(parse_when);
    Ok(ClaudeCred {
        access_token,
        refresh_token,
        expires_at,
    })
}

fn write_back_claude(path: &Path, cred: &ClaudeCred) {
    let Ok(text) = std::fs::read_to_string(path) else {
        return;
    };
    let Ok(mut v) = serde_json::from_str::<Value>(&text) else {
        return;
    };
    if let Some(oauth) = v.pointer_mut("/claudeAiOauth") {
        if let Some(obj) = oauth.as_object_mut() {
            obj.insert(
                "accessToken".into(),
                Value::String(cred.access_token.clone()),
            );
            obj.insert(
                "refreshToken".into(),
                Value::String(cred.refresh_token.clone()),
            );
            if let Some(ts) = cred.expires_at {
                obj.insert("expiresAt".into(), Value::from(ts * 1000));
            }
        }
    }
    let Ok(out) = serde_json::to_string_pretty(&v) else {
        return;
    };
    atomic_write(path, out.as_bytes());
}

fn refresh_claude(cred: &mut ClaudeCred) -> Result<(), String> {
    let agent = http_agent();
    let resp = post_json(
        &agent,
        "https://platform.claude.com/v1/oauth/token",
        serde_json::json!({
            "client_id": CLAUDE_CLIENT_ID,
            "grant_type": "refresh_token",
            "refresh_token": cred.refresh_token,
            "scope": CLAUDE_SCOPES,
        }),
        &[("user-agent", "axios/1.15.2")],
    )?;
    let access = json_field(&resp, &["access_token"])
        .and_then(Value::as_str)
        .ok_or_else(|| "refresh response missing access_token".to_string())?
        .to_string();
    cred.access_token = access;
    if let Some(rt) = json_field(&resp, &["refresh_token"]).and_then(Value::as_str) {
        cred.refresh_token = rt.to_string();
    }
    let expires_in = json_field(&resp, &["expires_in"]).and_then(Value::as_f64);
    cred.expires_at = expires_in.map(|s| now_sec() + s as i64);
    Ok(())
}

fn fetch_claude(path: &Path) -> Result<QuotaResult, String> {
    let mut cred = read_claude_cred(path)?;
    let agent = http_agent();
    let headers = |token: &str| -> [(&'static str, String); 3] {
        [
            ("authorization", format!("Bearer {token}")),
            ("anthropic-beta", "oauth-2025-04-20".to_string()),
            (
                "user-agent",
                "claude-cli/1.0.90 (external, cli)".to_string(),
            ),
        ]
    };

    let mut usage: Result<Value, String>;
    let mut refreshed = false;
    loop {
        let h = headers(&cred.access_token);
        let refs: Vec<(&str, &str)> = h.iter().map(|(k, v)| (*k, v.as_str())).collect();
        usage = get_json(&agent, "https://api.anthropic.com/api/oauth/usage", &refs);
        match &usage {
            Err(e) if e == "HTTP 401" && !refreshed && !cred.refresh_token.is_empty() => {
                refresh_claude(&mut cred)?;
                write_back_claude(path, &cred);
                refreshed = true;
                continue;
            }
            _ => break,
        }
    }
    let usage = usage?;

    // 套餐信息失败不影响主结果
    let plan = {
        let h = headers(&cred.access_token);
        let refs: Vec<(&str, &str)> = h.iter().map(|(k, v)| (*k, v.as_str())).collect();
        get_json(&agent, "https://api.anthropic.com/api/oauth/profile", &refs)
            .ok()
            .and_then(|p| claude_plan(&p))
    };

    Ok(parse_claude_usage(&usage, plan))
}

/// macOS Keychain 版 Claude Code 查询：从 `Claude Code-credentials` 读取 JSON，
/// 刷新后写回 Keychain。
fn fetch_claude_keychain() -> Result<QuotaResult, String> {
    #[cfg(target_os = "macos")]
    {
        let raw = keychain_read("Claude Code-credentials")
            .ok_or_else(|| "Claude Code-credentials not found in Keychain".to_string())?;
        let v: Value =
            serde_json::from_str(&raw).map_err(|e| format!("Keychain JSON parse error: {e}"))?;
        let oauth = json_field(&v, &["claudeAiOauth"])
            .ok_or_else(|| "Keychain entry missing claudeAiOauth".to_string())?;
        let mut cred = ClaudeCred {
            access_token: json_field(oauth, &["accessToken"])
                .and_then(Value::as_str)
                .ok_or_else(|| "missing accessToken".to_string())?
                .to_string(),
            refresh_token: json_field(oauth, &["refreshToken"])
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
            expires_at: json_field(oauth, &["expiresAt"]).and_then(parse_when),
        };

        let agent = http_agent();
        let headers = |token: &str| -> [(&'static str, String); 3] {
            [
                ("authorization", format!("Bearer {token}")),
                ("anthropic-beta", "oauth-2025-04-20".to_string()),
                (
                    "user-agent",
                    "claude-cli/1.0.90 (external, cli)".to_string(),
                ),
            ]
        };

        let mut usage: Result<Value, String>;
        let mut refreshed = false;
        loop {
            let h = headers(&cred.access_token);
            let refs: Vec<(&str, &str)> = h.iter().map(|(k, v)| (*k, v.as_str())).collect();
            usage = get_json(&agent, "https://api.anthropic.com/api/oauth/usage", &refs);
            match &usage {
                Err(e) if e == "HTTP 401" && !refreshed && !cred.refresh_token.is_empty() => {
                    refresh_claude(&mut cred)?;
                    // 写回 Keychain
                    if let Some(oauth) = v.pointer("/claudeAiOauth").and_then(|o| o.as_object()) {
                        let mut obj = oauth.clone();
                        obj.insert(
                            "accessToken".into(),
                            Value::String(cred.access_token.clone()),
                        );
                        obj.insert(
                            "refreshToken".into(),
                            Value::String(cred.refresh_token.clone()),
                        );
                        if let Some(ts) = cred.expires_at {
                            obj.insert("expiresAt".into(), Value::from(ts * 1000));
                        }
                        let mut full = v.clone();
                        if let Some(target) = full.pointer_mut("/claudeAiOauth") {
                            *target = Value::Object(obj);
                        }
                        if let Ok(out) = serde_json::to_string(&full) {
                            keychain_write("Claude Code-credentials", "Claude Code", &out);
                        }
                    }
                    refreshed = true;
                    continue;
                }
                _ => break,
            }
        }
        let usage = usage?;

        let plan = {
            let h = headers(&cred.access_token);
            let refs: Vec<(&str, &str)> = h.iter().map(|(k, v)| (*k, v.as_str())).collect();
            get_json(&agent, "https://api.anthropic.com/api/oauth/profile", &refs)
                .ok()
                .and_then(|p| claude_plan(&p))
        };

        Ok(parse_claude_usage(&usage, plan))
    }
    #[cfg(not(target_os = "macos"))]
    {
        Err("Claude Keychain is only supported on macOS".into())
    }
}

fn claude_plan(profile: &Value) -> Option<String> {
    let has_max = json_field(profile, &["account", "has_claude_max"])
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if has_max {
        return Some("Max".into());
    }
    let has_pro = json_field(profile, &["account", "has_claude_pro"])
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if has_pro {
        return Some("Pro".into());
    }
    let org_type = json_field(profile, &["organization", "organization_type"])
        .and_then(Value::as_str)
        .unwrap_or_default();
    let status = json_field(profile, &["organization", "subscription_status"])
        .and_then(Value::as_str)
        .unwrap_or_default();
    if org_type.eq_ignore_ascii_case("claude_team") && status.eq_ignore_ascii_case("active") {
        return Some("Team".into());
    }
    None
}

pub fn parse_claude_usage(usage: &Value, plan: Option<String>) -> QuotaResult {
    let keys: &[(&str, WindowLabel)] = &[
        ("five_hour", WindowLabel::FiveHour),
        ("seven_day", WindowLabel::SevenDay),
        ("seven_day_oauth_apps", WindowLabel::SevenDayOauthApps),
        ("seven_day_opus", WindowLabel::SevenDayOpus),
        ("seven_day_sonnet", WindowLabel::SevenDaySonnet),
    ];
    let mut windows = Vec::new();
    for (key, label) in keys {
        let Some(w) = usage.get(*key) else { continue };
        let Some(pct) = json_field(w, &["utilization"]).and_then(Value::as_f64) else {
            continue;
        };
        let resets_at = json_field(w, &["resets_at"])
            .and_then(parse_when)
            .or_else(|| {
                // 当 resets_at 为 null 时，按窗口类型估算下一次重置时间
                match label {
                    WindowLabel::FiveHour => Some(now_sec() + 5 * 3600),
                    WindowLabel::SevenDay
                    | WindowLabel::SevenDayOauthApps
                    | WindowLabel::SevenDayOpus
                    | WindowLabel::SevenDaySonnet => Some(now_sec() + 7 * 86400),
                    _ => None,
                }
            });
        windows.push(QuotaWindow {
            label: label.clone(),
            used_percent: pct,
            resets_at,
        });
    }

    let extra = json_field(usage, &["extra_usage"]).and_then(|e| {
        let enabled = json_field(e, &["is_enabled"])
            .and_then(Value::as_bool)
            .unwrap_or(false);
        if !enabled {
            return None;
        }
        let used = json_field(e, &["used_credits"]).and_then(Value::as_f64);
        let limit = json_field(e, &["monthly_limit"]).and_then(Value::as_f64);
        match (used, limit) {
            (Some(u), Some(l)) => Some(format!("${u:.2} / ${l:.2}")),
            (Some(u), None) => Some(format!("${u:.2}")),
            _ => None,
        }
    });

    QuotaResult {
        plan,
        windows,
        extra,
    }
}

// ---------------------------------------------------------------------------
// Codex（ChatGPT 订阅）
// ---------------------------------------------------------------------------

const CODEX_CLIENT_ID: &str = "app_EMoamEEZ73f0CkXaXp7hrann";
const CODEX_UA: &str =
    "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)";

struct CodexCred {
    access_token: String,
    refresh_token: String,
    id_token: Option<String>,
    expires_at: Option<i64>,
}

fn read_codex_cred(path: &Path) -> Result<CodexCred, String> {
    let text = std::fs::read_to_string(path).map_err(|e| e.to_string())?;
    let v: Value = serde_json::from_str(&text).map_err(|e| e.to_string())?;
    let access = v
        .get("access_token")
        .and_then(Value::as_str)
        .ok_or_else(|| "API-key mode; run `codex login` to use a ChatGPT subscription".to_string())?
        .to_string();
    let refresh_token = v
        .get("refresh_token")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();
    let id_token = v
        .get("id_token")
        .and_then(Value::as_str)
        .map(str::to_string);
    let expires_at = v.get("expired").and_then(parse_when);
    Ok(CodexCred {
        access_token: access,
        refresh_token,
        id_token,
        expires_at,
    })
}

fn write_back_codex(path: &Path, cred: &CodexCred) {
    let Ok(text) = std::fs::read_to_string(path) else {
        return;
    };
    let Ok(mut v) = serde_json::from_str::<Value>(&text) else {
        return;
    };
    if let Some(obj) = v.as_object_mut() {
        obj.insert(
            "access_token".into(),
            Value::String(cred.access_token.clone()),
        );
        obj.insert(
            "refresh_token".into(),
            Value::String(cred.refresh_token.clone()),
        );
        if let Some(id) = &cred.id_token {
            obj.insert("id_token".into(), Value::String(id.clone()));
        }
        if let Some(ts) = cred.expires_at {
            let rfc = chrono::Utc
                .timestamp_opt(ts, 0)
                .single()
                .map(|t| t.to_rfc3339())
                .unwrap_or_default();
            obj.insert("expired".into(), Value::String(rfc));
            obj.insert(
                "last_refresh".into(),
                Value::String(chrono::Utc::now().to_rfc3339()),
            );
        }
    }
    let Ok(out) = serde_json::to_string_pretty(&v) else {
        return;
    };
    atomic_write(path, out.as_bytes());
}

fn refresh_codex(cred: &mut CodexCred) -> Result<(), String> {
    let agent = http_agent();
    let form = format!(
        "client_id={CODEX_CLIENT_ID}&grant_type=refresh_token&refresh_token={}&scope=openid+profile+email",
        urlencoded(&cred.refresh_token)
    );
    let resp = post_form(&agent, "https://auth.openai.com/oauth/token", &form, &[])?;
    cred.access_token = json_field(&resp, &["access_token"])
        .and_then(Value::as_str)
        .ok_or_else(|| "refresh response missing access_token".to_string())?
        .to_string();
    if let Some(rt) = json_field(&resp, &["refresh_token"]).and_then(Value::as_str) {
        cred.refresh_token = rt.to_string();
    }
    if let Some(id) = json_field(&resp, &["id_token"]).and_then(Value::as_str) {
        cred.id_token = Some(id.to_string());
    }
    let expires_in = json_field(&resp, &["expires_in"]).and_then(Value::as_f64);
    cred.expires_at = expires_in.map(|s| now_sec() + s as i64);
    Ok(())
}

fn fetch_codex(path: &Path) -> Result<QuotaResult, String> {
    // API-key mode：auth.json 没有 access_token，无法查询订阅配额。
    // 直接返回一个标识性的结果，不当作错误。
    let mut cred = match read_codex_cred(path) {
        Ok(c) => c,
        Err(_) => {
            return Ok(QuotaResult {
                plan: Some("API MODE".into()),
                windows: Vec::new(),
                extra: None,
            });
        }
    };
    let agent = http_agent();

    let mut usage: Result<Value, String>;
    let mut refreshed = false;
    loop {
        let headers: [(&str, &str); 2] = [
            ("authorization", &*format!("Bearer {}", cred.access_token)),
            ("user-agent", CODEX_UA),
        ];
        usage = get_json(
            &agent,
            "https://chatgpt.com/backend-api/wham/usage",
            &headers,
        );
        match &usage {
            Err(e) if e == "HTTP 401" && !refreshed && !cred.refresh_token.is_empty() => {
                refresh_codex(&mut cred)?;
                write_back_codex(path, &cred);
                refreshed = true;
                continue;
            }
            _ => break,
        }
    }
    let usage = usage?;
    let plan = usage
        .get("plan_type")
        .and_then(Value::as_str)
        .map(str::to_string);
    Ok(parse_codex_usage(&usage, plan))
}

pub fn parse_codex_usage(usage: &Value, plan: Option<String>) -> QuotaResult {
    let mut windows = Vec::new();

    if let Some(rl) = usage.get("rate_limit") {
        for key in ["primary_window", "secondary_window"] {
            if let Some(w) = codex_window(rl.get(key)) {
                windows.push(w);
            }
        }
    }
    if let Some(additional) = usage
        .get("additional_rate_limits")
        .and_then(Value::as_array)
    {
        for item in additional {
            let name = item
                .get("limit_name")
                .and_then(Value::as_str)
                .unwrap_or("limit")
                .to_string();
            if let Some(mut w) = codex_window(item.get("rate_limit")) {
                w.label = WindowLabel::Custom(name);
                windows.push(w);
            }
        }
    }

    QuotaResult {
        plan,
        windows,
        extra: None,
    }
}

fn codex_window(v: Option<&Value>) -> Option<QuotaWindow> {
    let v = v?;
    let pct = v.get("used_percent").and_then(Value::as_f64)?;
    let secs = v.get("limit_window_seconds").and_then(Value::as_f64);
    let label = match secs {
        Some(s) if (16000.0..20000.0).contains(&s) => WindowLabel::FiveHour,
        Some(s) if (600000.0..610000.0).contains(&s) => WindowLabel::SevenDay,
        Some(s) if (2400000.0..2800000.0).contains(&s) => WindowLabel::Monthly,
        _ => WindowLabel::Weekly,
    };
    let resets_at = v.get("reset_at").and_then(parse_when).or_else(|| {
        let after = v.get("reset_after_seconds").and_then(Value::as_f64)?;
        Some(now_sec() + after as i64)
    });
    Some(QuotaWindow {
        label,
        used_percent: pct,
        resets_at,
    })
}

// ---------------------------------------------------------------------------
// Grok（grok CLI OAuth → cli-chat-proxy 计费）
// ---------------------------------------------------------------------------

const GROK_HEADERS_BASE: &[(&str, &str)] = &[
    ("x-xai-token-auth", "xai-grok-cli"),
    ("x-grok-client-version", "0.2.91"),
    ("accept", "*/*"),
    (
        "user-agent",
        "grok-pager/0.2.91 grok-shell/0.2.91 (windows; x86_64)",
    ),
];

struct GrokCred {
    /// auth.json 顶层键名（如 "https://auth.x.ai::<client_id>"）
    entry_key: String,
    access_token: String,
    refresh_token: String,
    client_id: String,
    issuer: String,
    /// unix 秒
    expires_at: Option<i64>,
}

fn read_grok_cred(path: &Path) -> Result<GrokCred, String> {
    let text = std::fs::read_to_string(path).map_err(|e| e.to_string())?;
    let v: Value = serde_json::from_str(&text).map_err(|e| e.to_string())?;
    let obj = v
        .as_object()
        .ok_or_else(|| "invalid auth.json".to_string())?;
    let (entry_key, entry) = obj
        .iter()
        .find(|(_, val)| val.is_object() && val.get("refresh_token").is_some())
        .ok_or_else(|| "no OAuth entry in ~/.grok/auth.json".to_string())?;
    let access_token = entry
        .get("key")
        .and_then(Value::as_str)
        .ok_or_else(|| "missing key".to_string())?
        .to_string();
    let refresh_token = entry
        .get("refresh_token")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();
    let client_id = entry
        .get("oidc_client_id")
        .and_then(Value::as_str)
        .unwrap_or("b1a00492-073a-47ea-816f-4c329264a828")
        .to_string();
    let issuer = entry
        .get("oidc_issuer")
        .and_then(Value::as_str)
        .unwrap_or("https://auth.x.ai")
        .trim_end_matches('/')
        .to_string();
    let expires_at = entry.get("expires_at").and_then(parse_when);
    Ok(GrokCred {
        entry_key: entry_key.clone(),
        access_token,
        refresh_token,
        client_id,
        issuer,
        expires_at,
    })
}

fn write_back_grok(path: &Path, cred: &GrokCred) {
    let Ok(text) = std::fs::read_to_string(path) else {
        return;
    };
    let Ok(mut v) = serde_json::from_str::<Value>(&text) else {
        return;
    };
    if let Some(entry) = v.get_mut(&cred.entry_key).and_then(Value::as_object_mut) {
        // 保持与读取时一致的时间单位（原值 > 1e12 视为毫秒）
        let unit_ms = entry
            .get("expires_at")
            .and_then(Value::as_f64)
            .is_some_and(|n| n > 1e12);
        entry.insert("key".into(), Value::String(cred.access_token.clone()));
        entry.insert(
            "refresh_token".into(),
            Value::String(cred.refresh_token.clone()),
        );
        if let Some(ts) = cred.expires_at {
            let stored = if unit_ms { ts * 1000 } else { ts };
            entry.insert("expires_at".into(), Value::from(stored));
        }
    }
    let Ok(out) = serde_json::to_string_pretty(&v) else {
        return;
    };
    atomic_write(path, out.as_bytes());
}

fn refresh_grok(cred: &mut GrokCred) -> Result<(), String> {
    let agent = http_agent();
    // OIDC discovery 取 token_endpoint；失败则退回默认路径
    let token_endpoint = get_json(
        &agent,
        &format!("{}/.well-known/openid-configuration", cred.issuer),
        &[],
    )
    .ok()
    .and_then(|d| {
        d.get("token_endpoint")
            .and_then(Value::as_str)
            .map(str::to_string)
    })
    .filter(|u| u.starts_with("https://") && u.contains("x.ai"))
    .unwrap_or_else(|| format!("{}/oauth2/token", cred.issuer));

    let form = format!(
        "grant_type=refresh_token&client_id={}&refresh_token={}",
        urlencoded(&cred.client_id),
        urlencoded(&cred.refresh_token)
    );
    let resp = post_form(&agent, &token_endpoint, &form, &[])?;
    cred.access_token = json_field(&resp, &["access_token"])
        .and_then(Value::as_str)
        .ok_or_else(|| "refresh response missing access_token".to_string())?
        .to_string();
    if let Some(rt) = json_field(&resp, &["refresh_token"]).and_then(Value::as_str) {
        cred.refresh_token = rt.to_string();
    }
    let expires_in = json_field(&resp, &["expires_in"]).and_then(Value::as_f64);
    cred.expires_at = expires_in.map(|s| now_sec() + s as i64);
    Ok(())
}

fn fetch_grok(path: &Path) -> Result<QuotaResult, String> {
    let mut cred = read_grok_cred(path)?;
    let agent = http_agent();

    let mut body: Result<Value, String>;
    let mut refreshed = false;
    loop {
        let bearer = format!("Bearer {}", cred.access_token);
        let mut headers: Vec<(&str, &str)> = vec![("authorization", &bearer)];
        headers.extend_from_slice(GROK_HEADERS_BASE);
        body = get_json(
            &agent,
            "https://cli-chat-proxy.grok.com/v1/billing?format=credits",
            &headers,
        );
        match &body {
            Err(e) if e == "HTTP 401" && !refreshed && !cred.refresh_token.is_empty() => {
                refresh_grok(&mut cred)?;
                write_back_grok(path, &cred);
                refreshed = true;
                continue;
            }
            _ => break,
        }
    }
    // Grok 未使用过时 billing API 可能返回错误，视为 100% 剩余（0% 已用）
    let weekly = match body {
        Ok(v) => v,
        Err(_) => {
            return Ok(QuotaResult {
                plan: None,
                windows: vec![QuotaWindow {
                    label: WindowLabel::Weekly,
                    used_percent: 0.0,
                    resets_at: None,
                }],
                extra: None,
            });
        }
    };

    let bearer = format!("Bearer {}", cred.access_token);
    let mut headers: Vec<(&str, &str)> = vec![("authorization", &bearer)];
    headers.extend_from_slice(GROK_HEADERS_BASE);
    let monthly = get_json(
        &agent,
        "https://cli-chat-proxy.grok.com/v1/billing",
        &headers,
    )
    .ok();

    Ok(parse_grok_billing(&weekly, monthly.as_ref()))
}

pub fn parse_grok_billing(weekly: &Value, monthly: Option<&Value>) -> QuotaResult {
    let mut windows = Vec::new();
    let mut plan: Option<String> = None;

    let config = weekly.get("config");
    if let Some(cfg) = config {
        let pct = json_field(cfg, &["creditUsagePercent"])
            .or_else(|| json_field(cfg, &["credit_usage_percent"]))
            .and_then(Value::as_f64);
        let period =
            json_field(cfg, &["currentPeriod"]).or_else(|| json_field(cfg, &["current_period"]));
        let raw_type = period
            .and_then(|p| p.get("type"))
            .and_then(Value::as_str)
            .unwrap_or("weekly");
        let period_type = raw_type
            .strip_prefix("USAGE_PERIOD_TYPE_")
            .unwrap_or(raw_type)
            .to_lowercase();
        let resets_at = period.and_then(|p| p.get("end")).and_then(parse_when);
        if let Some(pct) = pct {
            let label = match period_type.as_str() {
                "monthly" => WindowLabel::Monthly,
                _ => WindowLabel::Weekly,
            };
            windows.push(QuotaWindow {
                label,
                used_percent: pct,
                resets_at,
            });
            plan = Some(period_type.to_string());
        }
    }

    if let Some(m) = monthly.and_then(|v| v.get("config")) {
        let limit = cents_value(m.get("monthlyLimit").or_else(|| m.get("monthly_limit")));
        let used = cents_value(m.get("used"));
        let resets_at = m
            .get("billingPeriodEnd")
            .or_else(|| m.get("billing_period_end"))
            .and_then(parse_when);
        if let (Some(limit), Some(used)) = (limit, used) {
            if limit > 0.0 {
                let pct = (used / limit * 100.0).min(999.0);
                windows.push(QuotaWindow {
                    label: WindowLabel::Monthly,
                    used_percent: pct,
                    resets_at,
                });
            }
        }
    }

    QuotaResult {
        plan,
        windows,
        extra: None,
    }
}

fn cents_value(v: Option<&Value>) -> Option<f64> {
    match v? {
        Value::Object(_) => v
            .and_then(|o| o.get("val"))
            .and_then(Value::as_f64)
            .map(|c| c / 100.0),
        other => other.as_f64(),
    }
}

// ---------------------------------------------------------------------------
// Devin（浏览器 session cookie）
// ---------------------------------------------------------------------------

fn fetch_devin(cookie: &str, org_id: Option<&str>) -> Result<QuotaResult, String> {
    let agent = http_agent();
    let org = match org_id {
        Some(id) => id.to_string(),
        None => discover_devin_org(&agent, cookie)?,
    };
    let headers: [(&str, &str); 2] = [("cookie", cookie), ("accept", "application/json")];

    let quota = get_json(
        &agent,
        &format!("https://app.devin.ai/api/{org}/billing/quota/usage"),
        &headers,
    )?;
    let plan = get_json(
        &agent,
        &format!("https://app.devin.ai/api/{org}/billing/status"),
        &headers,
    )
    .ok()
    .and_then(|s| {
        s.get("plan_slug")
            .and_then(Value::as_str)
            .map(str::to_string)
    });

    Ok(parse_devin_quota(&quota, plan))
}

/// 递归找第一个形如 "org-xxxx" 的字符串（/api/organizations 响应结构未公开，防御式处理）。
fn find_org_id(v: &Value) -> Option<String> {
    match v {
        Value::String(s) if s.starts_with("org-") => Some(s.clone()),
        Value::Array(items) => items.iter().find_map(find_org_id),
        Value::Object(map) => map.values().find_map(find_org_id),
        _ => None,
    }
}

pub fn discover_devin_org(agent: &ureq::Agent, cookie: &str) -> Result<String, String> {
    let headers: [(&str, &str); 2] = [("cookie", cookie), ("accept", "application/json")];
    let v = get_json(agent, "https://app.devin.ai/api/organizations", &headers)?;
    find_org_id(&v).ok_or_else(|| "no org found in /api/organizations".to_string())
}

/// 粘贴 cookie 后的连通性验证，成功返回 (展示名, org_id)。
pub fn validate_devin_cookie(cookie: &str) -> Result<(String, String), String> {
    let agent = http_agent();
    let headers: [(&str, &str); 2] = [("cookie", cookie), ("accept", "application/json")];
    let info = get_json(&agent, "https://app.devin.ai/api/users/info", &headers)?;
    let org = discover_devin_org(&agent, cookie)?;
    let label = json_field(&info, &["email"])
        .or_else(|| json_field(&info, &["username"]))
        .and_then(Value::as_str)
        .map(str::to_string)
        .unwrap_or_else(|| "Devin".into());
    Ok((label, org))
}

pub fn parse_devin_quota(quota: &Value, plan: Option<String>) -> QuotaResult {
    let mut windows = Vec::new();
    let hide_daily = quota
        .get("hide_daily_quota")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    if let (Some(pct), false) = (
        quota.get("daily_percentage").and_then(Value::as_f64),
        hide_daily,
    ) {
        windows.push(QuotaWindow {
            label: WindowLabel::Daily,
            used_percent: 100.0 - pct,
            resets_at: quota.get("daily_reset_at").and_then(parse_when),
        });
    }
    if let Some(pct) = quota.get("weekly_percentage").and_then(Value::as_f64) {
        windows.push(QuotaWindow {
            label: WindowLabel::Weekly,
            used_percent: 100.0 - pct,
            resets_at: quota.get("weekly_reset_at").and_then(parse_when),
        });
    }
    let extra = quota
        .get("overage_balance")
        .and_then(Value::as_f64)
        .filter(|b| *b > 0.0)
        .map(|b| format!("{b:.2} ACU"));
    QuotaResult {
        plan,
        windows,
        extra,
    }
}

// ---------------------------------------------------------------------------
// Devin CLI（本地 session token 自动发现 + GetUserStatus API 调用）
// ---------------------------------------------------------------------------

/// 从 credentials.toml 读取简单 key="value" 行。避免引入 toml 依赖。
fn parse_simple_toml(text: &str, key: &str) -> Option<String> {
    let prefix = key;
    for line in text.lines() {
        let line = line.trim();
        if let Some(rest) = line.strip_prefix(prefix) {
            let rest = rest.trim_start();
            if let Some(rest) = rest.strip_prefix('=') {
                let rest = rest.trim();
                if rest.starts_with('"') && rest.ends_with('"') && rest.len() >= 2 {
                    return Some(rest[1..rest.len() - 1].to_string());
                }
            }
        }
    }
    None
}

/// 读取 Devin CLI 本地凭证：credentials.toml 中的 session token + config.json 中的 org_id。
///
/// Devin CLI 在所有平台上都用 XDG 路径：
/// - credentials.toml → `~/.local/share/devin/credentials.toml`
/// - config.json → `~/.config/devin/config.json`
///
/// 两者都不走 `dirs::config_dir()`（macOS 上是 `~/Library/Application Support/`）。
fn read_devin_cli_credentials() -> Option<(String, String)> {
    let home = dirs::home_dir()?;

    // credentials.toml：优先 XDG data dir，回退 config_dir
    let cred_path = home.join(".local/share/devin/credentials.toml");
    let cred_text = std::fs::read_to_string(&cred_path)
        .or_else(|_| {
            dirs::config_dir()
                .map(|d| std::fs::read_to_string(d.join("devin/credentials.toml")))
                .unwrap_or(Err(std::io::Error::other("no config dir")))
        })
        .ok()?;
    let token = parse_simple_toml(&cred_text, "windsurf_api_key")?;

    // config.json：优先 XDG config dir，回退 config_dir
    let config_path = home.join(".config/devin/config.json");
    let config_text = std::fs::read_to_string(&config_path)
        .or_else(|_| {
            dirs::config_dir()
                .map(|d| std::fs::read_to_string(d.join("devin/config.json")))
                .unwrap_or(Err(std::io::Error::other("no config dir")))
        })
        .ok()?;
    let config: Value = serde_json::from_str(&config_text).ok()?;
    let org_id = json_field(&config, &["devin", "org_id"])
        .and_then(Value::as_str)
        .map(str::to_string)?;

    Some((token, org_id))
}

/// 最小 protobuf wire-format 解析与编码。
/// 仅支持本项目需要的子集（varint + length-delimited），无 schema 依赖。
mod pb {
    /// 读取 varint，返回 (值, 新位置)。
    fn read_varint(data: &[u8], pos: usize) -> Option<(u64, usize)> {
        let mut result: u64 = 0;
        let mut shift = 0;
        let mut p = pos;
        loop {
            if p >= data.len() {
                return None;
            }
            let b = data[p];
            result |= (b as u64 & 0x7f) << shift;
            p += 1;
            if b & 0x80 == 0 {
                break;
            }
            shift += 7;
            if shift > 63 {
                return None;
            }
        }
        Some((result, p))
    }

    /// 编码 varint。
    fn write_varint(val: u64, out: &mut Vec<u8>) {
        let mut v = val;
        loop {
            let b = (v & 0x7f) as u8;
            v >>= 7;
            if v != 0 {
                out.push(b | 0x80);
            } else {
                out.push(b);
            }
            if v == 0 {
                break;
            }
        }
    }

    /// 编码一个 string 字段 (wire type 2)。
    pub fn write_string_field(out: &mut Vec<u8>, field_num: u32, s: &str) {
        write_varint((field_num as u64) << 3 | 2, out);
        write_varint(s.len() as u64, out);
        out.extend_from_slice(s.as_bytes());
    }

    /// 编码一个嵌套 message 字段 (wire type 2)。
    pub fn write_message_field(out: &mut Vec<u8>, field_num: u32, msg: &[u8]) {
        write_varint((field_num as u64) << 3 | 2, out);
        write_varint(msg.len() as u64, out);
        out.extend_from_slice(msg);
    }

    /// 在 protobuf 消息中找第一个指定 field number 的 length-delimited 子消息。
    pub fn find_submessage(data: &[u8], target_field: u32) -> Option<&[u8]> {
        let mut pos = 0;
        while pos < data.len() {
            let Some((tag, p)) = read_varint(data, pos) else {
                break;
            };
            pos = p;
            let field_num = (tag >> 3) as u32;
            let wire_type = (tag & 0x7) as u8;
            match wire_type {
                0 => {
                    let Some((_, p)) = read_varint(data, pos) else {
                        break;
                    };
                    pos = p;
                }
                2 => {
                    let Some((len, p)) = read_varint(data, pos) else {
                        break;
                    };
                    pos = p;
                    if pos + len as usize > data.len() {
                        break;
                    }
                    if field_num == target_field {
                        return Some(&data[pos..pos + len as usize]);
                    }
                    pos += len as usize;
                }
                1 => pos += 8,
                5 => pos += 4,
                _ => break,
            }
        }
        None
    }

    /// 在 protobuf 消息中找第一个指定 field number 的 varint 值。
    pub fn find_varint(data: &[u8], target_field: u32) -> Option<u64> {
        let mut pos = 0;
        while pos < data.len() {
            let Some((tag, p)) = read_varint(data, pos) else {
                break;
            };
            pos = p;
            let field_num = (tag >> 3) as u32;
            let wire_type = (tag & 0x7) as u8;
            match wire_type {
                0 => {
                    let Some((val, p)) = read_varint(data, pos) else {
                        break;
                    };
                    pos = p;
                    if field_num == target_field {
                        return Some(val);
                    }
                }
                2 => {
                    let Some((len, p)) = read_varint(data, pos) else {
                        break;
                    };
                    pos = p;
                    if pos + len as usize > data.len() {
                        break;
                    }
                    pos += len as usize;
                }
                1 => pos += 8,
                5 => pos += 4,
                _ => break,
            }
        }
        None
    }

    /// 在 protobuf 消息中找第一个指定 field number 的字符串。
    pub fn find_string(data: &[u8], target_field: u32) -> Option<String> {
        let raw = find_submessage(data, target_field)?;
        std::str::from_utf8(raw).ok().map(|s| s.to_string())
    }
}

/// 调用 Devin CLI 的 GetUserStatus API 获取实时配额数据。
///
/// API 端点：`server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus`
/// 协议：Connect-RPC (protobuf over HTTP POST)
/// 认证：`Authorization: Basic <token>-<token>`（token 重复一次，用 dash 分隔）
///
/// 请求体（protobuf，逆向自 Devin CLI 3000.6.11 抓包）：
/// ```text
/// F1 (msg): Metadata
///   F1 (str): client_name = "chisel"   // 必须为 "chisel"，其他值返回 500
///   F2 (str): client_version           // 任意版本字符串
///   F3 (str): auth_token               // "devin-session-token$<JWT>"
///   F7 (str): version                  // 必须存在，否则返回 400
/// ```
///
/// 响应体（protobuf）：
/// ```text
/// F1 (msg): UserStatus
///   F13 (msg): QuotaInfo
///     F1 (msg): PlanInfo { F2 (str): plan_name }
///     F14 (varint): daily_percentage
///     F15 (varint): weekly_percentage
///     F16 (varint): overage_credits × 1,000,000
///     F17 (varint): daily_reset_at (unix 秒)
///     F18 (varint): weekly_reset_at (unix 秒)
/// ```
fn fetch_devin_cli(token: &str, _org_id: &str) -> Result<QuotaResult, String> {
    // 构造 protobuf 请求体
    let mut meta = Vec::new();
    pb::write_string_field(&mut meta, 1, "chisel");
    pb::write_string_field(&mut meta, 2, "3000.6.11");
    pb::write_string_field(&mut meta, 3, token);
    pb::write_string_field(&mut meta, 7, "3000.6.11");
    let mut body = Vec::new();
    pb::write_message_field(&mut body, 1, &meta);

    // 发送请求
    let agent = http_agent();
    let auth = format!("Basic {token}-{token}");
    let url =
        "https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus";
    let resp = agent
        .post(url)
        .header("authorization", &auth)
        .header("content-type", "application/proto")
        .header("connect-protocol-version", "1")
        .header("accept", "*/*")
        .send(&body)
        .map_err(http_err)?;

    // 读取二进制响应体
    let resp_bytes: Vec<u8> = {
        let mut r = resp.into_body();
        r.read_to_vec().map_err(|e: ureq::Error| e.to_string())?
    };

    // 解析响应：F1 (UserStatus) -> F13 (QuotaInfo)
    let f1 = pb::find_submessage(&resp_bytes, 1)
        .ok_or_else(|| "GetUserStatus response missing F1".to_string())?;
    let f13 = pb::find_submessage(f1, 13)
        .ok_or_else(|| "GetUserStatus response missing F13 (quota)".to_string())?;

    // Devin API 返回的是剩余百分比，需转换为已用百分比
    let daily_remaining = pb::find_varint(f13, 14).map(|v| v as f64);
    let weekly_remaining = pb::find_varint(f13, 15).map(|v| v as f64);
    let overage_raw = pb::find_varint(f13, 16);
    let daily_reset = pb::find_varint(f13, 17).map(|v| v as i64);
    let weekly_reset = pb::find_varint(f13, 18).map(|v| v as i64);

    // F13.1 (PlanInfo) -> F2 = plan name
    let plan = pb::find_submessage(f13, 1)
        .and_then(|m| pb::find_string(m, 2))
        .map(|s| s.to_lowercase());

    let mut windows = Vec::new();
    if let Some(remaining) = daily_remaining {
        windows.push(QuotaWindow {
            label: WindowLabel::Daily,
            used_percent: 100.0 - remaining,
            resets_at: daily_reset,
        });
    }
    if let Some(remaining) = weekly_remaining {
        windows.push(QuotaWindow {
            label: WindowLabel::Weekly,
            used_percent: 100.0 - remaining,
            resets_at: weekly_reset,
        });
    }

    let extra = overage_raw
        .map(|v| v as f64 / 1_000_000.0)
        .filter(|b| *b > 0.0)
        .map(|b| format!("{b:.2} ACU"));

    if plan.is_none() && windows.is_empty() && extra.is_none() {
        return Err("GetUserStatus returned no quota fields".into());
    }

    Ok(QuotaResult {
        plan,
        windows,
        extra,
    })
}

// ---------------------------------------------------------------------------
// Antigravity (Google Gemini Code Assist)
// ---------------------------------------------------------------------------

/// Antigravity 配额查询端点。
const ANTIGRAVITY_QUOTA_URL: &str =
    "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary";

/// 从 Antigravity 的 language_server 二进制中提取 Google OAuth client_id 和 client_secret。
/// 这些值是 Antigravity 内置的公开 OAuth 凭据，不是用户密钥。
/// 返回 (client_id, client_secret)。
fn antigravity_oauth_client() -> Option<(String, String)> {
    // 优先用环境变量覆盖
    if let (Ok(id), Ok(secret)) = (
        std::env::var("ANTIGRAVITY_CLIENT_ID"),
        std::env::var("ANTIGRAVITY_CLIENT_SECRET"),
    ) {
        if !id.is_empty() && !secret.is_empty() {
            return Some((id, secret));
        }
    }

    #[cfg(target_os = "windows")]
    {
        let path = PathBuf::from(std::env::var_os("LOCALAPPDATA")?)
            .join("Programs/Antigravity/resources/bin/language_server.exe");
        let binary = std::fs::read(path).ok()?;
        antigravity_windows_client(&binary)
    }

    // 从 language_server 二进制中 strings 提取
    #[cfg(target_os = "macos")]
    {
        let path =
            Path::new("/Applications/Antigravity.app/Contents/Resources/bin/language_server");
        let output = std::process::Command::new("strings")
            .arg(path)
            .output()
            .ok()?;
        let text = String::from_utf8_lossy(&output.stdout);
        // client_id 格式: <digits>-<alphanum>.apps.googleusercontent.com
        let client_id = text
            .lines()
            .find(|l| l.ends_with(".apps.googleusercontent.com") && l.len() > 30)?
            .to_string();
        // client_secret 格式: GOCSPX-<alphanum>
        let client_secret = text
            .lines()
            .find(|l| l.starts_with("GOCSPX-") && l.len() > 15)?
            .to_string();
        Some((client_id, client_secret))
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    {
        None
    }
}

fn antigravity_windows_client(binary: &[u8]) -> Option<(String, String)> {
    fn u16_at(b: &[u8], p: usize) -> Option<u16> {
        Some(u16::from_le_bytes(b.get(p..p + 2)?.try_into().ok()?))
    }
    fn u32_at(b: &[u8], p: usize) -> Option<u32> {
        Some(u32::from_le_bytes(b.get(p..p + 4)?.try_into().ok()?))
    }
    let pe = u32_at(binary, 60)? as usize;
    if binary.get(pe..pe + 4)? != b"PE\0\0" {
        return None;
    }
    let table = pe.checked_add(24 + u16_at(binary, pe + 20)? as usize)?;
    let mut sections = Vec::new();
    for index in 0..u16_at(binary, pe + 6)? as usize {
        let p = table.checked_add(index.checked_mul(40)?)?;
        sections.push((
            u32_at(binary, p + 12)? as usize,
            u32_at(binary, p + 16)? as usize,
            u32_at(binary, p + 20)? as usize,
        ));
    }
    let file_offset = |rva: i64| -> Option<usize> {
        let rva = usize::try_from(rva).ok()?;
        sections.iter().find_map(|&(start, size, raw)| {
            (rva >= start && rva - start < size)
                .then(|| raw.checked_add(rva - start))
                .flatten()
        })
    };
    let mut pairs = Vec::new();
    for &(rva, size, raw) in &sections {
        let bytes = binary.get(raw..raw.checked_add(size)?)?;
        let mut previous_id: Option<(usize, String)> = None;
        for (offset, instruction) in bytes.windows(7).enumerate() {
            if !matches!(instruction[0], 0x48 | 0x4c)
                || instruction[1] != 0x8d
                || instruction[2] & 0xc7 != 5
            {
                continue;
            }
            let displacement = i32::from_le_bytes(instruction[3..7].try_into().ok()?);
            let Some(target) = file_offset((rva + offset + 7) as i64 + i64::from(displacement))
            else {
                continue;
            };
            let Some(text) = binary.get(target..target.saturating_add(100)) else {
                continue;
            };
            if text.starts_with(b"1071006060591-") {
                let suffix = b".apps.googleusercontent.com";
                if let Some(end) = text.windows(suffix.len()).position(|part| part == suffix) {
                    previous_id = Some((
                        offset,
                        String::from_utf8(text[..end + suffix.len()].to_vec()).ok()?,
                    ));
                }
            } else if text.starts_with(b"GOCSPX-") {
                if let Some((id_offset, id)) = &previous_id {
                    // The Go OAuth config constructor references its two strings
                    // together. Do not mix the separate Cloud Auth client pair.
                    if offset - id_offset <= 128 {
                        let secret = std::str::from_utf8(&text[..35]).ok()?;
                        if secret
                            .bytes()
                            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
                        {
                            pairs.push((id.clone(), secret.to_string()));
                        }
                    }
                }
            }
        }
    }
    pairs.sort();
    pairs.dedup();
    if pairs.len() == 1 {
        pairs.pop()
    } else {
        None
    }
}

fn antigravity_state_path() -> Option<PathBuf> {
    #[cfg(target_os = "windows")]
    {
        Some(
            PathBuf::from(std::env::var_os("APPDATA")?)
                .join("Antigravity/User/globalStorage/state.vscdb"),
        )
    }
    #[cfg(not(target_os = "windows"))]
    {
        None
    }
}

fn protobuf_bytes(input: &[u8], wanted: u64) -> Option<Vec<&[u8]>> {
    fn varint(input: &[u8], offset: &mut usize) -> Option<u64> {
        let mut value = 0u64;
        for shift in (0..64).step_by(7) {
            let byte = *input.get(*offset)?;
            *offset += 1;
            if shift == 63 && byte > 1 {
                return None;
            }
            value |= u64::from(byte & 127) << shift;
            if byte < 128 {
                return Some(value);
            }
        }
        None
    }
    let mut result = Vec::new();
    let mut offset = 0;
    while offset < input.len() {
        let tag = varint(input, &mut offset)?;
        if tag >> 3 == 0 {
            return None;
        }
        match tag & 7 {
            0 => {
                varint(input, &mut offset)?;
            }
            1 => {
                offset = offset.checked_add(8)?;
            }
            2 => {
                let len = usize::try_from(varint(input, &mut offset)?).ok()?;
                let end = offset.checked_add(len)?;
                let bytes = input.get(offset..end)?;
                if tag >> 3 == wanted {
                    result.push(bytes);
                }
                offset = end;
            }
            5 => {
                offset = offset.checked_add(4)?;
            }
            _ => return None,
        }
        if offset > input.len() {
            return None;
        }
    }
    Some(result)
}

fn antigravity_state_credentials(raw: &str) -> Option<AntigravityCred> {
    let state = base64_decode(raw)?;
    for entry in protobuf_bytes(&state, 1)? {
        let keys = protobuf_bytes(entry, 1)?;
        if keys.first().copied()? != b"oauthTokenInfoSentinelKey" {
            continue;
        }
        let values = protobuf_bytes(entry, 2)?;
        let wrapped = protobuf_bytes(values.first().copied()?, 1)?;
        let encoded = std::str::from_utf8(wrapped.first().copied()?).ok()?;
        let token_info = base64_decode(encoded)?;
        let tokens = protobuf_bytes(&token_info, 1)?;
        let token = std::str::from_utf8(tokens.first().copied()?).ok()?;
        if !token.is_empty() {
            let refresh = protobuf_bytes(&token_info, 3)?;
            let refresh_token = refresh
                .first()
                .and_then(|bytes| std::str::from_utf8(bytes).ok())
                .unwrap_or_default()
                .to_string();
            return Some(AntigravityCred {
                access_token: token.to_string(),
                refresh_token,
                expires_at: None,
            });
        }
    }
    None
}

fn read_antigravity_state_token(path: &Path) -> Option<AntigravityCred> {
    let connection =
        rusqlite::Connection::open_with_flags(path, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY)
            .ok()?;
    let raw: String = connection
        .query_row(
            "SELECT value FROM ItemTable WHERE key = 'antigravityUnifiedStateSync.oauthToken'",
            [],
            |row| row.get(0),
        )
        .ok()?;
    antigravity_state_credentials(&raw)
}

fn read_antigravity_state_email(path: &Path) -> Option<String> {
    let connection =
        rusqlite::Connection::open_with_flags(path, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY)
            .ok()?;
    let raw: String = connection
        .query_row(
            "SELECT value FROM ItemTable WHERE key = 'antigravityAuthStatus'",
            [],
            |row| row.get(0),
        )
        .ok()?;
    let value: Value = serde_json::from_str(&raw).ok()?;
    value.get("email")?.as_str().map(str::to_string)
}

struct AntigravityCred {
    access_token: String,
    refresh_token: String,
    /// unix 秒；None = 未知/已过期，需刷新
    expires_at: Option<i64>,
}

/// 读取 Antigravity OAuth 凭据。
/// `from_keychain=true` 时只读 Keychain（用户已显式同意）；否则只读文件。
fn read_antigravity_cred(path: &Path, from_keychain: bool) -> Result<AntigravityCred, String> {
    #[cfg(not(target_os = "macos"))]
    let _ = from_keychain;

    #[cfg(target_os = "macos")]
    if from_keychain {
        if let Some(raw) = keychain_read("gemini") {
            let b64 = raw.strip_prefix("go-keyring-base64:").unwrap_or(&raw);
            if let Some(decoded) = base64_decode(b64) {
                if let Ok(text) = String::from_utf8(decoded) {
                    if let Ok(v) = serde_json::from_str::<Value>(&text) {
                        if let Some(token) = v.get("token") {
                            let access = token
                                .get("access_token")
                                .and_then(Value::as_str)
                                .unwrap_or_default()
                                .to_string();
                            let refresh = token
                                .get("refresh_token")
                                .and_then(Value::as_str)
                                .unwrap_or_default()
                                .to_string();
                            if !refresh.is_empty() {
                                return Ok(AntigravityCred {
                                    access_token: access,
                                    refresh_token: refresh,
                                    expires_at: token
                                        .get("expiry")
                                        .and_then(Value::as_str)
                                        .and_then(|s| DateTime::parse_from_rfc3339(s).ok())
                                        .map(|dt| dt.timestamp()),
                                });
                            }
                        }
                    }
                }
            }
        }
        return Err("Antigravity Keychain credentials unavailable".into());
    }

    if path.extension().and_then(|ext| ext.to_str()) == Some("vscdb") {
        return read_antigravity_state_token(path).ok_or_else(|| {
            "Antigravity current login token unavailable; reopen Antigravity and sign in".into()
        });
    }

    // 文件路径
    if path.exists() {
        let text = std::fs::read_to_string(path).map_err(|e| e.to_string())?;
        let v: Value = serde_json::from_str(&text).map_err(|e| e.to_string())?;
        return Ok(AntigravityCred {
            access_token: v
                .get("access_token")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
            refresh_token: v
                .get("refresh_token")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_string(),
            expires_at: v.get("expiry_date").and_then(parse_when),
        });
    }

    Err("Antigravity credentials file not found".into())
}

/// 刷新 Google OAuth access_token。
fn refresh_antigravity(cred: &mut AntigravityCred) -> Result<(), String> {
    let (client_id, client_secret) = antigravity_oauth_client()
        .ok_or_else(|| "Antigravity OAuth client not found".to_string())?;
    let agent = http_agent();
    let form = format!(
        "client_id={}&client_secret={}&refresh_token={}&grant_type=refresh_token",
        urlencoded(&client_id),
        urlencoded(&client_secret),
        urlencoded(&cred.refresh_token)
    );
    let resp = post_form(&agent, "https://oauth2.googleapis.com/token", &form, &[])?;
    cred.access_token = json_field(&resp, &["access_token"])
        .and_then(Value::as_str)
        .ok_or_else(|| "refresh response missing access_token".to_string())?
        .to_string();
    let expires_in = json_field(&resp, &["expires_in"]).and_then(Value::as_f64);
    cred.expires_at = expires_in.map(|s| now_sec() + s as i64);
    Ok(())
}

/// 写回刷新后的 token 到文件（Keychain 版由 Antigravity 自身管理，不写回）。
fn write_back_antigravity(path: &Path, cred: &AntigravityCred) {
    if path.extension().and_then(|ext| ext.to_str()) == Some("vscdb") {
        return;
    }
    if !path.exists() {
        return; // Keychain 模式不写回文件
    }
    let Ok(text) = std::fs::read_to_string(path) else {
        return;
    };
    let Ok(mut v) = serde_json::from_str::<Value>(&text) else {
        return;
    };
    if let Some(obj) = v.as_object_mut() {
        obj.insert(
            "access_token".into(),
            Value::String(cred.access_token.clone()),
        );
        if let Some(ts) = cred.expires_at {
            obj.insert("expiry_date".into(), Value::from(ts * 1000));
        }
    }
    let Ok(out) = serde_json::to_string_pretty(&v) else {
        return;
    };
    atomic_write(path, out.as_bytes());
}

/// 查询 Antigravity (Gemini Code Assist) 配额。
///
/// API 端点：`https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary`
/// 认证：Google OAuth Bearer token
/// 必须带 `User-Agent: antigravity/<version>` 和 `X-Goog-Api-Client` 头，否则返回 403。
fn fetch_antigravity(path: &Path, from_keychain: bool) -> Result<QuotaResult, String> {
    let mut cred = read_antigravity_cred(path, from_keychain)?;
    if cred.access_token.is_empty() {
        return Err("Antigravity: no access token".into());
    }

    let agent = http_agent();
    let headers = |token: &str| -> [(&'static str, String); 4] {
        [
            ("authorization", format!("Bearer {token}")),
            ("content-type", "application/json".into()),
            ("user-agent", "antigravity/2.11.0".into()),
            ("x-goog-api-client", "antigravity/2.11.0".into()),
        ]
    };

    let mut body: Result<Value, String>;
    let mut refreshed = false;
    loop {
        let h = headers(&cred.access_token);
        let refs: Vec<(&str, &str)> = h.iter().map(|(k, v)| (*k, v.as_str())).collect();
        body = post_json(&agent, ANTIGRAVITY_QUOTA_URL, serde_json::json!({}), &refs);
        match &body {
            Err(e) if e.contains("401") && !refreshed && !cred.refresh_token.is_empty() => {
                refresh_antigravity(&mut cred)?;
                write_back_antigravity(path, &cred);
                refreshed = true;
                continue;
            }
            _ => break,
        }
    }
    let response = body?;
    Ok(parse_antigravity_quota(&response))
}

/// 解析 Antigravity `retrieveUserQuotaSummary` 响应。
///
/// 响应结构：
/// ```json
/// {
///   "groups": [
///     {
///       "displayName": "Gemini Models",
///       "description": "...",
///       "buckets": [
///         { "bucketId": "gemini-weekly", "displayName": "Weekly Limit Remaining",
///           "window": "weekly", "resetTime": "2026-09-23T03:15:56Z",
///           "remainingFraction": 1.0 },
///         { "bucketId": "gemini-5h", "displayName": "Five Hour Limit Remaining",
///           "window": "5h", "resetTime": "...", "remainingFraction": 1.0 }
///       ]
///     }
///   ]
/// }
/// ```
pub fn parse_antigravity_quota(response: &Value) -> QuotaResult {
    let mut windows = Vec::new();

    if let Some(groups) = response.get("groups").and_then(Value::as_array) {
        for group in groups {
            let group_name = group
                .get("displayName")
                .and_then(Value::as_str)
                .unwrap_or_default();

            if let Some(buckets) = group.get("buckets").and_then(Value::as_array) {
                for bucket in buckets {
                    let window_str = bucket
                        .get("window")
                        .and_then(Value::as_str)
                        .unwrap_or_default();
                    let remaining = bucket
                        .get("remainingFraction")
                        .and_then(Value::as_f64)
                        .unwrap_or(1.0);
                    let used_percent = ((1.0 - remaining) * 100.0).clamp(0.0, 999.0);
                    let resets_at = bucket
                        .get("resetTime")
                        .and_then(Value::as_str)
                        .and_then(|s| DateTime::parse_from_rfc3339(s).ok())
                        .map(|dt| dt.timestamp());

                    // 窗口标签：用组名区分不同模型组的同名窗口
                    let label = match window_str {
                        "weekly" => WindowLabel::Custom(format!("{group_name} – Weekly")),
                        "5h" => WindowLabel::Custom(format!("{group_name} – 5h")),
                        other => WindowLabel::Custom(format!("{group_name} – {other}")),
                    };

                    windows.push(QuotaWindow {
                        label,
                        used_percent,
                        resets_at,
                    });
                }
            }
        }
    }

    QuotaResult {
        plan: None,
        windows,
        extra: None,
    }
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

fn now_sec() -> i64 {
    chrono::Utc::now().timestamp()
}

fn urlencoded(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        match b {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(b as char)
            }
            _ => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

/// 倒计时文案：如 "2小时15分" / "3天4小时" / "12分钟"
pub fn fmt_countdown(resets_at: i64) -> String {
    let diff = resets_at - now_sec();
    if diff <= 0 {
        return String::new();
    }
    let days = diff / 86400;
    let hours = (diff % 86400) / 3600;
    let minutes = (diff % 3600) / 60;
    match i18n_lang() {
        crate::i18n::Lang::Zh => {
            if days > 0 {
                format!("{days}天{hours}小时")
            } else if hours > 0 {
                format!("{hours}小时{minutes}分")
            } else {
                format!("{minutes}分钟")
            }
        }
        crate::i18n::Lang::En => {
            if days > 0 {
                format!("{days}d {hours}h")
            } else if hours > 0 {
                format!("{hours}h {minutes}m")
            } else {
                format!("{minutes}m")
            }
        }
    }
}

fn i18n_lang() -> crate::i18n::Lang {
    crate::i18n::lang()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn antigravity_state_token_reads_current_access_and_refresh() {
        let cred = antigravity_state_credentials("CkMKGW9hdXRoVG9rZW5JbmZvU2VudGluZWxLZXkSJgokQ2d0bVlXdGxMV0ZqWTJWemN4b01abUZyWlMxeVpXWnlaWE5v").unwrap();
        assert_eq!(cred.access_token, "fake-access");
        assert_eq!(cred.refresh_token, "fake-refresh");
        assert!(antigravity_state_credentials("invalid").is_none());
        assert!(protobuf_bytes(&[10, 255], 1).is_none());
    }

    #[test]
    fn antigravity_windows_client_pairs_config_references() {
        let mut binary = vec![0u8; 2048];
        binary[60..64].copy_from_slice(&128u32.to_le_bytes());
        binary[128..132].copy_from_slice(b"PE\0\0");
        binary[134..136].copy_from_slice(&1u16.to_le_bytes());
        binary[164..168].copy_from_slice(&4096u32.to_le_bytes());
        binary[168..172].copy_from_slice(&1536u32.to_le_bytes());
        binary[172..176].copy_from_slice(&512u32.to_le_bytes());
        let id = format!(
            "{}{}.{}",
            "1071006060591-", "test", "apps.googleusercontent.com"
        )
        .into_bytes();
        let secret = format!("{}{}", "GOCSPX-", "x".repeat(28)).into_bytes();
        binary[1000..1000 + id.len()].copy_from_slice(&id);
        binary[1200..1200 + secret.len()].copy_from_slice(&secret);
        for (instruction, target) in [(600usize, 1000usize), (618, 1200)] {
            binary[instruction..instruction + 3].copy_from_slice(&[0x48, 0x8d, 0x0d]);
            binary[instruction + 3..instruction + 7]
                .copy_from_slice(&((target as i32) - (instruction as i32) - 7).to_le_bytes());
        }
        let pair = antigravity_windows_client(&binary).unwrap();
        assert_eq!(pair.0, String::from_utf8(id.to_vec()).unwrap());
        assert_eq!(pair.1, String::from_utf8(secret.to_vec()).unwrap());
        binary[618] = 0;
        assert!(antigravity_windows_client(&binary).is_none());
        assert!(antigravity_windows_client(&[0; 10]).is_none());
    }
    #[test]
    fn parse_claude_usage_windows() {
        let usage: Value = serde_json::from_str(
            r#"{
              "five_hour": {"utilization": 23.5, "resets_at": "2026-08-30T18:00:00Z"},
              "seven_day": {"utilization": 80.0, "resets_at": "2026-09-03T00:00:00Z"},
              "seven_day_opus": {"utilization": 0, "resets_at": null},
              "extra_usage": {"is_enabled": true, "used_credits": 12.5, "monthly_limit": 1000}
            }"#,
        )
        .unwrap();
        let r = parse_claude_usage(&usage, Some("Max".into()));
        assert_eq!(r.plan.as_deref(), Some("Max"));
        assert_eq!(r.windows.len(), 3);
        assert_eq!(r.windows[0].label, WindowLabel::FiveHour);
        assert!((r.windows[0].used_percent - 23.5).abs() < 1e-9);
        assert_eq!(r.windows[0].resets_at, Some(1788112800));
        assert_eq!(r.extra.as_deref(), Some("$12.50 / $1000.00"));
    }

    #[test]
    fn parse_codex_wham_usage() {
        let usage: Value = serde_json::from_str(
            r#"{
              "plan_type": "pro",
              "rate_limit": {
                "allowed": true,
                "limit_reached": false,
                "primary_window": {
                  "used_percent": 3.4,
                  "limit_window_seconds": 18000,
                  "reset_after_seconds": 14331,
                  "reset_at": "2026-08-31T03:18:51Z"
                },
                "secondary_window": {
                  "used_percent": 0.0,
                  "limit_window_seconds": 604800,
                  "reset_after_seconds": 388554,
                  "reset_at": "2026-09-04T03:18:51Z"
                }
              },
              "additional_rate_limits": [
                {"limit_name": "Flex mode", "rate_limit": {
                  "used_percent": 12.0, "limit_window_seconds": 2592000,
                  "reset_after_seconds": 100, "reset_at": null
                }}
              ]
            }"#,
        )
        .unwrap();
        let r = parse_codex_usage(&usage, None);
        assert_eq!(r.windows.len(), 3);
        assert_eq!(r.windows[0].label, WindowLabel::FiveHour);
        assert_eq!(r.windows[1].label, WindowLabel::SevenDay);
        assert_eq!(r.windows[2].label, WindowLabel::Custom("Flex mode".into()));
        assert_eq!(r.windows[2].used_percent, 12.0);
    }

    #[test]
    fn parse_devin_quota_windows() {
        let quota: Value = serde_json::from_str(
            r#"{
              "is_quota_plan": true,
              "daily_percentage": 42,
              "weekly_percentage": 17,
              "daily_reset_at": "2026-08-31T00:00:00-08:00",
              "weekly_reset_at": "2026-09-06T00:00:00-08:00",
              "overage_balance": 193.449258,
              "hide_daily_quota": false
            }"#,
        )
        .unwrap();
        let r = parse_devin_quota(&quota, Some("pro".into()));
        assert_eq!(r.windows.len(), 2);
        assert_eq!(r.windows[0].label, WindowLabel::Daily);
        assert_eq!(r.windows[0].used_percent, 58.0);
        // -08:00 → UTC 08:00
        assert_eq!(r.windows[0].resets_at, Some(1788163200));
        assert_eq!(r.extra.as_deref(), Some("193.45 ACU"));
        assert_eq!(r.plan.as_deref(), Some("pro"));
    }

    #[test]
    fn parse_devin_quota_hides_daily() {
        let quota: Value = serde_json::from_str(
            r#"{"daily_percentage": 42, "weekly_percentage": 17, "hide_daily_quota": true}"#,
        )
        .unwrap();
        let r = parse_devin_quota(&quota, None);
        assert_eq!(r.windows.len(), 1);
        assert_eq!(r.windows[0].label, WindowLabel::Weekly);
    }

    #[test]
    fn parse_grok_billing_weekly_and_monthly() {
        let weekly: Value = serde_json::from_str(
            r#"{
              "config": {
                "currentPeriod": {"type": "weekly", "start": "2026-08-24T00:00:00Z", "end": "2026-08-31T00:00:00Z"},
                "creditUsagePercent": 25
              }
            }"#,
        )
        .unwrap();
        let monthly: Value = serde_json::from_str(
            r#"{
              "config": {
                "monthlyLimit": {"val": 10000},
                "used": {"val": 2500},
                "billingPeriodStart": "2026-07-01T00:00:00Z",
                "billingPeriodEnd": "2026-08-01T00:00:00Z"
              }
            }"#,
        )
        .unwrap();
        let r = parse_grok_billing(&weekly, Some(&monthly));
        assert_eq!(r.windows.len(), 2);
        assert_eq!(r.windows[0].label, WindowLabel::Weekly);
        assert_eq!(r.windows[0].used_percent, 25.0);
        assert_eq!(r.windows[1].label, WindowLabel::Monthly);
        assert!((r.windows[1].used_percent - 25.0).abs() < 1e-9);
    }

    #[test]
    fn parse_when_handles_three_shapes() {
        assert_eq!(
            parse_when(&Value::String("2026-08-31T00:00:00Z".into())),
            Some(1788134400)
        );
        assert_eq!(parse_when(&Value::from(1788163200i64)), Some(1788163200));
        assert_eq!(parse_when(&Value::from(1788163200000i64)), Some(1788163200));
        assert_eq!(parse_when(&Value::Null), None);
    }

    #[test]
    fn find_org_id_recurses() {
        let v: Value =
            serde_json::from_str(r#"{"orgs": [{"name": "x", "org_id": "org-abc123"}]}"#).unwrap();
        assert_eq!(find_org_id(&v).as_deref(), Some("org-abc123"));
    }

    #[test]
    fn urlencoded_escapes() {
        assert_eq!(urlencoded("a b+c/d"), "a%20b%2Bc%2Fd");
    }

    #[test]
    fn parse_simple_toml_extracts_quoted_value() {
        let text = r#"
api_server_url = "https://server.codeium.com"
windsurf_api_key = "devin-session-token$abc123"
devin_webapp_host = "app.devin.ai"
"#;
        assert_eq!(
            parse_simple_toml(text, "windsurf_api_key").as_deref(),
            Some("devin-session-token$abc123")
        );
        assert_eq!(
            parse_simple_toml(text, "devin_webapp_host").as_deref(),
            Some("app.devin.ai")
        );
        assert_eq!(parse_simple_toml(text, "nonexistent"), None);
    }

    #[test]
    #[allow(clippy::vec_init_then_push)] // 逐字节构造 protobuf，保留每一段的注释
    fn pb_find_varint_and_submessage() {
        // 手工构造一个 protobuf 消息:
        // F1 (varint) = 42
        // F2 (string) = "hello"
        // F13 (msg) = { F14 (varint) = 72, F15 (varint) = 68, F1 (msg) = { F2 (string) = "Pro" } }
        let mut msg = Vec::new();
        // F1 = 42 (varint)
        msg.push(0x08); // tag: field 1, wire type 0
        msg.push(0x2a); // 42
                        // F2 = "hello" (length-delimited)
        msg.push(0x12); // tag: field 2, wire type 2
        msg.push(0x05); // length 5
        msg.extend_from_slice(b"hello");
        // F13 = submessage
        let mut sub = Vec::new();
        // F14 = 72
        sub.push(0x70); // tag: field 14, wire type 0
        sub.push(0x48); // 72
                        // F15 = 68
        sub.push(0x78); // tag: field 15, wire type 0
        sub.push(0x44); // 68
                        // F1 (msg) = { F2 = "Pro" }
        let mut inner = Vec::new();
        inner.push(0x12); // tag: field 2, wire type 2
        inner.push(0x03); // length 3
        inner.extend_from_slice(b"Pro");
        sub.push(0x0a); // tag: field 1, wire type 2
        sub.push(inner.len() as u8);
        sub.extend_from_slice(&inner);

        msg.push(0x6a); // tag: field 13, wire type 2
        msg.push(sub.len() as u8);
        msg.extend_from_slice(&sub);

        assert_eq!(pb::find_varint(&msg, 1), Some(42));
        assert_eq!(pb::find_string(&msg, 2).as_deref(), Some("hello"));
        let f13 = pb::find_submessage(&msg, 13).unwrap();
        assert_eq!(pb::find_varint(f13, 14), Some(72));
        assert_eq!(pb::find_varint(f13, 15), Some(68));
        let f13_1 = pb::find_submessage(f13, 1).unwrap();
        assert_eq!(pb::find_string(f13_1, 2).as_deref(), Some("Pro"));
    }

    #[test]
    fn pb_encode_and_decode_roundtrip() {
        // 编码一个请求体，然后解码验证
        let mut meta = Vec::new();
        pb::write_string_field(&mut meta, 1, "chisel");
        pb::write_string_field(&mut meta, 2, "3000.6.11");
        pb::write_string_field(&mut meta, 3, "devin-session-token$test");
        pb::write_string_field(&mut meta, 7, "3000.6.11");
        let mut body = Vec::new();
        pb::write_message_field(&mut body, 1, &meta);

        // 解码外层 F1
        let f1 = pb::find_submessage(&body, 1).unwrap();
        assert_eq!(pb::find_string(f1, 1).as_deref(), Some("chisel"));
        assert_eq!(pb::find_string(f1, 2).as_deref(), Some("3000.6.11"));
        assert_eq!(
            pb::find_string(f1, 3).as_deref(),
            Some("devin-session-token$test")
        );
        assert_eq!(pb::find_string(f1, 7).as_deref(), Some("3000.6.11"));
    }

    #[test]
    fn fetch_real_devin_cli() {
        if let Some((token, org_id)) = read_devin_cli_credentials() {
            println!("org_id: {org_id}");
            match fetch_devin_cli(&token, &org_id) {
                Ok(r) => {
                    println!("plan: {:?}", r.plan);
                    println!("windows: {:?}", r.windows);
                    println!("extra: {:?}", r.extra);
                    assert!(
                        r.plan.is_some() || !r.windows.is_empty(),
                        "should get some quota data from API"
                    );
                }
                Err(e) => {
                    // 网络不可用时不应该 panic，只打印
                    println!("error (network may be unavailable): {e}");
                }
            }
        } else {
            println!("No Devin CLI credentials found (skipping)");
        }
    }

    #[test]
    fn parse_antigravity_quota_basic() {
        let response: Value = serde_json::from_str(
            r#"{
              "groups": [
                {
                  "displayName": "Gemini Models",
                  "description": "Models within this group: Gemini Flash, Gemini Pro",
                  "buckets": [
                    {
                      "bucketId": "gemini-weekly",
                      "displayName": "Weekly Limit Remaining",
                      "window": "weekly",
                      "resetTime": "2026-09-23T03:15:56Z",
                      "remainingFraction": 0.75
                    },
                    {
                      "bucketId": "gemini-5h",
                      "displayName": "Five Hour Limit Remaining",
                      "window": "5h",
                      "resetTime": "2026-09-16T08:15:56Z",
                      "remainingFraction": 1.0
                    }
                  ]
                },
                {
                  "displayName": "Claude and GPT models",
                  "description": "Models within this group: Claude Opus, Claude Sonnet, GPT-OSS",
                  "buckets": [
                    {
                      "bucketId": "3p-weekly",
                      "displayName": "Weekly Limit Remaining",
                      "window": "weekly",
                      "resetTime": "2026-09-20T09:35:50Z",
                      "remainingFraction": 0.5
                    }
                  ]
                }
              ],
              "description": "Within each group, models share a weekly limit and a 5-hour limit."
            }"#,
        )
        .unwrap();

        let r = parse_antigravity_quota(&response);
        assert_eq!(r.plan, None);
        assert_eq!(r.windows.len(), 3);

        // Gemini weekly: 75% remaining → 25% used
        assert!(
            matches!(&r.windows[0].label, WindowLabel::Custom(s) if s.contains("Gemini") && s.contains("Weekly"))
        );
        assert!((r.windows[0].used_percent - 25.0).abs() < 1e-9);
        assert!(r.windows[0].resets_at.is_some());

        // Gemini 5h: 100% remaining → 0% used
        assert!(
            matches!(&r.windows[1].label, WindowLabel::Custom(s) if s.contains("Gemini") && s.contains("5h"))
        );
        assert!((r.windows[1].used_percent - 0.0).abs() < 1e-9);

        // 3p weekly: 50% remaining → 50% used
        assert!(
            matches!(&r.windows[2].label, WindowLabel::Custom(s) if s.contains("Claude") && s.contains("Weekly"))
        );
        assert!((r.windows[2].used_percent - 50.0).abs() < 1e-9);

        assert_eq!(r.extra, None);
    }

    #[test]
    fn parse_antigravity_quota_empty() {
        let response: Value = serde_json::json!({});
        let r = parse_antigravity_quota(&response);
        assert_eq!(r.windows.len(), 0);
        assert_eq!(r.extra, None);
    }
}
