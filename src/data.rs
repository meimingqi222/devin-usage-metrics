use crate::i18n;
use fs2::FileExt;
use rusqlite::{Connection, OpenFlags};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet};
use std::fs::{File, OpenOptions};
use std::io::{BufReader, BufWriter, Write};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use crate::local_sources;

const CACHE_TTL_SECS: i64 = 300; // 5 minutes cache TTL
/// 缓存结构版本。任何会影响 turn 集合/时间戳的解析改动都必须递增，否则旧的
/// (且不带 dedup_key 的) 快照会被当作增量基线复用，修复无法生效。
const CACHE_SCHEMA_VERSION: u32 = 11;

/// `message_nodes.created_at` 是节点**落盘**时间，不是真实生成时间：Devin 在
/// 会话恢复/压缩时会把同一条消息以新的 created_at 重写一遍。实测比真实生成时间
/// 晚，中位数约 1.6 小时，最大 1.5 天。SQL 里按 created_at 做时间范围裁剪时，
/// 上界必须向外放宽这么多，否则窗口末尾的真实 turn 会漏掉；精确过滤交给 Rust
/// 侧的 `metadata.started_generation_at`。
const ROW_TIME_SLACK_SECS: i64 = 7 * 86400;
/// 下界方向的余量。落盘时间理论上不早于生成时间（实测最小差 -0.2s，属时钟抖动），
/// 留 1 天已足够，同时避免增量查询把扫描范围撑得太大。
const ROW_TIME_SLACK_LOWER_SECS: i64 = 86400;

/// 单个数据源的会话上限。超出时按 `last_activity_at` 保留最近的若干个。
/// 上限不能解除：turn 查询用 `session_id IN (...)` 绑定参数，SQLite 的变量上限是
/// 32766，无上界时会话一多查询会直接报错。同时取值要足够大，并且真的截断时打
/// 告警，避免用户看到一份没有提示的残缺统计。
const MAX_SESSIONS_PER_SOURCE: usize = 20_000;

fn cache_path() -> PathBuf {
    #[cfg(target_os = "windows")]
    let base = dirs::cache_dir().unwrap_or_else(|| {
        dirs::home_dir()
            .unwrap_or_else(|| PathBuf::from("."))
            .join(".cache")
    });
    #[cfg(not(target_os = "windows"))]
    let base = dirs::home_dir()
        .map(|h| h.join(".cache"))
        .unwrap_or_else(|| PathBuf::from(".cache"));
    base.join("devin-usage-metrics/cache-v2.json")
}

pub fn agent_cache_path(agent: AgentKind) -> PathBuf {
    #[cfg(target_os = "windows")]
    let base = dirs::cache_dir().unwrap_or_else(|| {
        dirs::home_dir()
            .unwrap_or_else(|| PathBuf::from("."))
            .join(".cache")
    });
    #[cfg(not(target_os = "windows"))]
    let base = dirs::home_dir()
        .map(|h| h.join(".cache"))
        .unwrap_or_else(|| PathBuf::from(".cache"));
    let slug = agent.label().to_ascii_lowercase().replace(' ', "-");
    base.join(format!("devin-usage-metrics/cache-{}.json", slug))
}

fn log_path() -> PathBuf {
    cache_path().with_file_name("load.log")
}

pub fn log_event(message: impl AsRef<str>) {
    let path = log_path();
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    let Ok(file) = OpenOptions::new().create(true).append(true).open(path) else {
        return;
    };
    let mut writer = BufWriter::new(file);
    let timestamp = chrono::Local::now().format("%Y-%m-%d %H:%M:%S");
    let _ = writeln!(writer, "[{timestamp}] {}", message.as_ref());
}

fn log_loaded_part(source: &str, started: Instant, data: &LoadedData) {
    log_event(format!(
        "source={source} elapsed_ms={} sessions={} turns={} errors={}",
        started.elapsed().as_millis(),
        data.sessions.len(),
        data.turns.len(),
        data.errors.len()
    ));
}

#[derive(
    Debug,
    Default,
    Clone,
    Copy,
    PartialEq,
    Eq,
    PartialOrd,
    Ord,
    Hash,
    serde::Serialize,
    serde::Deserialize,
)]
#[serde(rename_all = "lowercase")]
pub enum AgentKind {
    #[default]
    Devin,
    Amp,
    Claude,
    Codex,
    Antigravity,
    Grok,
    ZCode,
    OpenCode,
    Pi,
    MimoCode,
}

impl AgentKind {
    /// Product-visible agents. Amp is intentionally omitted because its CLI
    /// exports every thread from the remote service and makes routine loads
    /// and sync collection unacceptably slow. Keep `AgentKind::Amp` itself so
    /// existing caches and synchronized historical records still deserialize.
    pub const ALL: [Self; 9] = [
        Self::Devin,
        Self::Claude,
        Self::Codex,
        Self::Antigravity,
        Self::Grok,
        Self::ZCode,
        Self::OpenCode,
        Self::Pi,
        Self::MimoCode,
    ];

    pub fn label(self) -> &'static str {
        match self {
            Self::Devin => "Devin",
            Self::Amp => "Amp",
            Self::Claude => "Claude Code",
            Self::Codex => "Codex",
            Self::Antigravity => "Antigravity",
            Self::Grok => "Grok Build",
            Self::ZCode => "ZCode",
            Self::OpenCode => "OpenCode",
            Self::Pi => "pi-agent",
            Self::MimoCode => "MimoCode",
        }
    }

    /// 检查该 agent 是否在本地安装过（数据目录存在）。
    pub fn is_installed(self) -> bool {
        let home = || dirs::home_dir().unwrap_or_else(|| std::path::PathBuf::from("."));
        match self {
            Self::Devin => devin_db_paths().iter().any(|(_, p)| p.exists()),
            Self::Amp => {
                let root = std::env::var_os("AMP_DATA_DIR")
                    .map(std::path::PathBuf::from)
                    .unwrap_or_else(|| home().join(".local/share/amp"))
                    .join("threads");
                root.exists()
            }
            Self::Claude => home().join(".claude/projects").exists(),
            Self::Codex => {
                let base = std::env::var_os("CODEX_HOME")
                    .map(std::path::PathBuf::from)
                    .unwrap_or_else(|| home().join(".codex"));
                base.join("sessions").exists() || base.join("archived_sessions").exists()
            }
            Self::Antigravity => home().join(".gemini/antigravity/conversations").exists(),
            Self::Grok => home().join(".grok/sessions").exists(),
            Self::ZCode => {
                home().join(".zcode/cli/db/db.sqlite").exists()
                    || home()
                        .join("Library/Application Support/zcode/cli/db/db.sqlite")
                        .exists()
            }
            Self::OpenCode => {
                dirs::data_dir()
                    .map(|d| d.join("opencode/opencode.db").exists())
                    .unwrap_or(false)
                    || home().join(".local/share/opencode/opencode.db").exists()
            }
            Self::Pi => home().join(".pi/agent/sessions").exists(),
            Self::MimoCode => {
                dirs::data_dir()
                    .map(|d| d.join("mimocode/mimocode.db").exists())
                    .unwrap_or(false)
                    || home().join(".local/share/mimocode/mimocode.db").exists()
            }
        }
    }
}

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct DataError {
    pub agent: AgentKind,
    pub message: String,
}

#[derive(serde::Deserialize)]
struct CacheFile {
    #[serde(default)]
    schema_version: u32,
    cached_at: i64,
    turns_start: i64,
    turns_end: i64,
    sessions: Vec<SessionRec>,
    turns: Vec<TurnRec>,
    errors: Vec<DataError>,
    #[serde(default)]
    file_mtimes: HashMap<String, i64>,
    #[serde(default)]
    file_sessions: HashMap<String, String>,
    /// 会话和 turn 的内容指纹。同步导出可据此复用已发布的 v3 清单，避免每轮
    /// 再次对全部历史 turn 分组、排序和序列化。
    #[serde(default)]
    sync_content_hash: String,
}

#[derive(serde::Serialize)]
struct CacheFileRef<'a> {
    schema_version: u32,
    cached_at: i64,
    turns_start: i64,
    turns_end: i64,
    sessions: &'a [SessionRec],
    turns: &'a [TurnRec],
    errors: &'a [DataError],
    file_mtimes: &'a HashMap<String, i64>,
    file_sessions: &'a HashMap<String, String>,
    sync_content_hash: &'a str,
}

fn now_timestamp() -> Option<i64> {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .map(|duration| duration.as_secs() as i64)
}

// ── 设备标识 ──────────────────────────────────────────────────────────
// 本机数据在加载出口处统一打上设备 ID / 名称，多设备合并时据此区分来源。

/// 设备 ID 的持久化文件路径，与 config.json 同目录。
fn device_id_path() -> Option<PathBuf> {
    #[cfg(target_os = "macos")]
    {
        dirs::config_dir().map(|d| d.join("devin-usage-metrics/device.json"))
    }
    #[cfg(target_os = "windows")]
    {
        dirs::config_dir().map(|d| d.join("devin-usage-metrics\\device.json"))
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    {
        dirs::config_dir().map(|d| d.join("devin-usage-metrics/device.json"))
    }
}

/// 读取或生成本机稳定设备 ID（UUID v4 字符串），首次调用时落盘。
pub fn device_id() -> String {
    if let Some(path) = device_id_path() {
        if let Some(parent) = path.parent() {
            let _ = std::fs::create_dir_all(parent);
        }
        // A separate stable lock file avoids the rename/inode locking trap and
        // serializes first creation across processes.
        let lock_path = path.with_file_name("device.lock");
        let lock = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(lock_path)
            .ok()
            .filter(|file| file.lock_exclusive().is_ok());
        if lock.is_none() {
            return fallback_device_id();
        }
        if let Ok(text) = std::fs::read_to_string(&path) {
            #[derive(serde::Deserialize)]
            struct DeviceFile {
                id: String,
            }
            if let Ok(df) = serde_json::from_str::<DeviceFile>(&text) {
                if !df.id.is_empty() {
                    return df.id;
                }
            }
        }
        // 生成新 UUID v4（不引入 uuid crate，用随机字节手写）
        let id = random_uuid_v4();
        let temp = path.with_extension(format!(
            "json.tmp-{}-{}",
            std::process::id(),
            now_timestamp().unwrap_or(0)
        ));
        let payload = serde_json::json!({ "id": id }).to_string();
        if std::fs::write(&temp, &payload).is_ok() {
            // The lock guarantees no competing creator. If replacement fails,
            // re-read the winner rather than returning an unpersisted ID.
            let _ = std::fs::rename(&temp, &path);
        } else {
            let _ = std::fs::remove_file(&temp);
        }
        if let Ok(text) = std::fs::read_to_string(&path) {
            if let Some(saved) = serde_json::from_str::<serde_json::Value>(&text)
                .ok()
                .and_then(|v| v.get("id")?.as_str().map(str::to_owned))
            {
                return saved;
            }
        }
        return fallback_device_id();
    }
    // 兜底：用 hostname 哈希，保证有非空标识
    fallback_device_id()
}

fn fallback_device_id() -> String {
    hostname()
        .map(|h| format!("fallback-{:x}", fnv1a(h.as_bytes())))
        .unwrap_or_else(|| "fallback-unknown".into())
}

/// 读取用户保存的设备名称；未自定义时返回 None。
pub fn custom_device_name() -> Option<String> {
    if let Some(path) = device_id_path() {
        if let Ok(text) = std::fs::read_to_string(path) {
            return serde_json::from_str::<serde_json::Value>(&text)
                .ok()
                .and_then(|value| {
                    value
                        .get("name")?
                        .as_str()
                        .map(|name| name.trim().to_owned())
                })
                .filter(|name| !name.is_empty());
        }
    }
    None
}

/// 本机设备名称：用户自定义优先，未设置时才使用 hostname。
pub fn device_name() -> String {
    custom_device_name().unwrap_or_else(|| hostname().unwrap_or_else(|| "This Device".into()))
}

/// 保存用户可读的设备名称；空值表示回退到系统 hostname，不影响稳定 device ID。
pub fn set_device_name(name: &str) -> Result<(), String> {
    let name = name.trim();
    if name.chars().count() > 64 || name.chars().any(char::is_control) {
        return Err("设备名称最长 64 个字符，且不能包含控制字符".into());
    }
    let path = device_id_path().ok_or_else(|| "无法定位设备配置目录".to_string())?;
    let id = device_id();
    let payload = serde_json::json!({ "id": id, "name": name }).to_string();
    let temp = path.with_extension(format!("json.tmp-{}", std::process::id()));
    std::fs::write(&temp, payload).map_err(|error| format!("写入设备名称失败: {error}"))?;
    std::fs::rename(&temp, path).map_err(|error| format!("保存设备名称失败: {error}"))
}

fn hostname() -> Option<String> {
    std::env::var("HOSTNAME")
        .ok()
        .filter(|s| !s.is_empty())
        .or_else(|| {
            #[cfg(unix)]
            {
                // 读取 /etc/hostname 作为更稳定的来源
                std::fs::read_to_string("/etc/hostname")
                    .ok()
                    .map(|s| s.trim().to_string())
                    .filter(|s| !s.is_empty())
            }
            #[cfg(not(unix))]
            {
                std::env::var("COMPUTERNAME").ok().filter(|s| !s.is_empty())
            }
        })
}

/// 用系统随机源生成 UUID v4 字符串。
fn random_uuid_v4() -> String {
    uuid::Uuid::new_v4().to_string()
}

fn fnv1a(data: &[u8]) -> u64 {
    let mut h: u64 = 0xcbf29ce484222325;
    for &b in data {
        h ^= b as u64;
        h = h.wrapping_mul(0x100000001b3);
    }
    h
}

/// 给本机加载的数据打上设备标签：device_id 为空的记录填本机 ID/名称。
/// 远程导入的数据已带各自设备 ID，不会被覆盖。
pub fn stamp_device(data: &mut LoadedData) {
    let id = device_id();
    let name = device_name();
    for s in &mut data.sessions {
        if s.device_id.is_empty() {
            s.device_id = id.clone();
            s.device_name = name.clone();
        }
    }
    for t in &mut data.turns {
        if t.device_id.is_empty() {
            t.device_id = id.clone();
            t.device_name = name.clone();
        }
    }
}

fn cache_covers_range(cache_start: i64, cache_end: i64, start: i64, end: i64, now: i64) -> bool {
    // `end` is intentionally one hour in the future. A cache produced a few
    // minutes ago cannot cover that moving future boundary byte-for-byte, but
    // it does cover all rows that can exist as of now.
    cache_start <= start && cache_end >= end.min(now)
}

fn cache_covers(
    cached_at: i64,
    cache_start: i64,
    cache_end: i64,
    start: i64,
    end: i64,
    now: i64,
) -> bool {
    now.saturating_sub(cached_at) <= CACHE_TTL_SECS
        && cache_covers_range(cache_start, cache_end, start, end, now)
}

fn load_agent_cache(
    agent: AgentKind,
    start: i64,
    end: i64,
    require_fresh: bool,
) -> Option<LoadedData> {
    let file = File::open(agent_cache_path(agent)).ok()?;
    let cached: CacheFile = serde_json::from_reader(BufReader::new(file)).ok()?;
    if cached.schema_version != CACHE_SCHEMA_VERSION {
        return None;
    }
    let now = now_timestamp()?;
    let covered = if require_fresh {
        cache_covers(
            cached.cached_at,
            cached.turns_start,
            cached.turns_end,
            start,
            end,
            now,
        )
    } else {
        cache_covers_range(cached.turns_start, cached.turns_end, start, end, now)
    };
    if !covered {
        return None;
    }

    Some(LoadedData {
        sessions: cached.sessions,
        turns: cached.turns,
        turns_start: cached.turns_start,
        turns_end: cached.turns_end,
        errors: cached.errors,
        file_mtimes: cached.file_mtimes,
        file_sessions: cached.file_sessions,
        sync_content_hash: cached.sync_content_hash,
    })
}

fn sync_content_hash(data: &LoadedData) -> String {
    // 仅在 Agent 缓存首次建立或本机数据真的变化时计算。哈希输入覆盖 export 会
    // 用到的全部原始记录；随后 v3 可安全地直接复用同一份 manifest。
    let bytes = serde_json::to_vec(&(&data.sessions, &data.turns))
        .expect("sync cache content is serializable");
    format!("{:x}", Sha256::digest(bytes))
}

fn same_sync_content(left: &LoadedData, right: &LoadedData) -> bool {
    left.sessions == right.sessions && left.turns == right.turns
}

pub fn save_agent_cache(agent: AgentKind, data: &LoadedData) {
    let path = agent_cache_path(agent);
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    let Some(cached_at) = now_timestamp() else {
        return;
    };
    let cache = CacheFileRef {
        schema_version: CACHE_SCHEMA_VERSION,
        cached_at,
        turns_start: data.turns_start,
        turns_end: data.turns_end,
        sessions: &data.sessions,
        turns: &data.turns,
        errors: &data.errors,
        file_mtimes: &data.file_mtimes,
        file_sessions: &data.file_sessions,
        sync_content_hash: &data.sync_content_hash,
    };
    let temp_path = path.with_extension(format!("json.tmp-{}", std::process::id()));
    if let Ok(file) = File::create(&temp_path) {
        let mut writer = BufWriter::new(file);
        if serde_json::to_writer(&mut writer, &cache).is_ok() && writer.flush().is_ok() {
            let _ = std::fs::rename(&temp_path, &path);
        } else {
            let _ = std::fs::remove_file(&temp_path);
        }
    }
}

pub fn load_agent(agent: AgentKind, start: i64, end: i64) -> LoadedData {
    if let Some(data) = load_agent_cache(agent, start, end, true) {
        log_event(format!(
            "load_agent agent={} cache_hit sessions={} turns={}",
            agent.label(),
            data.sessions.len(),
            data.turns.len()
        ));
        data
    } else {
        let previous = load_agent_cache(agent, start, end, false).map(Arc::new);
        let mut data = load_selected_agent(start, end, agent, previous.clone());
        if let Some(previous) = previous.as_deref() {
            if !previous.sync_content_hash.is_empty() && same_sync_content(previous, &data) {
                data.sync_content_hash = previous.sync_content_hash.clone();
            }
        }
        if data.sync_content_hash.is_empty() {
            data.sync_content_hash = sync_content_hash(&data);
        }
        save_agent_cache(agent, &data);
        log_event(format!(
            "load_agent agent={} cache_miss fresh_loaded sessions={} turns={}",
            agent.label(),
            data.sessions.len(),
            data.turns.len()
        ));
        data
    }
}

/// 同步导出用。复用五分钟内的新鲜缓存；缓存过期后按各 Agent 的增量加载逻辑
/// 检查本地数据源，确保周期同步能发现新会话。
pub fn load_agent_for_sync(agent: AgentKind, start: i64, end: i64) -> LoadedData {
    load_agent(agent, start, end)
}

fn load_cache(start: i64, end: i64) -> Option<LoadedData> {
    let file = File::open(cache_path()).ok()?;
    let cached: CacheFile = serde_json::from_reader(BufReader::new(file)).ok()?;
    if cached.schema_version != CACHE_SCHEMA_VERSION {
        return None;
    }
    let now = now_timestamp()?;
    if !cache_covers(
        cached.cached_at,
        cached.turns_start,
        cached.turns_end,
        start,
        end,
        now,
    ) {
        return None;
    }

    Some(LoadedData {
        sessions: cached.sessions,
        turns: cached.turns,
        turns_start: cached.turns_start,
        turns_end: cached.turns_end,
        errors: cached.errors,
        file_mtimes: cached.file_mtimes,
        file_sessions: cached.file_sessions,
        sync_content_hash: cached.sync_content_hash,
    })
}

fn load_from_cache(start: i64, end: i64) -> Option<LoadedData> {
    load_cache(start, end)
}

fn save_to_cache(data: &LoadedData) {
    let path = cache_path();
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    let Some(cached_at) = now_timestamp() else {
        return;
    };
    let cache = CacheFileRef {
        schema_version: CACHE_SCHEMA_VERSION,
        cached_at,
        turns_start: data.turns_start,
        turns_end: data.turns_end,
        sessions: &data.sessions,
        turns: &data.turns,
        errors: &data.errors,
        file_mtimes: &data.file_mtimes,
        file_sessions: &data.file_sessions,
        sync_content_hash: &data.sync_content_hash,
    };
    let temp_path = path.with_extension(format!("json.tmp-{}", std::process::id()));
    if let Ok(file) = File::create(&temp_path) {
        let mut writer = BufWriter::new(file);
        if serde_json::to_writer(&mut writer, &cache).is_ok() && writer.flush().is_ok() {
            let _ = std::fs::rename(&temp_path, &path);
        } else {
            let _ = std::fs::remove_file(&temp_path);
        }
    }
}

#[derive(serde::Deserialize)]
struct DevinChatMessage {
    #[serde(default)]
    message_id: Option<String>,
    metadata: Option<DevinMetadata>,
}

#[derive(serde::Deserialize)]
struct DevinMetadata {
    metrics: Option<DevinMetrics>,
    #[serde(default)]
    generation_model: Option<String>,
    #[serde(default)]
    request_id: Option<String>,
    /// 该轮真实开始生成的时间（RFC3339 UTC）。所有兄弟节点副本共享同一个值，
    /// 是唯一可靠的 turn 时间来源。
    #[serde(default)]
    started_generation_at: Option<String>,
    /// 该轮生成完成的时间（RFC3339 UTC），作为 started_generation_at 的兜底。
    #[serde(default)]
    created_at: Option<String>,
}

#[derive(serde::Deserialize)]
struct DevinMetrics {
    ttft_ms: Option<f64>,
    input_tokens: Option<f64>,
    output_tokens: Option<f64>,
    cache_read_tokens: Option<f64>,
    cache_creation_tokens: Option<f64>,
    total_time_ms: Option<f64>,
}

#[derive(Debug, Default, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct SessionRec {
    pub agent: AgentKind,
    pub key: String, // "<source>/<id>"
    pub source: String,
    pub id: String,
    pub title: String,
    pub working_directory: String,
    pub selected_model: String,
    pub real_model: Option<String>,
    pub agent_mode: String,
    pub created_at: i64,
    pub last_activity_at: i64,
    pub input_tokens: f64,
    pub output_tokens: f64,
    pub cached_tokens: f64,
    /// 缓存写入 token 数（Claude 的 cache_creation，区分 5m/1h）
    #[serde(default)]
    pub cache_creation_tokens: f64,
    /// 5 分钟 TTL 的缓存写入 token 数（仅 Claude 区分）
    #[serde(default)]
    pub cache_creation_5m_tokens: f64,
    /// 1 小时 TTL 的缓存写入 token 数（仅 Claude 区分）
    #[serde(default)]
    pub cache_creation_1h_tokens: f64,
    pub agent_messages: f64,
    /// Agent 会话自身记录的费用（美元），如 Grok 的 costUsdTicks。
    /// 有值时优先展示，不再按定价表计算。
    #[serde(default)]
    pub recorded_cost: Option<f64>,
    /// 该会话来源设备的稳定 ID（本机数据在加载出口处统一打标）
    #[serde(default)]
    pub device_id: String,
    /// 该会话来源设备的人类可读名称（hostname）
    #[serde(default)]
    pub device_name: String,
}

impl SessionRec {
    pub fn total_tokens(&self) -> f64 {
        self.input_tokens + self.output_tokens + self.cached_tokens + self.cache_creation_tokens
    }

    pub fn display_model(&self) -> String {
        self.real_model
            .clone()
            .filter(|s| !s.is_empty())
            .or_else(|| {
                if self.selected_model.is_empty() {
                    None
                } else {
                    Some(self.selected_model.clone())
                }
            })
            .unwrap_or_else(|| "unknown".into())
    }
}

#[derive(Debug, Default, Clone, PartialEq, serde::Serialize, serde::Deserialize)]
pub struct TurnRec {
    pub agent: AgentKind,
    pub session_key: String,
    pub created_at: i64,
    pub input_tokens: f64,
    pub output_tokens: f64,
    pub cache_read_tokens: f64,
    /// 缓存写入 token 总数（cache_creation）
    #[serde(default)]
    pub cache_creation_tokens: f64,
    /// 5 分钟 TTL 的缓存写入 token 数（仅 Claude 区分）
    #[serde(default)]
    pub cache_creation_5m_tokens: f64,
    /// 1 小时 TTL 的缓存写入 token 数（仅 Claude 区分）
    #[serde(default)]
    pub cache_creation_1h_tokens: f64,
    /// 该 turn 实际使用的模型（Devin 为 generation_model，其他为 session model）
    #[serde(default)]
    pub model: String,
    pub ttft_ms: f64,
    pub total_time_ms: f64,
    /// Agent 自身记录的该轮费用（美元），如 Grok 的 costUsdTicks / 1e10。
    /// 有值时聚合费用优先用它，而不是按定价表计算。
    #[serde(default)]
    pub recorded_cost: Option<f64>,
    /// 该轮来源设备的稳定 ID（本机数据在加载出口处统一打标）
    #[serde(default)]
    pub device_id: String,
    /// 该轮来源设备的人类可读名称（hostname）
    #[serde(default)]
    pub device_name: String,
    /// 用于跨文件去重的稳定 key（目前仅 Claude Code 使用，取 message.id/
    /// requestId）。`/resume` 续接会话时 Claude Code 会把历史 message 原样
    /// 复制进新文件，必须靠这个 key 才能跨会话文件识别出同一次真实调用。
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub dedup_key: String,
}

#[derive(Debug, Default, Clone, serde::Serialize, serde::Deserialize)]
pub struct LoadedData {
    pub sessions: Vec<SessionRec>,
    pub turns: Vec<TurnRec>,
    pub turns_start: i64,
    pub turns_end: i64,
    pub errors: Vec<DataError>,
    /// 数据文件路径 → mtime（秒）。增量 reload 时跳过 mtime 未变化的文件，
    /// 其会话结果直接从上次快照恢复，避免全量重解析。
    #[serde(default)]
    pub file_mtimes: HashMap<String, i64>,
    /// 数据文件路径 → 会话 key。记录每个文件解析出的会话，
    /// 增量 reload 时据此把未变化文件映射回可复用的会话。
    #[serde(default)]
    pub file_sessions: HashMap<String, String>,
    /// Agent 缓存的组合内容指纹；仅用于 v3 同步的 no-op 快速路径。
    #[serde(default)]
    pub sync_content_hash: String,
}

pub fn devin_db_paths() -> Vec<(String, PathBuf)> {
    #[cfg(target_os = "windows")]
    let base = dirs::data_dir()
        .map(|d| d.join("devin"))
        .unwrap_or_else(|| {
            dirs::home_dir()
                .map(|h| h.join("AppData/Roaming/devin"))
                .unwrap_or_else(|| PathBuf::from("devin"))
        });
    #[cfg(not(target_os = "windows"))]
    let base = dirs::home_dir()
        .map(|h| h.join(".local/share/devin"))
        .unwrap_or_else(|| PathBuf::from(".local/share/devin"));
    vec![
        ("cli".into(), base.join("cli/sessions.db")),
        ("cli-next".into(), base.join("cli-next/sessions.db")),
    ]
}

fn open_readonly(path: &Path) -> Result<Connection, String> {
    let conn = Connection::open_with_flags(
        path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_NO_MUTEX,
    )
    .map_err(|e| format!("{}: {}", path.display(), e))?;
    // Avoid blocking forever if another process holds a write lock.
    conn.busy_timeout(std::time::Duration::from_millis(1000))
        .map_err(|e| format!("{}: {}", path.display(), e))?;
    Ok(conn)
}

/// Parse sessions.metadata.response_dimensions for real model + token totals.
fn parse_metadata(meta_json: &str) -> (Option<String>, f64, f64, f64, f64) {
    let mut real_model = None;
    let (mut i, mut o, mut c, mut m) = (0.0, 0.0, 0.0, 0.0);
    if let Ok(v) = serde_json::from_str::<Value>(meta_json) {
        if let Some(dims) = v.get("response_dimensions").and_then(|d| d.as_array()) {
            for d in dims {
                let uid = d.get("uid").and_then(|u| u.as_str()).unwrap_or("");
                let kind = d.get("kind");
                let val = kind
                    .and_then(|k| k.get("Metric").or_else(|| k.get("CumulativeMetric")))
                    .and_then(|mtr| mtr.get("value"));
                match uid {
                    "model" => real_model = val.and_then(|x| x.as_str()).map(|s| s.to_string()),
                    "input_tokens" => i = val.and_then(|x| x.as_f64()).unwrap_or(0.0),
                    "output_tokens" => o = val.and_then(|x| x.as_f64()).unwrap_or(0.0),
                    "cached_input_tokens" => c = val.and_then(|x| x.as_f64()).unwrap_or(0.0),
                    "agent_messages" => m = val.and_then(|x| x.as_f64()).unwrap_or(0.0),
                    _ => {}
                }
            }
        }
    }
    (real_model, i, o, c, m)
}

/// 读取会话列表。
///
/// 返回窗口内**所有**会话的 (id, created_at, last_activity_at)，包括 `hidden`
/// 的会话：hidden 只影响会话列表要不要显示它，不影响它真实花掉的 token。用量
/// 统计按 turn 汇总，如果只查未隐藏会话，被隐藏会话的消费会静默消失。
fn load_sessions_from(
    conn: &Connection,
    source: &str,
    out: &mut Vec<SessionRec>,
) -> Vec<(String, i64, i64)> {
    let sql = format!(
        "SELECT id, ifnull(title,''), ifnull(working_directory,''), \
               ifnull(model,''), ifnull(agent_mode,''), created_at, last_activity_at, \
               ifnull(metadata,''), ifnull(hidden,0) FROM sessions \
               ORDER BY last_activity_at DESC LIMIT {}",
        MAX_SESSIONS_PER_SOURCE + 1
    );
    let mut stmt = match conn.prepare(&sql) {
        Ok(s) => s,
        Err(e) => {
            out.reserve(0);
            eprintln!("prepare failed for {source}: {e}");
            return Vec::new();
        }
    };
    let rows = stmt.query_map([], |r| {
        Ok((
            r.get::<_, String>(0)?,
            r.get::<_, String>(1)?,
            r.get::<_, String>(2)?,
            r.get::<_, String>(3)?,
            r.get::<_, String>(4)?,
            r.get::<_, i64>(5)?,
            r.get::<_, i64>(6)?,
            r.get::<_, String>(7)?,
            r.get::<_, i64>(8)?,
        ))
    });
    let rows = match rows {
        Ok(r) => r,
        Err(e) => {
            eprintln!("query failed for {source}: {e}");
            return Vec::new();
        }
    };
    let mut all = Vec::new();
    let mut truncated = 0usize;
    for row in rows.flatten() {
        let (id, title, wd, model, mode, created, last_act, meta, hidden) = row;
        if all.len() >= MAX_SESSIONS_PER_SOURCE {
            // 多取一行的哨兵：能取到说明已经截断，报出来而不是默默丢掉。
            truncated += 1;
            continue;
        }
        all.push((id.clone(), created, last_act));
        if hidden != 0 {
            continue;
        }
        let (real_model, ti, to, tc, msgs) = parse_metadata(&meta);
        out.push(SessionRec {
            agent: AgentKind::Devin,
            key: format!("{source}/{id}"),
            source: source.to_string(),
            id,
            title,
            working_directory: wd,
            selected_model: model,
            real_model,
            agent_mode: mode,
            created_at: created,
            last_activity_at: last_act,
            input_tokens: ti,
            output_tokens: to,
            cached_tokens: tc,
            cache_creation_tokens: 0.0,
            cache_creation_5m_tokens: 0.0,
            cache_creation_1h_tokens: 0.0,
            agent_messages: msgs,
            recorded_cost: None,
            ..Default::default()
        });
    }
    if truncated > 0 {
        log_event(format!(
            "devin sessions truncated source={source} over_limit={truncated} kept={MAX_SESSIONS_PER_SOURCE}"
        ));
        eprintln!(
            "WARN {source}: 会话数超过上限 {MAX_SESSIONS_PER_SOURCE}，已忽略更早的会话，统计会偏低"
        );
    }
    all
}

/// Load per-turn metrics for sessions overlapping [start, end).
fn load_turns_from(
    conn: &Connection,
    source: &str,
    session_ids: &[String],
    start: i64,
    end: i64,
    out: &mut Vec<TurnRec>,
) {
    if session_ids.is_empty() {
        return;
    }

    // Build IN clause for batch query
    let placeholders: Vec<String> = session_ids
        .iter()
        .enumerate()
        .map(|(i, _)| format!("?{}", i + 1))
        .collect();
    let in_clause = placeholders.join(",");

    // 不在 SQLite 中解析 chat_message：它通常是几十 KB 的大 JSON，SQLite 的
    // json_extract 会重复扫描整段内容。只按会话和时间筛选，metrics 交给 Rust
    // 的反序列化器提取需要的字段。
    // LIKE 在 SQLite 的 C 层完成大 JSON 正文扫描：没有真实 metrics 的节点不再
    // 物化成 Rust 字符串。用 "ttft_ms" 而不是 "metrics"——后者作为 key 在所有
    // 节点上都存在（值为 null），没有筛选力。实测 "ttft_ms" 是「该节点带有真实
    // metrics 对象」的精确代理（匹配集与 metrics != null 完全重合）；正文恰好
    // 含该字样的误报交给 Rust 侧过滤，不影响正确性。
    //
    // created_at 是**落盘**时间，可能比 metadata 里的真实生成时间晚若干天，所以
    // 这里向两侧放宽 ROW_TIME_SLACK_SECS，精确过滤交给下面按 metadata 时间的判断。
    let sql = format!(
        "SELECT session_id, created_at, chat_message \
         FROM message_nodes \
         WHERE session_id IN ({}) AND created_at >= ?{} AND created_at < ?{} \
           AND chat_message LIKE '%\"ttft_ms\"%'",
        in_clause,
        session_ids.len() + 1,
        session_ids.len() + 2
    );
    let query_start = start.saturating_sub(ROW_TIME_SLACK_LOWER_SECS);
    let query_end = end.saturating_add(ROW_TIME_SLACK_SECS);

    let mut stmt = match conn.prepare(&sql) {
        Ok(s) => s,
        Err(e) => {
            eprintln!("turns prepare failed for {source}: {e}");
            return;
        }
    };

    // Build params: session_ids + start + end
    let mut params: Vec<&dyn rusqlite::types::ToSql> = Vec::new();
    for sid in session_ids {
        params.push(sid);
    }
    params.push(&query_start);
    params.push(&query_end);

    let rows = stmt.query_map(params.as_slice(), |r| {
        Ok((
            r.get::<_, String>(0)?,
            r.get::<_, i64>(1)?,
            r.get::<_, String>(2)?,
        ))
    });

    let rows = match rows {
        Ok(r) => r,
        Err(e) => {
            eprintln!("turns query failed for {source}: {e}");
            return;
        }
    };

    // Devin 会把同一次响应以多个 message_nodes 兄弟节点持久化（实测平均 2.5 份，
    // 最多 9 份）；这些节点共享 message_id/request_id。用稳定的 message_id 去重，
    // 并且**不依赖查询窗口**：每条记录带上 dedup_key，缓存快照里的旧 turn 也参与
    // 同一个去重集合，避免「不同窗口各自留下一份副本」导致重复计数。
    let mut seen_message_ids = HashSet::new();
    for row in rows.flatten() {
        let (session_id, row_created_at, chat_message) = row;
        // SQL 已用 LIKE 预筛（见上），这里是第二道保险。
        if !chat_message.contains("\"ttft_ms\"") {
            continue;
        }
        let Ok(message) = serde_json::from_str::<DevinChatMessage>(&chat_message) else {
            continue;
        };
        let Some(metadata) = message.metadata.as_ref() else {
            continue;
        };
        // metrics 存在就说明是一次真实 inference：`telemetry.operation` 全部是
        // "inference"，token 字段齐全。曾经用 `ttft_ms > 0` 当「有真实指标」的
        // 代理，结果把 37% 的真实调用（ttft_ms 为 null 的 tool_calls 轮次，
        // 30 天窗口内约 39% 的 cache_read token）整段丢弃。ttft 只能当可选指标。
        let Some(metrics) = metadata.metrics.as_ref() else {
            continue;
        };
        let ttft = metrics.ttft_ms.unwrap_or(0.0).max(0.0);
        let source_id = message
            .message_id
            .as_deref()
            .filter(|id| !id.is_empty())
            .or_else(|| metadata.request_id.as_deref().filter(|id| !id.is_empty()));
        // 真实生成时间：metadata 里的 RFC3339；缺失时回退到落盘时间。
        let created_at = metadata
            .started_generation_at
            .as_deref()
            .or(metadata.created_at.as_deref())
            .and_then(parse_rfc3339_secs)
            .filter(|at| *at > 0)
            .unwrap_or(row_created_at);
        // 同一逻辑 turn 的若干副本共享同一个 metadata 时间，所以先去重再按窗口
        // 过滤，结果与窗口无关。
        let dedup_key = source_id.map(|id| format!("{session_id}\0{id}"));
        if let Some(key) = dedup_key.as_deref() {
            if !seen_message_ids.insert(key.to_owned()) {
                continue;
            }
        }
        if created_at < start || created_at >= end {
            continue;
        }
        let model = metadata
            .generation_model
            .as_deref()
            .unwrap_or("")
            .to_owned();
        out.push(TurnRec {
            agent: AgentKind::Devin,
            session_key: format!("{source}/{session_id}"),
            created_at,
            input_tokens: metrics.input_tokens.unwrap_or(0.0),
            output_tokens: metrics.output_tokens.unwrap_or(0.0),
            cache_read_tokens: metrics.cache_read_tokens.unwrap_or(0.0),
            cache_creation_tokens: metrics.cache_creation_tokens.unwrap_or(0.0),
            cache_creation_5m_tokens: 0.0,
            cache_creation_1h_tokens: 0.0,
            model,
            ttft_ms: ttft,
            total_time_ms: metrics.total_time_ms.unwrap_or(0.0),
            recorded_cost: None,
            dedup_key: dedup_key.unwrap_or_default(),
            ..Default::default()
        });
    }
}

/// 解析 RFC3339 时间戳（Devin metadata 用 UTC 的 "...Z" 形式）为 Unix 秒。
fn parse_rfc3339_secs(text: &str) -> Option<i64> {
    chrono::DateTime::parse_from_rfc3339(text)
        .ok()
        .map(|date| date.timestamp())
}

fn turn_key(turn: &TurnRec) -> String {
    format!(
        "{}:{}:{}:{}:{:x}:{:x}:{:x}:{:x}:{:x}:{:x}",
        turn.agent.label(),
        turn.session_key,
        turn.created_at,
        turn.model,
        turn.input_tokens.to_bits(),
        turn.output_tokens.to_bits(),
        turn.cache_read_tokens.to_bits(),
        turn.cache_creation_tokens.to_bits(),
        turn.ttft_ms.to_bits(),
        turn.total_time_ms.to_bits(),
    )
}

/// turn 的唯一身份：有 dedup_key 的用 dedup_key（跨查询窗口稳定），否则退回
/// 内容指纹。缓存快照和新解析结果必须用同一套身份判定，否则增量合并无法识别
/// 同一条记录的两次副本。
fn turn_identity(turn: &TurnRec) -> String {
    if turn.dedup_key.is_empty() {
        turn_key(turn)
    } else {
        format!(
            "{}:{}:{}",
            turn.agent.label(),
            turn.session_key,
            turn.dedup_key
        )
    }
}

fn dedup_cached_turns(turns: &mut Vec<TurnRec>) {
    let mut seen = HashSet::new();
    turns.retain(|turn| seen.insert(turn_identity(turn)));
}

fn load_source(
    source: String,
    path: PathBuf,
    start: i64,
    end: i64,
    previous: Option<&LoadedData>,
) -> LoadedData {
    let mut data = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };
    if !path.exists() {
        data.errors.push(DataError {
            agent: AgentKind::Devin,
            message: i18n::tf(i18n::Key::ErrNotExist, &[&path.display().to_string()]),
        });
        return data;
    }
    let conn = match open_readonly(&path) {
        Ok(connection) => connection,
        Err(error) => {
            data.errors.push(DataError {
                agent: AgentKind::Devin,
                message: i18n::tf(i18n::Key::ErrOpenFailed, &[&error.to_string()]),
            });
            return data;
        }
    };
    let sessions_started = Instant::now();
    let all_sessions = load_sessions_from(&conn, &source, &mut data.sessions);
    let sessions_elapsed = sessions_started.elapsed();
    // 全量范围用于判断会话是否与当前页重叠。这里用含 hidden 的完整列表，
    // 否则被隐藏会话的 turn 会被一并排除出用量统计。
    let all_ids: Vec<String> = all_sessions
        .iter()
        .filter(|(_, created_at, last_activity_at)| *last_activity_at >= start && *created_at < end)
        .map(|(id, _, _)| id.clone())
        .collect();
    let prefix = format!("{source}/");
    let mut cached_turns: Vec<TurnRec> = previous
        .into_iter()
        .flat_map(|cached| cached.turns.iter())
        .filter(|turn| {
            turn.agent == AgentKind::Devin
                && turn.session_key.starts_with(&prefix)
                && turn.created_at >= start
                && turn.created_at < end
        })
        .cloned()
        .collect();
    // 旧版本可能已经把重复节点写入缓存；先稳定清理缓存自身，再做增量合并。
    dedup_cached_turns(&mut cached_turns);
    let cached_count = cached_turns.len();
    let query_start = cached_turns
        .iter()
        .map(|turn| turn.created_at)
        .max()
        .map(|latest| start.max(latest))
        .unwrap_or(start);
    // 增量查询时只传入可能在 [query_start, end) 窗口内有新 turn 的会话。
    // 一个会话的最后活动时间 < query_start 时，不可能在该窗口内有新消息，
    // 排除后可将 IN 列表从 ~195 缩小到几个，大幅减少 SQLite 扫描量。
    // last_activity_at 也是落盘时间，和行的 created_at 一样可能落后于真实生成
    // 时间，因此同样向外放宽，宁可多查几个会话不能漏。
    let ids: Vec<String> = if query_start > start {
        let floor = query_start.saturating_sub(ROW_TIME_SLACK_LOWER_SECS);
        all_sessions
            .iter()
            .filter(|(_, created_at, last_activity_at)| {
                *last_activity_at >= floor && *created_at < end
            })
            .map(|(id, _, _)| id.clone())
            .collect()
    } else {
        all_ids.clone()
    };
    let turns_started = Instant::now();
    load_turns_from(&conn, &source, &ids, query_start, end, &mut data.turns);
    let queried_turns = data.turns.len();
    if !cached_turns.is_empty() {
        let seen: HashSet<String> = cached_turns.iter().map(turn_identity).collect();
        let mut merged = cached_turns;
        for turn in data.turns.drain(..) {
            // 查询窗口包含最新缓存时间点；这里只排除与缓存重叠的记录。
            // 用 turn_identity（dedup_key 优先）而不是内容指纹：同一条逻辑 turn
            // 在不同窗口下的落盘时间不同，内容指纹认不出来，会被重复计入。
            if !seen.contains(&turn_identity(&turn)) {
                merged.push(turn);
            }
        }
        data.turns = merged;
    }
    log_event(format!(
        "source=Devin/{source} sessions_ms={} turns_ms={} sessions={} all_ids={} query_ids={} query_start={} cached_turns={} queried_turns={} turns={}",
        sessions_elapsed.as_millis(),
        turns_started.elapsed().as_millis(),
        data.sessions.len(),
        all_ids.len(),
        ids.len(),
        query_start,
        cached_count,
        queried_turns,
        data.turns.len()
    ));
    data
}

fn load_uncached(start: i64, end: i64, previous: Option<Arc<LoadedData>>) -> LoadedData {
    let started = Instant::now();
    log_event(format!(
        "load_uncached start={start} end={end} incremental_cache={}",
        previous.is_some()
    ));
    let sources = devin_db_paths();
    // Run all sources on rayon's single global pool. The SQLite loaders are
    // single tasks, while the file loaders fan out their file parsing with
    // `par_iter` on the same pool, so work-stealing balances them without
    // oversubscribing the CPU.
    let parts: std::sync::Mutex<Vec<LoadedData>> = std::sync::Mutex::new(Vec::new());
    rayon::scope(|scope| {
        for (source, path) in sources {
            let parts = &parts;
            let previous = previous.clone();
            scope.spawn(move |_| {
                let data = load_source(source.clone(), path, start, end, previous.as_deref());
                parts.lock().unwrap().push(data);
            });
        }
        scope.spawn(|_| {
            let started = Instant::now();
            let data = local_sources::load_claude(start, end, previous.as_deref());
            log_loaded_part("Claude Code", started, &data);
            parts.lock().unwrap().push(data);
        });
        scope.spawn(|_| {
            let started = Instant::now();
            let data = local_sources::load_codex(start, end, previous.as_deref());
            log_loaded_part("Codex", started, &data);
            parts.lock().unwrap().push(data);
        });
        scope.spawn(|_| {
            let started = Instant::now();
            let data = local_sources::load_antigravity(start, end, previous.as_deref());
            log_loaded_part("Antigravity", started, &data);
            parts.lock().unwrap().push(data);
        });
        scope.spawn(|_| {
            let started = Instant::now();
            let data = local_sources::load_grok(start, end, previous.as_deref());
            log_loaded_part("Grok Build", started, &data);
            parts.lock().unwrap().push(data);
        });
        scope.spawn(|_| {
            let started = Instant::now();
            let data = local_sources::load_zcode(start, end);
            log_loaded_part("ZCode", started, &data);
            parts.lock().unwrap().push(data);
        });
        scope.spawn(|_| {
            let started = Instant::now();
            let data = local_sources::load_opencode(start, end);
            log_loaded_part("OpenCode", started, &data);
            parts.lock().unwrap().push(data);
        });
        scope.spawn(|_| {
            let started = Instant::now();
            let data = local_sources::load_pi(start, end);
            log_loaded_part("pi-agent", started, &data);
            parts.lock().unwrap().push(data);
        });
        scope.spawn(|_| {
            let started = Instant::now();
            let data = local_sources::load_mimocode(start, end);
            log_loaded_part("MimoCode", started, &data);
            parts.lock().unwrap().push(data);
        });
    });
    let parts = parts.into_inner().unwrap();

    let mut data = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };
    for mut part in parts {
        data.sessions.append(&mut part.sessions);
        data.turns.append(&mut part.turns);
        data.errors.append(&mut part.errors);
        data.file_mtimes.extend(part.file_mtimes);
        data.file_sessions.extend(part.file_sessions);
    }
    data.turns.sort_by_key(|turn| turn.created_at);
    stamp_device(&mut data);
    let save_started = Instant::now();
    save_to_cache(&data);
    log_event(format!(
        "load_uncached done elapsed_ms={} save_cache_ms={} sessions={} turns={} errors={}",
        started.elapsed().as_millis(),
        save_started.elapsed().as_millis(),
        data.sessions.len(),
        data.turns.len(),
        data.errors.len()
    ));
    data
}

fn load_selected_agent(
    start: i64,
    end: i64,
    agent: AgentKind,
    previous: Option<Arc<LoadedData>>,
) -> LoadedData {
    if agent == AgentKind::Devin {
        let parts: std::sync::Mutex<Vec<LoadedData>> = std::sync::Mutex::new(Vec::new());
        rayon::scope(|scope| {
            for (source, path) in devin_db_paths() {
                let parts = &parts;
                let previous = previous.clone();
                scope.spawn(move |_| {
                    let data = load_source(source, path, start, end, previous.as_deref());
                    parts.lock().unwrap().push(data);
                });
            }
        });
        let parts = parts.into_inner().unwrap();
        let mut data = LoadedData {
            turns_start: start,
            turns_end: end,
            ..Default::default()
        };
        for mut part in parts {
            data.sessions.append(&mut part.sessions);
            data.turns.append(&mut part.turns);
            data.errors.append(&mut part.errors);
        }
        data.turns.sort_by_key(|turn| turn.created_at);
        stamp_device(&mut data);
        data
    } else {
        let mut data = match agent {
            AgentKind::Amp => local_sources::load_amp(start, end, previous.as_deref()),
            AgentKind::Claude => local_sources::load_claude(start, end, previous.as_deref()),
            AgentKind::Codex => local_sources::load_codex(start, end, previous.as_deref()),
            AgentKind::Antigravity => {
                local_sources::load_antigravity(start, end, previous.as_deref())
            }
            AgentKind::Grok => local_sources::load_grok(start, end, previous.as_deref()),
            AgentKind::ZCode => local_sources::load_zcode(start, end),
            AgentKind::OpenCode => local_sources::load_opencode(start, end),
            AgentKind::Pi => local_sources::load_pi(start, end),
            AgentKind::MimoCode => local_sources::load_mimocode(start, end),
            AgentKind::Devin => unreachable!(),
        };
        stamp_device(&mut data);
        data
    }
}

/// 强制重新加载指定 Agent 的目标时间窗口。
///
/// `previous` 可能已经合并了远端设备数据，因此不能作为本次本机解析的增量
/// 输入，也不能把目标窗口的旧 turn 留在结果中。否则每次手动刷新都会把远端
/// 或旧快照残留的 turn 再与新结果叠加，造成用量不断偏大。
pub fn reload_agent_from(
    previous: Arc<LoadedData>,
    start: i64,
    end: i64,
    agent: AgentKind,
) -> LoadedData {
    // 强制刷新必须直接读取源数据。传入 previous 会复用已合并远端设备的 turn，
    // 使其被错误视为本机缓存。
    let replacement = load_selected_agent(start, end, agent, None);
    let local_id = device_id();
    let mut data = LoadedData {
        sessions: previous
            .sessions
            .iter()
            .filter(|session| {
                session.agent != agent
                    && (session.device_id.is_empty() || session.device_id == local_id)
            })
            .cloned()
            .collect(),
        turns: previous
            .turns
            .iter()
            .filter(|turn| {
                turn.agent != agent && (turn.device_id.is_empty() || turn.device_id == local_id)
            })
            .cloned()
            .collect(),
        errors: previous
            .errors
            .iter()
            .filter(|error| error.agent != agent)
            .cloned()
            .collect(),
        file_mtimes: previous.file_mtimes.clone(),
        file_sessions: previous.file_sessions.clone(),
        ..Default::default()
    };
    save_agent_cache(agent, &replacement);
    data.sessions.extend(replacement.sessions);
    data.errors.extend(replacement.errors);
    data.file_mtimes.extend(replacement.file_mtimes);
    data.file_sessions.extend(replacement.file_sessions);
    data.turns.extend(replacement.turns);
    data.turns_start = start;
    data.turns_end = end;
    data.turns.sort_by_key(|turn| turn.created_at);
    log_event(format!(
        "reload_agent_from agent={} source_fresh=true range={}..{} sessions={} turns={}",
        agent.label(),
        data.turns_start,
        data.turns_end,
        data.sessions.len(),
        data.turns.len()
    ));
    data
}

/// Load everything, preferring a recent on-disk snapshot for fast startup.
pub fn load_all(start: i64, end: i64) -> LoadedData {
    if let Some(data) = load_from_cache(start, end) {
        log_event(format!(
            "load_all cache_hit sessions={} turns={}",
            data.sessions.len(),
            data.turns.len()
        ));
        data
    } else {
        log_event("load_all cache_miss");
        load_uncached(start, end, None)
    }
}

/// 忽略聚合缓存并重新读取全部 Agent 数据源。
pub fn reload_all(start: i64, end: i64) -> LoadedData {
    load_uncached(start, end, None)
}

#[cfg(test)]
mod tests {
    use super::{
        cache_covers, dedup_cached_turns, load_sessions_from, load_turns_from, AgentKind, TurnRec,
    };
    use rusqlite::{params, Connection};

    #[test]
    fn json_extract_available() {
        let conn = Connection::open_in_memory().unwrap();
        let result: i64 = conn
            .query_row("SELECT json_extract('{\"a\":5}', '$.a');", [], |r| r.get(0))
            .unwrap();
        assert_eq!(result, 5);
    }

    #[test]
    fn recent_cache_covers_a_moving_future_end() {
        let cached_at = 10_000;
        assert!(cache_covers(
            cached_at,
            1_000,
            cached_at + 3_600,
            2_000,
            cached_at + 3_720,
            cached_at + 120,
        ));
    }

    #[test]
    fn expired_cache_is_rejected() {
        let cached_at = 10_000;
        assert!(!cache_covers(
            cached_at,
            1_000,
            cached_at + 3_600,
            2_000,
            cached_at + 4_000,
            cached_at + 301,
        ));
    }

    #[test]
    fn devin_turns_read_cache_creation_and_dedup_by_message_id() {
        let conn = Connection::open_in_memory().unwrap();
        conn.execute(
            "CREATE TABLE message_nodes (session_id TEXT, created_at INTEGER, chat_message TEXT)",
            [],
        )
        .unwrap();
        let message = |message_id: &str| {
            serde_json::json!({
                "message_id": message_id,
                "metadata": {
                    "generation_model": "gpt-5-6-luna-high",
                    "metrics": {
                        "ttft_ms": 5291,
                        "input_tokens": 3,
                        "output_tokens": 310,
                        "cache_read_tokens": null,
                        "cache_creation_tokens": 95970,
                        "total_time_ms": 6000
                    }
                }
            })
            .to_string()
        };
        for message_id in ["same-response", "same-response", "other-response"] {
            conn.execute(
                "INSERT INTO message_nodes VALUES (?1, ?2, ?3)",
                params!["session-1", 100_i64, message(message_id)],
            )
            .unwrap();
        }

        let mut turns = Vec::new();
        load_turns_from(&conn, "cli", &["session-1".to_string()], 0, 200, &mut turns);

        // 相同 message_id 的兄弟节点只保留一次；指标相同但 message_id 不同的
        // 两次真实请求必须同时保留。
        assert_eq!(turns.len(), 2);
        assert!(turns
            .iter()
            .all(|turn| turn.cache_creation_tokens == 95_970.0));
    }

    #[test]
    fn devin_nodes_without_metrics_are_skipped_cheaply() {
        let conn = Connection::open_in_memory().unwrap();
        conn.execute(
            "CREATE TABLE message_nodes (session_id TEXT, created_at INTEGER, chat_message TEXT)",
            [],
        )
        .unwrap();
        // 无 metrics 的普通节点
        let user_node = serde_json::json!({
            "message_id": "user-1",
            "metadata": { "finish_reason": null }
        })
        .to_string();
        // 正文恰好含 "metrics" 字样但没有真正的 metrics 字段——快筛的误报
        // 必须仍被完整解析路径正常过滤，不能产生 turn
        let false_positive = serde_json::json!({
            "message_id": "user-2",
            "metadata": { "text": "the word \"metrics\" appears here" }
        })
        .to_string();
        for node in [&user_node, &false_positive] {
            conn.execute(
                "INSERT INTO message_nodes VALUES (?1, ?2, ?3)",
                params!["session-1", 100_i64, node],
            )
            .unwrap();
        }

        let mut turns = Vec::new();
        load_turns_from(&conn, "cli", &["session-1".to_string()], 0, 200, &mut turns);

        assert!(turns.is_empty());
    }

    #[test]
    fn old_cached_devin_duplicates_are_removed_with_full_metric_key() {
        let base = TurnRec {
            agent: AgentKind::Devin,
            session_key: "cli/session-1".into(),
            created_at: 100,
            input_tokens: 3.0,
            output_tokens: 310.0,
            cache_read_tokens: 0.0,
            cache_creation_tokens: 95_970.0,
            model: "gpt-5-6-luna-high".into(),
            ttft_ms: 5291.0,
            total_time_ms: 6000.0,
            ..Default::default()
        };
        let mut different_cache_write = base.clone();
        different_cache_write.cache_creation_tokens += 1.0;
        let mut turns = vec![base.clone(), base, different_cache_write];

        dedup_cached_turns(&mut turns);

        assert_eq!(turns.len(), 2);
    }

    /// 回归：`hidden = 1` 只应影响会话列表，不应影响用量统计。被隐藏的会话同样
    /// 真实花掉了 token，若把它的 id 从 turn 查询里一并排除，这部分消费会静默
    /// 消失（本机实测：1 个隐藏会话 / 3 input + 425 output + 61,686 cache_write）。
    #[test]
    fn devin_hidden_sessions_still_contribute_their_turns() {
        let conn = Connection::open_in_memory().unwrap();
        conn.execute(
            "CREATE TABLE sessions (id TEXT, title TEXT, working_directory TEXT, model TEXT, \
             agent_mode TEXT, created_at INTEGER, last_activity_at INTEGER, metadata TEXT, \
             hidden INTEGER)",
            [],
        )
        .unwrap();
        for (id, hidden) in [("visible", 0_i64), ("hidden-one", 1_i64)] {
            conn.execute(
                "INSERT INTO sessions VALUES (?1, '', '', '', '', 100, 200, '', ?2)",
                params![id, hidden],
            )
            .unwrap();
        }

        let mut sessions = Vec::new();
        let all = load_sessions_from(&conn, "cli", &mut sessions);

        assert_eq!(sessions.len(), 1, "隐藏会话不进会话列表");
        assert_eq!(sessions[0].id, "visible");
        assert_eq!(all.len(), 2, "但它的 id 必须留给 turn 查询用");
        assert!(all.iter().any(|(id, _, _)| id == "hidden-one"));
    }

    /// 回归：`ttft_ms` 为 null 的节点也是真实 inference（`telemetry.operation`
    /// 全是 "inference"，token 字段齐全）。以前用 `ttft_ms > 0` 当“有真实指标”的
    /// 代理，30 天窗口内会丢掉 36% 的轮次和 39% 的 cache_read token。
    #[test]
    fn devin_turns_without_ttft_are_still_counted() {
        let conn = Connection::open_in_memory().unwrap();
        conn.execute(
            "CREATE TABLE message_nodes (session_id TEXT, created_at INTEGER, chat_message TEXT)",
            [],
        )
        .unwrap();
        // tool_calls 类型的响应常写成 ttft_ms: null
        let no_ttft = serde_json::json!({
            "message_id": "tool-turn",
            "metadata": {
                "generation_model": "glm-5-2",
                "finish_reason": "tool_calls",
                "metrics": {
                    "ttft_ms": null,
                    "input_tokens": 668,
                    "output_tokens": 347,
                    "cache_read_tokens": 25684,
                    "cache_creation_tokens": null,
                    "total_time_ms": 3054
                }
            }
        })
        .to_string();
        let with_ttft = serde_json::json!({
            "message_id": "streamed-turn",
            "metadata": {
                "generation_model": "glm-5-2",
                "metrics": {
                    "ttft_ms": 2142,
                    "input_tokens": 331,
                    "output_tokens": 1164,
                    "cache_read_tokens": 54337,
                    "cache_creation_tokens": null,
                    "total_time_ms": 24885
                }
            }
        })
        .to_string();
        for node in [&no_ttft, &with_ttft] {
            conn.execute(
                "INSERT INTO message_nodes VALUES (?1, ?2, ?3)",
                params!["session-1", 100_i64, node],
            )
            .unwrap();
        }

        let mut turns = Vec::new();
        load_turns_from(&conn, "cli", &["session-1".to_string()], 0, 200, &mut turns);

        assert_eq!(turns.len(), 2, "ttft_ms 为 null 的真实轮次不能被丢掉");
        let tool_turn = turns
            .iter()
            .find(|turn| turn.input_tokens == 668.0)
            .expect("缺 ttft 的轮次必须在结果里");
        assert_eq!(tool_turn.ttft_ms, 0.0, "缺 ttft 时按 0 处理，不影响计费");
        assert_eq!(tool_turn.cache_read_tokens, 25_684.0);
    }

    /// 回归：`message_nodes.created_at` 是节点回写时间（实测中位数滞后 1.6h，
    /// 最大 1.5 天），真实生成时间在 `metadata.started_generation_at`。按行时间
    /// 分天会让 21% 的轮次落到错误的日期，而且落点随查询窗口变化。
    #[test]
    fn devin_turn_time_comes_from_metadata_not_row_write_time() {
        let conn = Connection::open_in_memory().unwrap();
        conn.execute(
            "CREATE TABLE message_nodes (session_id TEXT, created_at INTEGER, chat_message TEXT)",
            [],
        )
        .unwrap();
        let node = |message_id: &str, started: &str| {
            serde_json::json!({
                "message_id": message_id,
                "metadata": {
                    "generation_model": "glm-5-2",
                    "started_generation_at": started,
                    "created_at": started,
                    "metrics": {
                        "ttft_ms": 100,
                        "input_tokens": 10,
                        "output_tokens": 1,
                        "total_time_ms": 1000
                    }
                }
            })
            .to_string()
        };
        // 行回写时间远超窗口上界（200），但真实生成时间是 100（=1970-01-01T00:01:40Z），
        // 必须算在窗口内。行时间在 slack（7 天）内，SQL 才会把它带出来。
        conn.execute(
            "INSERT INTO message_nodes VALUES (?1, ?2, ?3)",
            params![
                "session-1",
                200_000_i64,
                node("late-write", "1970-01-01T00:01:40Z")
            ],
        )
        .unwrap();
        // 行时间落在窗口内，但真实生成时间是 300（窗口外）——不能被算进来。
        conn.execute(
            "INSERT INTO message_nodes VALUES (?1, ?2, ?3)",
            params![
                "session-1",
                150_i64,
                node("early-write", "1970-01-01T00:05:00Z")
            ],
        )
        .unwrap();

        let mut turns = Vec::new();
        load_turns_from(&conn, "cli", &["session-1".to_string()], 0, 200, &mut turns);

        assert_eq!(turns.len(), 1, "必须按 metadata 时间而不是行时间过滤窗口");
        assert_eq!(turns[0].created_at, 100);
    }

    /// 回归：同一条逻辑 turn 的多个副本会带上不同的行回写时间。去重必须按
    /// dedup_key（且缓存快照也参与同一个集合），否则不同查询窗口会各自留下一
    /// 份副本，总量随刷新次数缓慢膨胀。
    #[test]
    fn devin_duplicate_copies_collapse_across_write_times_and_agents() {
        let conn = Connection::open_in_memory().unwrap();
        conn.execute(
            "CREATE TABLE message_nodes (session_id TEXT, created_at INTEGER, chat_message TEXT)",
            [],
        )
        .unwrap();
        let node = serde_json::json!({
            "message_id": "same-response",
            "metadata": {
                "generation_model": "glm-5-2",
                "started_generation_at": "1970-01-01T00:01:40Z",
                "metrics": {
                    "ttft_ms": 2142,
                    "input_tokens": 331,
                    "output_tokens": 1164,
                    "cache_read_tokens": 54337,
                    "total_time_ms": 24885
                }
            }
        })
        .to_string();
        for row_time in [1_000_i64, 100_000, 200_000] {
            conn.execute(
                "INSERT INTO message_nodes VALUES (?1, ?2, ?3)",
                params!["session-1", row_time, node],
            )
            .unwrap();
        }

        let mut turns = Vec::new();
        load_turns_from(&conn, "cli", &["session-1".to_string()], 0, 300, &mut turns);
        assert_eq!(turns.len(), 1, "三份副本只能算一轮");
        assert_eq!(turns[0].created_at, 100);
        assert_eq!(turns[0].dedup_key, "session-1\0same-response");

        // 缓存快照里已经存在同一 dedup_key、但 created_at 不同的旧 turn 时，
        // dedup_cached_turns 也必须能把它收敛成一条。
        let mut old = turns[0].clone();
        old.created_at = 99;
        let mut cache = vec![turns[0].clone(), old];
        dedup_cached_turns(&mut cache);
        assert_eq!(cache.len(), 1);

        // 同一 dedup_key 语义对 Codex 的 (session#signature) 同样成立。
        let codex = |created_at: i64| TurnRec {
            agent: AgentKind::Codex,
            session_key: "codex/s1".into(),
            created_at,
            input_tokens: 5.0,
            dedup_key: "s1#5:0:0:1".into(),
            ..Default::default()
        };
        let mut codex_turns = vec![codex(100), codex(200)];
        dedup_cached_turns(&mut codex_turns);
        assert_eq!(codex_turns.len(), 1);
    }
}
