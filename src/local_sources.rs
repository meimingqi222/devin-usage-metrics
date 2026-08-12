use crate::data::{AgentKind, DataError, LoadedData, SessionRec, TurnRec};
use chrono::DateTime;
use serde_json::Value;
use std::collections::{HashMap, HashSet};
use std::fs::File;
use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::time::UNIX_EPOCH;

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
    agent_messages: f64,
    seen_messages: HashSet<String>,
    seen_usage: HashSet<String>,
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

pub(crate) fn load_amp(start: i64, end: i64) -> LoadedData {
    let agent = AgentKind::Amp;
    let root = home_path(&[".local", "share", "amp", "threads"]);
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

    let files = recent_files(&[root], "json");
    let mut skipped = 0usize;
    for path in files {
        let Ok(file) = File::open(&path) else {
            skipped += 1;
            continue;
        };
        let Ok(value) = serde_json::from_reader::<_, Value>(BufReader::new(file)) else {
            skipped += 1;
            continue;
        };
        let id = value
            .get("id")
            .and_then(Value::as_str)
            .map(str::to_owned)
            .or_else(|| {
                path.file_stem()
                    .and_then(|stem| stem.to_str())
                    .map(str::to_owned)
            })
            .unwrap_or_default();
        if id.is_empty() {
            continue;
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
                let input = value_number(usage.get("inputTokens"))
                    + value_number(usage.get("cacheCreationInputTokens"));
                let output = value_number(usage.get("outputTokens"));
                let cached = value_number(usage.get("cacheReadInputTokens"));
                session.input_tokens += input;
                session.output_tokens += output;
                session.cached_tokens += cached;
                session.agent_messages += 1.0;
                if let Some(model) = usage.get("model").and_then(Value::as_str) {
                    session.model = model.to_owned();
                }
                if at >= start && at < end {
                    data.turns.push(TurnRec {
                        agent,
                        session_key: key.clone(),
                        created_at: at,
                        input_tokens: input,
                        output_tokens: output,
                        cache_read_tokens: cached,
                        ttft_ms: 0.0,
                        total_time_ms: 0.0,
                    });
                }
            }
        }
        data.sessions.push(session.finish(agent));
    }
    if skipped > 0 {
        data.errors
            .push(error(agent, format!("有 {skipped} 个会话文件无法读取")));
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

pub(crate) fn load_claude(start: i64, end: i64) -> LoadedData {
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
    let mut sessions: HashMap<String, SessionBuilder> = HashMap::new();
    let mut skipped = 0usize;
    for path in recent_files(&[root], "jsonl") {
        let fallback_id = claude_session_id(&path);
        if fallback_id.is_empty() {
            continue;
        }
        let Ok(file) = File::open(&path) else {
            skipped += 1;
            continue;
        };
        for line in BufReader::new(file).lines().map_while(Result::ok) {
            let Ok(value) = serde_json::from_str::<Value>(&line) else {
                continue;
            };
            let id = value
                .get("sessionId")
                .or_else(|| value.get("session_id"))
                .and_then(Value::as_str)
                .unwrap_or(&fallback_id)
                .to_owned();
            let session = sessions.entry(id.clone()).or_default();
            session.id = id.clone();
            let at = timestamp(value.get("timestamp"));
            session.observe_time(at);
            if let Some(cwd) = value
                .get("cwd")
                .and_then(Value::as_str)
                .filter(|cwd| !cwd.is_empty())
            {
                session.cwd = cwd.to_owned();
            }
            if let Some(entrypoint) = value.get("entrypoint").and_then(Value::as_str) {
                session.source = entrypoint.to_owned();
            }
            if let Some(effort) = value.get("effort").and_then(Value::as_str) {
                session.mode = effort.to_owned();
            }
            match value.get("type").and_then(Value::as_str).unwrap_or("") {
                "ai-title" => {
                    if let Some(title) = value.get("aiTitle").and_then(Value::as_str) {
                        session.title = compact_title(title);
                    }
                }
                "user" if session.title.is_empty() => {
                    session.title =
                        content_text(value.pointer("/message/content").unwrap_or(&Value::Null));
                }
                "assistant" => {
                    let Some(usage) = value.pointer("/message/usage") else {
                        continue;
                    };
                    let message_id = value
                        .pointer("/message/id")
                        .and_then(Value::as_str)
                        .or_else(|| value.get("requestId").and_then(Value::as_str))
                        .unwrap_or("");
                    if !message_id.is_empty()
                        && !session.seen_messages.insert(message_id.to_owned())
                    {
                        continue;
                    }
                    let input = value_number(usage.get("input_tokens"))
                        + value_number(usage.get("cache_creation_input_tokens"));
                    let output = value_number(usage.get("output_tokens"));
                    let cached = value_number(usage.get("cache_read_input_tokens"));
                    session.input_tokens += input;
                    session.output_tokens += output;
                    session.cached_tokens += cached;
                    session.agent_messages += 1.0;
                    if let Some(model) = value.pointer("/message/model").and_then(Value::as_str) {
                        session.model = model.to_owned();
                    }
                    if at >= start && at < end {
                        data.turns.push(TurnRec {
                            agent,
                            session_key: format!("claude-code/{id}"),
                            created_at: at,
                            input_tokens: input,
                            output_tokens: output,
                            cache_read_tokens: cached,
                            ttft_ms: 0.0,
                            total_time_ms: 0.0,
                        });
                    }
                }
                _ => {}
            }
        }
    }
    data.sessions = sessions
        .into_values()
        .map(|session| session.finish(agent))
        .collect();
    if skipped > 0 {
        data.errors
            .push(error(agent, format!("有 {skipped} 个会话文件无法读取")));
    }
    data
}

pub(crate) fn load_codex(start: i64, end: i64) -> LoadedData {
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
    let mut sessions: HashMap<String, SessionBuilder> = HashMap::new();
    let mut skipped = 0usize;
    for path in recent_files(&roots, "jsonl") {
        let fallback_id = path
            .file_stem()
            .and_then(|stem| stem.to_str())
            .and_then(|name| name.rsplit('-').next())
            .unwrap_or("")
            .to_owned();
        let Ok(file) = File::open(&path) else {
            skipped += 1;
            continue;
        };
        let mut active_id = fallback_id;
        let mut active_model = String::new();
        for line in BufReader::new(file).lines().map_while(Result::ok) {
            let Ok(value) = serde_json::from_str::<Value>(&line) else {
                continue;
            };
            let outer = value.get("type").and_then(Value::as_str).unwrap_or("");
            if outer == "session_meta" {
                if let Some(id) = value
                    .pointer("/payload/id")
                    .or_else(|| value.pointer("/payload/session_id"))
                    .and_then(Value::as_str)
                {
                    active_id = id.to_owned();
                }
            }
            if active_id.is_empty() {
                continue;
            }
            let session = sessions.entry(active_id.clone()).or_default();
            session.id = active_id.clone();
            let at = timestamp(value.get("timestamp"));
            session.observe_time(at);
            match outer {
                "session_meta" => {
                    if let Some(cwd) = value.pointer("/payload/cwd").and_then(Value::as_str) {
                        session.cwd = cwd.to_owned();
                    }
                    if let Some(source) = value.pointer("/payload/source").and_then(Value::as_str) {
                        session.source = source.to_owned();
                    }
                }
                "turn_context" => {
                    if let Some(model) = value.pointer("/payload/model").and_then(Value::as_str) {
                        active_model = model.to_owned();
                        session.model = active_model.clone();
                    }
                    if let Some(cwd) = value.pointer("/payload/cwd").and_then(Value::as_str) {
                        session.cwd = cwd.to_owned();
                    }
                    session.mode = value
                        .pointer("/payload/collaboration_mode/kind")
                        .or_else(|| value.pointer("/payload/collaboration_mode"))
                        .and_then(Value::as_str)
                        .unwrap_or("")
                        .to_owned();
                }
                "event_msg" => match value
                    .pointer("/payload/type")
                    .and_then(Value::as_str)
                    .unwrap_or("")
                {
                    "user_message" if session.title.is_empty() => {
                        session.title = value
                            .pointer("/payload/message")
                            .and_then(Value::as_str)
                            .map(compact_title)
                            .unwrap_or_default();
                    }
                    "token_count" => {
                        let Some(usage) = value.pointer("/payload/info/last_token_usage") else {
                            continue;
                        };
                        let total = value
                            .pointer("/payload/info/total_token_usage")
                            .unwrap_or(usage);
                        let signature = format!(
                            "{}:{}:{}",
                            value_number(total.get("input_tokens")),
                            value_number(total.get("cached_input_tokens")),
                            value_number(total.get("output_tokens"))
                        );
                        if !session.seen_usage.insert(signature) {
                            continue;
                        }
                        let raw_input = value_number(usage.get("input_tokens"));
                        let cached = value_number(usage.get("cached_input_tokens"));
                        let input = (raw_input - cached).max(0.0);
                        let output = value_number(usage.get("output_tokens"));
                        session.input_tokens += input;
                        session.output_tokens += output;
                        session.cached_tokens += cached;
                        session.agent_messages += 1.0;
                        if !active_model.is_empty() {
                            session.model = active_model.clone();
                        }
                        if at >= start && at < end {
                            data.turns.push(TurnRec {
                                agent,
                                session_key: format!("codex/{}", active_id),
                                created_at: at,
                                input_tokens: input,
                                output_tokens: output,
                                cache_read_tokens: cached,
                                ttft_ms: 0.0,
                                total_time_ms: 0.0,
                            });
                        }
                    }
                    _ => {}
                },
                _ => {}
            }
        }
    }
    data.sessions = sessions
        .into_values()
        .map(|session| session.finish(agent))
        .collect();
    if skipped > 0 {
        data.errors
            .push(error(agent, format!("有 {skipped} 个会话文件无法读取")));
    }
    data
}

#[cfg(test)]
mod tests {
    use super::{content_text, timestamp};
    use serde_json::json;

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
            content_text(&json!([{"type":"text","text":"  hello\n world  "}])),
            "hello world"
        );
    }
}
