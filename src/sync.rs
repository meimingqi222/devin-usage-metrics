//! 多设备数据同步：通过 iCloud Drive（或其他用户配置的共享目录）交换数据包。
//!
//! 工作方式：
//! - 用户在设置中开启同步并配置共享目录后，后台每 `SYNC_INTERVAL_SECS` 秒自动：
//!   1) 把本机已加载的数据导出成 `device-<id>.json` 写入共享目录
//!   2) 扫描共享目录下其他设备的数据包并合并进当前数据
//! - 同步默认关闭，用户可在侧边栏的同步开关处一键开启/关闭。
//! - 不发起任何网络请求，同步完全依赖用户已有的云盘（iCloud Drive / Dropbox 等）。

use crate::data::{self, LoadedData, SessionRec, TurnRec};
use std::collections::HashSet;
use std::fs;
use std::io::{BufReader, BufWriter, Read, Write};
use std::path::PathBuf;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

/// 自动同步间隔（秒）。iCloud Drive 的文件传播通常在 10-60 秒内完成，
/// 这里取 60 秒避免过于频繁的磁盘扫描。
pub const SYNC_INTERVAL_SECS: u64 = 60;

/// 同步配置：enabled + sync_dir，持久化到 sync.json。
#[derive(Debug, Clone, Default)]
pub struct SyncConfig {
    pub enabled: bool,
    pub sync_dir: String,
}

/// 默认同步目录：
/// - macOS：iCloud Drive 下的 `devin-usage-metrics/`
/// - 其他平台：`~/.devin-usage-metrics-sync/`（用户需自行用 Syncthing/Dropbox 等同步）
pub fn default_sync_dir() -> PathBuf {
    #[cfg(target_os = "macos")]
    {
        // iCloud Drive 的 Mobile Documents 路径
        let icloud = dirs::home_dir()
            .map(|h| {
                h.join("Library/Mobile Documents/com~apple~CloudDocs/devin-usage-metrics")
            })
            .unwrap_or_else(|| PathBuf::from("devin-usage-metrics"));
        if icloud.exists() || icloud.parent().map(|p| p.exists()).unwrap_or(false) {
            return icloud;
        }
        // iCloud 未启用时回退到本地目录
        dirs::home_dir()
            .map(|h| h.join(".devin-usage-metrics-sync"))
            .unwrap_or_else(|| PathBuf::from(".devin-usage-metrics-sync"))
    }
    #[cfg(not(target_os = "macos"))]
    {
        dirs::home_dir()
            .map(|h| h.join(".devin-usage-metrics-sync"))
            .unwrap_or_else(|| PathBuf::from(".devin-usage-metrics-sync"))
    }
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
        sync_dir: String,
    }
    match serde_json::from_str::<RawConfig>(&text) {
        Ok(raw) => SyncConfig {
            enabled: raw.enabled,
            sync_dir: raw.sync_dir,
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
        "sync_dir": config.sync_dir,
    })
    .to_string();
    let temp = path.with_extension(format!("json.tmp-{}", std::process::id()));
    if fs::write(&temp, &payload).is_ok() {
        let _ = fs::rename(&temp, &path);
    } else {
        let _ = fs::remove_file(&temp);
    }
}

/// 当前生效的同步目录：配置 > 默认。
pub fn sync_dir() -> PathBuf {
    let cfg = read_config();
    if !cfg.sync_dir.is_empty() {
        let p = PathBuf::from(&cfg.sync_dir);
        if p.is_absolute() {
            return p;
        }
    }
    default_sync_dir()
}

/// 同步是否已开启。
pub fn is_enabled() -> bool {
    read_config().enabled
}

/// 配置文件路径，与 device.json 同目录。
fn config_file_path() -> Option<PathBuf> {
    dirs::config_dir().map(|d| d.join("devin-usage-metrics/sync.json"))
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

// ── 导出 ───────────────────────────────────────────────────────────────

/// 把本机已加载的数据导出到同步目录。
/// 仅写入本机设备产生的记录（device_id 为空或等于本机 ID 的），
/// 避免把从其他设备导入的数据再回传一遍。
pub fn export_local(data: &LoadedData) -> Result<PathBuf, String> {
    let dir = sync_dir();
    fs::create_dir_all(&dir).map_err(|e| format!("创建同步目录失败: {e}"))?;

    let local_id = data::device_id();
    let local_name = data::device_name();

    // 只导出本机数据，过滤掉从其他设备导入的记录
    let sessions: Vec<SessionRec> = data
        .sessions
        .iter()
        .filter(|s| s.device_id.is_empty() || s.device_id == local_id)
        .cloned()
        .collect();
    let turns: Vec<TurnRec> = data
        .turns
        .iter()
        .filter(|t| t.device_id.is_empty() || t.device_id == local_id)
        .cloned()
        .collect();

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
        sessions,
        turns,
    };

    let path = dir.join(package_filename(&local_id));
    let temp = path.with_extension(format!("json.tmp-{}", std::process::id()));
    let file = fs::File::create(&temp).map_err(|e| format!("创建临时文件失败: {e}"))?;
    let mut writer = BufWriter::new(file);
    serde_json::to_writer(&mut writer, &package)
        .map_err(|e| format!("写入数据包失败: {e}"))?;
    writer.flush().map_err(|e| format!("刷新数据包失败: {e}"))?;
    fs::rename(&temp, &path).map_err(|e| format!("重命名数据包失败: {e}"))?;
    Ok(path)
}

// ── 导入 ───────────────────────────────────────────────────────────────

/// 扫描同步目录，读取所有非本机的设备数据包。
/// 返回 (合并后的远程数据, 发现的设备列表)。
pub fn import_remote() -> (LoadedData, Vec<RemoteDevice>) {
    let started = Instant::now();
    let dir = sync_dir();
    let local_id = data::device_id();
    let mut merged = LoadedData::default();
    let mut devices: Vec<RemoteDevice> = Vec::new();

    let entries = match fs::read_dir(&dir) {
        Ok(e) => e,
        Err(_) => {
            // 同步目录不存在不算错误，静默返回空
            return (merged, devices);
        }
    };

    for entry in entries.flatten() {
        let path = entry.path();
        // 只处理 device-*.json 文件
        let name = path.file_name().and_then(|n| n.to_str()).unwrap_or("");
        if !name.starts_with("device-") || !name.ends_with(".json") {
            continue;
        }
        // 跳过写入未完成的临时文件
        if name.contains(".tmp-") {
            continue;
        }

        let file = match fs::File::open(&path) {
            Ok(f) => f,
            Err(_) => continue,
        };
        let mut reader = BufReader::new(file);
        let mut text = String::new();
        if reader.read_to_string(&mut text).is_err() {
            continue;
        }
        let pkg: DevicePackage = match serde_json::from_str(&text) {
            Ok(p) => p,
            Err(_) => continue,
        };
        // 跳过本机自己的数据包
        if pkg.device_id == local_id {
            // 但记录本机也在同步中
            devices.push(RemoteDevice {
                device_id: pkg.device_id,
                device_name: pkg.device_name,
                exported_at: pkg.exported_at,
                session_count: pkg.sessions.len(),
                is_local: true,
            });
            continue;
        }
        // 跳过过时格式
        if pkg.schema_version != PACKAGE_SCHEMA_VERSION {
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
        "sync::import_remote devices={} remote_sessions={} remote_turns={} elapsed_ms={}",
        devices.len(),
        merged.sessions.len(),
        merged.turns.len(),
        started.elapsed().as_millis()
    ));

    (merged, devices)
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
    let mut seen_sessions: HashSet<String> = local
        .sessions
        .iter()
        .map(|s| session_dedup_key(&s.device_id, &s.key))
        .collect();
    let mut seen_turns: HashSet<String> = local
        .turns
        .iter()
        .map(|t| turn_dedup_key(&t.device_id, &t.session_key, t.created_at))
        .collect();

    for s in &remote.sessions {
        if seen_sessions.insert(session_dedup_key(&s.device_id, &s.key)) {
            merged.sessions.push(s.clone());
        }
    }
    for t in &remote.turns {
        if seen_turns.insert(turn_dedup_key(&t.device_id, &t.session_key, t.created_at)) {
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

fn session_dedup_key(device_id: &str, key: &str) -> String {
    format!("{device_id}::{key}")
}

fn turn_dedup_key(device_id: &str, session_key: &str, created_at: i64) -> String {
    format!("{device_id}::{session_key}::{created_at}")
}

/// 同步目录是否存在（用于 UI 提示用户是否已配置同步）。
pub fn sync_dir_exists() -> bool {
    sync_dir().exists()
}

/// 尝试创建同步目录。
pub fn ensure_sync_dir() -> Result<(), String> {
    fs::create_dir_all(sync_dir()).map_err(|e| format!("创建同步目录失败: {e}"))
}
