//! 订阅用量查询：Claude Code / Codex / Grok / Devin 的配额窗口。
//!
//! - 凭证默认读本机各 agent 的登录文件；刷新令牌后原子写回原文件，
//!   与 CLI 自身行为一致（这些 OAuth 的 refresh_token 是一次性轮换的，
//!   只刷不写回会让 CLI 下次刷新失败）。
//! - Devin 走浏览器 session cookie（手动粘贴，存本应用配置目录）。
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
}

impl Provider {
    pub fn label(self) -> &'static str {
        match self {
            Provider::Claude => "Claude Code",
            Provider::Codex => "Codex",
            Provider::Grok => "Grok",
            Provider::Devin => "Devin",
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
    ClaudeLocal { path: PathBuf },
    CodexLocal { path: PathBuf },
    GrokLocal { path: PathBuf },
    Devin { cookie: String, org_id: Option<String> },
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

pub fn discover_accounts() -> Vec<Account> {
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
    }

    for (i, d) in load_store().devin.into_iter().enumerate() {
        out.push(Account {
            key: format!("devin-{i}"),
            provider: Provider::Devin,
            label: if d.label.is_empty() { "Devin".into() } else { d.label },
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

fn read_codex_email(path: &Path) -> Option<String> {
    let v: Value = serde_json::from_str(&std::fs::read_to_string(path).ok()?).ok()?;
    v.get("email")
        .and_then(Value::as_str)
        .map(str::to_string)
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

fn post_form(agent: &ureq::Agent, url: &str, form: &str, headers: &[(&str, &str)]) -> Result<Value, String> {
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

fn post_json(agent: &ureq::Agent, url: &str, payload: Value, headers: &[(&str, &str)]) -> Result<Value, String> {
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
        AccountKind::CodexLocal { path } => fetch_codex(path),
        AccountKind::GrokLocal { path } => fetch_grok(path),
        AccountKind::Devin { cookie, org_id } => fetch_devin(cookie, org_id.as_deref()),
    }
}

// ---------------------------------------------------------------------------
// Claude Code
// ---------------------------------------------------------------------------

const CLAUDE_CLIENT_ID: &str = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";
const CLAUDE_SCOPES: &str = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload";

struct ClaudeCred {
    access_token: String,
    refresh_token: String,
    /// unix 秒；None = 未知，先试直接查询
    expires_at: Option<i64>,
}

fn read_claude_cred(path: &Path) -> Result<ClaudeCred, String> {
    let text = std::fs::read_to_string(path).map_err(|e| e.to_string())?;
    let v: Value = serde_json::from_str(&text).map_err(|e| e.to_string())?;
    let oauth = json_field(&v, &["claudeAiOauth"])
        .ok_or_else(|| "credentials.json missing claudeAiOauth (macOS Keychain not supported yet)".to_string())?;
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
    let Ok(text) = std::fs::read_to_string(path) else { return };
    let Ok(mut v) = serde_json::from_str::<Value>(&text) else { return };
    if let Some(oauth) = v.pointer_mut("/claudeAiOauth") {
        if let Some(obj) = oauth.as_object_mut() {
            obj.insert("accessToken".into(), Value::String(cred.access_token.clone()));
            obj.insert("refreshToken".into(), Value::String(cred.refresh_token.clone()));
            if let Some(ts) = cred.expires_at {
                obj.insert("expiresAt".into(), Value::from(ts * 1000));
            }
        }
    }
    let Ok(out) = serde_json::to_string_pretty(&v) else { return };
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
            ("user-agent", "claude-cli/1.0.90 (external, cli)".to_string()),
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
        windows.push(QuotaWindow {
            label: label.clone(),
            used_percent: pct,
            resets_at: json_field(w, &["resets_at"]).and_then(parse_when),
        });
    }

    let extra = json_field(usage, &["extra_usage"]).and_then(|e| {
        let enabled = json_field(e, &["is_enabled"]).and_then(Value::as_bool).unwrap_or(false);
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
const CODEX_UA: &str = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)";

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
    let id_token = v.get("id_token").and_then(Value::as_str).map(str::to_string);
    let expires_at = v.get("expired").and_then(parse_when);
    Ok(CodexCred {
        access_token: access,
        refresh_token,
        id_token,
        expires_at,
    })
}

fn write_back_codex(path: &Path, cred: &CodexCred) {
    let Ok(text) = std::fs::read_to_string(path) else { return };
    let Ok(mut v) = serde_json::from_str::<Value>(&text) else { return };
    if let Some(obj) = v.as_object_mut() {
        obj.insert("access_token".into(), Value::String(cred.access_token.clone()));
        obj.insert("refresh_token".into(), Value::String(cred.refresh_token.clone()));
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
    let Ok(out) = serde_json::to_string_pretty(&v) else { return };
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
    let mut cred = read_codex_cred(path)?;
    let agent = http_agent();

    let mut usage: Result<Value, String>;
    let mut refreshed = false;
    loop {
        let headers: [(&str, &str); 2] = [
            ("authorization", &*format!("Bearer {}", cred.access_token)),
            ("user-agent", CODEX_UA),
        ];
        usage = get_json(&agent, "https://chatgpt.com/backend-api/wham/usage", &headers);
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
    let plan = usage.get("plan_type").and_then(Value::as_str).map(str::to_string);
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
    if let Some(additional) = usage.get("additional_rate_limits").and_then(Value::as_array) {
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
    let resets_at = v
        .get("reset_at")
        .and_then(parse_when)
        .or_else(|| {
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
    ("user-agent", "grok-pager/0.2.91 grok-shell/0.2.91 (windows; x86_64)"),
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
    let obj = v.as_object().ok_or_else(|| "invalid auth.json".to_string())?;
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
    let Ok(text) = std::fs::read_to_string(path) else { return };
    let Ok(mut v) = serde_json::from_str::<Value>(&text) else { return };
    if let Some(entry) = v.get_mut(&cred.entry_key).and_then(Value::as_object_mut) {
        // 保持与读取时一致的时间单位（原值 > 1e12 视为毫秒）
        let unit_ms = entry
            .get("expires_at")
            .and_then(Value::as_f64)
            .is_some_and(|n| n > 1e12);
        entry.insert("key".into(), Value::String(cred.access_token.clone()));
        entry.insert("refresh_token".into(), Value::String(cred.refresh_token.clone()));
        if let Some(ts) = cred.expires_at {
            let stored = if unit_ms { ts * 1000 } else { ts };
            entry.insert("expires_at".into(), Value::from(stored));
        }
    }
    let Ok(out) = serde_json::to_string_pretty(&v) else { return };
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
    .and_then(|d| d.get("token_endpoint").and_then(Value::as_str).map(str::to_string))
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
    let weekly = body?;

    let bearer = format!("Bearer {}", cred.access_token);
    let mut headers: Vec<(&str, &str)> = vec![("authorization", &bearer)];
    headers.extend_from_slice(GROK_HEADERS_BASE);
    let monthly = get_json(&agent, "https://cli-chat-proxy.grok.com/v1/billing", &headers).ok();

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
        let period = json_field(cfg, &["currentPeriod"]).or_else(|| json_field(cfg, &["current_period"]));
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
    .and_then(|s| s.get("plan_slug").and_then(Value::as_str).map(str::to_string));

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
    if let (Some(pct), false) = (quota.get("daily_percentage").and_then(Value::as_f64), hide_daily) {
        windows.push(QuotaWindow {
            label: WindowLabel::Daily,
            used_percent: pct,
            resets_at: quota.get("daily_reset_at").and_then(parse_when),
        });
    }
    if let Some(pct) = quota.get("weekly_percentage").and_then(Value::as_f64) {
        windows.push(QuotaWindow {
            label: WindowLabel::Weekly,
            used_percent: pct,
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
        assert_eq!(r.windows[0].used_percent, 42.0);
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
        assert_eq!(
            parse_when(&Value::from(1788163200000i64)),
            Some(1788163200)
        );
        assert_eq!(parse_when(&Value::Null), None);
    }

    #[test]
    fn find_org_id_recurses() {
        let v: Value = serde_json::from_str(
            r#"{"orgs": [{"name": "x", "org_id": "org-abc123"}]}"#,
        )
        .unwrap();
        assert_eq!(find_org_id(&v).as_deref(), Some("org-abc123"));
    }

    #[test]
    fn urlencoded_escapes() {
        assert_eq!(urlencoded("a b+c/d"), "a%20b%2Bc%2Fd");
    }
}
