use crate::data::{AgentKind, DataError, LoadedData, SessionRec, TurnRec};
use chrono::DateTime;
use rayon::prelude::*;
use serde_json::Value;
use std::collections::{HashMap, HashSet};
use std::fs::File;
use std::io::{BufRead, BufReader, BufWriter};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Mutex;
use std::time::{SystemTime, UNIX_EPOCH};

#[cfg(target_os = "windows")]
use std::os::windows::process::CommandExt;

const MAX_SESSION_FILES: usize = 1500;

#[derive(Default)]
struct SessionBuilder {
    id: String,
    title: String,
    cwd: String,
    model: String,
    mode: String,
    source: String,
    created_at: i64,
    last_activity_at: i64,
    input_tokens: f64,
    output_tokens: f64,
    cached_tokens: f64,
    cache_creation_tokens: f64,
    cache_creation_5m_tokens: f64,
    cache_creation_1h_tokens: f64,
    agent_messages: f64,
    seen_messages: HashSet<String>,
}

impl SessionBuilder {
    fn observe_time(&mut self, timestamp: i64) {
        if timestamp <= 0 {
            return;
        }
        if self.created_at == 0 || timestamp < self.created_at {
            self.created_at = timestamp;
        }
        self.last_activity_at = self.last_activity_at.max(timestamp);
    }

    /// Merge another builder for the same session, gathered from a different
    /// file. Numeric fields are order-independent (sum / min / max); string
    /// metadata keeps the first non-empty value seen in file order.
    fn merge(&mut self, other: SessionBuilder) {
        if self.id.is_empty() && !other.id.is_empty() {
            self.id = other.id;
        }
        if self.title.is_empty() && !other.title.is_empty() {
            self.title = other.title;
        }
        if self.cwd.is_empty() && !other.cwd.is_empty() {
            self.cwd = other.cwd;
        }
        if self.model.is_empty() && !other.model.is_empty() {
            self.model = other.model;
        }
        if self.mode.is_empty() && !other.mode.is_empty() {
            self.mode = other.mode;
        }
        if self.source.is_empty() && !other.source.is_empty() {
            self.source = other.source;
        }
        if other.created_at != 0 && (self.created_at == 0 || other.created_at < self.created_at) {
            self.created_at = other.created_at;
        }
        self.last_activity_at = self.last_activity_at.max(other.last_activity_at);
        self.input_tokens += other.input_tokens;
        self.output_tokens += other.output_tokens;
        self.cached_tokens += other.cached_tokens;
        self.cache_creation_tokens += other.cache_creation_tokens;
        self.cache_creation_5m_tokens += other.cache_creation_5m_tokens;
        self.cache_creation_1h_tokens += other.cache_creation_1h_tokens;
        self.agent_messages += other.agent_messages;
        self.seen_messages.extend(other.seen_messages);
    }

    fn finish(self, agent: AgentKind) -> SessionRec {
        let id = self.id;
        SessionRec {
            agent,
            key: format!(
                "{}/{id}",
                agent.label().to_ascii_lowercase().replace(' ', "-")
            ),
            source: if self.source.is_empty() {
                "local".into()
            } else {
                self.source
            },
            id,
            title: self.title,
            working_directory: self.cwd,
            selected_model: self.model,
            real_model: None,
            agent_mode: self.mode,
            created_at: self.created_at,
            last_activity_at: self.last_activity_at,
            input_tokens: self.input_tokens,
            output_tokens: self.output_tokens,
            cached_tokens: self.cached_tokens,
            cache_creation_tokens: self.cache_creation_tokens,
            cache_creation_5m_tokens: self.cache_creation_5m_tokens,
            cache_creation_1h_tokens: self.cache_creation_1h_tokens,
            agent_messages: self.agent_messages,
        }
    }
}

fn error(agent: AgentKind, message: impl Into<String>) -> DataError {
    DataError {
        agent,
        message: message.into(),
    }
}

fn home_path(parts: &[&str]) -> PathBuf {
    let mut path = dirs::home_dir().unwrap_or_else(|| PathBuf::from("."));
    for part in parts {
        path.push(part);
    }
    path
}

fn collect_files(root: &Path, extension: &str, output: &mut Vec<PathBuf>) {
    let Ok(entries) = std::fs::read_dir(root) else {
        return;
    };
    for entry in entries.flatten() {
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        if file_type.is_symlink() {
            continue;
        }
        let path = entry.path();
        if file_type.is_dir() {
            collect_files(&path, extension, output);
        } else if path.extension().and_then(|value| value.to_str()) == Some(extension) {
            output.push(path);
        }
    }
}

fn recent_files(roots: &[PathBuf], extension: &str) -> Vec<PathBuf> {
    let mut files = Vec::new();
    for root in roots {
        collect_files(root, extension, &mut files);
    }
    files.sort_by_key(|path| {
        std::cmp::Reverse(
            path.metadata()
                .and_then(|metadata| metadata.modified())
                .unwrap_or(UNIX_EPOCH),
        )
    });
    files.truncate(MAX_SESSION_FILES);
    files
}

fn value_number(value: Option<&Value>) -> f64 {
    value.and_then(Value::as_f64).unwrap_or(0.0)
}

// ── 增量 reload ──────────────────────────────────────────────────────────
// 上次加载时为每个数据文件记录了 mtime 和解析出的会话 key（LoadedData 的
// file_mtimes / file_sessions）。reload 时 mtime 未变化的文件直接跳过解析，
// 其会话与窗口内 turns 从上次快照恢复；只有变化/新增的文件会重新解析。

/// 文件的 mtime（Unix 秒）。文件不存在或 stat 失败时返回 None。
fn file_mtime_secs(path: &Path) -> Option<i64> {
    std::fs::metadata(path)
        .ok()?
        .modified()
        .ok()?
        .duration_since(UNIX_EPOCH)
        .ok()
        .map(|d| d.as_secs() as i64)
}

/// 多个文件 mtime 的最大值，作为跨多个物理文件的"文件签名"
/// （Grok 的 updates.jsonl、Antigravity 的 -wal 附属文件）。
fn max_mtime(paths: &[&Path]) -> Option<i64> {
    paths.iter().filter_map(|p| file_mtime_secs(p)).max()
}

/// 可复用文件的集合：签名 mtime 未变化，且上次快照中记录了该文件的会话 key。
fn reusable_paths<'f, F>(
    previous: Option<&LoadedData>,
    files: &'f [PathBuf],
    signature: F,
) -> HashSet<&'f str>
where
    F: Fn(&Path) -> Option<i64>,
{
    let Some(prev) = previous else {
        return HashSet::new();
    };
    files
        .iter()
        .filter_map(|path| {
            let key = path.to_str()?;
            let mtime = signature(path)?;
            if prev.file_sessions.contains_key(key)
                && prev.file_mtimes.get(key).is_some_and(|m| *m == mtime)
            {
                Some(key)
            } else {
                None
            }
        })
        .collect()
}

/// 一组文件是否全部未变化（用于一个会话跨多个文件的场景，如 Claude subagent）。
fn all_unchanged(previous: Option<&LoadedData>, paths: &[&PathBuf]) -> bool {
    let Some(prev) = previous else {
        return false;
    };
    paths.iter().all(|path| {
        path.to_str()
            .and_then(|k| prev.file_mtimes.get(k).copied())
            .zip(file_mtime_secs(path))
            .is_some_and(|(old, new)| old == new)
    })
}

/// 从上次快照恢复可复用会话 key 对应的会话记录和窗口内 turns。
fn reuse_records(
    previous: Option<&LoadedData>,
    agent: AgentKind,
    start: i64,
    end: i64,
    keys: &HashSet<String>,
) -> (Vec<SessionRec>, Vec<TurnRec>) {
    let Some(prev) = previous else {
        return (Vec::new(), Vec::new());
    };
    let sessions = prev
        .sessions
        .iter()
        .filter(|s| s.agent == agent && keys.contains(&s.key))
        .cloned()
        .collect();
    let turns = prev
        .turns
        .iter()
        .filter(|t| {
            t.agent == agent
                && t.created_at >= start
                && t.created_at < end
                && keys.contains(&t.session_key)
        })
        .cloned()
        .collect();
    (sessions, turns)
}

/// 记录文件索引（路径 → 签名 mtime / 会话 key），供下次增量 reload 使用。
/// key 为 None 表示该文件上次没有解析出会话，下次仍会重新解析。
fn record_index(data: &mut LoadedData, path: &Path, signature: Option<i64>, key: Option<&str>) {
    let Some(path_str) = path.to_str() else {
        return;
    };
    if let Some(mtime) = signature {
        data.file_mtimes.insert(path_str.to_owned(), mtime);
    }
    if let Some(key) = key {
        data.file_sessions
            .insert(path_str.to_owned(), key.to_owned());
    }
}

fn timestamp(value: Option<&Value>) -> i64 {
    let Some(value) = value else { return 0 };
    if let Some(number) = value.as_i64() {
        return if number > 10_000_000_000 {
            number / 1000
        } else {
            number
        };
    }
    value
        .as_str()
        .and_then(|text| DateTime::parse_from_rfc3339(text).ok())
        .map(|date| date.timestamp())
        .unwrap_or(0)
}

fn compact_title(text: &str) -> String {
    text.split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
        .chars()
        .take(100)
        .collect()
}

fn content_text(value: &Value) -> String {
    if let Some(text) = value.as_str() {
        return compact_title(text);
    }
    value
        .as_array()
        .and_then(|items| {
            items.iter().find_map(|item| {
                item.get("text")
                    .and_then(Value::as_str)
                    .filter(|text| !text.trim().is_empty())
            })
        })
        .map(compact_title)
        .unwrap_or_default()
}

fn amp_cwd(value: &Value) -> String {
    let Some(uri) = value
        .pointer("/env/initial/trees/0/uri")
        .and_then(Value::as_str)
    else {
        return String::new();
    };
    let decoded = uri
        .trim_start_matches("file:///")
        .replace("%3A", ":")
        .replace("%3a", ":")
        .replace("%20", " ");
    if cfg!(windows) {
        decoded.replace('/', "\\")
    } else {
        format!("/{decoded}")
    }
}

fn parse_amp_value(
    value: Value,
    fallback_id: Option<&str>,
    start: i64,
    end: i64,
) -> (Option<SessionRec>, Vec<TurnRec>, usize) {
    let id = value
        .get("id")
        .and_then(Value::as_str)
        .map(str::to_owned)
        .or_else(|| fallback_id.map(str::to_owned))
        .unwrap_or_default();
    if id.is_empty() {
        return (None, Vec::new(), 0);
    }
    let mut session = SessionBuilder {
        id,
        ..Default::default()
    };
    session.title = value
        .get("title")
        .and_then(Value::as_str)
        .map(compact_title)
        .unwrap_or_default();
    session.cwd = amp_cwd(&value);
    session.mode = value
        .get("agentMode")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_owned();
    session.source = "local".into();
    session.observe_time(timestamp(value.get("created")));
    let key = format!("amp/{}", session.id);

    let mut turns = Vec::new();
    if let Some(messages) = value.get("messages").and_then(Value::as_array) {
        for message in messages {
            if session.title.is_empty()
                && message.get("role").and_then(Value::as_str) == Some("user")
            {
                session.title = content_text(message.get("content").unwrap_or(&Value::Null));
            }
            let Some(usage) = message.get("usage") else {
                continue;
            };
            let at = timestamp(usage.get("timestamp"));
            session.observe_time(at);
            // Amp 的 cacheCreationInputTokens 是缓存写入，不应合并到 input
            let cache_creation = value_number(usage.get("cacheCreationInputTokens"));
            let input = value_number(usage.get("inputTokens"));
            let output = value_number(usage.get("outputTokens"));
            let cached = value_number(usage.get("cacheReadInputTokens"));
            session.input_tokens += input;
            session.output_tokens += output;
            session.cached_tokens += cached;
            session.cache_creation_tokens += cache_creation;
            session.agent_messages += 1.0;
            if let Some(model) = usage.get("model").and_then(Value::as_str) {
                session.model = model.to_owned();
            }
            if at >= start && at < end {
                let turn_model = usage
                    .get("model")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                    .to_owned();
                turns.push(TurnRec {
                    agent: AgentKind::Amp,
                    session_key: key.clone(),
                    created_at: at,
                    input_tokens: input,
                    output_tokens: output,
                    cache_read_tokens: cached,
                    cache_creation_tokens: cache_creation,
                    cache_creation_5m_tokens: 0.0,
                    cache_creation_1h_tokens: 0.0,
                    model: turn_model,
                    ttft_ms: 0.0,
                    total_time_ms: 0.0,
                });
            }
        }
    }
    (Some(session.finish(AgentKind::Amp)), turns, 0)
}

fn parse_amp_file(path: &Path, start: i64, end: i64) -> (Option<SessionRec>, Vec<TurnRec>, usize) {
    let Ok(file) = File::open(path) else {
        return (None, Vec::new(), 1);
    };
    let Ok(value) = serde_json::from_reader::<_, Value>(BufReader::new(file)) else {
        return (None, Vec::new(), 1);
    };
    let fallback_id = path.file_stem().and_then(|stem| stem.to_str());
    parse_amp_value(value, fallback_id, start, end)
}

fn amp_threads_root() -> PathBuf {
    std::env::var_os("AMP_DATA_DIR")
        .map(PathBuf::from)
        .unwrap_or_else(|| home_path(&[".local", "share", "amp"]))
        .join("threads")
}

fn amp_cli_path() -> PathBuf {
    #[cfg(target_os = "windows")]
    {
        let candidates = [
            home_path(&[".amp", "bin", "amp.exe"]),
            home_path(&[".amp", "bin", "amp.bat"]),
            home_path(&[".amp", "bin", "amp.cmd"]),
        ];
        for candidate in candidates {
            if candidate.exists() {
                return candidate;
            }
        }
        if let Some(appdata) = std::env::var_os("APPDATA") {
            let appdata_path = PathBuf::from(appdata);
            let npm_cmd = appdata_path.join("npm").join("amp.cmd");
            if npm_cmd.exists() {
                return npm_cmd;
            }
            let npm_exe = appdata_path.join("npm").join("amp.exe");
            if npm_exe.exists() {
                return npm_exe;
            }
        }
    }

    #[cfg(not(target_os = "windows"))]
    {
        let bundled = home_path(&[".amp", "bin", "amp"]);
        if bundled.exists() {
            return bundled;
        }
    }

    PathBuf::from("amp")
}

#[cfg(target_os = "windows")]
const CREATE_NO_WINDOW: u32 = 0x08000000;

fn amp_cli_json(args: &[&str]) -> Option<Vec<u8>> {
    let cli = amp_cli_path();
    let is_batch = cfg!(target_os = "windows")
        && cli
            .extension()
            .and_then(|ext| ext.to_str())
            .map(|ext| ext.eq_ignore_ascii_case("cmd") || ext.eq_ignore_ascii_case("bat"))
            .unwrap_or(false);

    let mut cmd = if is_batch {
        let mut c = Command::new("cmd.exe");
        c.arg("/C").arg(&cli).args(args);
        c
    } else {
        let mut c = Command::new(&cli);
        c.args(args);
        c
    };

    #[cfg(target_os = "windows")]
    cmd.creation_flags(CREATE_NO_WINDOW);

    let output = cmd.output().ok()?;
    if !output.status.success() {
        return None;
    }
    Some(output.stdout)
}

// ── Amp 远程线程本地缓存 ──────────────────────────────────────────────
// amp threads list 和 amp threads export 都很慢（列表 ~6s，每个导出 ~1-7s）。
// 将列表结果和单个线程的导出 JSON 缓存到本地文件，分页时只读取本地缓存，
// 仅对新增或更新的线程执行 CLI 导出。

fn amp_cache_dir() -> PathBuf {
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
    base.join("devin-usage-metrics/amp-threads")
}

fn amp_list_cache_path() -> PathBuf {
    let dir = amp_cache_dir();
    let parent = dir.parent().unwrap_or(dir.as_path());
    parent.join("amp-list.json")
}

const AMP_LIST_TTL_SECS: i64 = 300; // 5 分钟

#[derive(serde::Serialize, serde::Deserialize, Clone)]
struct AmpListEntry {
    id: String,
    updated: i64,
}

#[derive(serde::Serialize, serde::Deserialize, Clone)]
struct AmpListCache {
    cached_at: i64,
    threads: Vec<AmpListEntry>,
}

// 进程内内存缓存，避免同一进程内分页时反复读取列表文件
static AMP_LIST_MEM: Mutex<Option<AmpListCache>> = Mutex::new(None);

fn now_secs() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// 获取 Amp 线程列表，优先使用内存缓存 → 文件缓存 → CLI 拉取。
fn load_amp_thread_list() -> Vec<AmpListEntry> {
    let now = now_secs();

    // 1. 内存缓存
    {
        let guard = AMP_LIST_MEM.lock().unwrap();
        if let Some(cache) = guard.as_ref() {
            if now - cache.cached_at <= AMP_LIST_TTL_SECS {
                return cache.threads.clone();
            }
        }
    }

    // 2. 文件缓存
    let list_path = amp_list_cache_path();
    if let Ok(file) = File::open(&list_path) {
        if let Ok(cache) = serde_json::from_reader::<_, AmpListCache>(BufReader::new(file)) {
            if now - cache.cached_at <= AMP_LIST_TTL_SECS {
                *AMP_LIST_MEM.lock().unwrap() = Some(cache.clone());
                return cache.threads;
            }
        }
    }

    // 3. CLI 拉取全部线程元数据
    let entries = fetch_amp_thread_list_all();
    let cache = AmpListCache {
        cached_at: now,
        threads: entries.clone(),
    };

    // 写入文件缓存
    if let Some(parent) = list_path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    if let Ok(file) = File::create(&list_path) {
        let _ = serde_json::to_writer(BufWriter::new(file), &cache);
    }

    // 更新内存缓存
    *AMP_LIST_MEM.lock().unwrap() = Some(cache);
    entries
}

/// 调用 amp threads list 拉取全部线程（分页直到取完）。
fn fetch_amp_thread_list_all() -> Vec<AmpListEntry> {
    const PAGE_SIZE: usize = 500;
    let mut offset = 0usize;
    let mut entries = Vec::new();

    loop {
        let limit = PAGE_SIZE.to_string();
        let offset_arg = offset.to_string();
        let Some(raw) = amp_cli_json(&[
            "threads",
            "list",
            "--include-archived",
            "--limit",
            &limit,
            "--offset",
            &offset_arg,
            "--json",
        ]) else {
            break;
        };
        let Ok(items) = serde_json::from_slice::<Vec<Value>>(&raw) else {
            break;
        };
        if items.is_empty() {
            break;
        }

        for item in &items {
            let updated = timestamp(item.get("updated"));
            if let Some(id) = item.get("id").and_then(Value::as_str) {
                entries.push(AmpListEntry {
                    id: id.to_owned(),
                    updated,
                });
            }
        }

        if items.len() < PAGE_SIZE {
            break;
        }
        offset += PAGE_SIZE;
    }
    entries
}

/// 获取单个线程的导出 JSON，优先使用本地缓存文件。
/// 缓存有效性：文件 mtime >= 线程的 updated 时间戳时视为有效。
fn load_amp_thread_export(id: &str, updated: i64) -> Option<Value> {
    let cache_dir = amp_cache_dir();
    let cache_path = cache_dir.join(format!("{id}.json"));

    // 检查缓存文件是否存在且足够新
    if let Ok(metadata) = std::fs::metadata(&cache_path) {
        if let Ok(mtime) = metadata.modified() {
            if let Ok(secs) = mtime.duration_since(UNIX_EPOCH) {
                let cached_mtime = secs.as_secs() as i64;
                if cached_mtime >= updated {
                    if let Ok(file) = File::open(&cache_path) {
                        if let Ok(value) = serde_json::from_reader::<_, Value>(BufReader::new(file))
                        {
                            return Some(value);
                        }
                    }
                }
            }
        }
    }

    // 缓存未命中或已过期，从 CLI 导出
    let raw = amp_cli_json(&["threads", "export", id])?;
    let value: Value = serde_json::from_slice(&raw).ok()?;

    // 写入缓存
    if let Some(parent) = cache_path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    if let Ok(file) = File::create(&cache_path) {
        let _ = serde_json::to_writer(BufWriter::new(file), &value);
    }

    Some(value)
}

fn load_amp_remote(start: i64, end: i64) -> (Vec<SessionRec>, Vec<TurnRec>, Vec<String>) {
    let entries = load_amp_thread_list();
    let selected: Vec<&AmpListEntry> = entries
        .iter()
        .filter(|e| e.updated >= start && e.updated < end)
        .collect();

    selected
        .par_iter()
        .map(|entry| {
            let value = match load_amp_thread_export(&entry.id, entry.updated) {
                Some(v) => v,
                None => return (None, Vec::new(), vec![entry.id.clone()]),
            };
            let (session, turns, _skipped) = parse_amp_value(value, Some(&entry.id), start, end);
            (session, turns, Vec::new())
        })
        .collect::<Vec<_>>()
        .into_iter()
        .fold(
            (Vec::new(), Vec::new(), Vec::new()),
            |(mut sessions, mut turns, mut failed), (session, mut new_turns, mut new_failed)| {
                if let Some(session) = session {
                    sessions.push(session);
                }
                turns.append(&mut new_turns);
                failed.append(&mut new_failed);
                (sessions, turns, failed)
            },
        )
}

pub(crate) fn load_amp(start: i64, end: i64, previous: Option<&LoadedData>) -> LoadedData {
    let agent = AgentKind::Amp;
    let root = amp_threads_root();
    let mut data = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };
    let mut skipped = 0usize;
    if root.exists() {
        let files = recent_files(&[root], "json");
        let reusable = reusable_paths(previous, &files, file_mtime_secs);
        let reusable_keys: HashSet<String> = previous
            .map(|prev| {
                reusable
                    .iter()
                    .filter_map(|path| prev.file_sessions.get(*path))
                    .cloned()
                    .collect()
            })
            .unwrap_or_default();
        let (reused_sessions, reused_turns) =
            reuse_records(previous, agent, start, end, &reusable_keys);

        let parse_files: Vec<&PathBuf> = files
            .iter()
            .filter(|path| path.to_str().map(|k| !reusable.contains(k)).unwrap_or(true))
            .collect();
        let results: Vec<(Option<SessionRec>, Vec<TurnRec>, usize)> = parse_files
            .par_iter()
            .map(|path| parse_amp_file(path, start, end))
            .collect();
        for (path, (session, mut turns, skip)) in parse_files.iter().zip(results) {
            record_index(
                &mut data,
                path,
                file_mtime_secs(path),
                session.as_ref().map(|s| s.key.as_str()),
            );
            if let Some(session) = session {
                data.sessions.push(session);
            }
            data.turns.append(&mut turns);
            skipped += skip;
        }
        // 复用的文件沿用原索引
        if let Some(prev) = previous {
            for path in &files {
                if let Some(key) = path.to_str() {
                    if reusable.contains(key) {
                        record_index(
                            &mut data,
                            path,
                            file_mtime_secs(path),
                            prev.file_sessions.get(key).map(String::as_str),
                        );
                    }
                }
            }
        }
        data.sessions.extend(reused_sessions);
        data.turns.extend(reused_turns);
    }

    // 新版 Amp 将线程保存在服务端，CLI 的 export 是本机可用的读取入口。
    // 列表和导出结果都会缓存到本地，分页时只读取本地缓存。
    let (remote_sessions, remote_turns, failed_ids) = load_amp_remote(start, end);
    let remote_keys: HashSet<String> = remote_sessions.iter().map(|s| s.key.clone()).collect();
    data.sessions
        .retain(|session| !remote_keys.contains(&session.key));
    data.turns
        .retain(|turn| !remote_keys.contains(&turn.session_key));
    data.sessions.extend(remote_sessions);
    data.turns.extend(remote_turns);

    if skipped > 0 || !failed_ids.is_empty() {
        let mut msg = String::new();
        if skipped > 0 {
            msg.push_str(&format!("有 {skipped} 个本地 Amp 会话无法读取"));
        }
        if !failed_ids.is_empty() {
            if !msg.is_empty() {
                msg.push('；');
            }
            let shown: Vec<&str> = failed_ids.iter().take(5).map(String::as_str).collect();
            msg.push_str(&format!(
                "{} 个远程线程导出失败: {}",
                failed_ids.len(),
                shown.join(", ")
            ));
            if failed_ids.len() > 5 {
                msg.push_str(&format!(" 等 {} 个", failed_ids.len()));
            }
        }
        data.errors.push(error(agent, msg));
    }
    data
}

fn claude_session_id(path: &Path) -> String {
    let is_subagent = path
        .parent()
        .and_then(Path::file_name)
        .and_then(|name| name.to_str())
        == Some("subagents");
    if is_subagent {
        if let Some(parent) = path
            .parent()
            .and_then(Path::parent)
            .and_then(Path::file_name)
            .and_then(|name| name.to_str())
        {
            return parent.to_owned();
        }
    }
    path.file_stem()
        .and_then(|stem| stem.to_str())
        .unwrap_or("")
        .to_owned()
}

#[derive(serde::Deserialize)]
struct ClaudeLine {
    #[serde(rename = "type")]
    kind: Option<String>,
    #[serde(rename = "sessionId")]
    session_id: Option<String>,
    #[serde(rename = "session_id")]
    session_id_snake: Option<String>,
    timestamp: Option<Value>,
    cwd: Option<String>,
    entrypoint: Option<String>,
    effort: Option<String>,
    #[serde(rename = "aiTitle")]
    ai_title: Option<String>,
    message: Option<ClaudeMessage>,
    #[serde(rename = "requestId")]
    request_id: Option<String>,
}

#[derive(serde::Deserialize)]
struct ClaudeMessage {
    id: Option<String>,
    model: Option<String>,
    content: Option<Value>,
    usage: Option<ClaudeUsage>,
}

#[derive(serde::Deserialize)]
struct ClaudeUsage {
    input_tokens: Option<f64>,
    output_tokens: Option<f64>,
    cache_creation_input_tokens: Option<f64>,
    cache_read_input_tokens: Option<f64>,
    /// Claude 新版 JSONL 会把 cache_creation 拆成 5m 和 1h 两档
    cache_creation: Option<ClaudeCacheCreation>,
}

#[derive(serde::Deserialize)]
struct ClaudeCacheCreation {
    #[serde(rename = "ephemeral_5m_input_tokens", default)]
    ephemeral_5m: Option<f64>,
    #[serde(rename = "ephemeral_1h_input_tokens", default)]
    ephemeral_1h: Option<f64>,
}

fn parse_claude_file(
    path: &Path,
    start: i64,
    end: i64,
) -> (HashMap<String, SessionBuilder>, Vec<TurnRec>, usize) {
    let mut sessions: HashMap<String, SessionBuilder> = HashMap::new();
    let mut turns = Vec::new();
    let fallback_id = claude_session_id(path);
    if fallback_id.is_empty() {
        return (sessions, turns, 0);
    }
    let Ok(file) = File::open(path) else {
        return (sessions, turns, 1);
    };
    for line in BufReader::new(file).lines().map_while(Result::ok) {
        let Ok(value) = serde_json::from_str::<ClaudeLine>(&line) else {
            continue;
        };
        let id = value
            .session_id
            .or(value.session_id_snake)
            .unwrap_or_else(|| fallback_id.clone());
        let session = sessions.entry(id.clone()).or_default();
        session.id = id.clone();
        let at = timestamp(value.timestamp.as_ref());
        session.observe_time(at);
        if let Some(cwd) = value.cwd.as_deref().filter(|cwd| !cwd.is_empty()) {
            session.cwd = cwd.to_owned();
        }
        if let Some(entrypoint) = value.entrypoint.as_deref() {
            session.source = entrypoint.to_owned();
        }
        if let Some(effort) = value.effort.as_deref() {
            session.mode = effort.to_owned();
        }
        match value.kind.as_deref().unwrap_or("") {
            "ai-title" => {
                if let Some(title) = value.ai_title.as_deref() {
                    session.title = compact_title(title);
                }
            }
            "user" if session.title.is_empty() => {
                if let Some(content) = value.message.as_ref().and_then(|m| m.content.as_ref()) {
                    session.title = content_text(content);
                }
            }
            "assistant" => {
                let Some(usage) = value.message.as_ref().and_then(|m| m.usage.as_ref()) else {
                    continue;
                };
                let message_id = value
                    .message
                    .as_ref()
                    .and_then(|m| m.id.as_deref())
                    .or(value.request_id.as_deref())
                    .unwrap_or("");
                if !message_id.is_empty() && !session.seen_messages.insert(message_id.to_owned()) {
                    continue;
                }
                // cache_creation: 优先用 5m/1h 拆分，否则用合并值（旧格式归入 5m）
                let (cc_5m, cc_1h) = if let Some(cc) = usage.cache_creation.as_ref() {
                    (
                        cc.ephemeral_5m.unwrap_or(0.0),
                        cc.ephemeral_1h.unwrap_or(0.0),
                    )
                } else {
                    // 旧格式没有 cache_creation 拆分，cache_creation_input_tokens 归入 5m
                    (usage.cache_creation_input_tokens.unwrap_or(0.0), 0.0)
                };
                let cache_creation = cc_5m + cc_1h;
                // input 不再包含 cache_creation，保持与 ccusage 一致
                let input = usage.input_tokens.unwrap_or(0.0);
                let output = usage.output_tokens.unwrap_or(0.0);
                let cached = usage.cache_read_input_tokens.unwrap_or(0.0);
                session.input_tokens += input;
                session.output_tokens += output;
                session.cached_tokens += cached;
                session.cache_creation_tokens += cache_creation;
                session.cache_creation_5m_tokens += cc_5m;
                session.cache_creation_1h_tokens += cc_1h;
                let turn_model = value
                    .message
                    .as_ref()
                    .and_then(|m| m.model.as_deref())
                    .unwrap_or("")
                    .to_owned();
                if at >= start && at < end {
                    turns.push(TurnRec {
                        agent: AgentKind::Claude,
                        session_key: format!("claude-code/{id}"),
                        created_at: at,
                        input_tokens: input,
                        output_tokens: output,
                        cache_read_tokens: cached,
                        cache_creation_tokens: cache_creation,
                        cache_creation_5m_tokens: cc_5m,
                        cache_creation_1h_tokens: cc_1h,
                        model: turn_model,
                        ttft_ms: 0.0,
                        total_time_ms: 0.0,
                    });
                }
            }
            _ => {}
        }
    }
    (sessions, turns, 0)
}

pub(crate) fn load_claude(start: i64, end: i64, previous: Option<&LoadedData>) -> LoadedData {
    let agent = AgentKind::Claude;
    let root = home_path(&[".claude", "projects"]);
    let mut data = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };
    if !root.exists() {
        data.errors
            .push(error(agent, format!("未找到 {}", root.display())));
        return data;
    }

    let files = recent_files(&[root], "jsonl");

    // 一个会话可能跨多个文件（主会话 + subagent）。按会话分组后，
    // 只有当会话的全部文件都未变化时，才能整体复用上次解析的结果。
    let mut files_by_key: HashMap<String, Vec<&PathBuf>> = HashMap::new();
    for path in &files {
        let id = claude_session_id(path);
        if id.is_empty() {
            continue;
        }
        files_by_key
            .entry(format!("claude-code/{id}"))
            .or_default()
            .push(path);
    }
    let prev_keys: HashSet<&str> = previous
        .map(|prev| {
            prev.sessions
                .iter()
                .filter(|s| s.agent == agent)
                .map(|s| s.key.as_str())
                .collect()
        })
        .unwrap_or_default();
    let reusable_keys: HashSet<String> = files_by_key
        .iter()
        .filter(|(key, paths)| prev_keys.contains(key.as_str()) && all_unchanged(previous, paths))
        .map(|(key, _)| key.clone())
        .collect();
    let (reused_sessions, reused_turns) =
        reuse_records(previous, agent, start, end, &reusable_keys);

    let parse_files: Vec<&PathBuf> = files_by_key
        .iter()
        .filter(|(key, _)| !reusable_keys.contains(*key))
        .flat_map(|(_, paths)| paths.iter().copied())
        .collect();
    let mut sessions: HashMap<String, SessionBuilder> = HashMap::new();
    let mut turns = Vec::new();
    let mut skipped = 0usize;
    let results: Vec<(HashMap<String, SessionBuilder>, Vec<TurnRec>, usize)> = parse_files
        .par_iter()
        .map(|path| parse_claude_file(path, start, end))
        .collect();
    for (path, (partial, mut new_turns, skip)) in parse_files.iter().zip(results) {
        // 文件内的 sessionId 可能与按路径推导的不同（subagent 场景），
        // 只在文件恰好对应一个会话时记录映射，否则下次保持重新解析
        let single_key = match partial.len() {
            1 => partial.keys().next().map(|id| format!("claude-code/{id}")),
            _ => None,
        };
        for (id, session) in partial {
            match sessions.get_mut(&id) {
                Some(existing) => existing.merge(session),
                None => {
                    sessions.insert(id, session);
                }
            }
        }
        record_index(
            &mut data,
            path,
            file_mtime_secs(path),
            single_key.as_deref(),
        );
        turns.append(&mut new_turns);
        skipped += skip;
    }
    // 复用的会话：组内文件沿用原索引
    if let Some(prev) = previous {
        for (key, paths) in &files_by_key {
            if reusable_keys.contains(key) {
                for path in paths {
                    let Some(path_key) = path.to_str() else {
                        continue;
                    };
                    record_index(
                        &mut data,
                        path,
                        file_mtime_secs(path),
                        prev.file_sessions.get(path_key).map(String::as_str),
                    );
                }
            }
        }
    }
    data.sessions = sessions
        .into_values()
        .map(|session| session.finish(agent))
        .chain(reused_sessions)
        .collect();
    data.turns = turns;
    data.turns.extend(reused_turns);
    if skipped > 0 {
        data.errors
            .push(error(agent, format!("有 {skipped} 个会话文件无法读取")));
    }
    data
}

/// A single per-turn token event parsed from a Codex file. Deduplication by
/// cumulative usage signature is deferred to the reduce step so that files can
/// be parsed in parallel while preserving cross-file dedup semantics.
struct CodexEvent {
    session_id: String,
    created_at: i64,
    input_tokens: f64,
    output_tokens: f64,
    cached_tokens: f64,
    cache_write_tokens: f64,
    signature: String,
}

#[derive(serde::Deserialize)]
struct CodexLine {
    #[serde(rename = "type")]
    kind: Option<String>,
    timestamp: Option<Value>,
    payload: Option<CodexPayload>,
}

#[derive(serde::Deserialize)]
struct CodexPayload {
    id: Option<String>,
    session_id: Option<String>,
    cwd: Option<String>,
    // `source` is occasionally a nested object (subagent thread metadata)
    // rather than a string, so keep it lenient.
    source: Option<Value>,
    model: Option<String>,
    collaboration_mode: Option<Value>,
    #[serde(rename = "type")]
    payload_type: Option<String>,
    message: Option<String>,
    info: Option<CodexInfo>,
}

#[derive(serde::Deserialize)]
struct CodexInfo {
    last_token_usage: Option<CodexUsage>,
    total_token_usage: Option<CodexUsage>,
}

#[derive(serde::Deserialize)]
struct CodexUsage {
    input_tokens: Option<f64>,
    cached_input_tokens: Option<f64>,
    output_tokens: Option<f64>,
    #[serde(default)]
    cache_write_input_tokens: Option<f64>,
}

fn parse_codex_file(path: &Path) -> (HashMap<String, SessionBuilder>, Vec<CodexEvent>, usize) {
    let mut metas: HashMap<String, SessionBuilder> = HashMap::new();
    let mut events: Vec<CodexEvent> = Vec::new();
    let fallback_id = path
        .file_stem()
        .and_then(|stem| stem.to_str())
        .and_then(|name| name.rsplit('-').next())
        .unwrap_or("")
        .to_owned();
    let Ok(file) = File::open(path) else {
        return (metas, events, 1);
    };
    let mut active_id = fallback_id;
    let mut active_model = String::new();
    for line in BufReader::new(file).lines().map_while(Result::ok) {
        let Ok(value) = serde_json::from_str::<CodexLine>(&line) else {
            continue;
        };
        let outer = value.kind.as_deref().unwrap_or("");
        if outer == "session_meta" {
            if let Some(id) = value
                .payload
                .as_ref()
                .and_then(|p| p.id.as_deref().or(p.session_id.as_deref()))
            {
                active_id = id.to_owned();
            }
        }
        if active_id.is_empty() {
            continue;
        }
        let session = metas.entry(active_id.clone()).or_default();
        session.id = active_id.clone();
        let at = timestamp(value.timestamp.as_ref());
        session.observe_time(at);
        let payload = value.payload.as_ref();
        match outer {
            "session_meta" => {
                if let Some(cwd) = payload.and_then(|p| p.cwd.as_deref()) {
                    session.cwd = cwd.to_owned();
                }
                if let Some(source) = payload
                    .and_then(|p| p.source.as_ref())
                    .and_then(Value::as_str)
                {
                    session.source = source.to_owned();
                }
            }
            "turn_context" => {
                if let Some(model) = payload.and_then(|p| p.model.as_deref()) {
                    active_model = model.to_owned();
                    session.model = active_model.clone();
                }
                if let Some(cwd) = payload.and_then(|p| p.cwd.as_deref()) {
                    session.cwd = cwd.to_owned();
                }
                session.mode = payload
                    .and_then(|p| p.collaboration_mode.as_ref())
                    .and_then(|cm| cm.get("kind").or(Some(cm)).and_then(Value::as_str))
                    .unwrap_or("")
                    .to_owned();
            }
            "event_msg" => match payload
                .and_then(|p| p.payload_type.as_deref())
                .unwrap_or("")
            {
                "user_message" if session.title.is_empty() => {
                    session.title = payload
                        .and_then(|p| p.message.as_deref())
                        .map(compact_title)
                        .unwrap_or_default();
                }
                "token_count" => {
                    let Some(info) = payload.and_then(|p| p.info.as_ref()) else {
                        continue;
                    };
                    let Some(usage) = info.last_token_usage.as_ref() else {
                        continue;
                    };
                    let total = info.total_token_usage.as_ref().unwrap_or(usage);
                    let signature = format!(
                        "{}:{}:{}:{}",
                        total.input_tokens.unwrap_or(0.0),
                        total.cached_input_tokens.unwrap_or(0.0),
                        total.cache_write_input_tokens.unwrap_or(0.0),
                        total.output_tokens.unwrap_or(0.0)
                    );
                    let raw_input = usage.input_tokens.unwrap_or(0.0);
                    let cached = usage.cached_input_tokens.unwrap_or(0.0);
                    let input = (raw_input - cached).max(0.0);
                    let output = usage.output_tokens.unwrap_or(0.0);
                    let cache_write = usage.cache_write_input_tokens.unwrap_or(0.0);
                    if !active_model.is_empty() {
                        session.model = active_model.clone();
                    }
                    events.push(CodexEvent {
                        session_id: active_id.clone(),
                        created_at: at,
                        input_tokens: input,
                        output_tokens: output,
                        cached_tokens: cached,
                        cache_write_tokens: cache_write,
                        signature,
                    });
                }
                _ => {}
            },
            _ => {}
        }
    }
    (metas, events, 0)
}

pub(crate) fn load_codex(start: i64, end: i64, previous: Option<&LoadedData>) -> LoadedData {
    let agent = AgentKind::Codex;
    let base = std::env::var_os("CODEX_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| home_path(&[".codex"]));
    let roots = [base.join("sessions"), base.join("archived_sessions")];
    let mut data = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };
    if !roots.iter().any(|root| root.exists()) {
        data.errors
            .push(error(agent, format!("未找到 {}", roots[0].display())));
        return data;
    }

    let files = recent_files(&roots, "jsonl");
    let reusable = reusable_paths(previous, &files, file_mtime_secs);
    let reusable_keys: HashSet<String> = previous
        .map(|prev| {
            reusable
                .iter()
                .filter_map(|path| prev.file_sessions.get(*path))
                .cloned()
                .collect()
        })
        .unwrap_or_default();
    let (reused_sessions, reused_turns) =
        reuse_records(previous, agent, start, end, &reusable_keys);

    let parse_files: Vec<&PathBuf> = files
        .iter()
        .filter(|path| path.to_str().map(|k| !reusable.contains(k)).unwrap_or(true))
        .collect();
    let mut metas: HashMap<String, SessionBuilder> = HashMap::new();
    let mut events: Vec<CodexEvent> = Vec::new();
    let mut skipped = 0usize;
    let results: Vec<(HashMap<String, SessionBuilder>, Vec<CodexEvent>, usize)> = parse_files
        .par_iter()
        .map(|path| parse_codex_file(path))
        .collect();
    for (path, (partial, partial_events, skip)) in parse_files.iter().zip(results) {
        // 一个文件只对应一个会话时才记录路径 → 会话 key 的映射，
        // 否则下次无法安全复用，保持重新解析。
        let single_key = match partial.len() {
            1 => partial.keys().next().map(|id| format!("codex/{id}")),
            _ => None,
        };
        for (id, session) in partial {
            match metas.get_mut(&id) {
                Some(existing) => existing.merge(session),
                None => {
                    metas.insert(id, session);
                }
            }
        }
        record_index(
            &mut data,
            path,
            file_mtime_secs(path),
            single_key.as_deref(),
        );
        events.extend(partial_events);
        skipped += skip;
    }
    // 复用的文件沿用原索引
    if let Some(prev) = previous {
        for path in &files {
            if let Some(key) = path.to_str() {
                if reusable.contains(key) {
                    record_index(
                        &mut data,
                        path,
                        file_mtime_secs(path),
                        prev.file_sessions.get(key).map(String::as_str),
                    );
                }
            }
        }
    }

    // Dedup by (session, cumulative usage signature) in original file order,
    // then accumulate tokens and turns. This mirrors the sequential behavior.
    let mut seen_usage: HashMap<String, HashSet<String>> = HashMap::new();
    for event in events {
        if !seen_usage
            .entry(event.session_id.clone())
            .or_default()
            .insert(event.signature)
        {
            continue;
        }
        let session = metas.entry(event.session_id.clone()).or_default();
        session.input_tokens += event.input_tokens;
        session.output_tokens += event.output_tokens;
        session.cached_tokens += event.cached_tokens;
        session.cache_creation_tokens += event.cache_write_tokens;
        session.agent_messages += 1.0;
        if event.created_at >= start && event.created_at < end {
            data.turns.push(TurnRec {
                agent,
                session_key: format!("codex/{}", event.session_id),
                created_at: event.created_at,
                input_tokens: event.input_tokens,
                output_tokens: event.output_tokens,
                cache_read_tokens: event.cached_tokens,
                cache_creation_tokens: event.cache_write_tokens,
                cache_creation_5m_tokens: 0.0,
                cache_creation_1h_tokens: 0.0,
                model: String::new(),
                ttft_ms: 0.0,
                total_time_ms: 0.0,
            });
        }
    }

    data.sessions = metas
        .into_values()
        .map(|session| session.finish(agent))
        .chain(reused_sessions)
        .collect();
    data.turns.extend(reused_turns);
    if skipped > 0 {
        data.errors
            .push(error(agent, format!("有 {skipped} 个会话文件无法读取")));
    }
    data
}

// ---------------------------------------------------------------------------
// Antigravity
//
// Google Antigravity stores each conversation as a SQLite database under
// `~/.gemini/antigravity/conversations/<uuid>.db`. The schema uses protobuf-
// encoded blobs for most fields, so we parse the relevant subset with a
// minimal varint/length-delimited reader instead of pulling in a protobuf
// crate.
//
// Key tables:
//   trajectory_metadata_blob  – session-level metadata (cwd, created_at)
//   gen_metadata              – per-LLM-generation token usage + model name
//   steps                     – per-step payloads (user messages for titles)
// ---------------------------------------------------------------------------

/// 解析 protobuf varint，返回 (值, 新位置)。
fn pb_varint(data: &[u8], mut pos: usize) -> Option<(u64, usize)> {
    let mut result: u64 = 0;
    let mut shift = 0;
    while pos < data.len() {
        let byte = data[pos];
        pos += 1;
        result |= u64::from(byte & 0x7f) << shift;
        if byte & 0x80 == 0 {
            return Some((result, pos));
        }
        shift += 7;
        if shift >= 64 {
            return None;
        }
    }
    None
}

/// 单个 protobuf 字段。
struct PbField<'a> {
    field_num: u32,
    wire_type: u32,
    bytes: &'a [u8],
}

/// 遍历 protobuf 消息中的所有顶层字段。
struct PbIter<'a> {
    data: &'a [u8],
    pos: usize,
}

impl<'a> PbIter<'a> {
    fn new(data: &'a [u8]) -> Self {
        Self { data, pos: 0 }
    }
}

impl<'a> Iterator for PbIter<'a> {
    type Item = PbField<'a>;

    fn next(&mut self) -> Option<PbField<'a>> {
        if self.pos >= self.data.len() {
            return None;
        }
        let (tag, pos) = pb_varint(self.data, self.pos)?;
        let field_num = (tag >> 3) as u32;
        let wire_type = (tag & 0x7) as u32;
        match wire_type {
            0 => {
                // varint
                let (_, end) = pb_varint(self.data, pos)?;
                let bytes = &self.data[pos..end];
                self.pos = end;
                Some(PbField {
                    field_num,
                    wire_type,
                    bytes,
                })
            }
            1 => {
                // 64-bit
                if pos + 8 > self.data.len() {
                    return None;
                }
                let bytes = &self.data[pos..pos + 8];
                self.pos = pos + 8;
                Some(PbField {
                    field_num,
                    wire_type,
                    bytes,
                })
            }
            2 => {
                // length-delimited
                let (length, lpos) = pb_varint(self.data, pos)?;
                let length = length as usize;
                if lpos + length > self.data.len() {
                    return None;
                }
                let bytes = &self.data[lpos..lpos + length];
                self.pos = lpos + length;
                Some(PbField {
                    field_num,
                    wire_type,
                    bytes,
                })
            }
            5 => {
                // 32-bit
                if pos + 4 > self.data.len() {
                    return None;
                }
                let bytes = &self.data[pos..pos + 4];
                self.pos = pos + 4;
                Some(PbField {
                    field_num,
                    wire_type,
                    bytes,
                })
            }
            _ => None, // 不支持 group 等已废弃的 wire type
        }
    }
}

/// 在 protobuf 消息中查找指定字段号的第一个 length-delimited 字段。
fn pb_field(data: &[u8], num: u32) -> Option<&[u8]> {
    for field in PbIter::new(data) {
        if field.field_num == num && field.wire_type == 2 {
            return Some(field.bytes);
        }
    }
    None
}

/// 在 protobuf 消息中查找指定字段号的第一个 varint 字段。
fn pb_varint_field(data: &[u8], num: u32) -> Option<u64> {
    for field in PbIter::new(data) {
        if field.field_num == num && field.wire_type == 0 {
            return pb_varint(field.bytes, 0).map(|(v, _)| v);
        }
    }
    None
}

/// 沿字段号路径导航嵌套 protobuf 消息，返回最后一个 length-delimited 字段。
fn pb_path<'a>(data: &'a [u8], path: &[u32]) -> Option<&'a [u8]> {
    let mut cur = data;
    for &num in path {
        cur = pb_field(cur, num)?;
    }
    Some(cur)
}

/// 沿路径导航，最后一个字段取 varint 值。
fn pb_path_varint(data: &[u8], path: &[u32]) -> Option<u64> {
    if path.is_empty() {
        return None;
    }
    let parent = pb_path(data, &path[..path.len() - 1])?;
    pb_varint_field(parent, *path.last().unwrap())
}

/// 沿路径导航，最后一个字段取 UTF-8 字符串。
fn pb_path_string(data: &[u8], path: &[u32]) -> Option<String> {
    if path.is_empty() {
        return None;
    }
    let parent = pb_path(data, &path[..path.len() - 1])?;
    pb_field(parent, *path.last().unwrap())
        .and_then(|b| std::str::from_utf8(b).ok().map(str::to_owned))
}

/// 从 step_payload 中尝试提取用户消息文本（用作会话标题）。
fn antigravity_step_text(payload: &[u8]) -> Option<String> {
    // 尝试多种可能的字段路径
    for path in &[[26u32, 3, 1], [26, 2, 1], [19, 3, 1], [19, 2, 1]] {
        if let Some(text) = pb_path_string(payload, path) {
            if !text.trim().is_empty() {
                return Some(compact_title(&text));
            }
        }
    }
    None
}

/// `parse_antigravity_db` 的结果：区分“真正打不开”和“能读但没有任何可计量的
/// token 用量”（后者不应被当作读取失败报警）。
enum AgParse {
    /// 成功解析出一个会话。
    Ok(Box<(SessionRec, Vec<TurnRec>)>),
    /// 文件能读、结构正常，但没有任何可计量的 token 用量（空会话，通常是一次
    /// 未产生计费生成的失败调用）。
    Empty,
    /// 文件打不开或无法解析（连接失败、无可用的会话文件名等）。
    Error,
}

/// 解析单个 Antigravity 会话数据库。
fn parse_antigravity_db(path: &Path, start: i64, end: i64) -> AgParse {
    let session_id = path
        .file_stem()
        .and_then(|s| s.to_str())
        .unwrap_or("")
        .to_owned();
    if session_id.is_empty() {
        return AgParse::Error;
    }

    let conn = match rusqlite::Connection::open_with_flags(
        path,
        rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY | rusqlite::OpenFlags::SQLITE_OPEN_NO_MUTEX,
    ) {
        Ok(c) => c,
        Err(_) => return AgParse::Error,
    };
    let _ = conn.busy_timeout(std::time::Duration::from_millis(500));

    // 会话级元数据：trajectory_metadata_blob
    let mut cwd = String::new();
    let mut created_at: i64 = 0;
    if let Ok(blob) = conn.query_row(
        "SELECT data FROM trajectory_metadata_blob WHERE id='main'",
        [],
        |r| r.get::<_, Vec<u8>>(0),
    ) {
        // cwd 在 field 1.field 1（file:// URI）或 field 7
        if let Some(uri) = pb_path_string(&blob, &[1, 1]) {
            cwd = uri.trim_start_matches("file://").to_owned();
        }
        if cwd.is_empty() {
            if let Some(uri) = pb_path_string(&blob, &[7]) {
                cwd = uri.trim_start_matches("file://").to_owned();
            }
        }
        // 创建时间在 field 2.field 1（Unix 秒）
        if let Some(ts) = pb_path_varint(&blob, &[2, 1]) {
            created_at = ts as i64;
        }
    }

    // 用户消息（标题）：从 steps 表中找第一个 type=14 的步骤
    let mut title = String::new();
    if let Ok(mut stmt) =
        conn.prepare("SELECT step_payload FROM steps WHERE step_type=14 ORDER BY idx LIMIT 5")
    {
        let rows = stmt.query_map([], |r| r.get::<_, Vec<u8>>(0));
        if let Ok(rows) = rows {
            for row in rows.flatten() {
                if let Some(text) = antigravity_step_text(&row) {
                    title = text;
                    break;
                }
            }
        }
    }

    // 模型名从 gen_metadata 的 field 1.field 19 获取（每个生成步骤一条，
    // 取最后一个非空值；同一会话通常只用一两个模型）。
    let mut session = SessionBuilder {
        id: session_id.clone(),
        ..Default::default()
    };
    session.cwd = cwd;
    session.title = title;
    session.source = "local".into();
    session.created_at = created_at;
    if let Ok(mut stmt) = conn.prepare("SELECT data FROM gen_metadata ORDER BY idx") {
        let rows = stmt.query_map([], |r| r.get::<_, Vec<u8>>(0));
        if let Ok(rows) = rows {
            for row in rows.flatten() {
                if let Some(f1) = pb_field(&row, 1) {
                    if let Some(model) = pb_path_string(f1, &[19]) {
                        if !model.is_empty() {
                            session.model = model;
                        }
                    }
                }
            }
        }
    }

    // 逐个生成步骤的 token 使用量与时间戳：从 steps 表的 metadata 解析。
    // steps.metadata 包含时间戳（field 1.1，Unix 秒）与 token 用量
    // （field 9，对应 Antigravity 内部的 UsageMetadata proto）。
    //
    // 经数据验证的字段映射（与标准 Google AI / Vertex AI proto 编号不同，
    // Antigravity 使用自定义编号）：
    //   field 1  = prompt_token_count        (输入，不含 cached)
    //   field 2  = candidates_token_count    (输出)
    //   field 3  = cache_creation_token_count (缓存写入)
    //   field 5  = cached_content_token_count (缓存读取)
    //   field 10 = thoughts_token_count      (思考 tokens，按 output 价格计费)
    //
    // 验证依据：首次调用时 field 3 > 0 且 field 5 = 0（创建缓存），
    // 后续调用 field 3 较小且 field 5 很大（读取缓存），缓存过期后
    // field 5 归零、field 3 再次变大——典型的 cache 生命周期模式。
    //
    // Google 定价：output 价格包含 thinking tokens，因此 thoughts
    // 并入 output_tokens 按 output 价格计费。
    //
    // 部分新版会话的 steps.metadata 没有 field 9，但有 field 11
    // （prompt_token_count），此时仅能获取输入 token 数。
    let key = format!("antigravity/{session_id}");
    let mut turns = Vec::new();

    if let Ok(mut stmt) = conn.prepare("SELECT metadata FROM steps ORDER BY idx") {
        let rows = stmt.query_map([], |r| r.get::<_, Vec<u8>>(0));
        if let Ok(rows) = rows {
            for row in rows.flatten() {
                // 时间戳在 metadata field 1.field 1（Unix 秒）
                let at = pb_path_varint(&row, &[1, 1]).unwrap_or(0) as i64;
                if at > 0 {
                    session.observe_time(at);
                }

                // 优先从 field 9（UsageMetadata）读取完整 token 数据
                if let Some(usage) = pb_field(&row, 9) {
                    let prompt = pb_path_varint(usage, &[1]).unwrap_or(0) as f64;
                    let candidates = pb_path_varint(usage, &[2]).unwrap_or(0) as f64;
                    let cache_creation = pb_path_varint(usage, &[3]).unwrap_or(0) as f64;
                    let cached = pb_path_varint(usage, &[5]).unwrap_or(0) as f64;
                    // field 10 = thoughts_token_count，按 output 价格计费
                    let thoughts = pb_path_varint(usage, &[10]).unwrap_or(0) as f64;
                    let output = candidates + thoughts;

                    session.input_tokens += prompt;
                    session.output_tokens += output;
                    session.cached_tokens += cached;
                    session.cache_creation_tokens += cache_creation;
                    session.agent_messages += 1.0;
                    if at >= start && at < end {
                        turns.push(TurnRec {
                            agent: AgentKind::Antigravity,
                            session_key: key.clone(),
                            created_at: at,
                            input_tokens: prompt,
                            output_tokens: output,
                            cache_read_tokens: cached,
                            cache_creation_tokens: cache_creation,
                            cache_creation_5m_tokens: 0.0,
                            cache_creation_1h_tokens: 0.0,
                            model: String::new(),
                            ttft_ms: 0.0,
                            total_time_ms: 0.0,
                        });
                    }
                } else if let Some(prompt) = pb_varint_field(&row, 11) {
                    // 新版 schema：field 11 = prompt_token_count，无 output/cached
                    let prompt = prompt as f64;
                    session.input_tokens += prompt;
                    session.agent_messages += 1.0;
                    if at >= start && at < end {
                        turns.push(TurnRec {
                            agent: AgentKind::Antigravity,
                            session_key: key.clone(),
                            created_at: at,
                            input_tokens: prompt,
                            output_tokens: 0.0,
                            cache_read_tokens: 0.0,
                            cache_creation_tokens: 0.0,
                            cache_creation_5m_tokens: 0.0,
                            cache_creation_1h_tokens: 0.0,
                            model: String::new(),
                            ttft_ms: 0.0,
                            total_time_ms: 0.0,
                        });
                    }
                }
            }
        }
    }

    if session.agent_messages == 0.0 {
        AgParse::Empty
    } else {
        AgParse::Ok(Box::new((session.finish(AgentKind::Antigravity), turns)))
    }
}

pub(crate) fn load_antigravity(start: i64, end: i64, previous: Option<&LoadedData>) -> LoadedData {
    let agent = AgentKind::Antigravity;
    let root = home_path(&[".gemini", "antigravity", "conversations"]);
    let mut data = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };
    if !root.exists() {
        data.errors
            .push(error(agent, format!("未找到 {}", root.display())));
        return data;
    }

    // 收集 .db 文件（排除 -wal/-shm 附属文件），按修改时间倒序取最近的
    let mut files = Vec::new();
    collect_files(&root, "db", &mut files);
    files.retain(|p| {
        p.extension().and_then(|e| e.to_str()) == Some("db")
            && !p.to_string_lossy().contains("-wal")
            && !p.to_string_lossy().contains("-shm")
    });
    files.sort_by_key(|p| {
        std::cmp::Reverse(
            p.metadata()
                .and_then(|m| m.modified())
                .unwrap_or(UNIX_EPOCH),
        )
    });
    files.truncate(MAX_SESSION_FILES);

    // WAL 未 checkpoint 时主库 mtime 不变，签名要把 -wal 文件算进去
    let db_signature = |path: &Path| {
        let wal = path.with_extension("db-wal");
        max_mtime(&[path, &wal])
    };
    let reusable = reusable_paths(previous, &files, db_signature);
    let reusable_keys: HashSet<String> = previous
        .map(|prev| {
            reusable
                .iter()
                .filter_map(|path| prev.file_sessions.get(*path))
                .cloned()
                .collect()
        })
        .unwrap_or_default();
    let (reused_sessions, reused_turns) =
        reuse_records(previous, agent, start, end, &reusable_keys);

    let parse_files: Vec<&PathBuf> = files
        .iter()
        .filter(|path| path.to_str().map(|k| !reusable.contains(k)).unwrap_or(true))
        .collect();
    let mut parse_errors = 0usize;
    let results: Vec<AgParse> = parse_files
        .par_iter()
        .map(|path| parse_antigravity_db(path, start, end))
        .collect();
    for (path, result) in parse_files.iter().zip(results) {
        match result {
            AgParse::Ok(boxed) => {
                let (session, mut turns) = *boxed;
                record_index(
                    &mut data,
                    path,
                    db_signature(path),
                    Some(session.key.as_str()),
                );
                data.sessions.push(session);
                data.turns.append(&mut turns);
            }
            // 能读但没有任何可计量的用量（通常是未产生计费生成的失败会话）：
            // 直接跳过，不作为“无法读取”报警。
            AgParse::Empty => {
                record_index(&mut data, path, db_signature(path), None);
            }
            AgParse::Error => {
                record_index(&mut data, path, db_signature(path), None);
                parse_errors += 1;
            }
        }
    }
    // 复用的文件沿用原索引
    if let Some(prev) = previous {
        for path in &files {
            if let Some(key) = path.to_str() {
                if reusable.contains(key) {
                    record_index(
                        &mut data,
                        path,
                        db_signature(path),
                        prev.file_sessions.get(key).map(String::as_str),
                    );
                }
            }
        }
    }
    data.sessions.extend(reused_sessions);
    data.turns.extend(reused_turns);
    if parse_errors > 0 {
        data.errors.push(error(
            agent,
            format!("有 {parse_errors} 个会话文件无法读取"),
        ));
    }
    data
}

// ==================== Grok Build ====================

fn parse_grok_summary(path: &Path) -> Option<(SessionBuilder, PathBuf)> {
    let file = File::open(path).ok()?;
    let val: Value = serde_json::from_reader(BufReader::new(file)).ok()?;
    let sess_dir = path.parent()?.to_path_buf();
    let id = val
        .pointer("/info/id")
        .and_then(Value::as_str)
        .map(String::from)
        .or_else(|| {
            sess_dir
                .file_name()
                .and_then(|s| s.to_str())
                .map(String::from)
        })?;

    let cwd = val
        .pointer("/info/cwd")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();

    let title = val
        .get("generated_title")
        .or_else(|| val.get("session_summary"))
        .and_then(Value::as_str)
        .map(compact_title)
        .unwrap_or_else(|| id.clone());

    let model = val
        .get("current_model_id")
        .and_then(Value::as_str)
        .unwrap_or("grok")
        .to_string();

    let mode = val
        .get("agent_name")
        .and_then(Value::as_str)
        .unwrap_or("grok-build")
        .to_string();

    let created_at = timestamp(val.get("created_at"));
    let last_active_at = timestamp(val.get("updated_at"))
        .max(timestamp(val.get("last_active_at")))
        .max(created_at);

    let num_messages = val
        .get("num_messages")
        .or_else(|| val.get("num_chat_messages"))
        .and_then(Value::as_f64)
        .unwrap_or(0.0);

    let mut session = SessionBuilder {
        id,
        title,
        cwd,
        model,
        mode,
        source: "local".into(),
        created_at,
        last_activity_at: last_active_at,
        agent_messages: num_messages,
        ..Default::default()
    };
    session.observe_time(created_at);
    session.observe_time(last_active_at);

    Some((session, sess_dir))
}

fn parse_grok_session(
    summary_path: &Path,
    start: i64,
    end: i64,
) -> (Option<SessionRec>, Vec<TurnRec>) {
    let Some((mut session, sess_dir)) = parse_grok_summary(summary_path) else {
        return (None, Vec::new());
    };

    let mut turns = Vec::new();
    let updates_path = sess_dir.join("updates.jsonl");
    if let Ok(file) = File::open(&updates_path) {
        let reader = BufReader::new(file);
        for line in reader.lines().map_while(Result::ok) {
            if line.trim().is_empty() {
                continue;
            }
            let Ok(obj) = serde_json::from_str::<Value>(&line) else {
                continue;
            };
            let update = obj.pointer("/params/update");
            if update
                .and_then(|u| u.get("sessionUpdate"))
                .and_then(Value::as_str)
                != Some("turn_completed")
            {
                continue;
            }
            let Some(usage) = update.and_then(|u| u.get("usage")) else {
                continue;
            };

            let raw_inp = value_number(usage.get("inputTokens"));
            let out = value_number(usage.get("outputTokens"));
            let cached = value_number(usage.get("cachedReadTokens"));
            let cache_creation = value_number(usage.get("cacheCreationTokens"));
            let inp = (raw_inp - cached).max(0.0);
            let duration_ms = value_number(usage.get("apiDurationMs"));

            let mut turn_model = session.model.clone();
            if let Some(model_usage) = usage.get("modelUsage").and_then(Value::as_object) {
                if let Some(first_model) = model_usage.keys().next() {
                    turn_model = first_model.clone();
                }
            }

            let ts = timestamp(obj.get("timestamp"));
            let turn_ts = if ts > 0 {
                ts
            } else if let Some(ms) = obj
                .pointer("/_meta/agentTimestampMs")
                .and_then(Value::as_i64)
            {
                ms / 1000
            } else {
                session.created_at
            };

            session.observe_time(turn_ts);
            session.input_tokens += inp;
            session.output_tokens += out;
            session.cached_tokens += cached;
            session.cache_creation_tokens += cache_creation;

            if turn_ts >= start && turn_ts < end {
                turns.push(TurnRec {
                    session_key: format!("grok-build/{}", session.id),
                    created_at: turn_ts,
                    agent: AgentKind::Grok,
                    model: turn_model,
                    input_tokens: inp,
                    output_tokens: out,
                    cache_read_tokens: cached,
                    cache_creation_tokens: cache_creation,
                    cache_creation_5m_tokens: 0.0,
                    cache_creation_1h_tokens: 0.0,
                    ttft_ms: 0.0,
                    total_time_ms: duration_ms,
                });
            }
        }
    }

    (Some(session.finish(AgentKind::Grok)), turns)
}

pub(crate) fn load_grok(start: i64, end: i64, previous: Option<&LoadedData>) -> LoadedData {
    let agent = AgentKind::Grok;
    let root = home_path(&[".grok", "sessions"]);
    let mut data = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };
    if !root.exists() {
        data.errors
            .push(error(agent, format!("未找到 {}", root.display())));
        return data;
    }

    let mut summary_files = Vec::new();
    collect_files(&root, "json", &mut summary_files);
    summary_files.retain(|p| p.file_name().and_then(|s| s.to_str()) == Some("summary.json"));
    summary_files.sort_by_key(|p| {
        std::cmp::Reverse(
            p.metadata()
                .and_then(|m| m.modified())
                .unwrap_or(UNIX_EPOCH),
        )
    });
    summary_files.truncate(MAX_SESSION_FILES);

    // 会话进展写在 updates.jsonl，签名要把两个文件都算进去
    let grok_signature = |path: &Path| {
        let updates = path.with_file_name("updates.jsonl");
        max_mtime(&[path, &updates])
    };
    let reusable = reusable_paths(previous, &summary_files, grok_signature);
    let reusable_keys: HashSet<String> = previous
        .map(|prev| {
            reusable
                .iter()
                .filter_map(|path| prev.file_sessions.get(*path))
                .cloned()
                .collect()
        })
        .unwrap_or_default();
    let (reused_sessions, reused_turns) =
        reuse_records(previous, agent, start, end, &reusable_keys);

    let parse_files: Vec<&PathBuf> = summary_files
        .iter()
        .filter(|path| path.to_str().map(|k| !reusable.contains(k)).unwrap_or(true))
        .collect();
    let mut skipped = 0usize;
    let results: Vec<(Option<SessionRec>, Vec<TurnRec>)> = parse_files
        .par_iter()
        .map(|path| parse_grok_session(path, start, end))
        .collect();
    for (path, (session, mut turns)) in parse_files.iter().zip(results) {
        if let Some(session) = session {
            record_index(
                &mut data,
                path,
                grok_signature(path),
                Some(session.key.as_str()),
            );
            data.sessions.push(session);
        } else {
            record_index(&mut data, path, grok_signature(path), None);
            skipped += 1;
        }
        data.turns.append(&mut turns);
    }
    // 复用的文件沿用原索引
    if let Some(prev) = previous {
        for path in &summary_files {
            if let Some(key) = path.to_str() {
                if reusable.contains(key) {
                    record_index(
                        &mut data,
                        path,
                        grok_signature(path),
                        prev.file_sessions.get(key).map(String::as_str),
                    );
                }
            }
        }
    }
    data.sessions.extend(reused_sessions);
    data.turns.extend(reused_turns);
    if skipped > 0 {
        data.errors
            .push(error(agent, format!("有 {skipped} 个会话文件无法读取")));
    }
    data
}

// ==================== ZCode ====================

pub(crate) fn load_zcode(start: i64, end: i64) -> LoadedData {
    use rusqlite::{Connection, OpenFlags};

    let agent = AgentKind::ZCode;
    let candidates = [
        home_path(&[".zcode", "cli", "db", "db.sqlite"]),
        home_path(&[
            "Library",
            "Application Support",
            "zcode",
            "cli",
            "db",
            "db.sqlite",
        ]),
    ];
    let mut data = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };

    let db_path = candidates.into_iter().find(|p| p.exists());
    let Some(path) = db_path else {
        data.errors
            .push(error(agent, "未找到 ~/.zcode/cli/db/db.sqlite"));
        return data;
    };

    let conn = match Connection::open_with_flags(
        &path,
        OpenFlags::SQLITE_OPEN_READ_ONLY | OpenFlags::SQLITE_OPEN_URI,
    ) {
        Ok(c) => c,
        Err(e) => {
            data.errors
                .push(error(agent, format!("无法打开 ZCode 数据库: {e}")));
            return data;
        }
    };

    let mut session_builders: HashMap<String, SessionBuilder> = HashMap::new();
    let mut parent_map: HashMap<String, String> = HashMap::new();

    if let Ok(mut stmt) = conn.prepare(
        "SELECT id, parent_id, directory, title, time_created, time_updated, task_type FROM session",
    ) {
        let rows = stmt.query_map([], |row| {
            let id: String = row.get(0)?;
            let parent_id: Option<String> = row.get(1)?;
            let dir: Option<String> = row.get(2)?;
            let title: Option<String> = row.get(3)?;
            let time_created: Option<i64> = row.get(4)?;
            let time_updated: Option<i64> = row.get(5)?;
            let task_type: Option<String> = row.get(6)?;
            Ok((
                id,
                parent_id,
                dir,
                title,
                time_created,
                time_updated,
                task_type,
            ))
        });
        if let Ok(rows) = rows {
            for row in rows.flatten() {
                let (id, parent_id, dir, title, time_created, time_updated, task_type) = row;
                if let Some(ref p) = parent_id {
                    parent_map.insert(id.clone(), p.clone());
                }
                let created_sec = time_created
                    .map(|t| if t > 10_000_000_000 { t / 1000 } else { t })
                    .unwrap_or(0);
                let updated_sec = time_updated
                    .map(|t| if t > 10_000_000_000 { t / 1000 } else { t })
                    .unwrap_or(created_sec);

                session_builders.insert(
                    id.clone(),
                    SessionBuilder {
                        id: id.clone(),
                        title: title
                            .map(|t| compact_title(&t))
                            .unwrap_or_else(|| id.clone()),
                        cwd: dir.unwrap_or_default(),
                        model: String::new(),
                        mode: task_type.unwrap_or_else(|| "interactive".into()),
                        source: "local".into(),
                        created_at: created_sec,
                        last_activity_at: updated_sec,
                        ..Default::default()
                    },
                );
            }
        }
    }

    let resolve_root = |mut sid: String| -> String {
        let mut depth = 0;
        while let Some(parent) = parent_map.get(&sid) {
            sid = parent.clone();
            depth += 1;
            if depth > 20 {
                break;
            }
        }
        sid
    };

    if let Ok(mut stmt) = conn.prepare(
        "SELECT session_id, turn_id, model_id, started_at, completed_at, input_tokens, output_tokens, reasoning_tokens, cache_creation_input_tokens, cache_read_input_tokens, time_to_first_token_ms FROM model_usage WHERE status = 'completed' OR input_tokens > 0 OR output_tokens > 0",
    ) {
        let rows = stmt.query_map([], |row| {
            let session_id: String = row.get(0)?;
            let turn_id: Option<String> = row.get(1)?;
            let model_id: Option<String> = row.get(2)?;
            let started_at: Option<i64> = row.get(3)?;
            let completed_at: Option<i64> = row.get(4)?;
            let input_tokens: Option<f64> = row.get(5)?;
            let output_tokens: Option<f64> = row.get(6)?;
            let _reasoning_tokens: Option<f64> = row.get(7)?;
            let cache_creation_tokens: Option<f64> = row.get(8)?;
            let cache_read_tokens: Option<f64> = row.get(9)?;
            let ttft_ms: Option<f64> = row.get(10)?;
            Ok((
                session_id,
                turn_id,
                model_id,
                started_at,
                completed_at,
                input_tokens,
                output_tokens,
                cache_creation_tokens,
                cache_read_tokens,
                ttft_ms,
            ))
        });
        if let Ok(rows) = rows {
            for row in rows.flatten() {
                let (
                    session_id,
                    _turn_id,
                    model_id,
                    started_at,
                    completed_at,
                    input_tokens,
                    output_tokens,
                    cache_creation_tokens,
                    cache_read_tokens,
                    ttft_ms,
                ) = row;

                let root_id = resolve_root(session_id.clone());
                let raw_inp = input_tokens.unwrap_or(0.0);
                let out = output_tokens.unwrap_or(0.0);
                let cache_cr = cache_creation_tokens.unwrap_or(0.0);
                let cache_rd = cache_read_tokens.unwrap_or(0.0);
                let inp = (raw_inp - cache_rd).max(0.0);
                let model = model_id.filter(|m| !m.is_empty()).unwrap_or_else(|| "unknown".into());

                let start_sec = started_at
                    .map(|t| if t > 10_000_000_000 { t / 1000 } else { t })
                    .unwrap_or(0);
                let duration_ms = match (started_at, completed_at) {
                    (Some(s), Some(c)) if c >= s => (c - s) as f64,
                    _ => 0.0,
                };

                let builder = session_builders.entry(root_id.clone()).or_insert_with(|| SessionBuilder {
                    id: root_id.clone(),
                    title: root_id.clone(),
                    cwd: String::new(),
                    model: model.clone(),
                    mode: "interactive".into(),
                    source: "local".into(),
                    created_at: start_sec,
                    last_activity_at: start_sec,
                    ..Default::default()
                });
                builder.input_tokens += inp;
                builder.output_tokens += out;
                builder.cached_tokens += cache_rd;
                builder.cache_creation_tokens += cache_cr;
                builder.agent_messages += 1.0;
                if builder.model.is_empty() {
                    builder.model = model.clone();
                }
                builder.observe_time(start_sec);

                if start_sec >= start && start_sec < end {
                    data.turns.push(TurnRec {
                        session_key: format!("zcode/{}", root_id),
                        created_at: start_sec,
                        agent: AgentKind::ZCode,
                        model,
                        input_tokens: inp,
                        output_tokens: out,
                        cache_read_tokens: cache_rd,
                        cache_creation_tokens: cache_cr,
                        cache_creation_5m_tokens: 0.0,
                        cache_creation_1h_tokens: 0.0,
                        ttft_ms: ttft_ms.unwrap_or(0.0),
                        total_time_ms: duration_ms,
                    });
                }
            }
        }
    }

    let top_sessions: Vec<SessionRec> = session_builders
        .into_iter()
        .filter(|(id, _)| !parent_map.contains_key(id))
        .map(|(_, builder)| builder.finish(AgentKind::ZCode))
        .collect();

    data.sessions = top_sessions;
    data
}

#[cfg(test)]
mod tests {
    use super::{
        amp_cache_dir, amp_list_cache_path, content_text, load_amp_thread_export, parse_amp_value,
        timestamp, AmpListCache, AmpListEntry,
    };
    use serde_json::json;
    use std::fs::File;
    use std::io::BufWriter;

    #[test]
    fn parses_iso_and_millisecond_timestamps() {
        assert_eq!(timestamp(Some(&json!(1_700_000_000_123i64))), 1_700_000_000);
        assert_eq!(
            timestamp(Some(&json!("2026-08-12T12:37:36.155Z"))),
            1_786_538_256
        );
    }

    #[test]
    fn extracts_text_from_message_content() {
        assert_eq!(
            content_text(&json!([{ "type": "text", "text": "  hello\n world  " }])),
            "hello world"
        );
    }

    #[test]
    fn parses_new_amp_export_usage() {
        let value = json!({
            "id": "T-new-amp",
            "created": 1_786_942_400_000i64,
            "messages": [{
                "role": "assistant",
                "messageId": 2,
                "usage": {
                    "model": "gpt-5.6-terra",
                    "timestamp": "2026-08-17T04:55:12.502Z",
                    "inputTokens": 1200,
                    "outputTokens": 240,
                    "cacheCreationInputTokens": 300,
                    "cacheReadInputTokens": 600,
                    "totalInputTokens": 2100
                }
            }]
        });
        let (session, turns, skipped) = parse_amp_value(value, None, 0, i64::MAX);
        assert_eq!(skipped, 0);
        assert_eq!(session.unwrap().selected_model, "gpt-5.6-terra");
        assert_eq!(turns.len(), 1);
        assert_eq!(turns[0].input_tokens, 1200.0);
        assert_eq!(turns[0].output_tokens, 240.0);
        assert_eq!(turns[0].cache_creation_tokens, 300.0);
        assert_eq!(turns[0].cache_read_tokens, 600.0);
    }

    /// 验证线程导出缓存：写入后能从本地文件读回，不需要 CLI 调用。
    #[test]
    fn amp_thread_export_cache_roundtrip() {
        let dir = amp_cache_dir();
        let test_id = format!("test-export-roundtrip-{}", std::process::id());
        let cache_path = dir.join(format!("{test_id}.json"));

        // 清理可能存在的残留文件
        let _ = std::fs::remove_file(&cache_path);

        let value = json!({
            "id": test_id,
            "created": 1_786_942_400_000i64,
            "messages": [{
                "role": "assistant",
                "usage": {
                    "model": "test-model",
                    "timestamp": "2026-08-17T04:55:12.502Z",
                    "inputTokens": 100,
                    "outputTokens": 50,
                    "cacheCreationInputTokens": 0,
                    "cacheReadInputTokens": 0,
                    "totalInputTokens": 150
                }
            }]
        });

        // 写入缓存文件
        if let Some(parent) = cache_path.parent() {
            let _ = std::fs::create_dir_all(parent);
        }
        let file = File::create(&cache_path).unwrap();
        serde_json::to_writer(BufWriter::new(file), &value).unwrap();

        // updated=0 表示线程从未更新，缓存 mtime 一定 >= 0，应命中缓存
        let loaded = load_amp_thread_export(&test_id, 0);
        assert!(loaded.is_some());
        let loaded_val = loaded.unwrap();
        assert_eq!(
            loaded_val.get("id").and_then(|v| v.as_str()),
            Some(test_id.as_str())
        );

        // 清理
        let _ = std::fs::remove_file(&cache_path);
    }

    /// 验证线程列表缓存文件能正确序列化/反序列化。
    #[test]
    fn amp_list_cache_serde_roundtrip() {
        let cache = AmpListCache {
            cached_at: 1_000_000,
            threads: vec![
                AmpListEntry {
                    id: "T-1".into(),
                    updated: 1_700_000_000,
                },
                AmpListEntry {
                    id: "T-2".into(),
                    updated: 1_700_000_100,
                },
            ],
        };
        let json_str = serde_json::to_string(&cache).unwrap();
        let decoded: AmpListCache = serde_json::from_str(&json_str).unwrap();
        assert_eq!(decoded.cached_at, 1_000_000);
        assert_eq!(decoded.threads.len(), 2);
        assert_eq!(decoded.threads[0].id, "T-1");
        assert_eq!(decoded.threads[1].updated, 1_700_000_100);
    }

    /// 验证缓存路径在 amp-threads 目录的父目录。
    #[test]
    fn amp_list_cache_path_is_sibling_of_threads_dir() {
        let dir = amp_cache_dir();
        let list_path = amp_list_cache_path();
        assert!(list_path.ends_with("amp-list.json"));
        assert_ne!(list_path, dir);
        // amp-list.json 应该和 amp-threads/ 在同一父目录
        assert_eq!(list_path.parent(), dir.parent());
    }
}
