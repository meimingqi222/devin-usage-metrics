use crate::i18n;
use rusqlite::{Connection, OpenFlags};
use serde_json::Value;
use std::collections::{HashMap, HashSet};
use std::fs::{File, OpenOptions};
use std::io::{BufReader, BufWriter, Write};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use crate::local_sources;

const CACHE_TTL_SECS: i64 = 300; // 5 minutes cache TTL
const CACHE_SCHEMA_VERSION: u32 = 5;

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

fn log_event(message: impl AsRef<str>) {
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
    Debug, Default, Clone, Copy, PartialEq, Eq, Hash, serde::Serialize, serde::Deserialize,
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
}

impl AgentKind {
    pub const ALL: [Self; 9] = [
        Self::Devin,
        Self::Amp,
        Self::Claude,
        Self::Codex,
        Self::Antigravity,
        Self::Grok,
        Self::ZCode,
        Self::OpenCode,
        Self::Pi,
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
}

fn now_timestamp() -> Option<i64> {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .map(|duration| duration.as_secs() as i64)
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
    })
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
        let data = load_selected_agent(start, end, agent, previous);
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

#[derive(Debug, Default, Clone, serde::Serialize, serde::Deserialize)]
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
}

impl SessionRec {
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

#[derive(Debug, Default, Clone, serde::Serialize, serde::Deserialize)]
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

fn load_sessions_from(conn: &Connection, source: &str, out: &mut Vec<SessionRec>) {
    let sql = "SELECT id, ifnull(title,''), ifnull(working_directory,''), \
               ifnull(model,''), ifnull(agent_mode,''), created_at, last_activity_at, \
               ifnull(metadata,'') FROM sessions \
               WHERE hidden = 0 OR hidden IS NULL \
               ORDER BY last_activity_at DESC LIMIT 1500";
    let mut stmt = match conn.prepare(sql) {
        Ok(s) => s,
        Err(e) => {
            out.reserve(0);
            eprintln!("prepare failed for {source}: {e}");
            return;
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
        ))
    });
    let rows = match rows {
        Ok(r) => r,
        Err(e) => {
            eprintln!("query failed for {source}: {e}");
            return;
        }
    };
    for row in rows.flatten() {
        let (id, title, wd, model, mode, created, last_act, meta) = row;
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
        });
    }
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
    let sql = format!(
        "SELECT session_id, created_at, chat_message \
         FROM message_nodes \
         WHERE session_id IN ({}) AND created_at >= ?{} AND created_at < ?{}",
        in_clause,
        session_ids.len() + 1,
        session_ids.len() + 2
    );

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
    params.push(&start);
    params.push(&end);

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

    // Devin 会把同一次响应以多个 message_nodes 兄弟节点持久化；这些节点共享
    // message_id/request_id。优先用稳定的 message_id 去重，避免依赖批量写入时间戳。
    let mut seen_message_ids = HashSet::new();
    for row in rows.flatten() {
        let (session_id, created_at, chat_message) = row;
        let Ok(message) = serde_json::from_str::<DevinChatMessage>(&chat_message) else {
            continue;
        };
        let Some(metadata) = message.metadata.as_ref() else {
            continue;
        };
        let Some(metrics) = metadata.metrics.as_ref() else {
            continue;
        };
        let ttft = metrics.ttft_ms.unwrap_or(0.0);
        if ttft <= 0.0 {
            continue;
        }
        let source_id = message
            .message_id
            .as_deref()
            .filter(|id| !id.is_empty())
            .or_else(|| metadata.request_id.as_deref().filter(|id| !id.is_empty()));
        if let Some(source_id) = source_id {
            let scoped_id = format!("{session_id}\0{source_id}");
            if !seen_message_ids.insert(scoped_id) {
                continue;
            }
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
        });
    }
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

fn dedup_cached_turns(turns: &mut Vec<TurnRec>) {
    let mut seen = HashSet::new();
    turns.retain(|turn| seen.insert(turn_key(turn)));
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
    load_sessions_from(&conn, &source, &mut data.sessions);
    let sessions_elapsed = sessions_started.elapsed();
    // 全量范围用于判断会话是否与当前页重叠
    let all_ids: Vec<String> = data
        .sessions
        .iter()
        .filter(|session| session.last_activity_at >= start && session.created_at < end)
        .map(|session| session.id.clone())
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
    let ids: Vec<String> = if query_start > start {
        data.sessions
            .iter()
            .filter(|session| session.last_activity_at >= query_start && session.created_at < end)
            .map(|session| session.id.clone())
            .collect()
    } else {
        all_ids.clone()
    };
    let turns_started = Instant::now();
    load_turns_from(&conn, &source, &ids, query_start, end, &mut data.turns);
    let queried_turns = data.turns.len();
    if !cached_turns.is_empty() {
        let seen: HashSet<String> = cached_turns.iter().map(turn_key).collect();
        let mut merged = cached_turns;
        for turn in data.turns.drain(..) {
            // 查询窗口包含最新缓存时间点；这里只排除与缓存重叠的记录。查询结果
            // 已按 message_id 去重，不能再把两个指标恰好相同的真实请求合并掉。
            if !seen.contains(&turn_key(&turn)) {
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
            let data = local_sources::load_amp(start, end, previous.as_deref());
            log_loaded_part("Amp", started, &data);
            parts.lock().unwrap().push(data);
        });
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
        data
    } else {
        match agent {
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
            AgentKind::Devin => unreachable!(),
        }
    }
}

/// 以当前内存快照为基底，只重新加载指定 Agent 的目标时间窗口。
/// 新窗口会合并进已有数据，返回已加载页时不需要再次读取源数据。
pub fn reload_agent_from(
    previous: Arc<LoadedData>,
    start: i64,
    end: i64,
    agent: AgentKind,
) -> LoadedData {
    let replacement = load_selected_agent(start, end, agent, Some(previous.clone()));
    let mut data = (*previous).clone();
    let replacement_keys: HashSet<String> = replacement
        .sessions
        .iter()
        .map(|session| session.key.clone())
        .collect();
    save_agent_cache(agent, &replacement);
    data.sessions
        .retain(|session| session.agent != agent || !replacement_keys.contains(&session.key));
    data.errors.retain(|error| error.agent != agent);
    data.sessions.extend(replacement.sessions);
    data.errors.extend(replacement.errors);
    data.file_mtimes.extend(replacement.file_mtimes);
    data.file_sessions.extend(replacement.file_sessions);

    let mut seen_turns: HashSet<String> = data.turns.iter().map(turn_key).collect();
    for turn in replacement.turns {
        if seen_turns.insert(turn_key(&turn)) {
            data.turns.push(turn);
        }
    }

    if data.turns_start == 0 && data.turns_end == 0 {
        data.turns_start = start;
        data.turns_end = end;
    } else {
        data.turns_start = data.turns_start.min(start);
        data.turns_end = data.turns_end.max(end);
    }
    data.turns.sort_by_key(|turn| turn.created_at);
    log_event(format!(
        "reload_agent_from agent={} merged=true range={}..{} sessions={} turns={}",
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
    use super::{cache_covers, dedup_cached_turns, load_turns_from, AgentKind, TurnRec};
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
}
