//! 多设备数据同步：通过 GitHub 登录后的自建同步 API 交换数据包。
//!
//! 工作方式：
//! - 用户在设置中填入同步 API 地址并登录 GitHub 后，后台每 `SYNC_INTERVAL_SECS` 秒自动：
//!   1) 本机数据有变化时才发布内容寻址 block + manifest；内容没变则跳过
//!   2) 扫描后端中其他设备的数据包并合并进当前数据
//! - 同步默认关闭，用户可在侧边栏的同步开关处一键开启/关闭。
//! - 服务端以 GitHub 用户 ID 隔离数据；同一个 GitHub 账号在不同设备登录后共享同一份同步数据。

use crate::data::{self, LoadedData, SessionRec, TurnRec};
use sha2::{Digest, Sha256};
use std::collections::HashSet;
use std::fs;
use std::hash::Hasher;
#[cfg(test)]
use std::io::BufWriter;
use std::io::Write;
use std::path::PathBuf;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

mod v2;
mod v3;

/// 自动同步间隔（秒）。
/// v3 使用内容寻址小 block；无变化时上传为零。仍限制为每小时一次，避免同步 API
/// 被频繁 list/head 请求触发限流。
pub const SYNC_INTERVAL_SECS: u64 = 60 * 60;
const TRANSIENT_FAILURE_COOLDOWN_SECS: u64 = 30 * 60;

/// WebDAV 单个响应体上限。同步包可能远大于 ureq 默认的 10 MiB；仍保留
/// 有界限制，避免异常或恶意服务端耗尽内存。
const MAX_WEBDAV_RESPONSE_BYTES: u64 = 256 * 1024 * 1024;

/// 同步配置：只有开关和服务端 API 地址，持久化到 sync.json。
#[derive(Debug, Clone, Default)]
pub struct SyncConfig {
    pub enabled: bool,
    /// 自建同步 API 基础地址，例如 `https://sync.example.com`。
    pub api_url: String,
}

/// 读取同步配置。文件不存在或解析失败时返回默认值（同步关闭、用默认目录）。
pub fn read_config() -> SyncConfig {
    let path = match config_file_path() {
        Some(p) => p,
        None => return SyncConfig::default(),
    };
    let text = match fs::read_to_string(&path) {
        Ok(t) => t,
        Err(_) => return SyncConfig::default(),
    };
    #[derive(serde::Deserialize)]
    struct RawConfig {
        #[serde(default)]
        enabled: bool,
        #[serde(default)]
        api_url: String,
        /// 迁移旧配置时继续使用原自建服务地址，但不再保留其余后端配置。
        #[serde(default)]
        sync_server_url: String,
    }
    match serde_json::from_str::<RawConfig>(&text) {
        Ok(raw) => SyncConfig {
            enabled: raw.enabled,
            api_url: if raw.api_url.trim().is_empty() {
                raw.sync_server_url
            } else {
                raw.api_url
            },
        },
        Err(_) => SyncConfig::default(),
    }
}

/// 持久化同步配置。
pub fn save_config(config: &SyncConfig) {
    let Some(path) = config_file_path() else {
        return;
    };
    if let Some(parent) = path.parent() {
        let _ = fs::create_dir_all(parent);
    }
    let payload = serde_json::json!({
        "enabled": config.enabled,
        "api_url": config.api_url,
    })
    .to_string();
    let temp = path.with_extension(format!("json.tmp-{}", std::process::id()));
    if fs::write(&temp, &payload).is_ok() {
        let _ = fs::rename(&temp, &path);
    } else {
        let _ = fs::remove_file(&temp);
    }
}

pub(crate) fn destination_key(cfg: &SyncConfig) -> String {
    format!("github:{}", cfg.api_url.trim_end_matches('/'))
}

/// 同步是否已开启。
pub fn is_enabled() -> bool {
    read_config().enabled
}

fn now_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

fn cooldown_path() -> Option<PathBuf> {
    let cfg = read_config();
    let key = sha256_hex(destination_key(&cfg).as_bytes());
    dirs::config_dir().map(|dir| dir.join(format!("devin-usage-metrics/sync-cooldown-{key}")))
}

/// 503/502/504 后持久化冷却，防止用户反复点击或重启应用继续打满服务端。
pub fn record_transient_failure(error: &str) {
    if !["HTTP 502", "HTTP 503", "HTTP 504"]
        .iter()
        .any(|value| error.contains(value))
    {
        return;
    }
    let Some(path) = cooldown_path() else { return };
    if let Some(parent) = path.parent() {
        let _ = fs::create_dir_all(parent);
    }
    let until = now_secs().saturating_add(TRANSIENT_FAILURE_COOLDOWN_SECS);
    let _ = atomic_replace(&path, until.to_string().as_bytes());
}

pub fn cooldown_remaining() -> Option<Duration> {
    let until = fs::read_to_string(cooldown_path()?)
        .ok()?
        .trim()
        .parse::<u64>()
        .ok()?;
    let now = now_secs();
    if until > now {
        Some(Duration::from_secs(until - now))
    } else {
        None
    }
}

pub fn clear_transient_failure() {
    if let Some(path) = cooldown_path() {
        let _ = fs::remove_file(path);
    }
}

/// 配置文件路径，与 device.json 同目录。
fn config_file_path() -> Option<PathBuf> {
    dirs::config_dir().map(|d| d.join("devin-usage-metrics/sync.json"))
}

#[allow(dead_code)]
fn export_hash_path() -> Option<PathBuf> {
    dirs::config_dir().map(|d| d.join("devin-usage-metrics/export.hash"))
}

#[allow(dead_code)]
fn read_last_export_meta() -> Option<(String, String)> {
    let path = export_hash_path()?;
    let text = fs::read_to_string(path).ok()?;
    let mut lines = text.lines();
    let dest = lines.next()?.trim().to_string();
    let hash = lines.next()?.trim().to_string();
    if dest.is_empty() || hash.is_empty() {
        return None;
    }
    Some((dest, hash))
}

#[allow(dead_code)]
fn write_last_export_meta(dest: &str, hash: &str) {
    let Some(path) = export_hash_path() else {
        return;
    };
    if let Some(parent) = path.parent() {
        let _ = fs::create_dir_all(parent);
    }
    let _ = fs::write(path, format!("{dest}\n{hash}\n"));
}

/// 不含 `exported_at`：时间戳每次都会变，不能拿来判断数据有没有变。
#[allow(dead_code)]
fn content_fingerprint(package: &DevicePackage) -> Result<String, String> {
    #[derive(serde::Serialize)]
    struct Fingerprint<'a> {
        schema_version: u32,
        device_id: &'a str,
        device_name: &'a str,
        turns_start: i64,
        turns_end: i64,
        sessions: &'a [SessionRec],
        turns: &'a [TurnRec],
    }
    let bytes = serde_json::to_vec(&Fingerprint {
        schema_version: package.schema_version,
        device_id: &package.device_id,
        device_name: &package.device_name,
        turns_start: package.turns_start,
        turns_end: package.turns_end,
        sessions: &package.sessions,
        turns: &package.turns,
    })
    .map_err(|e| format!("序列化指纹失败: {e}"))?;
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    hasher.write(&bytes);
    Ok(format!("{:016x}", hasher.finish()))
}

// ── GitHub 同步会话（系统钥匙串）──────────────────────────────────────

const KEYRING_SERVICE: &str = "devin-usage-metrics";
const KEYRING_GITHUB_SYNC_SESSION: &str = "github-sync-session";

/// 服务端在 GitHub Device Flow 成功后签发的同步会话；GitHub access token 不会写入本机。
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct GitHubSyncSession {
    pub sync_token: String,
    pub login: String,
    pub expires_at: i64,
}

pub fn save_github_sync_session(session: &GitHubSyncSession) -> Result<(), String> {
    let payload = serde_json::to_string(session).map_err(|e| format!("会话序列化失败: {e}"))?;
    let entry = keyring::Entry::new(KEYRING_SERVICE, KEYRING_GITHUB_SYNC_SESSION)
        .map_err(|e| format!("钥匙串初始化失败: {e}"))?;
    entry
        .set_password(&payload)
        .map_err(|e| format!("GitHub 同步会话存储失败: {e}"))
}

pub fn load_github_sync_session() -> Option<GitHubSyncSession> {
    let entry = keyring::Entry::new(KEYRING_SERVICE, KEYRING_GITHUB_SYNC_SESSION).ok()?;
    serde_json::from_str(&entry.get_password().ok()?).ok()
}

pub fn delete_github_sync_session() {
    if let Ok(entry) = keyring::Entry::new(KEYRING_SERVICE, KEYRING_GITHUB_SYNC_SESSION) {
        let _ = entry.delete_credential();
    }
}

pub fn github_sync_session_is_valid() -> bool {
    load_github_sync_session()
        .map(|session| session.expires_at > chrono::Utc::now().timestamp())
        .unwrap_or(false)
}

/// GitHub Device Flow 的一次待完成授权。客户端只展示验证码并轮询自建 API，
/// 不会接触或保存 GitHub access token。
#[derive(Debug, Clone, serde::Deserialize)]
pub struct GitHubDeviceAuthorization {
    pub device_code: String,
    pub user_code: String,
    pub verification_uri: String,
    pub expires_in: u64,
    pub interval: u64,
}

fn sync_api_base(api_url: &str) -> Result<String, String> {
    let base = api_url.trim().trim_end_matches('/');
    if !(base.starts_with("https://") || base.starts_with("http://")) {
        return Err("同步 API 地址必须以 http:// 或 https:// 开头".into());
    }
    Ok(base.to_string())
}

fn sync_api_agent() -> ureq::Agent {
    ureq::Agent::config_builder()
        .timeout_global(Some(Duration::from_secs(30)))
        .http_status_as_error(false)
        .build()
        .new_agent()
}

fn sync_api_error(status: u16, body: &[u8], action: &str) -> String {
    let detail = String::from_utf8_lossy(body)
        .trim()
        .chars()
        .take(300)
        .collect::<String>();
    if detail.is_empty() {
        format!("同步 API {action}返回 HTTP {status}")
    } else {
        format!("同步 API {action}返回 HTTP {status}: {detail}")
    }
}

/// 向自建 API 获取 GitHub Device Flow 验证码。
pub fn begin_github_login(api_url: &str) -> Result<GitHubDeviceAuthorization, String> {
    let base = sync_api_base(api_url)?;
    let request = ureq::http::Request::builder()
        .method("POST")
        .uri(format!("{base}/v1/auth/github/device"))
        .body(Vec::new())
        .map_err(|e| format!("构造 GitHub 登录请求失败: {e}"))?;
    let response = sync_api_agent()
        .run(request)
        .map_err(|e| format!("启动 GitHub 登录失败: {e}"))?;
    let status = response.status().as_u16();
    let body = WebDavTransport::read_body(response)?;
    if status != 200 {
        return Err(sync_api_error(status, &body, "启动 GitHub 登录"));
    }
    let mut authorization: GitHubDeviceAuthorization =
        serde_json::from_slice(&body).map_err(|e| format!("解析 GitHub 登录响应失败: {e}"))?;
    if authorization.device_code.is_empty()
        || authorization.user_code.is_empty()
        || authorization.verification_uri.is_empty()
    {
        return Err("同步 API 返回的 GitHub 登录信息不完整".into());
    }
    authorization.interval = authorization.interval.max(1);
    authorization.expires_in = authorization.expires_in.max(60);
    Ok(authorization)
}

/// 轮询自建 API，直到用户在浏览器完成 GitHub 登录或设备验证码过期。
pub fn wait_for_github_login(
    api_url: &str,
    authorization: &GitHubDeviceAuthorization,
) -> Result<GitHubSyncSession, String> {
    let base = sync_api_base(api_url)?;
    let deadline = Instant::now() + Duration::from_secs(authorization.expires_in);
    let mut interval = authorization.interval.max(1);
    loop {
        if Instant::now() >= deadline {
            return Err("GitHub 登录验证码已过期，请重新登录".into());
        }
        let body = serde_json::to_vec(&serde_json::json!({
            "device_code": authorization.device_code,
        }))
        .map_err(|e| format!("构造 GitHub 登录请求失败: {e}"))?;
        let request = ureq::http::Request::builder()
            .method("POST")
            .uri(format!("{base}/v1/auth/github/token"))
            .header("Content-Type", "application/json")
            .body(body)
            .map_err(|e| format!("构造 GitHub 登录请求失败: {e}"))?;
        let response = sync_api_agent()
            .run(request)
            .map_err(|e| format!("等待 GitHub 登录失败: {e}"))?;
        let status = response.status().as_u16();
        if status == 202 {
            if let Some(retry_after) = response
                .headers()
                .get("retry-after")
                .and_then(|value| value.to_str().ok())
                .and_then(|value| value.parse::<u64>().ok())
            {
                interval = retry_after.max(1);
            }
            let _ = WebDavTransport::read_body(response)?;
            std::thread::sleep(Duration::from_secs(interval));
            continue;
        }
        let response_body = WebDavTransport::read_body(response)?;
        if status != 200 {
            return Err(sync_api_error(status, &response_body, "完成 GitHub 登录"));
        }
        let session: GitHubSyncSession = serde_json::from_slice(&response_body)
            .map_err(|e| format!("解析 GitHub 登录结果失败: {e}"))?;
        if session.sync_token.is_empty()
            || session.login.is_empty()
            || session.expires_at <= chrono::Utc::now().timestamp()
        {
            return Err("同步 API 返回了无效的登录会话".into());
        }
        return Ok(session);
    }
}

// ── 传输层 trait ────────────────────────────────────────────────────────

/// 同步传输层：把数据包写入/读取自同步后端。
pub type HeadRead = Option<(Vec<u8>, Option<String>)>;

pub trait SyncTransport: Sync {
    /// 写入（或覆盖）一个数据包。name 是文件名如 `device-xxx.json`。
    fn put(&self, name: &str, data: &[u8]) -> Result<(), String>;
    /// 读取一个数据包。不存在时返回 Err。
    fn get(&self, name: &str) -> Result<Vec<u8>, String>;
    /// 按调用方声明的协议上限读取，避免小型 v2 对象走整包 256MiB 上限。
    fn get_limited(&self, name: &str, limit: u64) -> Result<Vec<u8>, String>;
    /// 判断数据包是否存在，不下载文件正文。
    fn exists(&self, name: &str) -> Result<bool, String>;
    /// 列出后端中所有匹配 `device-*.json` 的文件名。
    fn list_device_packages(&self) -> Result<Vec<String>, String>;

    fn read_head(&self, name: &str) -> Result<HeadRead, String>;
    fn put_immutable(&self, name: &str, data: &[u8]) -> Result<bool, String>;
    fn cas_head(&self, name: &str, data: &[u8], revision: Option<&str>) -> Result<(), String>;
    fn list_sync_files(&self) -> Result<Vec<String>, String>;
}

/// 本地文件系统传输层，仅保留给协议回归测试。
#[cfg(test)]
struct LocalTransport {
    dir: PathBuf,
}

#[cfg(test)]
impl LocalTransport {
    fn new(dir: PathBuf) -> Self {
        Self { dir }
    }
}

#[cfg(test)]
impl SyncTransport for LocalTransport {
    fn put(&self, name: &str, data: &[u8]) -> Result<(), String> {
        fs::create_dir_all(&self.dir).map_err(|e| format!("创建同步目录失败: {e}"))?;
        let path = self.dir.join(name);
        let temp = path.with_extension(format!("json.tmp-{}", std::process::id()));
        let file = fs::File::create(&temp).map_err(|e| format!("创建临时文件失败: {e}"))?;
        let mut writer = BufWriter::new(file);
        writer
            .write_all(data)
            .map_err(|e| format!("写入失败: {e}"))?;
        writer.flush().map_err(|e| format!("刷新失败: {e}"))?;
        fs::rename(&temp, &path).map_err(|e| format!("重命名失败: {e}"))
    }

    fn get(&self, name: &str) -> Result<Vec<u8>, String> {
        let path = self.dir.join(name);
        fs::read(&path).map_err(|e| format!("读取失败: {e}"))
    }

    fn get_limited(&self, name: &str, limit: u64) -> Result<Vec<u8>, String> {
        let path = self.dir.join(name);
        let size = fs::metadata(&path)
            .map_err(|e| format!("读取元数据失败: {e}"))?
            .len();
        if size > limit {
            return Err(format!("{name} 超过协议大小上限: {size} > {limit}"));
        }
        fs::read(path).map_err(|e| format!("读取失败: {e}"))
    }

    fn exists(&self, name: &str) -> Result<bool, String> {
        match fs::metadata(self.dir.join(name)) {
            Ok(metadata) => Ok(metadata.is_file()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
            Err(error) => Err(format!("检查文件失败: {error}")),
        }
    }

    fn list_device_packages(&self) -> Result<Vec<String>, String> {
        let entries = match fs::read_dir(&self.dir) {
            Ok(e) => e,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(format!("读取同步目录失败: {error}")),
        };
        let mut names = Vec::new();
        for entry in entries.flatten() {
            let path = entry.path();
            let fname = path.file_name().and_then(|n| n.to_str()).unwrap_or("");
            if fname.starts_with("device-") && fname.ends_with(".json") && !fname.contains(".tmp-")
            {
                names.push(fname.to_string());
            }
        }
        Ok(names)
    }

    fn read_head(&self, name: &str) -> Result<HeadRead, String> {
        match fs::read(self.dir.join(name)) {
            Ok(bytes) => Ok(Some((bytes.clone(), Some(sha256_hex(&bytes))))),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(format!("读取 head 失败: {e}")),
        }
    }

    fn put_immutable(&self, name: &str, data: &[u8]) -> Result<bool, String> {
        fs::create_dir_all(&self.dir).map_err(|e| format!("创建同步目录失败: {e}"))?;
        let path = self.dir.join(name);
        if path.exists() {
            let existing = fs::read(&path).map_err(|e| format!("校验不可变对象失败: {e}"))?;
            if existing == data {
                return Ok(false);
            }
            atomic_replace(&path, data)?;
            return Ok(true);
        }
        atomic_replace(&path, data)?;
        Ok(true)
    }

    fn cas_head(&self, name: &str, data: &[u8], revision: Option<&str>) -> Result<(), String> {
        let current = self.read_head(name)?;
        if current.as_ref().and_then(|(_, r)| r.as_deref()) != revision {
            return Err("CAS_CONFLICT".into());
        }
        atomic_replace(&self.dir.join(name), data)
    }

    fn list_sync_files(&self) -> Result<Vec<String>, String> {
        list_local_sync_files(&self.dir)
    }
}

/// WebDAV 传输层：通过 HTTP PUT/GET/PROPFIND 与 WebDAV 服务器交互。
struct WebDavTransport {
    base_url: String,
    username: String,
    password: String,
    agent: ureq::Agent,
}

impl WebDavTransport {
    fn auth_header(&self) -> String {
        let encoded = base64_encode(&format!("{}:{}", self.username, self.password));
        format!("Basic {encoded}")
    }

    fn join(&self, name: &str) -> String {
        let base = self.base_url.trim_end_matches('/');
        let name = name.trim_start_matches('/');
        format!("{base}/{name}")
    }

    fn read_body(resp: ureq::http::Response<ureq::Body>) -> Result<Vec<u8>, String> {
        Self::read_body_limited(resp, MAX_WEBDAV_RESPONSE_BYTES)
    }

    fn read_body_limited(
        resp: ureq::http::Response<ureq::Body>,
        limit: u64,
    ) -> Result<Vec<u8>, String> {
        resp.into_body()
            .into_with_config()
            .limit(limit)
            .read_to_vec()
            .map_err(|e| format!("读取响应失败: {e}"))
    }

    fn status_err(op: &str, status: u16) -> String {
        let hint = match status {
            401 | 403 => "（认证失败，请检查用户名和应用密码）",
            404 => "（请确认地址指向已存在的文件夹，并以 / 结尾）",
            405 => "（服务器不允许该操作）",
            413 => "（文件太大）",
            507 => "（网盘空间不足）",
            _ => "",
        };
        format!("{op} 返回 HTTP {status}{hint}")
    }

    fn retryable_status(status: u16) -> bool {
        matches!(status, 502..=504)
    }

    fn run(
        &self,
        method: &str,
        url: &str,
        body: &[u8],
        extra: &[(&str, &str)],
    ) -> Result<(u16, Vec<u8>), String> {
        // 坚果云等 WebDAV 服务会偶发 503。这里涉及的 GET/HEAD/PROPFIND/PUT
        // 均可安全重试；PUT 要么是内容寻址对象，要么带 CAS 条件，重复请求不会
        // 造成重复记录。退避只在后台同步线程中发生，不阻塞界面线程。
        const RETRY_DELAYS: [Duration; 2] = [Duration::from_secs(1), Duration::from_secs(3)];
        for (attempt, delay) in RETRY_DELAYS
            .iter()
            .copied()
            .map(Some)
            .chain(std::iter::once(None))
            .enumerate()
        {
            let http_method = ureq::http::Method::from_bytes(method.as_bytes())
                .map_err(|e| format!("无效方法 {method}: {e}"))?;
            let mut builder = ureq::http::Request::builder()
                .method(http_method)
                .uri(url)
                .header("Authorization", self.auth_header());
            for (key, value) in extra {
                builder = builder.header(*key, *value);
            }
            let request = builder
                .body(body.to_vec())
                .map_err(|e| format!("构造 {method} 请求失败: {e}"))?;
            let resp = self
                .agent
                .run(request)
                .map_err(|e| format!("{method} 失败: {e}"))?;
            let status = resp.status().as_u16();
            let bytes = Self::read_body(resp)?;
            if Self::retryable_status(status) {
                if let Some(delay) = delay {
                    data::log_event(format!(
                        "sync WebDAV {method} got HTTP {status}; retry={} delay_secs={}",
                        attempt + 1,
                        delay.as_secs()
                    ));
                    std::thread::sleep(delay);
                    continue;
                }
            }
            return Ok((status, bytes));
        }
        unreachable!("retry loop always returns")
    }

    fn list_all_files(&self) -> Result<Vec<String>, String> {
        let body = br#"<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/></D:prop></D:propfind>"#;
        let (status, bytes) = self.run(
            "PROPFIND",
            &self.base_url,
            body,
            &[("Depth", "1"), ("Content-Type", "application/xml")],
        )?;
        if status == 404 {
            return Ok(Vec::new());
        }
        if status != 207 {
            return Err(Self::status_err("PROPFIND", status));
        }
        Ok(extract_filenames_from_propfind(
            &String::from_utf8_lossy(&bytes),
            &self.base_url,
        ))
    }
}

impl SyncTransport for WebDavTransport {
    fn put(&self, name: &str, data: &[u8]) -> Result<(), String> {
        let url = self.join(name);
        let (status, _) = self.run(
            "PUT",
            &url,
            data,
            &[("Content-Type", "application/octet-stream")],
        )?;
        if matches!(status, 200 | 201 | 204) {
            Ok(())
        } else {
            Err(Self::status_err("PUT", status))
        }
    }

    fn get(&self, name: &str) -> Result<Vec<u8>, String> {
        let url = self.join(name);
        let (status, body) = self.run("GET", &url, &[], &[])?;
        if status == 200 {
            Ok(body)
        } else {
            Err(Self::status_err("GET", status))
        }
    }

    fn get_limited(&self, name: &str, limit: u64) -> Result<Vec<u8>, String> {
        let request = ureq::http::Request::builder()
            .method("GET")
            .uri(self.join(name))
            .header("Authorization", self.auth_header())
            .body(Vec::new())
            .map_err(|e| format!("构造 GET 请求失败: {e}"))?;
        let resp = self
            .agent
            .run(request)
            .map_err(|e| format!("GET 失败: {e}"))?;
        if resp.status().as_u16() != 200 {
            return Err(Self::status_err("GET", resp.status().as_u16()));
        }
        Self::read_body_limited(resp, limit)
    }

    fn exists(&self, name: &str) -> Result<bool, String> {
        let url = self.join(name);
        let (status, _) = self.run("HEAD", &url, &[], &[])?;
        match status {
            200..=299 => Ok(true),
            404 => Ok(false),
            _ => Err(Self::status_err("HEAD", status)),
        }
    }

    fn list_device_packages(&self) -> Result<Vec<String>, String> {
        let propfind_body = r#"<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:resourcetype/>
  </D:prop>
</D:propfind>"#;
        let (status, body) = self.run(
            "PROPFIND",
            &self.base_url,
            propfind_body.as_bytes(),
            &[("Depth", "1"), ("Content-Type", "application/xml")],
        )?;
        if status == 404 {
            // 目录还不存在：当作空列表，导出时 PUT 会创建文件
            return Ok(Vec::new());
        }
        if status != 207 {
            return Err(Self::status_err("PROPFIND", status));
        }
        let text = String::from_utf8_lossy(&body);
        Ok(extract_device_filenames_from_propfind(
            &text,
            &self.base_url,
        ))
    }

    fn read_head(&self, name: &str) -> Result<HeadRead, String> {
        let request = ureq::http::Request::builder()
            .method("GET")
            .uri(self.join(name))
            .header("Authorization", self.auth_header())
            .body(Vec::new())
            .map_err(|e| format!("构造 GET 请求失败: {e}"))?;
        let resp = self
            .agent
            .run(request)
            .map_err(|e| format!("GET 失败: {e}"))?;
        if resp.status().as_u16() == 404 {
            return Ok(None);
        }
        if resp.status().as_u16() != 200 {
            return Err(Self::status_err("GET", resp.status().as_u16()));
        }
        let etag = resp
            .headers()
            .get("etag")
            .and_then(|v| v.to_str().ok())
            .map(str::to_owned);
        Ok(Some((Self::read_body_limited(resp, 1024 * 1024)?, etag)))
    }

    fn put_immutable(&self, name: &str, data: &[u8]) -> Result<bool, String> {
        let (status, _) = self.run(
            "PUT",
            &self.join(name),
            data,
            &[
                ("Content-Type", "application/octet-stream"),
                ("If-None-Match", "*"),
            ],
        )?;
        match status {
            200 | 201 | 204 => Ok(true),
            412 => Ok(false),
            _ => Err(Self::status_err("PUT", status)),
        }
    }

    fn cas_head(&self, name: &str, data: &[u8], revision: Option<&str>) -> Result<(), String> {
        let condition = revision.unwrap_or("*");
        let header = if revision.is_some() {
            "If-Match"
        } else {
            "If-None-Match"
        };
        let (status, _) = self.run(
            "PUT",
            &self.join(name),
            data,
            &[("Content-Type", "application/json"), (header, condition)],
        )?;
        match status {
            200 | 201 | 204 => Ok(()),
            409 | 412 => Err("CAS_CONFLICT".into()),
            _ => Err(Self::status_err("PUT", status)),
        }
    }

    fn list_sync_files(&self) -> Result<Vec<String>, String> {
        self.list_all_files()
    }
}

/// 自建 Go 服务的对象传输。接口故意与本地/WebDAV 对齐，v3 对象命名保持扁平，
/// 因而服务端不需要暴露文件系统或 WebDAV。
struct SyncServerTransport {
    base_url: String,
    token: String,
    agent: ureq::Agent,
}

impl SyncServerTransport {
    fn new(url: &str, token: &str) -> Self {
        Self {
            base_url: url.trim_end_matches('/').to_string(),
            token: token.to_string(),
            agent: ureq::Agent::config_builder()
                .timeout_global(Some(Duration::from_secs(120)))
                .http_status_as_error(false)
                .redirect_auth_headers(ureq::config::RedirectAuthHeaders::SameHost)
                .build()
                .new_agent(),
        }
    }

    fn object_url(&self, name: &str) -> String {
        format!("{}/v1/objects/{name}", self.base_url)
    }

    fn run(
        &self,
        method: &str,
        url: &str,
        body: &[u8],
        extra: &[(&str, &str)],
    ) -> Result<(u16, Option<String>, Vec<u8>), String> {
        const RETRIES: [Duration; 2] = [Duration::from_secs(1), Duration::from_secs(3)];
        for delay in RETRIES
            .iter()
            .copied()
            .map(Some)
            .chain(std::iter::once(None))
        {
            let mut request = ureq::http::Request::builder()
                .method(
                    ureq::http::Method::from_bytes(method.as_bytes()).map_err(|e| e.to_string())?,
                )
                .uri(url)
                .header("Authorization", format!("Bearer {}", self.token));
            for (key, value) in extra {
                request = request.header(*key, *value);
            }
            let response = self
                .agent
                .run(request.body(body.to_vec()).map_err(|e| e.to_string())?)
                .map_err(|e| format!("自建同步服务 {method} 失败: {e}"))?;
            let status = response.status().as_u16();
            let etag = response
                .headers()
                .get("etag")
                .and_then(|v| v.to_str().ok())
                .map(str::to_owned);
            let payload = WebDavTransport::read_body(response)?;
            if matches!(status, 502..=504) {
                if let Some(delay) = delay {
                    data::log_event(format!(
                        "sync self-hosted {method} got HTTP {status}; retry in {}s",
                        delay.as_secs()
                    ));
                    std::thread::sleep(delay);
                    continue;
                }
            }
            return Ok((status, etag, payload));
        }
        unreachable!()
    }

    fn error(op: &str, status: u16) -> String {
        let hint = match status {
            401 | 403 => "（令牌无效）",
            413 => "（对象过大）",
            507 => "（服务端存储配额已满）",
            _ => "",
        };
        format!("自建同步服务 {op} 返回 HTTP {status}{hint}")
    }
}

impl SyncTransport for SyncServerTransport {
    fn put(&self, name: &str, data: &[u8]) -> Result<(), String> {
        let (status, _, _) = self.run(
            "PUT",
            &self.object_url(name),
            data,
            &[("Content-Type", "application/octet-stream")],
        )?;
        if matches!(status, 200 | 201 | 204) {
            Ok(())
        } else {
            Err(Self::error("PUT", status))
        }
    }
    fn get(&self, name: &str) -> Result<Vec<u8>, String> {
        let (status, _, body) = self.run("GET", &self.object_url(name), &[], &[])?;
        if status == 200 {
            Ok(body)
        } else {
            Err(Self::error("GET", status))
        }
    }
    fn get_limited(&self, name: &str, limit: u64) -> Result<Vec<u8>, String> {
        let value = self.get(name)?;
        if value.len() as u64 > limit {
            Err(format!("{name} 超过协议大小上限"))
        } else {
            Ok(value)
        }
    }
    fn exists(&self, name: &str) -> Result<bool, String> {
        let (status, _, _) = self.run("HEAD", &self.object_url(name), &[], &[])?;
        match status {
            200..=299 => Ok(true),
            404 => Ok(false),
            _ => Err(Self::error("HEAD", status)),
        }
    }
    fn list_device_packages(&self) -> Result<Vec<String>, String> {
        self.list_sync_files().map(|v| {
            v.into_iter()
                .filter(|n| n.starts_with("device-") && n.ends_with(".json"))
                .collect()
        })
    }
    fn read_head(&self, name: &str) -> Result<HeadRead, String> {
        let (status, etag, body) = self.run("GET", &self.object_url(name), &[], &[])?;
        match status {
            200 => Ok(Some((body, etag))),
            404 => Ok(None),
            _ => Err(Self::error("GET", status)),
        }
    }
    fn put_immutable(&self, name: &str, data: &[u8]) -> Result<bool, String> {
        let (status, _, _) = self.run(
            "PUT",
            &self.object_url(name),
            data,
            &[
                ("Content-Type", "application/octet-stream"),
                ("If-None-Match", "*"),
            ],
        )?;
        match status {
            200 | 201 | 204 => Ok(true),
            412 => Ok(false),
            _ => Err(Self::error("PUT", status)),
        }
    }
    fn cas_head(&self, name: &str, data: &[u8], revision: Option<&str>) -> Result<(), String> {
        let condition = revision.unwrap_or("*");
        let header = if revision.is_some() {
            "If-Match"
        } else {
            "If-None-Match"
        };
        let (status, _, _) = self.run(
            "PUT",
            &self.object_url(name),
            data,
            &[("Content-Type", "application/json"), (header, condition)],
        )?;
        match status {
            200 | 201 | 204 => Ok(()),
            409 | 412 => Err("CAS_CONFLICT".into()),
            _ => Err(Self::error("PUT", status)),
        }
    }
    fn list_sync_files(&self) -> Result<Vec<String>, String> {
        let (status, _, body) =
            self.run("GET", &format!("{}/v1/objects", self.base_url), &[], &[])?;
        if status != 200 {
            return Err(Self::error("LIST", status));
        }
        serde_json::from_slice(&body).map_err(|e| format!("解析自建同步服务对象列表失败: {e}"))
    }
}

/// 从 PROPFIND 响应 XML 中提取 device-*.json 文件名。
/// 按 XML local-name 匹配 href，不依赖服务端选择的命名空间前缀。
fn extract_device_filenames_from_propfind(xml: &str, base_url: &str) -> Vec<String> {
    extract_filenames_from_propfind(xml, base_url)
        .into_iter()
        .filter(|filename| {
            filename.starts_with("device-")
                && filename.ends_with(".json")
                && !filename.contains(".tmp-")
        })
        .collect()
}

fn extract_filenames_from_propfind(xml: &str, base_url: &str) -> Vec<String> {
    let Ok(document) = roxmltree::Document::parse(xml) else {
        return Vec::new();
    };
    document
        .descendants()
        .filter(|node| node.is_element() && node.tag_name().name().eq_ignore_ascii_case("href"))
        .filter_map(|node| node.text())
        .map(|href| extract_filename_from_href(href, base_url))
        .filter(|filename| !filename.is_empty() && !filename.contains(".tmp-"))
        .collect()
}

fn sha256_hex(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn unique_suffix() -> String {
    format!(
        "{}-{}",
        std::process::id(),
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos()
    )
}

fn atomic_replace(path: &std::path::Path, data: &[u8]) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    }
    let temp = path.with_extension(format!("tmp-{}", unique_suffix()));
    let mut file = fs::File::create(&temp).map_err(|e| format!("创建临时文件失败: {e}"))?;
    file.write_all(data)
        .and_then(|_| file.flush())
        .and_then(|_| file.sync_all())
        .map_err(|e| format!("持久化临时文件失败: {e}"))?;
    fs::rename(&temp, path).map_err(|e| format!("原子替换失败: {e}"))
}

#[cfg(test)]
fn list_local_sync_files(dir: &std::path::Path) -> Result<Vec<String>, String> {
    let entries = match fs::read_dir(dir) {
        Ok(v) => v,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(e.to_string()),
    };
    Ok(entries
        .flatten()
        .filter_map(|e| e.file_name().to_str().map(str::to_owned))
        .filter(|n| !n.contains(".tmp-"))
        .collect())
}

/// 从 href（可能是完整 URL 或路径）中提取文件名。
fn extract_filename_from_href(href: &str, base_url: &str) -> String {
    // 去掉 base_url 前缀（如果有）
    let path = if let Some(stripped) = href.strip_prefix(base_url) {
        stripped
    } else if href.starts_with("http://") || href.starts_with("https://") {
        // 完整 URL，取 path 部分
        if let Some(scheme_end) = href.find("://") {
            let after_scheme = &href[scheme_end + 3..];
            if let Some(path_start) = after_scheme.find('/') {
                &after_scheme[path_start..]
            } else {
                href
            }
        } else {
            href
        }
    } else {
        href
    };
    // URL 解码（处理 %XX）
    let decoded = url_decode(path);
    // 取最后一段
    decoded.rsplit('/').next().unwrap_or(&decoded).to_string()
}

/// 简单的 URL 解码（处理 %XX 转义）。
fn url_decode(s: &str) -> String {
    let mut result = String::with_capacity(s.len());
    let bytes = s.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let (Some(h), Some(l)) = (hex_digit(bytes[i + 1]), hex_digit(bytes[i + 2])) {
                result.push((h * 16 + l) as char);
                i += 3;
                continue;
            }
        }
        result.push(bytes[i] as char);
        i += 1;
    }
    result
}

fn hex_digit(b: u8) -> Option<u8> {
    match b {
        b'0'..=b'9' => Some(b - b'0'),
        b'a'..=b'f' => Some(b - b'a' + 10),
        b'A'..=b'F' => Some(b - b'A' + 10),
        _ => None,
    }
}

/// 简单的 Base64 编码（避免引入额外依赖）。
fn base64_encode(input: &str) -> String {
    const TABLE: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let bytes = input.as_bytes();
    let mut result = String::with_capacity(bytes.len().div_ceil(3) * 4);
    let mut i = 0;
    while i + 2 < bytes.len() {
        let n = ((bytes[i] as u32) << 16) | ((bytes[i + 1] as u32) << 8) | (bytes[i + 2] as u32);
        result.push(TABLE[((n >> 18) & 63) as usize] as char);
        result.push(TABLE[((n >> 12) & 63) as usize] as char);
        result.push(TABLE[((n >> 6) & 63) as usize] as char);
        result.push(TABLE[(n & 63) as usize] as char);
        i += 3;
    }
    let remaining = bytes.len() - i;
    if remaining == 1 {
        let n = (bytes[i] as u32) << 16;
        result.push(TABLE[((n >> 18) & 63) as usize] as char);
        result.push(TABLE[((n >> 12) & 63) as usize] as char);
        result.push('=');
        result.push('=');
    } else if remaining == 2 {
        let n = ((bytes[i] as u32) << 16) | ((bytes[i + 1] as u32) << 8);
        result.push(TABLE[((n >> 18) & 63) as usize] as char);
        result.push(TABLE[((n >> 12) & 63) as usize] as char);
        result.push(TABLE[((n >> 6) & 63) as usize] as char);
        result.push('=');
    }
    result
}

/// 根据当前配置构造传输层实例。
fn build_transport(cfg: &SyncConfig) -> Result<Box<dyn SyncTransport>, String> {
    let base = sync_api_base(&cfg.api_url)?;
    let session = load_github_sync_session().ok_or_else(|| "请先登录 GitHub".to_string())?;
    if session.expires_at <= chrono::Utc::now().timestamp() {
        delete_github_sync_session();
        return Err("GitHub 同步登录已过期，请重新登录".to_string());
    }
    Ok(Box::new(SyncServerTransport::new(
        &base,
        &session.sync_token,
    )))
}

/// 用当前配置构造传输层（便捷方法）。
pub fn current_transport() -> Result<Box<dyn SyncTransport>, String> {
    build_transport(&read_config())
}

// ── 数据包格式 ─────────────────────────────────────────────────────────

/// 一个设备导出的数据包，写入同步目录供其他设备读取。
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct DevicePackage {
    /// 数据包格式版本，便于未来演进
    pub schema_version: u32,
    /// 导出设备的稳定 ID
    pub device_id: String,
    /// 导出设备的人类可读名称
    pub device_name: String,
    /// 导出时间（Unix 秒）
    pub exported_at: i64,
    /// 导出时数据覆盖的时间窗口起点（Unix 秒），0 表示无限制
    pub turns_start: i64,
    /// 导出时数据覆盖的时间窗口终点（Unix 秒）
    pub turns_end: i64,
    /// 全部 agent 的会话
    pub sessions: Vec<SessionRec>,
    /// 全部 agent 的轮次
    pub turns: Vec<TurnRec>,
}

const PACKAGE_SCHEMA_VERSION: u32 = 1;

/// 本机数据包在同步目录中的文件名。
fn package_filename(device_id: &str) -> String {
    format!("device-{device_id}.json")
}

/// 把已有远端包并进本次导出，避免更窄的时间窗口覆盖掉更早的记录。
fn union_into_package(dest: &mut DevicePackage, src: DevicePackage) {
    if src.schema_version != PACKAGE_SCHEMA_VERSION || src.device_id != dest.device_id {
        return;
    }
    let mut seen_sessions: HashSet<String> = dest.sessions.iter().map(session_dedup_key).collect();
    let mut seen_turns: HashSet<String> = dest.turns.iter().map(v2::stable_turn_id).collect();
    for session in src.sessions {
        if seen_sessions.insert(session_dedup_key(&session)) {
            dest.sessions.push(session);
        }
    }
    for turn in src.turns {
        if seen_turns.insert(v2::stable_turn_id(&turn)) {
            dest.turns.push(turn);
        }
    }
    dest.turns_start = match (dest.turns_start, src.turns_start) {
        (0, start) => start,
        (start, 0) => start,
        (a, b) => a.min(b),
    };
    dest.turns_end = dest.turns_end.max(src.turns_end);
}

// ── 导出 ───────────────────────────────────────────────────────────────

/// 把本机已加载的数据导出到同步后端。
/// 仅写入本机设备产生的记录（device_id 为空或等于本机 ID 的），
/// 避免把从其他设备导入的数据再回传一遍。
#[allow(dead_code)]
fn export_local_v1(mut data: LoadedData) -> Result<(), String> {
    let local_id = data::device_id();
    let local_name = data::device_name();

    // 只导出本机数据，过滤掉从其他设备导入的记录
    data.sessions
        .retain(|s| s.device_id.is_empty() || s.device_id == local_id);
    data.turns
        .retain(|t| t.device_id.is_empty() || t.device_id == local_id);

    let exported_at = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);

    let package = DevicePackage {
        schema_version: PACKAGE_SCHEMA_VERSION,
        device_id: local_id.clone(),
        device_name: local_name,
        exported_at,
        turns_start: data.turns_start,
        turns_end: data.turns_end,
        sessions: data.sessions,
        turns: data.turns,
    };

    let session_count = package.sessions.len();
    let turn_count = package.turns.len();
    let mut agent_labels: Vec<&str> = package.sessions.iter().map(|s| s.agent.label()).collect();
    agent_labels.sort_unstable();
    agent_labels.dedup();
    let fingerprint = match content_fingerprint(&package) {
        Ok(h) => h,
        Err(e) => {
            crate::data::log_event(format!("sync::export_local failed: {e}"));
            return Err(e);
        }
    };
    let cfg = read_config();
    let dest = destination_key(&cfg);
    let transport = match build_transport(&cfg) {
        Ok(t) => t,
        Err(e) => {
            crate::data::log_event(format!("sync::export_local failed: {e}"));
            return Err(e);
        }
    };
    let filename = package_filename(&local_id);
    if let Some((last_dest, last_hash)) = read_last_export_meta() {
        if last_dest == dest
            && last_hash == fingerprint
            && transport.exists(&filename).unwrap_or(false)
        {
            crate::data::log_event(format!(
                "sync::export_local skipped unchanged agents={} sessions={session_count} turns={turn_count}",
                agent_labels.join(",")
            ));
            return Ok(());
        }
    }

    let mut package = package;
    if let Ok(bytes) = transport.get(&filename) {
        if let Ok(existing) = serde_json::from_slice::<DevicePackage>(&bytes) {
            union_into_package(&mut package, existing);
        }
    }
    let payload = match serde_json::to_vec(&package) {
        Ok(p) => p,
        Err(e) => {
            let msg = format!("序列化数据包失败: {e}");
            crate::data::log_event(format!("sync::export_local failed: {msg}"));
            return Err(msg);
        }
    };
    match transport.put(&filename, &payload) {
        Ok(()) => {
            write_last_export_meta(&dest, &fingerprint);
            crate::data::log_event(format!(
                "sync::export_local ok file={filename} agents={} sessions={} turns={} bytes={}",
                agent_labels.join(","),
                package.sessions.len(),
                package.turns.len(),
                payload.len()
            ));
            Ok(())
        }
        Err(e) => {
            crate::data::log_event(format!("sync::export_local failed: {e}"));
            Err(e)
        }
    }
}

// ── 导入 ───────────────────────────────────────────────────────────────

/// 扫描同步后端，读取所有非本机的设备数据包。
/// 返回 (合并后的远程数据, 发现的设备列表, 失败原因)。
/// 列目录或读取任一数据包失败时 error 有值，调用方应保留上次完整快照。
#[allow(dead_code)]
fn import_remote_v1() -> (LoadedData, Vec<RemoteDevice>, Option<String>) {
    let started = Instant::now();
    let local_id = data::device_id();
    let mut merged = LoadedData::default();
    let mut devices: Vec<RemoteDevice> = Vec::new();
    let mut first_error = None;

    let transport = match current_transport() {
        Ok(t) => t,
        Err(e) => {
            crate::data::log_event(format!("sync::import_remote failed: {e}"));
            return (merged, devices, Some(e));
        }
    };

    let mut names = match transport.list_device_packages() {
        Ok(n) => n,
        Err(e) => {
            crate::data::log_event(format!("sync::import_remote failed: {e}"));
            return (merged, devices, Some(e));
        }
    };
    names.sort_unstable();

    let own_file = package_filename(&local_id);
    if names.binary_search(&own_file).is_ok() {
        devices.push(RemoteDevice {
            device_id: local_id.clone(),
            device_name: data::device_name(),
            exported_at: 0,
            session_count: 0,
            is_local: true,
        });
    }
    let downloads: Vec<_> = names
        .iter()
        .filter(|name| *name != &own_file)
        .map(|name| (name, transport.get(name)))
        .collect();
    for (name, result) in downloads {
        let bytes = match result {
            Ok(b) => b,
            Err(e) => {
                crate::data::log_event(format!("sync::import_remote get {name} failed: {e}"));
                if first_error.is_none() {
                    first_error = Some(format!("读取 {name} 失败: {e}"));
                }
                continue;
            }
        };
        let text = match std::str::from_utf8(&bytes) {
            Ok(s) => s,
            Err(_) => {
                crate::data::log_event(format!("sync::import_remote skip {name}: not utf-8"));
                if first_error.is_none() {
                    first_error = Some(format!("{name} 不是有效的 UTF-8 数据包"));
                }
                continue;
            }
        };
        let pkg: DevicePackage = match serde_json::from_str(text) {
            Ok(p) => p,
            Err(e) => {
                crate::data::log_event(format!("sync::import_remote skip {name}: {e}"));
                if first_error.is_none() {
                    first_error = Some(format!("解析 {name} 失败: {e}"));
                }
                continue;
            }
        };
        if pkg.device_id == local_id {
            continue;
        }
        if pkg.schema_version != PACKAGE_SCHEMA_VERSION {
            crate::data::log_event(format!(
                "sync::import_remote skip {name}: schema {} != {PACKAGE_SCHEMA_VERSION}",
                pkg.schema_version
            ));
            continue;
        }

        devices.push(RemoteDevice {
            device_id: pkg.device_id.clone(),
            device_name: pkg.device_name.clone(),
            exported_at: pkg.exported_at,
            session_count: pkg.sessions.len(),
            is_local: false,
        });

        // 合并到总数据，去重靠 session.key + device_id
        merged.sessions.extend(pkg.sessions);
        merged.turns.extend(pkg.turns);
        merged.turns_start = if merged.turns_start == 0 {
            pkg.turns_start
        } else {
            merged.turns_start.min(pkg.turns_start)
        };
        merged.turns_end = merged.turns_end.max(pkg.turns_end);
    }

    merged.turns.sort_by_key(|t| t.created_at);

    crate::data::log_event(format!(
        "sync::import_remote {} files={} devices={} remote_sessions={} remote_turns={} elapsed_ms={}",
        if first_error.is_some() { "partial" } else { "ok" },
        names.len(),
        devices.len(),
        merged.sessions.len(),
        merged.turns.len(),
        started.elapsed().as_millis()
    ));

    (merged, devices, first_error)
}

pub fn export_local(data: LoadedData) -> Result<(), String> {
    v3::export_local_v3(data)
}

pub fn import_remote() -> (LoadedData, Vec<RemoteDevice>, Option<String>) {
    v3::import_remote_v3()
}

pub type RemoteImport = (LoadedData, Vec<RemoteDevice>, Option<String>);
pub type SyncCycleResult = (Result<(), String>, RemoteImport);

/// 在同一个传输实例上完成一轮导出与导入，让 HTTP 连接池复用 TLS 连接。
/// 导出失败时仍继续导入，保持原有“尽量拉取其他设备”的恢复语义。
pub fn sync_cycle(data: LoadedData) -> SyncCycleResult {
    let transport = match current_transport() {
        Ok(value) => value,
        Err(error) => {
            return (
                Err(error.clone()),
                (LoadedData::default(), Vec::new(), Some(error)),
            )
        }
    };
    let export = v3::export_local_v3_with_transport(data, transport.as_ref());
    let imported = v3::import_remote_v3_with_transport(transport.as_ref());
    (export, imported)
}

/// 同步目录中发现的一个设备。
#[derive(Debug, Clone)]
pub struct RemoteDevice {
    pub device_id: String,
    pub device_name: String,
    pub exported_at: i64,
    pub session_count: usize,
    pub is_local: bool,
}

// ── 合并 ───────────────────────────────────────────────────────────────

/// 把本机数据和远程导入的数据合并成一份完整的 LoadedData。
/// 以本机数据为基底，追加远程数据，按 (device_id, session_key) 去重。
pub fn merge_local_with_remote(local: &LoadedData, remote: &LoadedData) -> LoadedData {
    let mut merged = local.clone();

    // 本机已有的会话 key 集合（含 device_id 前缀，避免不同设备同 key 冲突）
    let mut seen_sessions: HashSet<String> = local.sessions.iter().map(session_dedup_key).collect();
    let mut seen_turns: HashSet<String> = local.turns.iter().map(v2::stable_turn_id).collect();

    for s in &remote.sessions {
        if seen_sessions.insert(session_dedup_key(s)) {
            merged.sessions.push(s.clone());
        }
    }
    for t in &remote.turns {
        if seen_turns.insert(v2::stable_turn_id(t)) {
            merged.turns.push(t.clone());
        }
    }

    merged.turns.sort_by_key(|t| t.created_at);
    merged.turns_start = if merged.turns_start == 0 && remote.turns_start == 0 {
        0
    } else if merged.turns_start == 0 {
        remote.turns_start
    } else if remote.turns_start == 0 {
        merged.turns_start
    } else {
        merged.turns_start.min(remote.turns_start)
    };
    merged.turns_end = merged.turns_end.max(remote.turns_end);

    merged
}

fn session_dedup_key(session: &SessionRec) -> String {
    format!(
        "{}::{:?}::{}",
        session.device_id, session.agent, session.key
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn empty_package(exported_at: i64, turns_end: i64) -> DevicePackage {
        DevicePackage {
            schema_version: 1,
            device_id: "dev".into(),
            device_name: "host".into(),
            exported_at,
            turns_start: 0,
            turns_end,
            sessions: Vec::new(),
            turns: Vec::new(),
        }
    }

    #[test]
    fn fingerprint_ignores_export_timestamp() {
        let a = empty_package(1, 10);
        let b = empty_package(999, 10);
        assert_eq!(
            content_fingerprint(&a).unwrap(),
            content_fingerprint(&b).unwrap()
        );
    }

    #[test]
    fn fingerprint_changes_when_window_changes() {
        let a = empty_package(1, 10);
        let b = empty_package(1, 20);
        assert_ne!(
            content_fingerprint(&a).unwrap(),
            content_fingerprint(&b).unwrap()
        );
    }

    #[test]
    fn union_keeps_older_sessions() {
        let mut dest = empty_package(1, 20);
        dest.sessions.push(SessionRec {
            key: "new".into(),
            device_id: "dev".into(),
            ..Default::default()
        });
        dest.turns_start = 10;
        dest.turns_end = 20;
        let mut src = empty_package(1, 15);
        src.sessions.push(SessionRec {
            key: "old".into(),
            device_id: "dev".into(),
            ..Default::default()
        });
        src.sessions.push(SessionRec {
            key: "new".into(),
            device_id: "dev".into(),
            ..Default::default()
        });
        src.turns_start = 5;
        src.turns_end = 15;
        union_into_package(&mut dest, src);
        let keys: Vec<_> = dest.sessions.iter().map(|s| s.key.as_str()).collect();
        assert!(keys.contains(&"new"));
        assert!(keys.contains(&"old"));
        assert_eq!(dest.sessions.len(), 2);
        assert_eq!(dest.turns_start, 5);
        assert_eq!(dest.turns_end, 20);
    }

    #[test]
    fn merge_keeps_distinct_turns_with_the_same_timestamp() {
        let first = TurnRec {
            device_id: "dev".into(),
            session_key: "session".into(),
            created_at: 100,
            input_tokens: 1.0,
            ..Default::default()
        };
        let mut second = first.clone();
        second.input_tokens = 2.0;
        let local = LoadedData {
            turns: vec![first],
            ..Default::default()
        };
        let remote = LoadedData {
            turns: vec![second],
            ..Default::default()
        };

        let merged = merge_local_with_remote(&local, &remote);

        assert_eq!(merged.turns.len(), 2);
    }

    #[test]
    fn destination_key_uses_normalized_api_address() {
        let first = SyncConfig {
            api_url: "https://sync.example.com".into(),
            ..Default::default()
        };
        let same_with_slash = SyncConfig {
            api_url: "https://sync.example.com/".into(),
            ..Default::default()
        };
        let other = SyncConfig {
            api_url: "https://other.example.com".into(),
            ..Default::default()
        };
        assert_eq!(destination_key(&first), destination_key(&same_with_slash));
        assert_ne!(destination_key(&first), destination_key(&other));
    }

    #[test]
    fn propfind_parser_accepts_arbitrary_namespace_prefixes() {
        let xml = r#"<?xml version="1.0"?>
<x:multistatus xmlns:x="DAV:">
  <x:response><x:href>/dav/device-one.json</x:href></x:response>
  <response xmlns="DAV:"><href>/dav/device-two.json</href></response>
  <x:response><x:href>/dav/not-a-package.txt</x:href></x:response>
</x:multistatus>"#;
        let mut names = extract_device_filenames_from_propfind(xml, "https://dav.example/dav/");
        names.sort();
        assert_eq!(names, ["device-one.json", "device-two.json"]);
    }

    #[test]
    fn local_transport_exists_tracks_package_removal() {
        let dir = std::env::temp_dir().join(format!(
            "devin-usage-metrics-sync-test-{}-{}",
            std::process::id(),
            SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        let transport = LocalTransport::new(dir.clone());
        assert!(!transport.exists("device-test.json").unwrap());
        transport.put("device-test.json", b"{}").unwrap();
        assert!(transport.exists("device-test.json").unwrap());
        fs::remove_file(dir.join("device-test.json")).unwrap();
        assert!(!transport.exists("device-test.json").unwrap());
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn webdav_reader_accepts_packages_above_ureq_default_limit() {
        let bytes = vec![7; 10 * 1024 * 1024 + 1];
        let response = ureq::http::Response::builder()
            .status(200)
            .body(ureq::Body::builder().data(bytes.clone()))
            .unwrap();
        assert_eq!(WebDavTransport::read_body(response).unwrap(), bytes);
    }
}
