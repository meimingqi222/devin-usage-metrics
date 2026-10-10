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
    /// 非 None 表示这是缓存/过期数据（限流冷却中或本次查询失败时回退到上次成功结果）
    pub stale: Option<StaleInfo>,
}

/// 数据来自缓存而非本次查询。
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct StaleInfo {
    /// 这份数据是多少秒前查到的
    pub age_secs: u64,
    pub reason: StaleReason,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum StaleReason {
    /// 429 冷却中，`retry_in_secs` 秒后才会再次请求
    RateLimited { retry_in_secs: u64 },
    /// 本次查询失败（错误信息）
    Error(String),
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
    // Claude Code 在 macOS 默认把 OAuth 放 Keychain；即使 .claude/.credentials.json 还在，
    // 也可能只是旧版本遗留的过期副本，所以只要装过 Claude Code 就提示。
    let claude_keychain_likely = h.join(".claude").exists();
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
///
/// Unix 上保留原文件权限（新文件默认 0600）：这些文件存放 OAuth token / cookie，
/// 临时文件若沿用 umask 默认的 0644，替换后会把 `~/.claude/.credentials.json`
/// 之类的凭据文件变成同机其他用户可读。
fn atomic_write(path: &Path, data: &[u8]) -> bool {
    let temp = path.with_extension(format!("tmp-{}", std::process::id()));
    if write_private_file(&temp, data, path).is_err() {
        let _ = std::fs::remove_file(&temp);
        return false;
    }
    if std::fs::rename(&temp, path).is_ok() {
        return true;
    }
    let _ = std::fs::remove_file(path);
    if std::fs::rename(&temp, path).is_ok() {
        return true;
    }
    let _ = std::fs::remove_file(&temp);
    false
}

/// 写 `temp`，权限取 `original` 现有权限，不存在则 0600（非 Unix 平台忽略）。
fn write_private_file(temp: &Path, data: &[u8], original: &Path) -> std::io::Result<()> {
    #[cfg(unix)]
    {
        use std::io::Write;
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
        let mode = std::fs::metadata(original)
            .map(|m| m.permissions().mode() & 0o777)
            .unwrap_or(0o600);
        let _ = std::fs::remove_file(temp);
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(mode)
            .open(temp)?;
        file.write_all(data)?;
        // open(2) 的 mode 受 umask 影响，这里显式设成与原文件一致
        file.set_permissions(std::fs::Permissions::from_mode(mode))
    }
    #[cfg(not(unix))]
    {
        let _ = original;
        std::fs::write(temp, data)
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
        // macOS：Claude Code 的真实凭据在钥匙串里，`.credentials.json` 可能是旧版本遗留的
        // 过期副本，所以钥匙串优先（需用户已同意）；文件只作兜底。
        // 账号展示名仍从文件里取邮箱（钥匙串探测不读密文）。
        if allow_keychain
            && cfg!(target_os = "macos")
            && keychain_item_exists(CLAUDE_KEYCHAIN_SERVICE)
        {
            // 只探测条目是否存在；真正读密文发生在 fetch 阶段
            out.push(Account {
                key: "claude-keychain".into(),
                provider: Provider::Claude,
                label: read_claude_email(&claude).unwrap_or_else(|| "Claude Code".into()),
                kind: AccountKind::ClaudeKeychain,
            });
        } else if claude.exists() {
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

/// macOS Keychain 读取：`security find-generic-password -s <service> -w`
/// 仅在用户显式同意后、真正查询配额时调用。
///
/// **不做进程内缓存**：Claude Code 会轮换钥匙串里的 token，缓存的旧密文
/// 会让我们拿旧 token/refresh_token 去请求（401 / invalid_grant）。
/// 用户授权过一次（"始终允许"）之后，`security -w` 不会再弹窗。
#[cfg(target_os = "macos")]
fn keychain_read(service: &str) -> Option<String> {
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
    Some(trimmed.to_string())
}

/// 从 `security find-generic-password -s <service>`（不带 -w）的属性输出里取出 acct。
/// 形如：`    "acct"<blob>="alice"`。取不到（`<NULL>` / 十六进制形式）返回 None。
#[cfg(any(target_os = "macos", test))]
fn parse_keychain_account(output: &str) -> Option<String> {
    let line = output
        .lines()
        .map(str::trim_start)
        .find(|l| l.starts_with("\"acct\"<blob>="))?;
    let value = line.split_once('=')?.1.trim();
    let inner = value.strip_prefix('"')?.strip_suffix('"')?;
    (!inner.is_empty()).then(|| inner.to_string())
}

/// macOS Keychain 写入：**原地更新**现有条目（`add-generic-password -U`，沿用原
/// account），不存在才新建。
///
/// 旧实现是「先 delete 再用固定 account 添加」：会删掉 Claude Code 自己的条目
/// （account 是登录用户名），换成 account 不同、ACL 不同的新条目，Claude Code
/// 再读就读不到 → 被迫重新登录。
#[cfg(target_os = "macos")]
fn keychain_write(service: &str, value: &str) -> Result<(), String> {
    let existing = std::process::Command::new("security")
        .args(["find-generic-password", "-s", service])
        .output()
        .ok()
        .filter(|o| o.status.success())
        .and_then(|o| parse_keychain_account(&String::from_utf8_lossy(&o.stdout)));
    let account = existing
        .or_else(|| std::env::var("USER").ok())
        .ok_or_else(|| "cannot determine Keychain account".to_string())?;
    let out = std::process::Command::new("security")
        .args([
            "add-generic-password",
            "-U",
            "-s",
            service,
            "-a",
            &account,
            "-w",
            value,
        ])
        .output()
        .map_err(|e| e.to_string())?;
    if out.status.success() {
        Ok(())
    } else {
        Err(format!(
            "security add-generic-password exited with {}",
            out.status
        ))
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

/// 查询类请求的全局超时（含读 body）。
const HTTP_TIMEOUT: Duration = Duration::from_secs(10);
/// OAuth refresh 请求的全局超时（服务端偶尔较慢，且失败代价高）。
const REFRESH_TIMEOUT: Duration = Duration::from_secs(15);
/// 错误响应 body 最多保留的字节数（只用于识别 `invalid_grant` 等错误码）。
const ERROR_BODY_LIMIT: u64 = 4096;

/// 带结构化信息的查询错误：保留 HTTP 状态码、`Retry-After` 和（截断的）响应 body，
/// 让上层能区分 401 / 429 / `invalid_grant`，而不是只拿到一个 "HTTP 429" 字符串。
/// `Display` 与旧的字符串错误保持一致（"HTTP 429"），UI 展示不变。
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct QuotaError {
    pub message: String,
    /// HTTP 状态码；非 HTTP 错误（网络、解析、凭据读取）为 None
    pub status: Option<u16>,
    /// `Retry-After` 解析出的秒数
    pub retry_after_secs: Option<u64>,
    /// 错误响应 body（截断）
    pub body: Option<String>,
}

impl QuotaError {
    fn http(status: u16, retry_after_secs: Option<u64>, body: Option<String>) -> Self {
        QuotaError {
            message: format!("HTTP {status}"),
            status: Some(status),
            retry_after_secs,
            body,
        }
    }

    fn is_status(&self, code: u16) -> bool {
        self.status == Some(code)
    }
}

impl std::fmt::Display for QuotaError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

impl From<String> for QuotaError {
    fn from(message: String) -> Self {
        QuotaError {
            message,
            status: None,
            retry_after_secs: None,
            body: None,
        }
    }
}

impl From<&str> for QuotaError {
    fn from(message: &str) -> Self {
        message.to_string().into()
    }
}

impl From<QuotaError> for String {
    fn from(e: QuotaError) -> Self {
        e.message
    }
}

/// 全进程共享的 HTTP agent：复用连接池 / TLS 会话，避免每次查询都重新握手。
/// `http_status_as_error(false)`：4xx/5xx 也返回响应，这样才能读到 `Retry-After` 和 body。
fn http_agent() -> ureq::Agent {
    use std::sync::OnceLock;
    static AGENT: OnceLock<ureq::Agent> = OnceLock::new();
    AGENT
        .get_or_init(|| {
            ureq::Agent::config_builder()
                .timeout_global(Some(HTTP_TIMEOUT))
                .http_status_as_error(false)
                .build()
                .new_agent()
        })
        .clone()
}

/// 解析 `Retry-After`：整数秒或 HTTP-date（RFC 2822/1123）。无法解析返回 None。
fn parse_retry_after(raw: &str, now: chrono::DateTime<chrono::Utc>) -> Option<u64> {
    let raw = raw.trim();
    if raw.is_empty() {
        return None;
    }
    if let Ok(secs) = raw.parse::<u64>() {
        return Some(secs);
    }
    let when = DateTime::parse_from_rfc2822(raw).ok()?;
    Some((when.timestamp() - now.timestamp()).max(0) as u64)
}

/// 4xx/5xx → 结构化错误（读取 `Retry-After` 与截断的 body）；其余原样放行。
fn check_status(
    resp: ureq::http::Response<ureq::Body>,
) -> Result<ureq::http::Response<ureq::Body>, QuotaError> {
    let status = resp.status().as_u16();
    if status < 400 {
        return Ok(resp);
    }
    let retry_after_secs = resp
        .headers()
        .get("retry-after")
        .and_then(|v| v.to_str().ok())
        .and_then(|v| parse_retry_after(v, chrono::Utc::now()));
    let body = resp
        .into_body()
        .with_config()
        .limit(ERROR_BODY_LIMIT)
        .lossy_utf8(true)
        .read_to_string()
        .ok()
        .filter(|b| !b.is_empty());
    Err(QuotaError::http(status, retry_after_secs, body))
}

fn get_json(agent: &ureq::Agent, url: &str, headers: &[(&str, &str)]) -> Result<Value, QuotaError> {
    let mut req = agent.get(url);
    for &(k, v) in headers {
        req = req.header(k, v);
    }
    let resp = check_status(req.call().map_err(http_err)?)?;
    read_json(resp)
}

fn post_form(
    agent: &ureq::Agent,
    url: &str,
    form: &str,
    headers: &[(&str, &str)],
) -> Result<Value, QuotaError> {
    let mut req = agent
        .post(url)
        .config()
        .timeout_global(Some(REFRESH_TIMEOUT))
        .build()
        .header("content-type", "application/x-www-form-urlencoded")
        .header("accept", "application/json");
    for &(k, v) in headers {
        req = req.header(k, v);
    }
    let resp = check_status(req.send(form.as_bytes()).map_err(http_err)?)?;
    read_json(resp)
}

/// POST JSON。`timeout` 为 None 时用 agent 默认超时；OAuth refresh 传 `REFRESH_TIMEOUT`。
fn post_json(
    agent: &ureq::Agent,
    url: &str,
    payload: Value,
    headers: &[(&str, &str)],
    timeout: Option<Duration>,
) -> Result<Value, QuotaError> {
    let mut req = agent
        .post(url)
        .config()
        .timeout_global(Some(timeout.unwrap_or(HTTP_TIMEOUT)))
        .build()
        .header("accept", "application/json");
    for &(k, v) in headers {
        req = req.header(k, v);
    }
    let resp = check_status(req.send_json(payload).map_err(http_err)?)?;
    read_json(resp)
}

fn read_json(resp: ureq::http::Response<ureq::Body>) -> Result<Value, QuotaError> {
    let mut body = resp.into_body();
    let text = body
        .read_to_string()
        .map_err(|e: ureq::Error| QuotaError::from(e.to_string()))?;
    serde_json::from_str(&text).map_err(|e| QuotaError::from(format!("JSON: {e}")))
}

fn http_err(e: ureq::Error) -> QuotaError {
    match e {
        ureq::Error::StatusCode(code) => QuotaError::http(code, None, None),
        other => other.to_string().into(),
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

/// 查询一个账号（走 TTL 缓存、429 冷却、失败退避）。
pub fn fetch_account(account: &Account) -> Result<QuotaResult, String> {
    fetch_account_opts(account, false)
}

/// `force=true`（用户手动刷新）绕过 TTL 缓存和失败退避，但**仍然尊重 429 冷却**——
/// 冷却期内再请求只会继续被限流。
pub fn fetch_account_opts(account: &Account, force: bool) -> Result<QuotaResult, String> {
    let entry = account_entry(&account.key, account_fingerprint(&account.kind));
    cached_fetch(&entry, force, || fetch_account_uncached(account)).map_err(String::from)
}

/// 并发查询所有账号，返回顺序与 `accounts` 一致（UI 卡片顺序不变）。
pub fn fetch_accounts(accounts: &[Account], force: bool) -> Vec<Result<QuotaResult, String>> {
    parallel_map(accounts, |a| fetch_account_opts(a, force))
        .into_iter()
        .map(|r| r.unwrap_or_else(|| Err("quota query thread panicked".to_string())))
        .collect()
}

/// 每个元素一个线程（账号数很少），结果按输入顺序返回；线程 panic 对应位置为 None。
fn parallel_map<T, R, F>(items: &[T], f: F) -> Vec<Option<R>>
where
    T: Sync,
    R: Send,
    F: Fn(&T) -> R + Sync,
{
    std::thread::scope(|scope| {
        let f = &f;
        let handles: Vec<_> = items
            .iter()
            .map(|item| scope.spawn(move || f(item)))
            .collect();
        handles.into_iter().map(|h| h.join().ok()).collect()
    })
}

fn fetch_account_uncached(account: &Account) -> Result<QuotaResult, QuotaError> {
    match &account.kind {
        AccountKind::ClaudeLocal { .. } | AccountKind::ClaudeKeychain => {
            fetch_claude(&account.kind)
        }
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
// 结果缓存 / 429 冷却 / 失败退避（按账号）
// ---------------------------------------------------------------------------

/// 成功结果的缓存有效期（与 openusage 的默认刷新间隔一致）。
pub const CACHE_TTL: Duration = Duration::from_secs(5 * 60);
/// 429 没带 `Retry-After` 时的冷却时间。
const RATE_LIMIT_DEFAULT_COOLDOWN: Duration = Duration::from_secs(5 * 60);
/// `Retry-After` 过大时的上限，避免一次异常响应让账号长时间不可查。
const RATE_LIMIT_MAX_COOLDOWN: Duration = Duration::from_secs(60 * 60);
/// 失败（非 429）后的退避：期间自动刷新直接返回上次结果/错误，不再打接口。
const FAILURE_BACKOFF: Duration = Duration::from_secs(60);
/// 兜底展示的旧结果最长年龄；再老的数据（窗口早已重置）不如直接报错。
const MAX_STALE_AGE: Duration = Duration::from_secs(2 * 60 * 60);

#[derive(Default)]
struct AccountState {
    /// 上次成功的结果及时间
    last_ok: Option<(std::time::Instant, QuotaResult)>,
    /// 429 冷却截止时间
    cooldown_until: Option<std::time::Instant>,
    /// 失败退避截止时间及当时的错误
    backoff: Option<(std::time::Instant, QuotaError)>,
}

enum Decision {
    /// 需要真正发请求
    Fetch,
    /// 无需请求，直接返回（缓存命中 / 冷却中 / 退避中）
    Done(Result<QuotaResult, QuotaError>),
}

/// 上次成功结果的副本，标注为缓存/过期数据。太旧或不存在返回 None。
fn stale_copy(
    state: &AccountState,
    now: std::time::Instant,
    reason: StaleReason,
) -> Option<QuotaResult> {
    let (at, result) = state.last_ok.as_ref()?;
    let age = now.saturating_duration_since(*at);
    if age > MAX_STALE_AGE {
        return None;
    }
    let mut out = result.clone();
    out.stale = Some(StaleInfo {
        age_secs: age.as_secs(),
        reason,
    });
    Some(out)
}

/// 纯逻辑：根据当前状态决定是否发请求。
fn decide(state: &AccountState, now: std::time::Instant, force: bool) -> Decision {
    // 1. 429 冷却（手动刷新也不能绕过）
    if let Some(until) = state.cooldown_until {
        if now < until {
            let retry_in = until.saturating_duration_since(now);
            let secs = retry_in.as_secs() + u64::from(retry_in.subsec_nanos() > 0);
            let reason = StaleReason::RateLimited {
                retry_in_secs: secs,
            };
            return Decision::Done(stale_copy(state, now, reason).ok_or_else(|| QuotaError {
                message: format!("HTTP 429 (retry in {})", fmt_retry_in(secs)),
                status: Some(429),
                retry_after_secs: Some(secs),
                body: None,
            }));
        }
    }
    if !force {
        // 2. TTL 缓存
        if let Some((at, result)) = &state.last_ok {
            if now.saturating_duration_since(*at) < CACHE_TTL {
                return Decision::Done(Ok(result.clone()));
            }
        }
        // 3. 失败退避
        if let Some((until, err)) = &state.backoff {
            if now < *until {
                let reason = StaleReason::Error(err.message.clone());
                return Decision::Done(stale_copy(state, now, reason).ok_or_else(|| err.clone()));
            }
        }
    }
    Decision::Fetch
}

/// 纯逻辑：记录一次真实请求的结果，更新冷却/退避/缓存，并给出最终返回值
/// （失败时尽量回退到上次成功的结果）。
fn record(
    state: &mut AccountState,
    now: std::time::Instant,
    result: Result<QuotaResult, QuotaError>,
) -> Result<QuotaResult, QuotaError> {
    match result {
        Ok(r) => {
            state.last_ok = Some((now, r.clone()));
            state.cooldown_until = None;
            state.backoff = None;
            Ok(r)
        }
        Err(e) if e.is_status(429) => {
            let cooldown = e
                .retry_after_secs
                .map(Duration::from_secs)
                .unwrap_or(RATE_LIMIT_DEFAULT_COOLDOWN)
                .clamp(FAILURE_BACKOFF, RATE_LIMIT_MAX_COOLDOWN);
            state.cooldown_until = Some(now + cooldown);
            let reason = StaleReason::RateLimited {
                retry_in_secs: cooldown.as_secs(),
            };
            stale_copy(state, now, reason).ok_or(e)
        }
        Err(e) => {
            state.backoff = Some((now + FAILURE_BACKOFF, e.clone()));
            let reason = StaleReason::Error(e.message.clone());
            stale_copy(state, now, reason).ok_or(e)
        }
    }
}

fn fmt_retry_in(secs: u64) -> String {
    if secs >= 60 {
        format!("{}m", secs.div_ceil(60))
    } else {
        format!("{secs}s")
    }
}

struct AccountEntry {
    fingerprint: u64,
    /// 同一账号同一时间只有一次真实请求（例如上一轮加载还没结束又触发了新一轮）
    fetch_lock: std::sync::Mutex<()>,
    state: std::sync::Mutex<AccountState>,
}

/// 凭据身份指纹：账号被删除/换号后 key（如 `devin-0`）会复用，指纹变了就丢弃旧缓存。
fn account_fingerprint(kind: &AccountKind) -> u64 {
    use std::hash::{Hash, Hasher};
    let mut h = std::collections::hash_map::DefaultHasher::new();
    match kind {
        AccountKind::ClaudeLocal { path } => ("claude-local", path).hash(&mut h),
        AccountKind::ClaudeKeychain => "claude-keychain".hash(&mut h),
        AccountKind::CodexLocal { path } => ("codex", path).hash(&mut h),
        AccountKind::GrokLocal { path } => ("grok", path).hash(&mut h),
        AccountKind::Devin { cookie, org_id } => ("devin", cookie, org_id).hash(&mut h),
        AccountKind::DevinCli { token, org_id } => ("devin-cli", token, org_id).hash(&mut h),
        AccountKind::AntigravityLocal {
            path,
            from_keychain,
        } => ("antigravity", path, from_keychain).hash(&mut h),
    }
    h.finish()
}

fn account_entry(key: &str, fingerprint: u64) -> std::sync::Arc<AccountEntry> {
    use std::collections::HashMap;
    use std::sync::{Arc, Mutex, OnceLock};
    static REGISTRY: OnceLock<Mutex<HashMap<String, Arc<AccountEntry>>>> = OnceLock::new();
    let mut registry = REGISTRY
        .get_or_init(|| Mutex::new(HashMap::new()))
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    if let Some(entry) = registry.get(key) {
        if entry.fingerprint == fingerprint {
            return entry.clone();
        }
    }
    let entry = Arc::new(AccountEntry {
        fingerprint,
        fetch_lock: Mutex::new(()),
        state: Mutex::new(AccountState::default()),
    });
    registry.insert(key.to_string(), entry.clone());
    entry
}

fn cached_fetch(
    entry: &AccountEntry,
    force: bool,
    fetch: impl FnOnce() -> Result<QuotaResult, QuotaError>,
) -> Result<QuotaResult, QuotaError> {
    use std::sync::PoisonError;
    let started = std::time::Instant::now();
    let _in_flight = entry
        .fetch_lock
        .lock()
        .unwrap_or_else(PoisonError::into_inner);
    {
        let state = entry.state.lock().unwrap_or_else(PoisonError::into_inner);
        // 等锁期间别的线程刚查完 → 直接用它的结果，哪怕是 force（避免同一时刻重复请求）
        if let Some((at, result)) = &state.last_ok {
            if *at > started && state.cooldown_until.is_none() {
                return Ok(result.clone());
            }
        }
        if let Decision::Done(result) = decide(&state, std::time::Instant::now(), force) {
            return result;
        }
    }
    let result = fetch();
    let mut state = entry.state.lock().unwrap_or_else(PoisonError::into_inner);
    record(&mut state, std::time::Instant::now(), result)
}

// ---------------------------------------------------------------------------
// Claude Code
// ---------------------------------------------------------------------------

const CLAUDE_CLIENT_ID: &str = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";
const CLAUDE_SCOPES: &str =
    "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload";
const CLAUDE_USAGE_URL: &str = "https://api.anthropic.com/api/oauth/usage";
const CLAUDE_PROFILE_URL: &str = "https://api.anthropic.com/api/oauth/profile";
const CLAUDE_TOKEN_URL: &str = "https://platform.claude.com/v1/oauth/token";
/// 与 openusage 的 ClaudeUsageClient 保持一致的 claude-cli UA。
const CLAUDE_UA: &str = "claude-cli/2.1.294 (external, cli)";
/// Claude Code 在 macOS 上存放 OAuth 凭据的钥匙串条目。
const CLAUDE_KEYCHAIN_SERVICE: &str = "Claude Code-credentials";
/// access token 过期前这么久就主动刷新（与 openusage 的 5 分钟一致）。
const CLAUDE_REFRESH_SKEW_SECS: i64 = 5 * 60;

#[derive(Clone, Debug, PartialEq, Eq)]
struct ClaudeCred {
    access_token: String,
    refresh_token: String,
    /// unix 秒；None = 未知，先试直接查询
    expires_at: Option<i64>,
}

/// 凭据来自哪里；刷新后写回同一处。
#[derive(Clone, Debug)]
enum ClaudeSource {
    File(PathBuf),
    #[cfg(target_os = "macos")]
    Keychain,
}

struct ClaudeLoaded {
    cred: ClaudeCred,
    source: ClaudeSource,
}

/// JSON 解析；`security -w` 遇到含不可打印字符的密文会输出十六进制，兜底解码一次。
fn decode_json_with_hex_fallback(text: &str) -> Result<Value, String> {
    let text = text.trim();
    match serde_json::from_str::<Value>(text) {
        Ok(v) => Ok(v),
        Err(e) => {
            let hex_like = (text.len() & 1) == 0
                && !text.is_empty()
                && text.bytes().all(|b| b.is_ascii_hexdigit());
            if hex_like {
                let bytes: Option<Vec<u8>> = (0..text.len())
                    .step_by(2)
                    .map(|i| u8::from_str_radix(&text[i..i + 2], 16).ok())
                    .collect();
                if let Some(decoded) = bytes.and_then(|b| String::from_utf8(b).ok()) {
                    if let Ok(v) = serde_json::from_str::<Value>(decoded.trim()) {
                        return Ok(v);
                    }
                }
            }
            Err(e.to_string())
        }
    }
}

/// 从 `{"claudeAiOauth": {...}}` 文档里取凭据。
fn claude_cred_from_doc(doc: &Value) -> Result<ClaudeCred, String> {
    let oauth =
        json_field(doc, &["claudeAiOauth"]).ok_or_else(|| "missing claudeAiOauth".to_string())?;
    let access_token = json_field(oauth, &["accessToken"])
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|t| !t.is_empty())
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

fn parse_claude_cred(text: &str) -> Result<ClaudeCred, String> {
    claude_cred_from_doc(&decode_json_with_hex_fallback(text)?)
}

/// 是否需要刷新：已过期或距过期不足 5 分钟。`expires_at` 未知则不主动刷新。
fn claude_needs_refresh(expires_at: Option<i64>, now: i64) -> bool {
    expires_at.is_some_and(|exp| exp - now <= CLAUDE_REFRESH_SKEW_SECS)
}

/// 刷新接口返回 400/401 时，只有 OAuth 错误码为 `invalid_grant` 才说明 refresh
/// token 真的失效（需要重新登录）；其它（网关/WAF 页面、`invalid_request` 等）只报状态码。
/// 与 openusage 一致：`error` 或 `error_description` 等于 `invalid_grant`，
/// 同时兼容 `{"error": {"type"|"code": "invalid_grant"}}` 的嵌套形态。
fn is_invalid_grant(body: Option<&str>) -> bool {
    let Some(v) = body.and_then(|b| serde_json::from_str::<Value>(b).ok()) else {
        return false;
    };
    let is = |x: Option<&Value>| x.and_then(Value::as_str) == Some("invalid_grant");
    is(v.get("error"))
        || is(v.get("error_description"))
        || is(json_field(&v, &["error", "type"]))
        || is(json_field(&v, &["error", "code"]))
}

/// 把新凭据合并进完整的凭据文档（保留其它字段，如 subscriptionType、scopes、mcpOAuth）。
/// 文档里没有 `claudeAiOauth` 对象时返回 false。
fn apply_claude_cred(doc: &mut Value, cred: &ClaudeCred) -> bool {
    let Some(obj) = doc
        .pointer_mut("/claudeAiOauth")
        .and_then(Value::as_object_mut)
    else {
        return false;
    };
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
    true
}

fn load_claude_file(path: &Path) -> Result<ClaudeLoaded, String> {
    let text = std::fs::read_to_string(path).map_err(|e| e.to_string())?;
    Ok(ClaudeLoaded {
        cred: parse_claude_cred(&text)?,
        source: ClaudeSource::File(path.to_path_buf()),
    })
}

/// 每次查询都**重新**读取凭据（不缓存）：Claude Code 运行中会自行刷新并轮换
/// token，读旧值会得到 401 / invalid_grant。
/// macOS 上钥匙串优先（Claude Code 的真实来源，文件可能是旧版本遗留的过期副本），
/// 钥匙串读不到/解析失败时才回退 `~/.claude/.credentials.json`。
fn load_claude(kind: &AccountKind) -> Result<ClaudeLoaded, String> {
    match kind {
        AccountKind::ClaudeLocal { path } => load_claude_file(path),
        AccountKind::ClaudeKeychain => load_claude_keychain_first(),
        _ => Err("not a Claude account".into()),
    }
}

fn load_claude_keychain_first() -> Result<ClaudeLoaded, String> {
    #[cfg(target_os = "macos")]
    {
        let from_keychain = keychain_read(CLAUDE_KEYCHAIN_SERVICE)
            .ok_or_else(|| format!("{CLAUDE_KEYCHAIN_SERVICE} not found in Keychain"))
            .and_then(|raw| parse_claude_cred(&raw).map_err(|e| format!("Keychain entry: {e}")));
        match from_keychain {
            Ok(cred) => Ok(ClaudeLoaded {
                cred,
                source: ClaudeSource::Keychain,
            }),
            Err(keychain_err) => {
                let file = home().map(|h| h.join(".claude").join(".credentials.json"));
                match file.filter(|p| p.exists()) {
                    Some(path) => load_claude_file(&path)
                        .map_err(|file_err| format!("{keychain_err}; {file_err}")),
                    None => Err(keychain_err),
                }
            }
        }
    }
    #[cfg(not(target_os = "macos"))]
    {
        Err("Claude Keychain is only supported on macOS".into())
    }
}

/// 把刷新得到的新凭据写回原来源。写之前重读一次：若来源里的 refresh token 已不是
/// 我们刷新时用的那个，说明 Claude Code 在此期间自己刷新过，**不覆盖**它的新凭据。
/// 返回 Ok(true)=已写入，Ok(false)=跳过（被 Claude Code 抢先）。
fn persist_claude(
    source: &ClaudeSource,
    used_refresh_token: &str,
    new: &ClaudeCred,
) -> Result<bool, String> {
    let (mut doc, current) = match source {
        ClaudeSource::File(path) => {
            let text = std::fs::read_to_string(path).map_err(|e| e.to_string())?;
            let doc = decode_json_with_hex_fallback(&text)?;
            let current = claude_cred_from_doc(&doc)?;
            (doc, current)
        }
        #[cfg(target_os = "macos")]
        ClaudeSource::Keychain => {
            let raw = keychain_read(CLAUDE_KEYCHAIN_SERVICE)
                .ok_or_else(|| "Keychain entry disappeared".to_string())?;
            let doc = decode_json_with_hex_fallback(&raw)?;
            let current = claude_cred_from_doc(&doc)?;
            (doc, current)
        }
    };
    if current.refresh_token != used_refresh_token {
        return Ok(false);
    }
    if !apply_claude_cred(&mut doc, new) {
        return Err("credentials missing claudeAiOauth".into());
    }
    match source {
        ClaudeSource::File(path) => {
            let out = serde_json::to_string_pretty(&doc).map_err(|e| e.to_string())?;
            if !atomic_write(path, out.as_bytes()) {
                return Err(format!("cannot write {}", path.display()));
            }
        }
        #[cfg(target_os = "macos")]
        ClaudeSource::Keychain => {
            let out = serde_json::to_string(&doc).map_err(|e| e.to_string())?;
            keychain_write(CLAUDE_KEYCHAIN_SERVICE, &out)?;
        }
    }
    Ok(true)
}

fn claude_get(agent: &ureq::Agent, url: &str, token: &str) -> Result<Value, QuotaError> {
    let auth = format!("Bearer {}", token.trim());
    get_json(
        agent,
        url,
        &[
            ("authorization", auth.as_str()),
            ("accept", "application/json"),
            ("content-type", "application/json"),
            ("anthropic-beta", "oauth-2025-04-20"),
            ("user-agent", CLAUDE_UA),
        ],
    )
}

/// 调 OAuth refresh 端点换新 token。400/401 解析 body：`invalid_grant` 才报「需重新登录」，
/// 其它只保留 "HTTP 400/401"。
fn refresh_claude_network(
    agent: &ureq::Agent,
    current: &ClaudeCred,
) -> Result<ClaudeCred, QuotaError> {
    let resp = post_json(
        agent,
        CLAUDE_TOKEN_URL,
        serde_json::json!({
            "client_id": CLAUDE_CLIENT_ID,
            "grant_type": "refresh_token",
            "refresh_token": current.refresh_token,
            "scope": CLAUDE_SCOPES,
        }),
        &[("user-agent", "axios/1.15.2")],
        Some(REFRESH_TIMEOUT),
    )
    .map_err(|mut e| {
        if matches!(e.status, Some(400 | 401)) && is_invalid_grant(e.body.as_deref()) {
            e.message =
                "Session expired (invalid_grant); run `claude` to sign in again".to_string();
        }
        e
    })?;
    let access_token = json_field(&resp, &["access_token"])
        .and_then(Value::as_str)
        .ok_or_else(|| QuotaError::from("refresh response missing access_token"))?
        .to_string();
    let refresh_token = json_field(&resp, &["refresh_token"])
        .and_then(Value::as_str)
        .map(str::to_string)
        .unwrap_or_else(|| current.refresh_token.clone());
    let expires_in = json_field(&resp, &["expires_in"]).and_then(Value::as_f64);
    Ok(ClaudeCred {
        access_token,
        refresh_token,
        expires_at: expires_in.map(|s| now_sec() + s as i64),
    })
}

/// 刷新前先重读凭据：
/// 1. Claude Code 已经换了新的 access token 且未临近过期 → 直接用新的，不发网络请求
///    （避免拿旧 refresh_token 触发 invalid_grant，也避免我们轮换后挤掉运行中的 Claude Code）；
/// 2. 否则用**最新**的 refresh token 请求刷新并写回；
/// 3. 刷新报 invalid_grant 时再重读一次：若 refresh token 在这期间被换过，改用新的。
fn refresh_claude_loaded(
    agent: &ureq::Agent,
    kind: &AccountKind,
    current: &ClaudeLoaded,
) -> Result<ClaudeLoaded, QuotaError> {
    let latest = load_claude(kind)?;
    if latest.cred.access_token != current.cred.access_token
        && !claude_needs_refresh(latest.cred.expires_at, now_sec())
    {
        return Ok(latest);
    }
    if latest.cred.refresh_token.is_empty() {
        return Err(
            "Claude token expired and no refresh token; run `claude` to sign in again".into(),
        );
    }
    match refresh_claude_network(agent, &latest.cred) {
        Ok(new) => {
            match persist_claude(&latest.source, &latest.cred.refresh_token, &new) {
                Ok(true) | Ok(false) => {}
                // 新 token 仍可在本次查询中使用；但旧 refresh token 已被服务端轮换作废，
                // 写回失败要让用户在日志里能看到原因。
                Err(e) => eprintln!("quota: failed to persist refreshed Claude credentials: {e}"),
            }
            Ok(ClaudeLoaded {
                cred: new,
                source: latest.source,
            })
        }
        Err(e) if is_invalid_grant(e.body.as_deref()) => match load_claude(kind) {
            Ok(again)
                if !again.cred.refresh_token.is_empty()
                    && again.cred.refresh_token != latest.cred.refresh_token =>
            {
                Ok(again)
            }
            _ => Err(e),
        },
        Err(e) => Err(e),
    }
}

fn token_hash(token: &str) -> u64 {
    use std::hash::{Hash, Hasher};
    let mut h = std::collections::hash_map::DefaultHasher::new();
    token.hash(&mut h);
    h.finish()
}

/// 套餐（profile）每个 access token 只查一次；失败也缓存（None），不重试。
/// 套餐信息不影响主结果，没必要为它多发一次请求（还容易触发 429）。
fn claude_plan_cached(agent: &ureq::Agent, token: &str) -> Option<String> {
    use std::collections::HashMap;
    use std::sync::{Mutex, OnceLock};
    static CACHE: OnceLock<Mutex<HashMap<u64, Option<String>>>> = OnceLock::new();
    let cache = CACHE.get_or_init(|| Mutex::new(HashMap::new()));
    let key = token_hash(token);
    if let Some(hit) = cache.lock().ok().and_then(|m| m.get(&key).cloned()) {
        return hit;
    }
    let plan = claude_get(agent, CLAUDE_PROFILE_URL, token)
        .ok()
        .and_then(|p| claude_plan(&p));
    if let Ok(mut m) = cache.lock() {
        if m.len() >= 16 {
            m.clear(); // token 轮换产生的旧条目，容量很小，直接清空
        }
        m.insert(key, plan.clone());
    }
    plan
}

/// Claude Code 用量查询（文件 / macOS 钥匙串两种来源共用）。
fn fetch_claude(kind: &AccountKind) -> Result<QuotaResult, QuotaError> {
    let agent = http_agent();
    let mut loaded = load_claude(kind)?;
    let mut refreshed = false;

    // 过期前不足 5 分钟（或已过期）才主动刷新；平时直接用现有 token。
    if claude_needs_refresh(loaded.cred.expires_at, now_sec())
        && !loaded.cred.refresh_token.is_empty()
    {
        refreshed = true; // 只尝试一次：失败后 401 路径不再重复刷新
        match refresh_claude_loaded(&agent, kind, &loaded) {
            Ok(l) => loaded = l,
            // 还没真正过期时，刷新失败不致命：继续用现有 token 试一次查询
            Err(_) if loaded.cred.expires_at.is_some_and(|exp| exp > now_sec()) => {}
            Err(e) => return Err(e),
        }
    }

    let mut usage = claude_get(&agent, CLAUDE_USAGE_URL, &loaded.cred.access_token);
    if usage.as_ref().err().is_some_and(|e| e.is_status(401)) {
        // 401：先重读凭据，Claude Code 可能刚轮换过 token
        if let Ok(latest) = load_claude(kind) {
            if latest.cred.access_token != loaded.cred.access_token {
                loaded = latest;
                usage = claude_get(&agent, CLAUDE_USAGE_URL, &loaded.cred.access_token);
            }
        }
        // 仍然 401 才（至多一次）刷新
        if usage.as_ref().err().is_some_and(|e| e.is_status(401))
            && !refreshed
            && !loaded.cred.refresh_token.is_empty()
        {
            loaded = refresh_claude_loaded(&agent, kind, &loaded)?;
            usage = claude_get(&agent, CLAUDE_USAGE_URL, &loaded.cred.access_token);
        }
    }
    let usage = usage?;

    // 套餐信息失败不影响主结果
    let plan = claude_plan_cached(&agent, &loaded.cred.access_token);
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
        stale: None,
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

/// `ApiKeyMode`：auth.json 里没有 ChatGPT access_token（不是错误）；
/// `Other`：读文件 / 解析失败（是错误）。
enum CodexCredError {
    ApiKeyMode,
    Other(String),
}

fn read_codex_cred(path: &Path) -> Result<CodexCred, CodexCredError> {
    let text = std::fs::read_to_string(path).map_err(|e| CodexCredError::Other(e.to_string()))?;
    let v: Value = serde_json::from_str(&text).map_err(|e| CodexCredError::Other(e.to_string()))?;
    let access = v
        .get("access_token")
        .and_then(Value::as_str)
        .ok_or(CodexCredError::ApiKeyMode)?
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

fn refresh_codex(cred: &mut CodexCred) -> Result<(), QuotaError> {
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

fn fetch_codex(path: &Path) -> Result<QuotaResult, QuotaError> {
    // API-key mode：auth.json 没有 access_token，无法查询订阅配额。
    // 直接返回一个标识性的结果，不当作错误。文件读不到 / 解析失败则是真错误，
    // 不能伪装成 API MODE。
    let mut cred = match read_codex_cred(path) {
        Ok(c) => c,
        Err(CodexCredError::ApiKeyMode) => {
            return Ok(QuotaResult {
                plan: Some("API MODE".into()),
                ..Default::default()
            });
        }
        Err(CodexCredError::Other(e)) => return Err(e.into()),
    };
    let agent = http_agent();

    let mut usage: Result<Value, QuotaError>;
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
            Err(e) if e.is_status(401) && !refreshed && !cred.refresh_token.is_empty() => {
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
        stale: None,
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

fn refresh_grok(cred: &mut GrokCred) -> Result<(), QuotaError> {
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

fn fetch_grok(path: &Path) -> Result<QuotaResult, QuotaError> {
    let mut cred = read_grok_cred(path)?;
    let agent = http_agent();

    let mut body: Result<Value, QuotaError>;
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
            Err(e) if e.is_status(401) && !refreshed && !cred.refresh_token.is_empty() => {
                refresh_grok(&mut cred)?;
                write_back_grok(path, &cred);
                refreshed = true;
                continue;
            }
            _ => break,
        }
    }
    // Grok 未使用过时 billing API 可能返回 4xx（如 404），视为 100% 剩余（0% 已用）。
    // 但网络错误、401/403（凭据问题）、429（限流）、5xx 是真错误，必须向上传，
    // 否则限流/掉线会被显示成「用量 0%」。
    let weekly = match body {
        Ok(v) => v,
        Err(e) if grok_billing_error_means_unused(&e) => {
            return Ok(QuotaResult {
                windows: vec![QuotaWindow {
                    label: WindowLabel::Weekly,
                    used_percent: 0.0,
                    resets_at: None,
                }],
                ..Default::default()
            });
        }
        Err(e) => return Err(e),
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

/// billing 接口对「从未用过」的账号返回的客户端错误（非鉴权、非限流）。
fn grok_billing_error_means_unused(e: &QuotaError) -> bool {
    matches!(e.status, Some(s) if (400..500).contains(&s) && !matches!(s, 401 | 403 | 408 | 429))
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
        stale: None,
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

fn fetch_devin(cookie: &str, org_id: Option<&str>) -> Result<QuotaResult, QuotaError> {
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
        stale: None,
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
fn fetch_devin_cli(token: &str, _org_id: &str) -> Result<QuotaResult, QuotaError> {
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
    let resp = check_status(resp)?;

    // 读取二进制响应体
    let resp_bytes: Vec<u8> = {
        let mut r = resp.into_body();
        r.read_to_vec()
            .map_err(|e: ureq::Error| QuotaError::from(e.to_string()))?
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
        stale: None,
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

    // Go 二进制中的字符串不是 NUL 结尾，`strings` 会把相邻字符串连成一行，
    // 按行匹配不可靠；改为与 Windows 相同的思路：解析指令对字符串的引用。
    #[cfg(target_os = "macos")]
    {
        let mut candidates = vec![PathBuf::from(
            "/Applications/Antigravity.app/Contents/Resources/bin/language_server",
        )];
        if let Some(h) = home() {
            candidates.push(
                h.join("Applications/Antigravity.app/Contents/Resources/bin/language_server"),
            );
        }
        candidates.iter().find_map(|path| {
            std::fs::read(path)
                .ok()
                .and_then(|binary| antigravity_macho_client(&binary))
        })
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

/// macOS arm64 版：Mach-O `__text` 中 Go 用 `adrp + add` 指令对加载字符串地址。
/// 与 `antigravity_windows_client` 相同：仅当 OAuth client_id 引用之后 128 字节
/// 内出现 GOCSPX- 引用时才配对，避免混到单独的 Cloud Auth 客户端。
fn antigravity_macho_client(binary: &[u8]) -> Option<(String, String)> {
    fn u32_at(b: &[u8], p: usize) -> Option<u32> {
        Some(u32::from_le_bytes(b.get(p..p + 4)?.try_into().ok()?))
    }
    fn u64_at(b: &[u8], p: usize) -> Option<u64> {
        Some(u64::from_le_bytes(b.get(p..p + 8)?.try_into().ok()?))
    }
    // Mach-O 64-bit little-endian（arm64/x86_64 thin binary）
    if u32_at(binary, 0)? != 0xFEEDFACF {
        return None;
    }
    let ncmds = u32_at(binary, 16)? as usize;
    let mut sections: Vec<(u64, u64, u64)> = Vec::new(); // (vmaddr, size, fileoff)
    let mut text: Option<(u64, u64, u64)> = None;
    let mut pos = 32usize; // mach_header_64 之后是 load commands
    for _ in 0..ncmds {
        let cmd = u32_at(binary, pos)?;
        let cmdsize = u32_at(binary, pos + 4)? as usize;
        if cmd == 0x19 && cmdsize >= 72 {
            // LC_SEGMENT_64：nsects 在 +64，section_64 数组从 +72 起，每项 80 字节
            let nsects = u32_at(binary, pos + 64)? as usize;
            for i in 0..nsects {
                let s = pos.checked_add(72 + i.checked_mul(80)?)?;
                let name = binary.get(s..s + 16)?;
                let addr = u64_at(binary, s + 32)?;
                let size = u64_at(binary, s + 40)?;
                let fileoff = u64::from(u32_at(binary, s + 48)?);
                sections.push((addr, size, fileoff));
                if name.starts_with(b"__text") {
                    text = Some((addr, size, fileoff));
                }
            }
        }
        pos = pos.checked_add(cmdsize.max(8))?;
    }
    let (text_va, text_size, text_fileoff) = text?;
    let file_offset = |va: u64| -> Option<usize> {
        sections.iter().find_map(|&(addr, size, fileoff)| {
            (va >= addr && va - addr < size)
                .then(|| usize::try_from(fileoff + (va - addr)).ok())
                .flatten()
        })
    };
    let text_start = usize::try_from(text_fileoff).ok()?;
    let text_len = text_start
        .checked_add(usize::try_from(text_size).ok()?)?
        .min(binary.len())
        .saturating_sub(text_start);
    let mut pairs = Vec::new();
    let mut previous_id: Option<(usize, String)> = None;
    let mut offset = 0usize;
    while offset + 8 <= text_len {
        let p = text_start + offset;
        let w1 = u32_at(binary, p)?;
        let w2 = u32_at(binary, p + 4)?;
        offset += 4;
        // ADRP Xd, #imm（bit31=1，bits28-24=10000）
        if w1 & 0x9F00_0000 != 0x9000_0000 {
            continue;
        }
        // ADD Xd2, Xn, #imm12（64 位、sh=0），且 Xn 必须是 ADRP 的目标寄存器
        if w2 & 0xFFC0_0000 != 0x9100_0000 || (w2 >> 5) & 31 != w1 & 31 {
            continue;
        }
        let imm = ((w1 >> 5) & 0x3_FFFF) << 2 | (w1 >> 29) & 3;
        let imm = if imm & (1 << 20) != 0 {
            imm as i64 - (1 << 21)
        } else {
            imm as i64
        };
        let pc = text_va + (offset - 4) as u64;
        let target = (pc & !0xFFF)
            .wrapping_add_signed(imm << 12)
            .wrapping_add(((w2 >> 10) & 0xFFF) as u64);
        let Some(target) = file_offset(target) else {
            continue;
        };
        let Some(text_bytes) = binary.get(target..target.saturating_add(100)) else {
            continue;
        };
        if text_bytes.starts_with(b"1071006060591-") {
            let suffix = b".apps.googleusercontent.com";
            if let Some(end) = text_bytes
                .windows(suffix.len())
                .position(|part| part == suffix)
            {
                previous_id = Some((
                    offset - 4,
                    String::from_utf8(text_bytes[..end + suffix.len()].to_vec()).ok()?,
                ));
            }
        } else if text_bytes.starts_with(b"GOCSPX-") {
            if let Some((id_offset, id)) = &previous_id {
                if offset - 4 - id_offset <= 128 {
                    let secret = std::str::from_utf8(&text_bytes[..35]).ok()?;
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
fn refresh_antigravity(cred: &mut AntigravityCred) -> Result<(), QuotaError> {
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
fn fetch_antigravity(path: &Path, from_keychain: bool) -> Result<QuotaResult, QuotaError> {
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

    let mut body: Result<Value, QuotaError>;
    let mut refreshed = false;
    loop {
        let h = headers(&cred.access_token);
        let refs: Vec<(&str, &str)> = h.iter().map(|(k, v)| (*k, v.as_str())).collect();
        body = post_json(
            &agent,
            ANTIGRAVITY_QUOTA_URL,
            serde_json::json!({}),
            &refs,
            None,
        );
        match &body {
            Err(e) if e.is_status(401) && !refreshed && !cred.refresh_token.is_empty() => {
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
        stale: None,
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
    fmt_duration(diff)
}

/// 时长文案（同 `fmt_countdown` 的格式）。
pub fn fmt_duration(diff: i64) -> String {
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
    fn antigravity_macho_client_pairs_config_references() {
        let mut binary = vec![0u8; 2048];
        // mach_header_64：magic + ncmds=1，load command 从 32 开始
        binary[0..4].copy_from_slice(&0xFEEDFACFu32.to_le_bytes());
        binary[16..20].copy_from_slice(&1u32.to_le_bytes());
        // LC_SEGMENT_64 "__TEXT"：vmaddr=0, fileoff=0, nsects=2
        binary[32..36].copy_from_slice(&0x19u32.to_le_bytes());
        binary[36..40].copy_from_slice(&232u32.to_le_bytes());
        binary[40..46].copy_from_slice(b"__TEXT");
        binary[64..72].copy_from_slice(&2048u64.to_le_bytes()); // vmsize
        binary[80..88].copy_from_slice(&2048u64.to_le_bytes()); // filesize
        binary[96..100].copy_from_slice(&2u32.to_le_bytes());
        // section_64 __text：addr=0, size=512, offset=0
        binary[104..110].copy_from_slice(b"__text");
        binary[136..144].copy_from_slice(&0u64.to_le_bytes());
        binary[144..152].copy_from_slice(&512u64.to_le_bytes());
        binary[152..156].copy_from_slice(&0u32.to_le_bytes());
        // section_64 __cstring：addr=512, size=1536, offset=512
        binary[184..193].copy_from_slice(b"__cstring");
        binary[216..224].copy_from_slice(&512u64.to_le_bytes());
        binary[224..232].copy_from_slice(&1536u64.to_le_bytes());
        binary[232..236].copy_from_slice(&512u32.to_le_bytes());

        let id = format!(
            "{}{}.{}",
            "1071006060591-", "test", "apps.googleusercontent.com"
        )
        .into_bytes();
        let secret = format!("{}{}", "GOCSPX-", "x".repeat(28)).into_bytes();
        binary[600..600 + id.len()].copy_from_slice(&id);
        binary[700..700 + secret.len()].copy_from_slice(&secret);
        // adrp x8, 0 ; add x9, x8, #600 与 adrp x10, 0 ; add x11, x10, #700
        binary[200..204].copy_from_slice(&(0x9000_0008u32).to_le_bytes());
        binary[204..208].copy_from_slice(&(0x9100_0000u32 | 600 << 10 | 8 << 5 | 9).to_le_bytes());
        binary[208..212].copy_from_slice(&(0x9000_000Au32).to_le_bytes());
        binary[212..216]
            .copy_from_slice(&(0x9100_0000u32 | 700 << 10 | 10 << 5 | 11).to_le_bytes());

        let pair = antigravity_macho_client(&binary).unwrap();
        assert_eq!(pair.0, String::from_utf8(id.to_vec()).unwrap());
        assert_eq!(pair.1, String::from_utf8(secret.to_vec()).unwrap());
        binary[212] = 0;
        assert!(antigravity_macho_client(&binary).is_none());
        assert!(antigravity_macho_client(&[0; 10]).is_none());
    }

    #[test]
    #[ignore = "requires installed Antigravity on macOS"]
    fn antigravity_macho_client_reads_installed_binary() {
        let binary =
            std::fs::read("/Applications/Antigravity.app/Contents/Resources/bin/language_server")
                .expect("installed language_server");
        let (id, secret) = antigravity_macho_client(&binary).expect("client pair");
        assert!(id.ends_with(".apps.googleusercontent.com"));
        assert!(secret.starts_with("GOCSPX-"));
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

    #[test]
    fn retry_after_parses_seconds_and_http_date() {
        let now = chrono::Utc
            .with_ymd_and_hms(2026, 10, 10, 12, 0, 0)
            .unwrap();
        assert_eq!(parse_retry_after("120", now), Some(120));
        assert_eq!(parse_retry_after(" 0 ", now), Some(0));
        assert_eq!(
            parse_retry_after("Sat, 10 Oct 2026 12:05:00 GMT", now),
            Some(300)
        );
        // 过去的日期 → 0，而不是负数/下溢
        assert_eq!(
            parse_retry_after("Sat, 10 Oct 2026 11:00:00 GMT", now),
            Some(0)
        );
        assert_eq!(parse_retry_after("", now), None);
        assert_eq!(parse_retry_after("soon", now), None);
        assert_eq!(parse_retry_after("-5", now), None);
    }

    #[test]
    fn quota_error_display_matches_legacy_string() {
        let e = QuotaError::http(429, Some(30), None);
        assert_eq!(e.to_string(), "HTTP 429");
        assert!(e.is_status(429) && !e.is_status(401));
        assert_eq!(String::from(e), "HTTP 429");
        let plain: QuotaError = "boom".into();
        assert_eq!(plain.status, None);
    }

    #[test]
    fn grok_unused_account_only_for_plain_client_errors() {
        let err = |s| QuotaError::http(s, None, None);
        assert!(grok_billing_error_means_unused(&err(404)));
        assert!(grok_billing_error_means_unused(&err(400)));
        for s in [401, 403, 429, 500, 503] {
            assert!(!grok_billing_error_means_unused(&err(s)), "{s}");
        }
        // 网络错误（无状态码）不能被当成 0%
        assert!(!grok_billing_error_means_unused(&"timeout".into()));
    }

    #[test]
    fn codex_unreadable_file_is_error_not_api_mode() {
        let dir = std::env::temp_dir().join(format!("quota-codex-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let missing = dir.join("nope.json");
        assert!(matches!(
            read_codex_cred(&missing),
            Err(CodexCredError::Other(_))
        ));
        let api = dir.join("api.json");
        std::fs::write(&api, r#"{"OPENAI_API_KEY":"sk-x"}"#).unwrap();
        assert!(matches!(
            read_codex_cred(&api),
            Err(CodexCredError::ApiKeyMode)
        ));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[cfg(unix)]
    #[test]
    fn atomic_write_keeps_permissions_and_defaults_to_private() {
        use std::os::unix::fs::PermissionsExt;
        let dir = std::env::temp_dir().join(format!("quota-aw-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let existing = dir.join("cred.json");
        std::fs::write(&existing, "old").unwrap();
        std::fs::set_permissions(&existing, std::fs::Permissions::from_mode(0o640)).unwrap();
        atomic_write(&existing, b"new");
        assert_eq!(std::fs::read_to_string(&existing).unwrap(), "new");
        let mode = std::fs::metadata(&existing).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o640);

        let fresh = dir.join("fresh.json");
        atomic_write(&fresh, b"x");
        let mode = std::fs::metadata(&fresh).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode, 0o600);
        let _ = std::fs::remove_dir_all(&dir);
    }

    const CLAUDE_DOC: &str = r#"{
        "claudeAiOauth": {
            "accessToken": "at-old",
            "refreshToken": "rt-old",
            "expiresAt": 1790000000000,
            "subscriptionType": "max",
            "scopes": ["user:profile"]
        },
        "mcpOAuth": {"x": 1}
    }"#;

    fn temp_dir(tag: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("quota-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn claude_refresh_only_within_five_minutes_of_expiry() {
        let now = 1_000_000;
        assert!(!claude_needs_refresh(None, now));
        assert!(!claude_needs_refresh(Some(now + 301), now));
        assert!(claude_needs_refresh(Some(now + 300), now));
        assert!(claude_needs_refresh(Some(now + 1), now));
        assert!(claude_needs_refresh(Some(now), now));
        assert!(claude_needs_refresh(Some(now - 3600), now));
    }

    #[test]
    fn invalid_grant_detection() {
        assert!(is_invalid_grant(Some(r#"{"error":"invalid_grant"}"#)));
        assert!(is_invalid_grant(Some(
            r#"{"error":"x","error_description":"invalid_grant"}"#
        )));
        assert!(is_invalid_grant(Some(
            r#"{"error":{"type":"invalid_grant","message":"Refresh token not found"}}"#
        )));
        // 其它 400：不算会话过期
        assert!(!is_invalid_grant(Some(r#"{"error":"invalid_request"}"#)));
        assert!(!is_invalid_grant(Some(
            r#"{"error":{"type":"invalid_request_error"}}"#
        )));
        assert!(!is_invalid_grant(Some("<html>Bad gateway</html>")));
        assert!(!is_invalid_grant(Some("")));
        assert!(!is_invalid_grant(None));
    }

    #[test]
    fn claude_cred_parses_ms_expiry_and_hex_fallback() {
        let cred = parse_claude_cred(CLAUDE_DOC).unwrap();
        assert_eq!(cred.access_token, "at-old");
        assert_eq!(cred.refresh_token, "rt-old");
        assert_eq!(cred.expires_at, Some(1_790_000_000));

        let compact: String =
            serde_json::to_string(&serde_json::from_str::<Value>(CLAUDE_DOC).unwrap()).unwrap();
        let hex: String = compact.bytes().map(|b| format!("{b:02x}")).collect();
        assert_eq!(parse_claude_cred(&hex).unwrap(), cred);

        assert!(parse_claude_cred("not json").is_err());
        assert!(parse_claude_cred(r#"{"other":1}"#).is_err());
        assert!(parse_claude_cred(r#"{"claudeAiOauth":{"accessToken":"  "}}"#).is_err());
    }

    #[test]
    fn apply_claude_cred_keeps_other_fields() {
        let mut doc: Value = serde_json::from_str(CLAUDE_DOC).unwrap();
        let new = ClaudeCred {
            access_token: "at-new".into(),
            refresh_token: "rt-new".into(),
            expires_at: Some(1_800_000_000),
        };
        assert!(apply_claude_cred(&mut doc, &new));
        assert_eq!(doc["claudeAiOauth"]["accessToken"], "at-new");
        assert_eq!(doc["claudeAiOauth"]["refreshToken"], "rt-new");
        assert_eq!(doc["claudeAiOauth"]["expiresAt"], 1_800_000_000_000i64);
        assert_eq!(doc["claudeAiOauth"]["subscriptionType"], "max");
        assert_eq!(doc["mcpOAuth"]["x"], 1);
        assert!(!apply_claude_cred(&mut serde_json::json!({}), &new));
    }

    #[test]
    fn persist_claude_skips_when_claude_code_rotated_first() {
        let dir = temp_dir("persist");
        let path = dir.join(".credentials.json");
        std::fs::write(&path, CLAUDE_DOC).unwrap();
        let source = ClaudeSource::File(path.clone());
        let new = ClaudeCred {
            access_token: "at-new".into(),
            refresh_token: "rt-new".into(),
            expires_at: Some(1_800_000_000),
        };

        // 我们刷新时用的 refresh token 已不是文件里的 → 不覆盖
        assert_eq!(persist_claude(&source, "rt-stale", &new), Ok(false));
        assert_eq!(
            parse_claude_cred(&std::fs::read_to_string(&path).unwrap())
                .unwrap()
                .access_token,
            "at-old"
        );

        // 一致 → 写入，且保留其它字段
        assert_eq!(persist_claude(&source, "rt-old", &new), Ok(true));
        let written: Value =
            serde_json::from_str(&std::fs::read_to_string(&path).unwrap()).unwrap();
        assert_eq!(written["claudeAiOauth"]["accessToken"], "at-new");
        assert_eq!(written["claudeAiOauth"]["scopes"][0], "user:profile");
        assert_eq!(written["mcpOAuth"]["x"], 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn load_claude_rereads_file_every_time() {
        let dir = temp_dir("reread");
        let path = dir.join(".credentials.json");
        std::fs::write(&path, CLAUDE_DOC).unwrap();
        let kind = AccountKind::ClaudeLocal { path: path.clone() };
        assert_eq!(load_claude(&kind).unwrap().cred.access_token, "at-old");
        std::fs::write(&path, CLAUDE_DOC.replace("at-old", "at-rotated")).unwrap();
        assert_eq!(load_claude(&kind).unwrap().cred.access_token, "at-rotated");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn keychain_account_is_parsed_from_attributes() {
        let out = "keychain: \"/Users/a/Library/Keychains/login.keychain-db\"\nclass: \"genp\"\nattributes:\n    0x00000007 <blob>=\"Claude Code-credentials\"\n    \"acct\"<blob>=\"alice\"\n    \"svce\"<blob>=\"Claude Code-credentials\"\n";
        assert_eq!(parse_keychain_account(out), Some("alice".into()));
        assert_eq!(parse_keychain_account("    \"acct\"<blob>=<NULL>\n"), None);
        assert_eq!(parse_keychain_account("nothing here"), None);
    }

    fn sample_result(tag: &str) -> QuotaResult {
        QuotaResult {
            plan: Some(tag.to_string()),
            ..Default::default()
        }
    }

    fn http_err_with(status: u16, retry_after: Option<u64>) -> QuotaError {
        QuotaError::http(status, retry_after, None)
    }

    fn secs(n: u64) -> Duration {
        Duration::from_secs(n)
    }

    #[test]
    fn cache_ttl_serves_fresh_result_until_expiry_unless_forced() {
        let t0 = std::time::Instant::now();
        let mut state = AccountState::default();
        assert!(matches!(decide(&state, t0, false), Decision::Fetch));
        record(&mut state, t0, Ok(sample_result("a"))).unwrap();

        // TTL 内：直接返回缓存，且没有 stale 标记
        match decide(&state, t0 + CACHE_TTL - secs(1), false) {
            Decision::Done(Ok(r)) => {
                assert_eq!(r.plan.as_deref(), Some("a"));
                assert!(r.stale.is_none());
            }
            _ => panic!("expected cache hit"),
        }
        // 手动刷新绕过缓存
        assert!(matches!(
            decide(&state, t0 + secs(1), true),
            Decision::Fetch
        ));
        // TTL 过后重新查询
        assert!(matches!(
            decide(&state, t0 + CACHE_TTL, false),
            Decision::Fetch
        ));
    }

    #[test]
    fn rate_limit_sets_cooldown_from_retry_after_and_serves_last_result() {
        let t0 = std::time::Instant::now();
        let mut state = AccountState::default();
        record(&mut state, t0, Ok(sample_result("good"))).unwrap();

        let t1 = t0 + CACHE_TTL + secs(10);
        let r = record(&mut state, t1, Err(http_err_with(429, Some(120)))).unwrap();
        assert_eq!(r.plan.as_deref(), Some("good"));
        let stale = r.stale.expect("stale marker");
        assert_eq!(
            stale.reason,
            StaleReason::RateLimited { retry_in_secs: 120 }
        );
        assert_eq!(stale.age_secs, (CACHE_TTL + secs(10)).as_secs());

        // 冷却中：普通刷新和手动刷新都不发请求，返回上次结果
        for force in [false, true] {
            match decide(&state, t1 + secs(30), force) {
                Decision::Done(Ok(r)) => {
                    assert_eq!(r.plan.as_deref(), Some("good"));
                    assert_eq!(
                        r.stale.unwrap().reason,
                        StaleReason::RateLimited { retry_in_secs: 90 }
                    );
                }
                _ => panic!("cooldown must short-circuit (force={force})"),
            }
        }
        // 冷却结束后恢复请求
        assert!(matches!(
            decide(&state, t1 + secs(120), true),
            Decision::Fetch
        ));
    }

    #[test]
    fn rate_limit_defaults_to_five_minutes_and_is_clamped() {
        let t0 = std::time::Instant::now();
        let mut state = AccountState::default();
        // 没有 Retry-After → 5 分钟；没有上次结果 → 返回 429 错误
        let err = record(&mut state, t0, Err(http_err_with(429, None))).unwrap_err();
        assert!(err.is_status(429));
        assert_eq!(state.cooldown_until, Some(t0 + RATE_LIMIT_DEFAULT_COOLDOWN));
        match decide(&state, t0 + secs(10), false) {
            Decision::Done(Err(e)) => {
                assert!(e.is_status(429));
                assert_eq!(e.to_string(), "HTTP 429 (retry in 5m)");
            }
            _ => panic!("expected 429 error during cooldown"),
        }

        // Retry-After: 0 不会变成「立刻重试」；过大的值被限制在上限
        let mut s2 = AccountState::default();
        let _ = record(&mut s2, t0, Err(http_err_with(429, Some(0))));
        assert_eq!(s2.cooldown_until, Some(t0 + FAILURE_BACKOFF));
        let mut s3 = AccountState::default();
        let _ = record(&mut s3, t0, Err(http_err_with(429, Some(999_999))));
        assert_eq!(s3.cooldown_until, Some(t0 + RATE_LIMIT_MAX_COOLDOWN));
    }

    #[test]
    fn failure_backs_off_for_60s_but_manual_refresh_bypasses() {
        let t0 = std::time::Instant::now();
        let mut state = AccountState::default();
        record(&mut state, t0, Ok(sample_result("good"))).unwrap();

        let t1 = t0 + CACHE_TTL + secs(1);
        let r = record(&mut state, t1, Err("HTTP 500".into())).unwrap();
        assert_eq!(
            r.stale.unwrap().reason,
            StaleReason::Error("HTTP 500".into())
        );

        // 退避期内：自动刷新直接回上次结果；手动刷新放行；60 秒后放行
        match decide(&state, t1 + secs(59), false) {
            Decision::Done(Ok(r)) => assert!(r.stale.is_some()),
            _ => panic!("expected backoff"),
        }
        assert!(matches!(
            decide(&state, t1 + secs(5), true),
            Decision::Fetch
        ));
        assert!(matches!(
            decide(&state, t1 + secs(60), false),
            Decision::Fetch
        ));

        // 没有上次结果时，退避期内返回同一个错误
        let mut empty = AccountState::default();
        let _ = record(&mut empty, t0, Err("boom".into()));
        match decide(&empty, t0 + secs(5), false) {
            Decision::Done(Err(e)) => assert_eq!(e.to_string(), "boom"),
            _ => panic!("expected cached error"),
        }
        // 成功后清除退避/冷却
        record(&mut empty, t0 + secs(70), Ok(sample_result("ok"))).unwrap();
        assert!(empty.backoff.is_none() && empty.cooldown_until.is_none());
    }

    #[test]
    fn stale_results_expire_after_max_age() {
        let t0 = std::time::Instant::now();
        let mut state = AccountState::default();
        record(&mut state, t0, Ok(sample_result("old"))).unwrap();
        let late = t0 + MAX_STALE_AGE + secs(1);
        let err = record(&mut state, late, Err("HTTP 503".into())).unwrap_err();
        assert_eq!(err.to_string(), "HTTP 503");
    }

    #[test]
    fn cached_fetch_dedupes_and_respects_force_and_cooldown() {
        let entry = AccountEntry {
            fingerprint: 1,
            fetch_lock: std::sync::Mutex::new(()),
            state: std::sync::Mutex::new(AccountState::default()),
        };
        let calls = std::cell::Cell::new(0);
        let ok = |tag: &'static str| {
            let calls = &calls;
            move || {
                calls.set(calls.get() + 1);
                Ok(sample_result(tag))
            }
        };
        assert_eq!(
            cached_fetch(&entry, false, ok("a"))
                .unwrap()
                .plan
                .as_deref(),
            Some("a")
        );
        // 第二次命中缓存，闭包不会被调用
        assert_eq!(
            cached_fetch(&entry, false, ok("b"))
                .unwrap()
                .plan
                .as_deref(),
            Some("a")
        );
        assert_eq!(calls.get(), 1);
        // force 绕过缓存
        assert_eq!(
            cached_fetch(&entry, true, ok("c")).unwrap().plan.as_deref(),
            Some("c")
        );
        assert_eq!(calls.get(), 2);
        // 429 → 进入冷却，之后 force 也不发请求，返回上次结果并标记 stale
        let limited = cached_fetch(&entry, true, || Err(http_err_with(429, Some(300)))).unwrap();
        assert!(limited.stale.is_some());
        let again = cached_fetch(&entry, true, ok("d")).unwrap();
        assert_eq!(again.plan.as_deref(), Some("c"));
        assert!(again.stale.is_some());
        assert_eq!(calls.get(), 2);
    }

    #[test]
    fn account_entry_resets_when_credentials_change() {
        let a = account_entry("test-key-fp", 10);
        a.state.lock().unwrap().cooldown_until = Some(std::time::Instant::now() + secs(100));
        assert!(std::sync::Arc::ptr_eq(
            &a,
            &account_entry("test-key-fp", 10)
        ));
        let b = account_entry("test-key-fp", 11);
        assert!(!std::sync::Arc::ptr_eq(&a, &b));
        assert!(b.state.lock().unwrap().cooldown_until.is_none());

        let k1 = AccountKind::Devin {
            cookie: "c1".into(),
            org_id: None,
        };
        let k2 = AccountKind::Devin {
            cookie: "c2".into(),
            org_id: None,
        };
        assert_ne!(account_fingerprint(&k1), account_fingerprint(&k2));
        assert_eq!(account_fingerprint(&k1), account_fingerprint(&k1));
    }

    #[test]
    fn parallel_map_runs_concurrently_and_keeps_order() {
        // 每个任务睡 200ms；串行需要 ≥1s，并发应远小于
        let items: Vec<u64> = (0..5).collect();
        let started = std::time::Instant::now();
        let out = parallel_map(&items, |n| {
            std::thread::sleep(Duration::from_millis(200));
            n * 10
        });
        assert!(started.elapsed() < Duration::from_millis(800));
        assert_eq!(out, vec![Some(0), Some(10), Some(20), Some(30), Some(40)]);
    }

    #[test]
    fn retry_in_formatting() {
        assert_eq!(fmt_retry_in(30), "30s");
        assert_eq!(fmt_retry_in(61), "2m");
        assert_eq!(fmt_retry_in(300), "5m");
    }

    /// 本地起一个只应答 `responses.len()` 次的 HTTP 服务，返回 base url。
    fn serve_canned(responses: Vec<String>) -> String {
        use std::io::{Read, Write};
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        std::thread::spawn(move || {
            for resp in responses {
                let Ok((mut stream, _)) = listener.accept() else {
                    return;
                };
                let mut buf = [0u8; 4096];
                let _ = stream.read(&mut buf);
                let _ = stream.write_all(resp.as_bytes());
            }
        });
        format!("http://{addr}")
    }

    fn canned(status: &str, headers: &str, body: &str) -> String {
        format!(
            "HTTP/1.1 {status}\r\n{headers}Content-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        )
    }

    #[test]
    fn http_error_keeps_status_retry_after_and_body() {
        let base = serve_canned(vec![
            canned("429 Too Many Requests", "Retry-After: 42\r\n", "{\"e\":1}"),
            canned(
                "400 Bad Request",
                "",
                r#"{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}"#,
            ),
            canned("200 OK", "", r#"{"ok":true}"#),
        ]);
        let agent = http_agent();

        let e = get_json(&agent, &format!("{base}/usage"), &[]).unwrap_err();
        assert_eq!(e.to_string(), "HTTP 429");
        assert_eq!(e.status, Some(429));
        assert_eq!(e.retry_after_secs, Some(42));
        assert_eq!(e.body.as_deref(), Some("{\"e\":1}"));

        // refresh 端点的 400：body 里是 invalid_grant → 报会话过期；message 区分于普通 "HTTP 400"
        let e = post_json(
            &agent,
            &format!("{base}/token"),
            serde_json::json!({}),
            &[],
            Some(Duration::from_secs(5)),
        )
        .unwrap_err();
        assert_eq!(e.status, Some(400));
        assert!(is_invalid_grant(e.body.as_deref()));

        let ok = get_json(&agent, &format!("{base}/ok"), &[]).unwrap();
        assert_eq!(ok["ok"], true);
    }
}
