use rusqlite::{Connection, OpenFlags};
use serde_json::Value;
use std::fs::File;
use std::io::{BufReader, BufWriter, Write};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use crate::local_sources;

const CACHE_TTL_SECS: i64 = 300; // 5 minutes cache TTL

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

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum AgentKind {
    Devin,
    Amp,
    Claude,
    Codex,
}

impl AgentKind {
    pub const ALL: [Self; 4] = [Self::Devin, Self::Amp, Self::Claude, Self::Codex];

    pub fn label(self) -> &'static str {
        match self {
            Self::Devin => "Devin",
            Self::Amp => "Amp",
            Self::Claude => "Claude Code",
            Self::Codex => "Codex",
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
    cached_at: i64,
    turns_start: i64,
    turns_end: i64,
    sessions: Vec<SessionRec>,
    turns: Vec<TurnRec>,
    errors: Vec<DataError>,
}

#[derive(serde::Serialize)]
struct CacheFileRef<'a> {
    cached_at: i64,
    turns_start: i64,
    turns_end: i64,
    sessions: &'a [SessionRec],
    turns: &'a [TurnRec],
    errors: &'a [DataError],
}

fn now_timestamp() -> Option<i64> {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .map(|duration| duration.as_secs() as i64)
}

fn cache_covers(
    cached_at: i64,
    cache_start: i64,
    cache_end: i64,
    start: i64,
    end: i64,
    now: i64,
) -> bool {
    // `end` is intentionally one hour in the future. A cache produced a few
    // minutes ago cannot cover that moving future boundary byte-for-byte, but
    // it does cover all rows that can exist as of now.
    now.saturating_sub(cached_at) <= CACHE_TTL_SECS
        && cache_start <= start
        && cache_end >= end.min(now)
}

fn load_from_cache(start: i64, end: i64) -> Option<LoadedData> {
    let file = File::open(cache_path()).ok()?;
    let cached: CacheFile = serde_json::from_reader(BufReader::new(file)).ok()?;
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
    })
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
        cached_at,
        turns_start: data.turns_start,
        turns_end: data.turns_end,
        sessions: &data.sessions,
        turns: &data.turns,
        errors: &data.errors,
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

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
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
    pub agent_messages: f64,
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

#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct TurnRec {
    pub agent: AgentKind,
    pub session_key: String,
    pub created_at: i64,
    pub input_tokens: f64,
    pub output_tokens: f64,
    pub cache_read_tokens: f64,
    pub ttft_ms: f64,
    pub total_time_ms: f64,
}

#[derive(Debug, Default, serde::Serialize, serde::Deserialize)]
pub struct LoadedData {
    pub sessions: Vec<SessionRec>,
    pub turns: Vec<TurnRec>,
    pub turns_start: i64,
    pub turns_end: i64,
    pub errors: Vec<DataError>,
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
    Connection::open_with_flags(path, OpenFlags::SQLITE_OPEN_READ_ONLY)
        .map_err(|e| format!("{}: {}", path.display(), e))
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
            agent_messages: msgs,
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

    let metrics_prefix = "$.metadata.metrics.";
    let sql = format!(
        "SELECT session_id, created_at, \
         json_extract(chat_message, '{metrics_prefix}ttft_ms'), \
         json_extract(chat_message, '{metrics_prefix}input_tokens'), \
         json_extract(chat_message, '{metrics_prefix}output_tokens'), \
         json_extract(chat_message, '{metrics_prefix}cache_read_tokens'), \
         json_extract(chat_message, '{metrics_prefix}total_time_ms') \
         FROM message_nodes \
         WHERE session_id IN ({}) AND created_at >= ?{} AND created_at < ?{} \
         AND json_extract(chat_message, '{metrics_prefix}ttft_ms') IS NOT NULL",
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
            r.get::<_, Option<f64>>(2)?,
            r.get::<_, Option<f64>>(3)?,
            r.get::<_, Option<f64>>(4)?,
            r.get::<_, Option<f64>>(5)?,
            r.get::<_, Option<f64>>(6)?,
        ))
    });

    let rows = match rows {
        Ok(r) => r,
        Err(e) => {
            eprintln!("turns query failed for {source}: {e}");
            return;
        }
    };

    for row in rows.flatten() {
        let (session_id, created_at, ttft, input, output, cached, total) = row;
        let ttft = ttft.unwrap_or(0.0);
        if ttft <= 0.0 {
            continue;
        }
        out.push(TurnRec {
            agent: AgentKind::Devin,
            session_key: format!("{source}/{session_id}"),
            created_at,
            input_tokens: input.unwrap_or(0.0),
            output_tokens: output.unwrap_or(0.0),
            cache_read_tokens: cached.unwrap_or(0.0),
            ttft_ms: ttft,
            total_time_ms: total.unwrap_or(0.0),
        });
    }
}

fn load_source(source: String, path: PathBuf, start: i64, end: i64) -> LoadedData {
    let mut data = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };
    if !path.exists() {
        data.errors.push(DataError {
            agent: AgentKind::Devin,
            message: format!("{} 不存在", path.display()),
        });
        return data;
    }
    let conn = match open_readonly(&path) {
        Ok(connection) => connection,
        Err(error) => {
            data.errors.push(DataError {
                agent: AgentKind::Devin,
                message: format!("打开失败 {error}"),
            });
            return data;
        }
    };
    load_sessions_from(&conn, &source, &mut data.sessions);
    let ids: Vec<String> = data
        .sessions
        .iter()
        .filter(|session| session.last_activity_at >= start && session.created_at < end)
        .map(|session| session.id.clone())
        .collect();
    load_turns_from(&conn, &source, &ids, start, end, &mut data.turns);
    data
}

fn load_uncached(start: i64, end: i64) -> LoadedData {
    let sources = devin_db_paths();
    let parts = std::thread::scope(|scope| {
        let mut handles: Vec<_> = sources
            .into_iter()
            .map(|(source, path)| scope.spawn(move || load_source(source, path, start, end)))
            .collect();
        handles.push(scope.spawn(move || local_sources::load_amp(start, end)));
        handles.push(scope.spawn(move || local_sources::load_claude(start, end)));
        handles.push(scope.spawn(move || local_sources::load_codex(start, end)));
        handles
            .into_iter()
            .filter_map(|handle| handle.join().ok())
            .collect::<Vec<_>>()
    });

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
    save_to_cache(&data);
    data
}

/// Load everything, preferring a recent on-disk snapshot for fast startup.
pub fn load_all(start: i64, end: i64) -> LoadedData {
    load_from_cache(start, end).unwrap_or_else(|| load_uncached(start, end))
}

/// Reload directly from Devin's databases, bypassing the startup cache.
pub fn reload_all(start: i64, end: i64) -> LoadedData {
    load_uncached(start, end)
}

#[cfg(test)]
mod tests {
    use super::cache_covers;

    #[test]
    fn json_extract_available() {
        use rusqlite::Connection;
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
}
