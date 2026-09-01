//! v3 增量同步协议。
//!
//! v2 的分片按“agent + 月”聚合：当天新增一条 turn 就会重写整月对象。v3 把
//! turn 固定到“设备 + agent + UTC 日”的不可变小 block；历史日期的 block 因此
//! 可以按内容哈希复用。session metadata 另存为小快照，head 仍是最后一步 CAS。

use super::*;
use crate::data::AgentKind;
use fs2::FileExt;
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, HashSet};
use std::fs::{self, File, OpenOptions};
use std::io::{Cursor, Read};

const PROTOCOL: u32 = 3;
const DATA_SCHEMA: u32 = 1;
const TARGET_BLOCK_BYTES: usize = 384 * 1024;
const MAX_BLOCK_BYTES: usize = 512 * 1024;
const MAX_BLOCK_RAW_BYTES: u64 = 8 * 1024 * 1024;
const MAX_BLOCKS: usize = 16_384;
const MAX_MANIFEST_BYTES: u64 = 8 * 1024 * 1024;
const RETAIN_DAYS: i64 = 366;

#[derive(Clone, Debug, Serialize, Deserialize)]
struct Head {
    protocol: u32,
    sequence: u64,
    manifest: String,
    previous_manifest: Option<String>,
    updated_at: i64,
    device_id: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct Manifest {
    protocol: u32,
    data_schema: u32,
    device_id: String,
    device_name: String,
    created_at: i64,
    parent: Option<String>,
    retained_since: i64,
    turns_start: i64,
    turns_end: i64,
    blocks: Vec<BlockRef>,
    sessions: Vec<SessionRef>,
    session_count: usize,
    turn_count: usize,
    source_hash: String,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct BlockRef {
    file: String,
    hash: String,
    agent: AgentKind,
    utc_day: String,
    part: u16,
    compressed_size: u64,
    uncompressed_size: u64,
    turn_count: usize,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
struct SessionRef {
    file: String,
    hash: String,
    #[serde(default)]
    utc_day: String,
    compressed_size: u64,
    uncompressed_size: u64,
    session_count: usize,
}

#[derive(Serialize, Deserialize)]
struct TurnBlock {
    protocol: u32,
    data_schema: u32,
    turns: Vec<TurnRec>,
}

#[derive(Serialize, Deserialize)]
struct SessionBlock {
    protocol: u32,
    data_schema: u32,
    sessions: Vec<SessionRec>,
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64
}

fn day(timestamp: i64) -> String {
    chrono::DateTime::from_timestamp(timestamp, 0)
        .map(|v| v.format("%Y-%m-%d").to_string())
        .unwrap_or_else(|| "unknown".into())
}

fn day_start(timestamp: i64) -> i64 {
    timestamp.div_euclid(86_400) * 86_400
}

fn valid_hash(value: &str) -> bool {
    value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn stable_turn_id(turn: &TurnRec) -> String {
    // 与 v2 使用相同的稳定 id，保证导入合并的去重语义不变。
    v2::stable_turn_id(turn)
}

fn stable_session_id(session: &SessionRec) -> String {
    sha256_hex(
        format!(
            "session-v1\0{}\0{:?}\0{}",
            session.device_id, session.agent, session.key
        )
        .as_bytes(),
    )
}

fn encode_turns(turns: &[TurnRec]) -> Result<(Vec<u8>, u64), String> {
    let raw = serde_json::to_vec(&TurnBlock {
        protocol: PROTOCOL,
        data_schema: DATA_SCHEMA,
        turns: turns.to_vec(),
    })
    .map_err(|e| format!("序列化 v3 turn block 失败: {e}"))?;
    let bytes = zstd::stream::encode_all(Cursor::new(&raw), 3)
        .map_err(|e| format!("压缩 v3 turn block 失败: {e}"))?;
    Ok((bytes, raw.len() as u64))
}

fn encode_sessions(sessions: &[SessionRec]) -> Result<(Vec<u8>, u64), String> {
    let raw = serde_json::to_vec(&SessionBlock {
        protocol: PROTOCOL,
        data_schema: DATA_SCHEMA,
        sessions: sessions.to_vec(),
    })
    .map_err(|e| format!("序列化 v3 session snapshot 失败: {e}"))?;
    let bytes = zstd::stream::encode_all(Cursor::new(&raw), 3)
        .map_err(|e| format!("压缩 v3 session snapshot 失败: {e}"))?;
    Ok((bytes, raw.len() as u64))
}

fn split_turns(
    turns: Vec<TurnRec>,
    output: &mut Vec<(Vec<TurnRec>, Vec<u8>, u64)>,
) -> Result<(), String> {
    let (bytes, raw) = encode_turns(&turns)?;
    if bytes.len() <= TARGET_BLOCK_BYTES || (bytes.len() <= MAX_BLOCK_BYTES && turns.len() == 1) {
        return if raw <= MAX_BLOCK_RAW_BYTES {
            output.push((turns, bytes, raw));
            Ok(())
        } else {
            Err("单个 v3 turn block 解压后超过 8MiB".into())
        };
    }
    if turns.len() < 2 {
        return Err("单条 turn 压缩后超过 v3 block 上限".into());
    }
    let middle = turns.len() / 2;
    split_turns(turns[..middle].to_vec(), output)?;
    split_turns(turns[middle..].to_vec(), output)
}

fn split_sessions(
    sessions: Vec<SessionRec>,
    output: &mut Vec<(Vec<SessionRec>, Vec<u8>, u64)>,
) -> Result<(), String> {
    let (bytes, raw) = encode_sessions(&sessions)?;
    if bytes.len() <= TARGET_BLOCK_BYTES || (bytes.len() <= MAX_BLOCK_BYTES && sessions.len() == 1)
    {
        return if raw <= MAX_BLOCK_RAW_BYTES {
            output.push((sessions, bytes, raw));
            Ok(())
        } else {
            Err("单个 v3 session snapshot 解压后超过 8MiB".into())
        };
    }
    if sessions.len() < 2 {
        return Err("单条 session metadata 压缩后超过 v3 block 上限".into());
    }
    let middle = sessions.len() / 2;
    split_sessions(sessions[..middle].to_vec(), output)?;
    split_sessions(sessions[middle..].to_vec(), output)
}

fn make_blocks(turns: Vec<TurnRec>) -> Result<Vec<(BlockRef, Vec<u8>)>, String> {
    let mut grouped: BTreeMap<(AgentKind, String), Vec<TurnRec>> = BTreeMap::new();
    for turn in turns {
        grouped
            .entry((turn.agent, day(turn.created_at)))
            .or_default()
            .push(turn);
    }
    let mut output = Vec::new();
    for ((agent, utc_day), mut values) in grouped {
        values.sort_by_key(stable_turn_id);
        let mut pieces = Vec::new();
        split_turns(values, &mut pieces)?;
        for (part, (turns, bytes, raw)) in pieces.into_iter().enumerate() {
            let hash = sha256_hex(&bytes);
            output.push((
                BlockRef {
                    file: format!("v3-block-{hash}.zst"),
                    hash,
                    agent,
                    utc_day: utc_day.clone(),
                    part: part as u16,
                    compressed_size: bytes.len() as u64,
                    uncompressed_size: raw,
                    turn_count: turns.len(),
                },
                bytes,
            ));
        }
    }
    output.sort_by(|a, b| a.0.file.cmp(&b.0.file));
    Ok(output)
}

fn make_session_snapshots(sessions: Vec<SessionRec>) -> Result<Vec<(SessionRef, Vec<u8>)>, String> {
    // Session 元数据通常很小；按 agent/day 分段，保持每个快照可独立复用。
    let mut grouped: BTreeMap<(AgentKind, String), Vec<SessionRec>> = BTreeMap::new();
    for session in sessions {
        grouped
            // 老会话在最近窗口仍有 turn 时也必须保留；优先按最后活动日归档，
            // 防止其 created_at 早于保留期而被服务端 GC。
            .entry((
                session.agent,
                day(session.last_activity_at.max(session.created_at)),
            ))
            .or_default()
            .push(session);
    }
    let mut output = Vec::new();
    for ((_, utc_day), mut values) in grouped {
        values.sort_by_key(stable_session_id);
        let mut pieces = Vec::new();
        split_sessions(values, &mut pieces)?;
        for (sessions, bytes, raw) in pieces {
            let hash = sha256_hex(&bytes);
            output.push((
                SessionRef {
                    file: format!("v3-session-{hash}.zst"),
                    hash,
                    utc_day: utc_day.clone(),
                    compressed_size: bytes.len() as u64,
                    uncompressed_size: raw,
                    session_count: sessions.len(),
                },
                bytes,
            ));
        }
    }
    output.sort_by(|a, b| a.0.file.cmp(&b.0.file));
    Ok(output)
}

fn decode_limited<T: for<'a> Deserialize<'a>>(
    bytes: &[u8],
    compressed: u64,
    raw_limit: u64,
    name: &str,
) -> Result<T, String> {
    if bytes.len() as u64 != compressed {
        return Err(format!("{name} 压缩大小不匹配"));
    }
    let decoder = zstd::stream::read::Decoder::new(Cursor::new(bytes))
        .map_err(|e| format!("解压 {name} 失败: {e}"))?;
    let mut limited = decoder.take(raw_limit + 1);
    let value =
        serde_json::from_reader(&mut limited).map_err(|e| format!("解析 {name} 失败: {e}"))?;
    if limited.limit() != 1 {
        return Err(format!("{name} 解压大小不匹配"));
    }
    Ok(value)
}

fn cache_dir() -> PathBuf {
    dirs::cache_dir()
        .unwrap_or_else(std::env::temp_dir)
        .join("devin-usage-metrics/sync-v3-objects")
}

fn read_object(
    transport: &dyn SyncTransport,
    name: &str,
    hash: &str,
    compressed_size: u64,
    _raw_limit: u64,
) -> Result<Vec<u8>, String> {
    fs::create_dir_all(cache_dir()).map_err(|e| e.to_string())?;
    let path = cache_dir().join(format!("{hash}.zst"));
    let bytes = match fs::read(&path) {
        Ok(value) if value.len() as u64 == compressed_size && sha256_hex(&value) == hash => value,
        _ => {
            let value = transport.get_limited(name, compressed_size + 1)?;
            if value.len() as u64 != compressed_size || sha256_hex(&value) != hash {
                return Err(format!("{name} 哈希或大小校验失败"));
            }
            atomic_replace(&path, &value)?;
            value
        }
    };
    Ok(bytes)
}

fn read_manifest(transport: &dyn SyncTransport, hash: &str) -> Result<Manifest, String> {
    if !valid_hash(hash) {
        return Err("v3 manifest 哈希格式无效".into());
    }
    let name = format!("v3-manifest-{hash}.json");
    let bytes = transport.get_limited(&name, MAX_MANIFEST_BYTES)?;
    if sha256_hex(&bytes) != hash {
        return Err("v3 manifest 哈希不匹配".into());
    }
    let manifest: Manifest =
        serde_json::from_slice(&bytes).map_err(|e| format!("解析 v3 manifest 失败: {e}"))?;
    if manifest.protocol != PROTOCOL
        || manifest.data_schema != DATA_SCHEMA
        || manifest.blocks.len() > MAX_BLOCKS
    {
        return Err("不支持或无效的 v3 manifest".into());
    }
    for block in &manifest.blocks {
        if !valid_hash(&block.hash)
            || block.file != format!("v3-block-{}.zst", block.hash)
            || block.compressed_size > MAX_BLOCK_BYTES as u64
            || block.uncompressed_size > MAX_BLOCK_RAW_BYTES
        {
            return Err(format!("v3 manifest 包含无效 block: {}", block.file));
        }
    }
    for session in &manifest.sessions {
        if !valid_hash(&session.hash)
            || session.file != format!("v3-session-{}.zst", session.hash)
            || session.compressed_size > MAX_BLOCK_BYTES as u64
            || session.uncompressed_size > MAX_BLOCK_RAW_BYTES
        {
            return Err(format!(
                "v3 manifest 包含无效 session snapshot: {}",
                session.file
            ));
        }
    }
    Ok(manifest)
}

/// 每天只抽检一个可轮换 block（session snapshot 也算），避免 generation 更新时
/// 全量下载审计；内容哈希失败会在 UI 中作为同步错误暴露出来。
fn audit_local_device(transport: &dyn SyncTransport, manifest: &Manifest) -> Result<(), String> {
    let day_number = now().div_euclid(86_400);
    let state_key = sha256_hex(
        format!(
            "{}\0{}",
            destination_key(&read_config()),
            manifest.device_id
        )
        .as_bytes(),
    );
    let path = dirs::config_dir()
        .unwrap_or_else(std::env::temp_dir)
        .join(format!("devin-usage-metrics/sync-v3-audit-{state_key}"));
    if fs::read_to_string(&path)
        .ok()
        .and_then(|value| value.trim().parse::<i64>().ok())
        == Some(day_number)
    {
        return Ok(());
    }
    let mut objects: Vec<(&str, &str, u64)> = manifest
        .sessions
        .iter()
        .map(|value| {
            (
                value.file.as_str(),
                value.hash.as_str(),
                value.compressed_size,
            )
        })
        .collect();
    objects.extend(manifest.blocks.iter().map(|value| {
        (
            value.file.as_str(),
            value.hash.as_str(),
            value.compressed_size,
        )
    }));
    if let Some((name, hash, size)) =
        objects.get(day_number.rem_euclid(objects.len().max(1) as i64) as usize)
    {
        let bytes = transport.get_limited(name, *size + 1)?;
        if bytes.len() as u64 != *size || sha256_hex(&bytes) != *hash {
            return Err(format!("v3 每日审计失败: {name} 哈希或大小不匹配"));
        }
    }
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    }
    atomic_replace(&path, day_number.to_string().as_bytes())?;
    Ok(())
}

fn local_package(mut data: LoadedData, id: &str) -> DevicePackage {
    let retained_since = now() - RETAIN_DAYS * 86_400;
    let retained_session_keys: std::collections::HashSet<_> = data
        .turns
        .iter()
        .filter(|value| {
            (value.device_id.is_empty() || value.device_id == id)
                && value.created_at >= retained_since
        })
        .map(|value| value.session_key.clone())
        .collect();
    data.sessions.retain(|value| {
        (value.device_id.is_empty() || value.device_id == id)
            && (value.last_activity_at.max(value.created_at) >= retained_since
                || retained_session_keys.contains(&value.key))
    });
    data.turns.retain(|value| {
        (value.device_id.is_empty() || value.device_id == id) && value.created_at >= retained_since
    });
    for session in &mut data.sessions {
        if session.device_id.is_empty() {
            session.device_id = id.into();
        }
    }
    for turn in &mut data.turns {
        if turn.device_id.is_empty() {
            turn.device_id = id.into();
        }
    }
    DevicePackage {
        schema_version: 1,
        device_id: id.to_string(),
        device_name: data::device_name(),
        exported_at: now(),
        turns_start: data.turns_start.max(retained_since),
        turns_end: data.turns_end,
        sessions: data.sessions,
        turns: data.turns,
    }
}

fn source_hash(package: &DevicePackage) -> String {
    let mut sessions = package.sessions.clone();
    let mut turns = package.turns.clone();
    sessions.sort_by_key(stable_session_id);
    turns.sort_by_key(stable_turn_id);
    sha256_hex(
        &serde_json::to_vec(&(
            &package.device_id,
            &package.device_name,
            package.turns_start,
            package.turns_end,
            sessions,
            turns,
        ))
        .expect("canonical v3 source hash"),
    )
}

fn writer_lock() -> Result<File, String> {
    let path = dirs::config_dir()
        .unwrap_or_else(std::env::temp_dir)
        .join("devin-usage-metrics/sync-v3-writer.lock");
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    }
    let file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(path)
        .map_err(|e| e.to_string())?;
    file.lock_exclusive()
        .map_err(|e| format!("获取 v3 writer lock 失败: {e}"))?;
    Ok(file)
}

fn strong_revision(revision: Option<&str>) -> Result<(), String> {
    match revision {
        Some(value) if !value.trim_start().starts_with("W/") => Ok(()),
        _ => Err("服务端已有 v3 head，但未提供用于 If-Match 的强 ETag".into()),
    }
}

pub(super) fn export_local_v3(data: LoadedData) -> Result<(), String> {
    let started = Instant::now();
    let id = data::device_id();
    let _lock = writer_lock()?;
    let transport = current_transport()?;
    let package = local_package(data, &id);
    let hash = source_hash(&package);
    let head_name = format!("v3-head-{id}.json");

    for retry in 0..3 {
        let (old_head, revision) = match transport.read_head(&head_name)? {
            Some((bytes, revision)) => {
                strong_revision(revision.as_deref())?;
                let head: Head = serde_json::from_slice(&bytes)
                    .map_err(|e| format!("解析 v3 head 失败: {e}"))?;
                if head.protocol != PROTOCOL || head.device_id != id || !valid_hash(&head.manifest)
                {
                    return Err("v3 head 协议、设备或 manifest 无效".into());
                }
                (Some(head), revision)
            }
            None => (None, None),
        };
        let old_manifest = if let Some(head) = &old_head {
            match read_manifest(transport.as_ref(), &head.manifest) {
                Ok(manifest) => {
                    if manifest.source_hash == hash {
                        data::log_event(format!("sync v3 export no-op blocks=0 uploaded_bytes=0 sessions={} turns={} elapsed_ms={}", package.sessions.len(), package.turns.len(), started.elapsed().as_millis()));
                        return Ok(());
                    }
                    Some(manifest)
                }
                Err(error) => {
                    data::log_event(format!(
                        "sync v3 current manifest unreadable; publishing a repair: {error}"
                    ));
                    None
                }
            }
        } else {
            None
        };
        if old_head.is_none() && package.sessions.is_empty() && package.turns.is_empty() {
            data::log_event("sync v3 export skipped empty initial manifest");
            return Ok(());
        }
        let blocks = make_blocks(package.turns.clone())?;
        let sessions = make_session_snapshots(package.sessions.clone())?;
        let old_objects: HashSet<&str> = old_manifest
            .as_ref()
            .into_iter()
            .flat_map(|manifest| {
                manifest
                    .blocks
                    .iter()
                    .map(|value| value.file.as_str())
                    .chain(manifest.sessions.iter().map(|value| value.file.as_str()))
            })
            .collect();
        let mut uploaded = 0usize;
        let mut reused = 0usize;
        let mut bytes = 0usize;
        for (reference, object) in blocks
            .iter()
            .map(|(r, b)| (&r.file, b))
            .chain(sessions.iter().map(|(r, b)| (&r.file, b)))
        {
            if old_objects.contains(reference.as_str()) {
                // 当前 head 已引用这些不可变对象；本次只上传相对旧 manifest 的
                // 新 hash，避免一条新 turn 触发全年 block 的 412 请求风暴。
                reused += 1;
            } else if transport.put_immutable(reference, object)? {
                uploaded += 1;
                bytes += object.len();
            } else {
                reused += 1;
            }
        }
        let manifest = Manifest {
            protocol: PROTOCOL,
            data_schema: DATA_SCHEMA,
            device_id: id.clone(),
            device_name: package.device_name.clone(),
            created_at: now(),
            parent: old_head.as_ref().map(|head| head.manifest.clone()),
            retained_since: now() - RETAIN_DAYS * 86_400,
            turns_start: package.turns_start,
            turns_end: package.turns_end,
            blocks: blocks.iter().map(|(value, _)| value.clone()).collect(),
            sessions: sessions.iter().map(|(value, _)| value.clone()).collect(),
            session_count: package.sessions.len(),
            turn_count: package.turns.len(),
            source_hash: hash.clone(),
        };
        let manifest_bytes = serde_json::to_vec(&manifest).map_err(|e| e.to_string())?;
        let manifest_hash = sha256_hex(&manifest_bytes);
        let manifest_name = format!("v3-manifest-{manifest_hash}.json");
        let _ = transport.put_immutable(&manifest_name, &manifest_bytes)?;
        let head = Head {
            protocol: PROTOCOL,
            sequence: old_head.as_ref().map_or(1, |value| value.sequence + 1),
            manifest: manifest_hash,
            previous_manifest: old_head.as_ref().map(|value| value.manifest.clone()),
            updated_at: now(),
            device_id: id.clone(),
        };
        match transport.cas_head(
            &head_name,
            &serde_json::to_vec(&head).unwrap(),
            revision.as_deref(),
        ) {
            Ok(()) => {
                data::log_event(format!("sync v3 export ok blocks={} uploaded={} reused={} uploaded_bytes={} sessions={} turns={} elapsed_ms={}", manifest.blocks.len() + manifest.sessions.len(), uploaded, reused, bytes, package.sessions.len(), package.turns.len(), started.elapsed().as_millis()));
                return Ok(());
            }
            Err(error) if error == "CAS_CONFLICT" && retry < 2 => continue,
            Err(error) if error == "CAS_CONFLICT" => {
                return Err("v3 head CAS 冲突，重试后仍无法安全发布".into())
            }
            Err(error) => return Err(error),
        }
    }
    Err("v3 head CAS 重试耗尽".into())
}

fn load_remote(
    transport: &dyn SyncTransport,
    manifest: &Manifest,
) -> Result<DevicePackage, String> {
    let retained_day = day(now() - RETAIN_DAYS * 86_400);
    let mut sessions = Vec::new();
    let mut turns = Vec::new();
    let mut expected_sessions = 0usize;
    let mut expected_turns = 0usize;
    for reference in &manifest.sessions {
        if !reference.utc_day.is_empty() && reference.utc_day < retained_day {
            continue;
        }
        expected_sessions += reference.session_count;
        let bytes = read_object(
            transport,
            &reference.file,
            &reference.hash,
            reference.compressed_size,
            reference.uncompressed_size,
        )?;
        let block: SessionBlock = decode_limited(
            &bytes,
            reference.compressed_size,
            reference.uncompressed_size,
            &reference.file,
        )?;
        if block.protocol != PROTOCOL
            || block.data_schema != DATA_SCHEMA
            || block.sessions.len() != reference.session_count
        {
            return Err(format!("{} 协议或记录数无效", reference.file));
        }
        sessions.extend(block.sessions);
    }
    for reference in &manifest.blocks {
        if reference.utc_day < retained_day {
            continue;
        }
        expected_turns += reference.turn_count;
        let bytes = read_object(
            transport,
            &reference.file,
            &reference.hash,
            reference.compressed_size,
            reference.uncompressed_size,
        )?;
        let block: TurnBlock = decode_limited(
            &bytes,
            reference.compressed_size,
            reference.uncompressed_size,
            &reference.file,
        )?;
        if block.protocol != PROTOCOL
            || block.data_schema != DATA_SCHEMA
            || block.turns.len() != reference.turn_count
        {
            return Err(format!("{} 协议或记录数无效", reference.file));
        }
        turns.extend(block.turns);
    }
    if sessions.len() != expected_sessions || turns.len() != expected_turns {
        return Err("v3 manifest 总记录数不匹配".into());
    }
    Ok(DevicePackage {
        schema_version: 1,
        device_id: manifest.device_id.clone(),
        device_name: manifest.device_name.clone(),
        exported_at: manifest.created_at,
        turns_start: manifest.turns_start,
        turns_end: manifest.turns_end,
        sessions,
        turns,
    })
}

pub(super) fn import_remote_v3() -> (LoadedData, Vec<RemoteDevice>, Option<String>) {
    let started = Instant::now();
    let local_id = data::device_id();
    let transport = match current_transport() {
        Ok(value) => value,
        Err(error) => return (LoadedData::default(), Vec::new(), Some(error)),
    };
    let names = match transport.list_sync_files() {
        Ok(value) => value,
        Err(error) => return (LoadedData::default(), Vec::new(), Some(error)),
    };
    let heads: Vec<_> = names
        .into_iter()
        .filter(|name| name.starts_with("v3-head-") && name.ends_with(".json"))
        .collect();
    // 双读迁移：按设备选协议。任何一台设备升级都不能让其他 v2 设备的
    // 数据从 UI 消失。
    if heads.is_empty() {
        return v2::import_remote_v2();
    }
    let mut merged = LoadedData::default();
    let mut devices = Vec::new();
    let mut first_error = None;
    // 只有成功读取完整 v3 包的设备才会屏蔽 v2。若 v3 head/manifest/block 损坏，
    // 必须仍可回退该设备的 v2 数据，不能因“看见一个 v3 head”而直接丢数据。
    let mut v3_devices = HashSet::new();
    for name in heads {
        let id = name
            .trim_start_matches("v3-head-")
            .trim_end_matches(".json")
            .to_string();
        let result = transport
            .read_head(&name)
            .and_then(|value| value.ok_or_else(|| "v3 head 已消失".into()))
            .and_then(|(bytes, _)| {
                let head: Head = serde_json::from_slice(&bytes)
                    .map_err(|e| format!("解析 v3 head 失败: {e}"))?;
                if head.protocol != PROTOCOL || head.device_id != id || !valid_hash(&head.manifest)
                {
                    return Err("v3 head 无效".into());
                }
                let manifest = read_manifest(transport.as_ref(), &head.manifest)?;
                if manifest.device_id != id {
                    return Err("v3 head 与 manifest 设备不一致".into());
                }
                Ok(manifest)
            });
        match result {
            Ok(manifest) => {
                devices.push(RemoteDevice {
                    device_id: manifest.device_id.clone(),
                    device_name: manifest.device_name.clone(),
                    exported_at: manifest.created_at,
                    session_count: manifest.session_count,
                    is_local: manifest.device_id == local_id,
                });
                if manifest.device_id != local_id {
                    match load_remote(transport.as_ref(), &manifest) {
                        Ok(package) => {
                            v3_devices.insert(manifest.device_id.clone());
                            merged.sessions.extend(package.sessions);
                            merged.turns.extend(package.turns);
                            merged.turns_start = if merged.turns_start == 0 {
                                package.turns_start
                            } else {
                                merged.turns_start.min(package.turns_start)
                            };
                            merged.turns_end = merged.turns_end.max(package.turns_end);
                        }
                        Err(error) if first_error.is_none() => {
                            first_error = Some(format!("读取设备 {id} 的 v3 block 失败: {error}"))
                        }
                        Err(_) => {}
                    }
                } else {
                    v3_devices.insert(manifest.device_id.clone());
                    if let Err(error) = audit_local_device(transport.as_ref(), &manifest) {
                        data::log_event(format!("sync v3 daily audit device={id} failed: {error}"));
                        if first_error.is_none() {
                            first_error = Some(format!("本机 v3 每日审计失败: {error}"));
                        }
                    }
                }
            }
            Err(error) if first_error.is_none() => {
                first_error = Some(format!("读取设备 {id} 的 v3 manifest 失败: {error}"))
            }
            Err(_) => {}
        }
    }
    let (legacy, legacy_devices, legacy_error) = v2::import_remote_v2_excluding(&v3_devices);
    merged.sessions.extend(legacy.sessions);
    merged.turns.extend(legacy.turns);
    if legacy.turns_start != 0 {
        merged.turns_start = if merged.turns_start == 0 {
            legacy.turns_start
        } else {
            merged.turns_start.min(legacy.turns_start)
        };
    }
    merged.turns_end = merged.turns_end.max(legacy.turns_end);
    devices.extend(legacy_devices);
    if first_error.is_none() {
        first_error = legacy_error;
    }
    merged.turns.sort_by_key(|value| value.created_at);
    data::log_event(format!(
        "sync v3 import {} devices={} sessions={} turns={} elapsed_ms={}",
        if first_error.is_some() {
            "partial"
        } else {
            "ok"
        },
        devices.len(),
        merged.sessions.len(),
        merged.turns.len(),
        started.elapsed().as_millis()
    ));
    (merged, devices, first_error)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn turn(day: i64, input: f64) -> TurnRec {
        TurnRec {
            device_id: "device-a".into(),
            session_key: "session".into(),
            created_at: day,
            input_tokens: input,
            agent: AgentKind::Devin,
            ..Default::default()
        }
    }

    #[test]
    fn historical_day_block_is_stable_when_today_changes() {
        let yesterday = day_start(now() - 86_400);
        let today = day_start(now());
        let first = make_blocks(vec![turn(yesterday, 1.0), turn(today, 2.0)]).unwrap();
        let second = make_blocks(vec![
            turn(yesterday, 1.0),
            turn(today, 2.0),
            turn(today, 3.0),
        ])
        .unwrap();
        let old = first
            .iter()
            .find(|(block, _)| block.utc_day == day(yesterday))
            .unwrap()
            .0
            .hash
            .clone();
        let unchanged = second
            .iter()
            .find(|(block, _)| block.utc_day == day(yesterday))
            .unwrap()
            .0
            .hash
            .clone();
        assert_eq!(old, unchanged);
    }

    #[test]
    fn blocks_are_bounded_and_parseable() {
        let today = day_start(now());
        let values = (0..200)
            .map(|value| TurnRec {
                device_id: "device".into(),
                session_key: format!("s-{value}"),
                created_at: today,
                model: "x".repeat(8_000),
                agent: AgentKind::Codex,
                ..Default::default()
            })
            .collect();
        let blocks = make_blocks(values).unwrap();
        assert!(blocks
            .iter()
            .all(|(reference, _)| reference.compressed_size <= MAX_BLOCK_BYTES as u64));
        for (reference, bytes) in blocks {
            let decoded: TurnBlock = decode_limited(
                &bytes,
                reference.compressed_size,
                reference.uncompressed_size,
                &reference.file,
            )
            .unwrap();
            assert_eq!(decoded.turns.len(), reference.turn_count);
        }
    }

    #[test]
    fn session_snapshots_are_split_and_keep_their_day() {
        let today = day_start(now());
        let sessions = (0..180)
            .map(|value| {
                // 使用高熵可打印文本，确保压缩后仍超过单 snapshot 上限；简单的
                // 周期字符串会被 zstd 压得过小，无法真正覆盖分块逻辑。
                let mut state = value as u64 + 1;
                let title: String = (0..8_000)
                    .map(|_| {
                        state = state
                            .wrapping_mul(6_364_136_223_846_793_005)
                            .wrapping_add(1);
                        ((state >> 32) % 94 + 33) as u8 as char
                    })
                    .collect();
                SessionRec {
                    agent: AgentKind::Devin,
                    key: format!("session-{value}"),
                    title,
                    created_at: today,
                    last_activity_at: today,
                    device_id: "device".into(),
                    ..Default::default()
                }
            })
            .collect();
        let snapshots = make_session_snapshots(sessions).unwrap();
        assert!(snapshots.len() > 1);
        for (reference, bytes) in snapshots {
            assert!(reference.compressed_size <= MAX_BLOCK_BYTES as u64);
            assert_eq!(reference.utc_day, day(today));
            let decoded: SessionBlock = decode_limited(
                &bytes,
                reference.compressed_size,
                reference.uncompressed_size,
                &reference.file,
            )
            .unwrap();
            assert_eq!(decoded.sessions.len(), reference.session_count);
        }
    }
}
