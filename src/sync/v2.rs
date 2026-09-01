use super::*;
use crate::data::AgentKind;
use serde::{Deserialize, Serialize};
#[cfg(test)]
use std::collections::BTreeMap;
use std::collections::HashSet;
use std::fs::{self};
use std::io::{Cursor, Read};

const PROTOCOL: u32 = 2;
const DATA_SCHEMA: u32 = 1;
const SHARD_LIMIT: usize = 8 * 1024 * 1024;
const SHARD_RAW_LIMIT: u64 = 64 * 1024 * 1024;
const MANIFEST_LIMIT: usize = 8 * 1024 * 1024;
const MAX_SHARDS: usize = 4096;

// v1 files and unreachable generations/objects are intentionally retained.
// A future GC must use a grace period and trace every current/previous head.

#[derive(Debug, Clone, Serialize, Deserialize)]
struct Head {
    protocol: u32,
    sequence: u64,
    generation: String,
    previous_generation: Option<String>,
    updated_at: i64,
    device_id: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct Generation {
    protocol: u32,
    data_schema: u32,
    device_id: String,
    device_name: String,
    created_at: i64,
    parent: Option<String>,
    turns_start: i64,
    turns_end: i64,
    shards: Vec<ShardRef>,
    session_count: usize,
    turn_count: usize,
    content_hash: String,
    /// Hash of the local snapshot before older exported history is merged in.
    /// Older v2 manifests omit it and take the one-time slow migration path.
    #[serde(default)]
    source_hash: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct ShardRef {
    file: String,
    hash: String,
    agent: AgentKind,
    utc_month: String,
    bucket: String,
    compressed_size: u64,
    uncompressed_size: u64,
    session_count: usize,
    turn_count: usize,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct Chunk {
    protocol: u32,
    data_schema: u32,
    records: Vec<Record>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum Record {
    Session {
        record_id: String,
        value: SessionRec,
    },
    Turn {
        record_id: String,
        value: TurnRec,
    },
}

impl Record {
    fn id(&self) -> &str {
        match self {
            Self::Session { record_id, .. } | Self::Turn { record_id, .. } => record_id,
        }
    }
    fn agent(&self) -> AgentKind {
        match self {
            Self::Session { value, .. } => value.agent,
            Self::Turn { value, .. } => value.agent,
        }
    }
    fn timestamp(&self) -> i64 {
        match self {
            Self::Session { value, .. } => value.created_at,
            Self::Turn { value, .. } => value.created_at,
        }
    }
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64
}
fn month(ts: i64) -> String {
    chrono::DateTime::from_timestamp(ts, 0)
        .map(|v| v.format("%Y-%m").to_string())
        .unwrap_or_else(|| "unknown".into())
}
fn agent_slug(agent: AgentKind) -> String {
    match agent {
        AgentKind::Devin => "devin",
        AgentKind::Amp => "amp",
        AgentKind::Claude => "claude_code",
        AgentKind::Codex => "codex",
        AgentKind::Antigravity => "antigravity",
        AgentKind::Grok => "grok_build",
        AgentKind::ZCode => "zcode",
        AgentKind::OpenCode => "opencode",
        AgentKind::Pi => "pi_agent",
    }
    .into()
}
fn session_id(s: &SessionRec) -> String {
    sha256_hex(
        format!(
            "session-v1\0{}\0{}\0{}",
            s.device_id,
            agent_slug(s.agent),
            s.key
        )
        .as_bytes(),
    )
}
pub(super) fn stable_turn_id(t: &TurnRec) -> String {
    #[derive(Serialize)]
    struct Canon<'a> {
        version: u8,
        device: &'a str,
        agent: AgentKind,
        session: &'a str,
        created_at: i64,
        input: u64,
        output: u64,
        cache_read: u64,
        cache_create: u64,
        cache_5m: u64,
        cache_1h: u64,
        model: &'a str,
        ttft: u64,
        total_time: u64,
        recorded_cost: Option<u64>,
    }
    let bytes = serde_json::to_vec(&Canon {
        version: 1,
        device: &t.device_id,
        agent: t.agent,
        session: &t.session_key,
        created_at: t.created_at,
        input: t.input_tokens.to_bits(),
        output: t.output_tokens.to_bits(),
        cache_read: t.cache_read_tokens.to_bits(),
        cache_create: t.cache_creation_tokens.to_bits(),
        cache_5m: t.cache_creation_5m_tokens.to_bits(),
        cache_1h: t.cache_creation_1h_tokens.to_bits(),
        model: &t.model,
        ttft: t.ttft_ms.to_bits(),
        total_time: t.total_time_ms.to_bits(),
        recorded_cost: t.recorded_cost.map(f64::to_bits),
    })
    .expect("canonical turn");
    sha256_hex(&bytes)
}

#[cfg(test)]
fn records(pkg: &DevicePackage) -> Vec<Record> {
    let mut out = Vec::with_capacity(pkg.sessions.len() + pkg.turns.len());
    out.extend(pkg.sessions.iter().cloned().map(|value| Record::Session {
        record_id: session_id(&value),
        value,
    }));
    out.extend(pkg.turns.iter().cloned().map(|value| Record::Turn {
        record_id: stable_turn_id(&value),
        value,
    }));
    out
}

fn package_from_records(g: &Generation, records: Vec<Record>) -> DevicePackage {
    let mut pkg = DevicePackage {
        schema_version: 1,
        device_id: g.device_id.clone(),
        device_name: g.device_name.clone(),
        exported_at: g.created_at,
        turns_start: g.turns_start,
        turns_end: g.turns_end,
        sessions: Vec::new(),
        turns: Vec::new(),
    };
    for record in records {
        match record {
            Record::Session { value, .. } => pkg.sessions.push(value),
            Record::Turn { value, .. } => pkg.turns.push(value),
        }
    }
    pkg
}

#[cfg(test)]
fn encode(records: Vec<Record>) -> Result<(Vec<u8>, u64), String> {
    let json = serde_json::to_vec(&Chunk {
        protocol: PROTOCOL,
        data_schema: DATA_SCHEMA,
        records,
    })
    .map_err(|e| e.to_string())?;
    let compressed = zstd::stream::encode_all(Cursor::new(&json), 3)
        .map_err(|e| format!("zstd 压缩失败: {e}"))?;
    Ok((compressed, json.len() as u64))
}

#[cfg(test)]
fn split_group(
    records: Vec<Record>,
    prefix: String,
    out: &mut Vec<(String, Vec<Record>, Vec<u8>, u64)>,
) -> Result<(), String> {
    let mut records = records;
    records.sort_by(|a, b| a.id().cmp(b.id()));
    let (bytes, raw) = encode(records.clone())?;
    if bytes.len() <= SHARD_LIMIT && raw <= SHARD_RAW_LIMIT {
        out.push((prefix, records, bytes, raw));
        return Ok(());
    }
    let depth = prefix.len();
    if depth >= 64 {
        return Err("单条同步记录压缩后超过 8MiB，无法安全分片".into());
    }
    let mut groups: BTreeMap<char, Vec<Record>> = BTreeMap::new();
    for r in records {
        groups
            .entry(r.id().as_bytes()[depth] as char)
            .or_default()
            .push(r);
    }
    if groups.len() == 1 {
        let (ch, group) = groups.into_iter().next().unwrap();
        return split_group(group, format!("{prefix}{ch}"), out);
    }
    for (ch, group) in groups {
        split_group(group, format!("{prefix}{ch}"), out)?;
    }
    Ok(())
}

#[cfg(test)]
fn make_shards(records: Vec<Record>) -> Result<Vec<(ShardRef, Vec<u8>)>, String> {
    let mut groups: BTreeMap<(String, String), Vec<Record>> = BTreeMap::new();
    for record in records {
        groups
            .entry((agent_slug(record.agent()), month(record.timestamp())))
            .or_default()
            .push(record);
    }
    let mut out = Vec::new();
    for ((_, utc_month), group) in groups {
        let agent = group[0].agent();
        let mut split = Vec::new();
        split_group(group, String::new(), &mut split)?;
        for (bucket, records, bytes, raw) in split {
            let hash = sha256_hex(&bytes);
            let sessions = records
                .iter()
                .filter(|r| matches!(r, Record::Session { .. }))
                .count();
            let turns = records.len() - sessions;
            out.push((
                ShardRef {
                    file: format!("v2-object-{hash}.json.zst"),
                    hash,
                    agent,
                    utc_month: utc_month.clone(),
                    bucket,
                    compressed_size: bytes.len() as u64,
                    uncompressed_size: raw,
                    session_count: sessions,
                    turn_count: turns,
                },
                bytes,
            ));
        }
    }
    out.sort_by(|a, b| a.0.file.cmp(&b.0.file));
    Ok(out)
}

fn read_generation(t: &dyn SyncTransport, hash: &str) -> Result<Generation, String> {
    if !valid_hash(hash) {
        return Err("generation hash 格式无效".into());
    }
    let name = format!("v2-generation-{hash}.json");
    let bytes = t.get_limited(&name, MANIFEST_LIMIT as u64)?;
    if bytes.len() > MANIFEST_LIMIT || sha256_hex(&bytes) != hash {
        return Err(format!("generation {hash} 哈希或大小校验失败"));
    }
    let g: Generation =
        serde_json::from_slice(&bytes).map_err(|e| format!("解析 {name} 失败: {e}"))?;
    if g.protocol != PROTOCOL || g.data_schema != DATA_SCHEMA {
        return Err(format!("不支持的 generation 协议: {}", g.protocol));
    }
    if !valid_hash(&g.content_hash) || (!g.source_hash.is_empty() && !valid_hash(&g.source_hash)) {
        return Err("generation 内容哈希无效".into());
    }
    if g.shards.len() > MAX_SHARDS {
        return Err(format!("generation 分片数量超过上限: {}", g.shards.len()));
    }
    let mut session_count = 0usize;
    let mut turn_count = 0usize;
    for shard in &g.shards {
        if !valid_hash(&shard.hash)
            || shard.file != format!("v2-object-{}.json.zst", shard.hash)
            || shard.compressed_size > SHARD_LIMIT as u64
            || shard.uncompressed_size > SHARD_RAW_LIMIT
        {
            return Err(format!("generation 包含无效分片: {}", shard.file));
        }
        session_count = session_count
            .checked_add(shard.session_count)
            .ok_or_else(|| "generation session 计数溢出".to_string())?;
        turn_count = turn_count
            .checked_add(shard.turn_count)
            .ok_or_else(|| "generation turn 计数溢出".to_string())?;
    }
    if session_count != g.session_count || turn_count != g.turn_count {
        return Err("generation 与分片记录计数不匹配".into());
    }
    Ok(g)
}

fn valid_hash(value: &str) -> bool {
    value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn cache_dir() -> PathBuf {
    dirs::cache_dir()
        .unwrap_or_else(std::env::temp_dir)
        .join("devin-usage-metrics/sync-v2-objects")
}
fn read_shard(
    t: &dyn SyncTransport,
    shard: &ShardRef,
    cache_dir: &std::path::Path,
) -> Result<Vec<Record>, String> {
    fs::create_dir_all(cache_dir).map_err(|e| e.to_string())?;
    let cached = cache_dir.join(format!("{}.zst", shard.hash));
    let bytes = match fs::read(&cached) {
        Ok(v) if v.len() as u64 == shard.compressed_size && sha256_hex(&v) == shard.hash => v,
        _ => {
            let v = t.get_limited(&shard.file, SHARD_LIMIT as u64 + 1)?;
            if v.len() as u64 != shard.compressed_size || sha256_hex(&v) != shard.hash {
                return Err(format!("shard {} 压缩大小或哈希校验失败", shard.file));
            }
            atomic_replace(&cached, &v)?;
            v
        }
    };
    let decoder = zstd::stream::read::Decoder::new(Cursor::new(bytes))
        .map_err(|e| format!("zstd 解压失败: {e}"))?;
    let mut limited = decoder.take(shard.uncompressed_size + 1);
    let chunk: Chunk = serde_json::from_reader(&mut limited)
        .map_err(|e| format!("解析 shard {} 失败: {e}", shard.file))?;
    if limited.limit() != 1 {
        return Err(format!("shard {} 解压大小不匹配", shard.file));
    }
    if chunk.protocol != PROTOCOL || chunk.data_schema != DATA_SCHEMA {
        return Err("shard schema 不兼容".into());
    }
    for record in &chunk.records {
        let expected_id = match record {
            Record::Session { value, .. } => session_id(value),
            Record::Turn { value, .. } => stable_turn_id(value),
        };
        if record.id() != expected_id
            || record.agent() != shard.agent
            || month(record.timestamp()) != shard.utc_month
            || !record.id().starts_with(&shard.bucket)
        {
            return Err(format!("shard {} 记录不属于声明分区", shard.file));
        }
    }
    let sessions = chunk
        .records
        .iter()
        .filter(|record| matches!(record, Record::Session { .. }))
        .count();
    if sessions != shard.session_count || chunk.records.len() - sessions != shard.turn_count {
        return Err(format!("shard {} 记录计数不匹配", shard.file));
    }
    Ok(chunk.records)
}

struct LoadedDevice {
    package: DevicePackage,
    #[cfg(test)]
    fell_back: bool,
    #[cfg(test)]
    generation: String,
}

enum ImportedDevice {
    /// 本机设备:元数据 + head 指向的真实 manifest SHA-256(作审计键)。
    Local(Generation, String),
    Remote(DevicePackage),
}

fn load_device(
    t: &dyn SyncTransport,
    head: &Head,
    cache_dir: &std::path::Path,
) -> Result<LoadedDevice, String> {
    let attempt = |hash: &str| -> Result<DevicePackage, String> {
        let g = read_generation(t, hash)?;
        if g.device_id != head.device_id {
            return Err("head 与 generation 设备不一致".into());
        }
        let mut all = Vec::with_capacity(g.session_count + g.turn_count);
        for shard in &g.shards {
            all.extend(read_shard(t, shard, cache_dir)?);
        }
        let pkg = package_from_records(&g, all);
        if pkg.sessions.len() != g.session_count || pkg.turns.len() != g.turn_count {
            return Err("generation 记录计数不匹配".into());
        }
        Ok(pkg)
    };
    match attempt(&head.generation) {
        Ok(package) => Ok(LoadedDevice {
            package,
            #[cfg(test)]
            fell_back: false,
            #[cfg(test)]
            generation: head.generation.clone(),
        }),
        Err(primary) => {
            if let Some(previous) = &head.previous_generation {
                data::log_event(format!(
                    "sync v2 generation={} failed={primary}; fallback={previous}",
                    head.generation
                ));
                attempt(previous)
                    .map(|package| LoadedDevice {
                        package,
                        #[cfg(test)]
                        fell_back: true,
                        #[cfg(test)]
                        generation: previous.clone(),
                    })
                    .map_err(|fallback| {
                        format!("当前 generation 失败: {primary}; previous 也失败: {fallback}")
                    })
            } else {
                Err(primary)
            }
        }
    }
}

/// 返回 (generation, 实际使用的 manifest SHA-256, 是否回退到 previous)。
/// manifest 哈希直接来自 head(或 previous_generation),是远端对象的真实文件名,
/// 不是反序列化后再序列化的重算值——旧 manifest 缺默认字段或 JSON 编码
/// 形式不同时,重算值会与真实对象名不一致,不能用作审计键。
fn read_device_generation(
    t: &dyn SyncTransport,
    head: &Head,
) -> Result<(Generation, String, bool), String> {
    let attempt = |hash: &str| {
        let generation = read_generation(t, hash)?;
        if generation.device_id != head.device_id {
            return Err("head 与 generation 设备不一致".into());
        }
        Ok(generation)
    };
    match attempt(&head.generation) {
        Ok(generation) => Ok((generation, head.generation.clone(), false)),
        Err(primary) => match &head.previous_generation {
            Some(previous) => attempt(previous)
                .map(|generation| (generation, previous.clone(), true))
                .map_err(|fallback| {
                    format!("当前 generation 失败: {primary}; previous 也失败: {fallback}")
                }),
            None => Err(primary),
        },
    }
}

/// 仅做远端存在性元数据检查,不解压不验哈希。
/// Ok(true)=全部在, Ok(false)=至少一个缺失, Err=传输层错误。
/// 用一次 list_sync_files(WebDAV 下是单次 PROPFIND)建立文件名集合,
/// 避免对每个 shard 串行发 HEAD——分片多时慢 WebDAV 会累积成分钟级延迟。
#[cfg(test)]
fn remote_shards_present(t: &dyn SyncTransport, generation: &Generation) -> Result<bool, String> {
    let files: HashSet<String> = t.list_sync_files()?.into_iter().collect();
    for shard in &generation.shards {
        if !files.contains(&shard.file) {
            data::log_event(format!("sync v2 remote shard missing: {}", shard.file));
            return Ok(false);
        }
    }
    Ok(true)
}

/// 深度审计间隔(秒)。本机设备的 shard 在本地内容寻址缓存命中时不会重新下载,
/// 无法发现"远端对象被删/被改",所以定期绕过缓存强制重新下载验哈希。
const AUDIT_INTERVAL_SECS: i64 = 24 * 3600;

fn audit_state_path() -> Option<PathBuf> {
    audit_state_path_for(&destination_key(&read_config()), &data::device_id())
}

/// 审计状态按 destination + device 分文件：切换同步后端或设备 ID 时不复用
/// 旧状态，避免换后端后沿用旧审计时间戳而跳过校验。
fn audit_state_path_for(destination: &str, device_id: &str) -> Option<PathBuf> {
    let key = sha256_hex(format!("{destination}\n{device_id}").as_bytes());
    dirs::config_dir().map(|d| d.join(format!("devin-usage-metrics/sync-v2-audit-{key}")))
}

fn read_last_audit() -> Option<(String, i64)> {
    let text = fs::read_to_string(audit_state_path()?).ok()?;
    let mut lines = text.lines();
    let generation = lines.next()?.trim().to_owned();
    let at: i64 = lines.next()?.trim().parse().ok()?;
    if generation.is_empty() {
        return None;
    }
    Some((generation, at))
}

fn write_last_audit(generation: &str, at: i64) {
    let Some(path) = audit_state_path() else {
        return;
    };
    // import 路径不持 writer lock,多进程并发审计时裸 fs::write 可能撕裂状态文件;
    // 用原子替换避免,读取侧 read_last_audit 解析失败也只会多跑一次审计。
    let _ = atomic_replace(&path, format!("{generation}\n{at}\n").as_bytes());
}

/// 对本机当前 generation 做一次绕过本地缓存的远端完整性审计:
/// 每个 shard 重新从远端下载,校验压缩大小与 SHA-256,且把通过校验的内容
/// 写回缓存,顺带修复"缓存完好但远端已被破坏"的反向漂移。
/// 远端对象损坏或缺失时,若本地缓存里有哈希正确的副本,用无条件 PUT 覆盖
/// 修复(内容寻址对象的文件名绑定 SHA-256,用匹配哈希的字节覆盖是安全的),
/// 并重新 GET 验证;没有可靠本地副本才报错。
/// 周期由 AUDIT_INTERVAL_SECS 控制, generation 变化时强制重跑。
/// 状态读写与缓存目录注入以便测试不触碰真实用户目录。
fn audit_local_device(
    t: &dyn SyncTransport,
    generation: &Generation,
    generation_hash: &str,
    cache_dir: &std::path::Path,
) -> Result<(), String> {
    audit_local_device_with(
        t,
        generation,
        generation_hash,
        cache_dir,
        read_last_audit,
        write_last_audit,
    )
}

fn audit_local_device_with(
    t: &dyn SyncTransport,
    generation: &Generation,
    generation_hash: &str,
    cache_dir: &std::path::Path,
    read_state: impl Fn() -> Option<(String, i64)>,
    write_state: impl Fn(&str, i64),
) -> Result<(), String> {
    let now_ts = now();
    let due = match read_state() {
        // at > now_ts:状态时间在未来(时钟回拨或状态文件异常),视为无效立即重跑,
        // 否则会一直跳过到系统时间追上它。
        Some((g, at)) => {
            g != generation_hash || at > now_ts || now_ts.saturating_sub(at) >= AUDIT_INTERVAL_SECS
        }
        None => true,
    };
    if !due {
        return Ok(());
    }
    data::log_event(format!(
        "sync v2 audit start shards={} generation={}",
        generation.shards.len(),
        generation.content_hash
    ));
    fs::create_dir_all(cache_dir).map_err(|e| e.to_string())?;
    let mut repaired = 0usize;
    for shard in &generation.shards {
        let cached = cache_dir.join(format!("{}.zst", shard.hash));
        let valid = |bytes: &[u8]| {
            bytes.len() as u64 == shard.compressed_size && sha256_hex(bytes) == shard.hash
        };
        let remote = t.get_limited(&shard.file, SHARD_LIMIT as u64 + 1);
        match remote {
            Ok(bytes) if valid(&bytes) => {
                atomic_replace(&cached, &bytes)?;
            }
            remote => {
                // 远端损坏/缺失/传输错误:尝试用本地缓存里的正确副本覆盖修复。
                let local = fs::read(&cached).ok().filter(|bytes| valid(bytes));
                let Some(local) = local else {
                    let detail = match remote {
                        Ok(_) => "内容哈希或大小不匹配".to_string(),
                        Err(e) => e,
                    };
                    return Err(format!(
                        "审计发现 shard {} 远端损坏且本地无可用副本: {detail}",
                        shard.file
                    ));
                };
                t.put(&shard.file, &local)
                    .map_err(|e| format!("审计修复 shard {} 上传失败: {e}", shard.file))?;
                let verified = t
                    .get_limited(&shard.file, SHARD_LIMIT as u64 + 1)
                    .map_err(|e| format!("审计修复 shard {} 回读失败: {e}", shard.file))?;
                if !valid(&verified) {
                    return Err(format!("审计修复 shard {} 后回读校验仍失败", shard.file));
                }
                repaired += 1;
                data::log_event(format!(
                    "sync v2 audit repaired remote shard from local cache: {}",
                    shard.file
                ));
            }
        }
    }
    write_state(generation_hash, now_ts);
    data::log_event(format!(
        "sync v2 audit ok shards={} repaired={repaired} generation={}",
        generation.shards.len(),
        generation.content_hash
    ));
    Ok(())
}

#[cfg(test)]
fn local_package(mut data: LoadedData, id: &str) -> DevicePackage {
    data.sessions
        .retain(|s| s.device_id.is_empty() || s.device_id == id);
    data.turns
        .retain(|s| s.device_id.is_empty() || s.device_id == id);
    for s in &mut data.sessions {
        if s.device_id.is_empty() {
            s.device_id = id.into();
        }
    }
    for t in &mut data.turns {
        if t.device_id.is_empty() {
            t.device_id = id.into();
        }
    }
    DevicePackage {
        schema_version: 1,
        device_id: id.into(),
        device_name: data::device_name(),
        exported_at: now(),
        turns_start: data.turns_start,
        turns_end: data.turns_end,
        sessions: data.sessions,
        turns: data.turns,
    }
}

#[cfg(test)]
fn content_hash(pkg: &DevicePackage, values: &mut [Record]) -> String {
    values.sort_by(|a, b| a.id().cmp(b.id()));
    sha256_hex(
        &serde_json::to_vec(&(
            &pkg.device_id,
            &pkg.device_name,
            pkg.turns_start,
            pkg.turns_end,
            &values,
        ))
        .unwrap(),
    )
}

#[cfg(test)]
fn writer_lock() -> Result<std::fs::File, String> {
    let path = dirs::config_dir()
        .unwrap_or_else(std::env::temp_dir)
        .join("devin-usage-metrics/sync-v2-writer.lock");
    if let Some(p) = path.parent() {
        fs::create_dir_all(p).map_err(|e| e.to_string())?;
    }
    let file = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(path)
        .map_err(|e| e.to_string())?;
    fs2::FileExt::lock_exclusive(&file).map_err(|e| format!("获取同步 writer lock 失败: {e}"))?;
    Ok(file)
}

/// 校验 CAS 所需的强 ETag。已有 head 时:无 ETag 或 weak ETag(W/ 前缀,RFC 7232
/// 强比较永不匹配)都拒绝,不降级为不安全并发发布。
#[cfg(test)]
fn ensure_strong_revision(revision: Option<&str>) -> Result<(), String> {
    match revision {
        Some(etag) if !etag.trim_start().starts_with("W/") => Ok(()),
        _ => Err(
            "WebDAV 服务端已有 head，但未提供可用于 If-Match 的强 ETag，不支持安全并发发布".into(),
        ),
    }
}

#[cfg(test)]
#[allow(dead_code)]
pub(super) fn export_local_v2(data: LoadedData) -> Result<(), String> {
    let started = Instant::now();
    let id = data::device_id();
    let _lock = writer_lock()?;
    let transport = current_transport()?;
    let head_name = format!("v2-head-{id}.json");
    let base_pkg = local_package(data, &id);
    let mut source_records = records(&base_pkg);
    let source_hash = content_hash(&base_pkg, &mut source_records);
    drop(source_records);
    for retry in 0..3 {
        let head_read = transport.read_head(&head_name)?;
        let (old_head, revision) = match head_read {
            Some((bytes, revision)) => {
                ensure_strong_revision(revision.as_deref())?;
                let head: Head =
                    serde_json::from_slice(&bytes).map_err(|e| format!("解析 head 失败: {e}"))?;
                if head.protocol != PROTOCOL
                    || head.device_id != id
                    || !valid_hash(&head.generation)
                {
                    return Err("head 协议或设备 ID 无效".into());
                }
                (Some(head), revision)
            }
            None => (None, None),
        };
        let mut pkg = base_pkg.clone();
        let mut parent_generation = old_head.as_ref().map(|head| head.generation.clone());
        if let Some(head) = &old_head {
            match read_generation(transport.as_ref(), &head.generation) {
                Ok(generation) => {
                    if generation.device_id != id {
                        return Err("head 与 generation 设备不一致".into());
                    }
                    if generation.source_hash == source_hash {
                        // manifest 完好且本地数据没变,仍须确认远端分片真实存在,
                        // 否则分片被删后 no-op 会让坏 head 永远得不到修复。
                        match remote_shards_present(transport.as_ref(), &generation) {
                            Ok(true) => {
                                data::log_event(format!(
                                    "sync v2 export no-op sessions={} turns={} elapsed_ms={}",
                                    pkg.sessions.len(),
                                    pkg.turns.len(),
                                    started.elapsed().as_millis()
                                ));
                                return Ok(());
                            }
                            Ok(false) => data::log_event(
                                "sync v2 export no-op skipped: remote shard missing, rebuilding",
                            ),
                            Err(error) => data::log_event(format!(
                                "sync v2 export no-op shard check failed, rebuilding: {error}"
                            )),
                        }
                    }
                }
                Err(error) => data::log_event(format!(
                    "sync v2 current generation unreadable; rebuilding from fallback: {error}"
                )),
            }
            let loaded = load_device(transport.as_ref(), head, &cache_dir())?;
            if loaded.fell_back {
                data::log_event("sync v2 export repairing head after generation fallback");
            }
            parent_generation = Some(loaded.generation);
            union_into_package(&mut pkg, loaded.package);
        } else {
            let v1_name = package_filename(&id);
            if transport.exists(&v1_name)? {
                let bytes = transport
                    .get(&v1_name)
                    .map_err(|e| format!("读取本机 v1 迁移包失败: {e}"))?;
                let v1: DevicePackage = serde_json::from_slice(&bytes)
                    .map_err(|e| format!("解析本机 v1 迁移包失败: {e}"))?;
                if v1.schema_version != PACKAGE_SCHEMA_VERSION || v1.device_id != id {
                    return Err("本机 v1 迁移包 schema 或设备 ID 不兼容".into());
                }
                union_into_package(&mut pkg, v1);
            }
        }
        if old_head.is_none() && pkg.sessions.is_empty() && pkg.turns.is_empty() {
            data::log_event("sync v2 export skipped empty initial generation");
            return Ok(());
        }
        let mut package_records = records(&pkg);
        let hash = content_hash(&pkg, &mut package_records);
        let shards = make_shards(package_records)?;
        // 上传前确保缓存目录可写:bytes 由 make_shards 压缩产出,其 SHA-256 即
        // reference.hash,写入本地缓存后为审计修复提供经过哈希验证的副本。
        fs::create_dir_all(cache_dir()).map_err(|e| e.to_string())?;
        let mut uploaded = 0;
        let mut reused = 0;
        let mut compressed = 0;
        for (reference, bytes) in &shards {
            compressed += bytes.len();
            if transport.put_immutable(&reference.file, bytes)? {
                uploaded += 1
            } else {
                let existing = transport.get_limited(&reference.file, SHARD_LIMIT as u64 + 1)?;
                if sha256_hex(&existing) != reference.hash {
                    return Err(format!("已存在对象哈希不匹配: {}", reference.file));
                }
                reused += 1
            }
            atomic_replace(&cache_dir().join(format!("{}.zst", reference.hash)), bytes)?;
        }
        let generation = Generation {
            protocol: PROTOCOL,
            data_schema: DATA_SCHEMA,
            device_id: id.clone(),
            device_name: pkg.device_name.clone(),
            created_at: now(),
            parent: parent_generation.clone(),
            turns_start: pkg.turns_start,
            turns_end: pkg.turns_end,
            shards: shards.iter().map(|v| v.0.clone()).collect(),
            session_count: pkg.sessions.len(),
            turn_count: pkg.turns.len(),
            content_hash: hash,
            source_hash: source_hash.clone(),
        };
        let manifest = serde_json::to_vec(&generation).map_err(|e| e.to_string())?;
        let generation_hash = sha256_hex(&manifest);
        let generation_name = format!("v2-generation-{generation_hash}.json");
        transport.put_immutable(&generation_name, &manifest)?;
        for shard in &generation.shards {
            if !transport.exists(&shard.file)? {
                return Err(format!("发布前依赖缺失: {}", shard.file));
            }
        }
        if !transport.exists(&generation_name)? {
            return Err("发布前 generation 缺失".into());
        }
        let head = Head {
            protocol: PROTOCOL,
            sequence: old_head.as_ref().map_or(1, |h| h.sequence + 1),
            generation: generation_hash,
            previous_generation: parent_generation,
            updated_at: now(),
            device_id: id.clone(),
        };
        match transport.cas_head(
            &head_name,
            &serde_json::to_vec(&head).unwrap(),
            revision.as_deref(),
        ) {
            Ok(()) => {
                data::log_event(format!("sync v2 export ok shards={} uploaded={uploaded} reused={reused} compressed_bytes={compressed} sessions={} turns={} elapsed_ms={}", generation.shards.len(), pkg.sessions.len(), pkg.turns.len(), started.elapsed().as_millis()));
                return Ok(());
            }
            Err(e) if e == "CAS_CONFLICT" && retry < 2 => continue,
            Err(e) => {
                return Err(if e == "CAS_CONFLICT" {
                    "head CAS 冲突，重试后仍无法安全发布".into()
                } else {
                    e
                })
            }
        }
    }
    Err("head CAS 重试耗尽".into())
}

pub(super) fn import_remote_v2_with_files(
    transport: &dyn SyncTransport,
    files: &[String],
) -> (LoadedData, Vec<RemoteDevice>, Option<String>) {
    import_remote_v2_excluding_with_files(transport, files, &HashSet::new())
}

/// 使用调用方已取得的目录清单导入旧协议，避免 v3 迁移路径重复 LIST/PROPFIND。
pub(super) fn import_remote_v2_excluding_with_files(
    transport: &dyn SyncTransport,
    files: &[String],
    v3_devices: &HashSet<String>,
) -> (LoadedData, Vec<RemoteDevice>, Option<String>) {
    let started = Instant::now();
    let local_id = data::device_id();
    let mut merged = LoadedData::default();
    let mut devices = Vec::new();
    let mut first_error = None;
    let mut v2_devices = HashSet::new();
    for name in files
        .iter()
        .filter(|n| n.starts_with("v2-head-") && n.ends_with(".json"))
    {
        let id = name
            .trim_start_matches("v2-head-")
            .trim_end_matches(".json")
            .to_owned();
        if v3_devices.contains(&id) {
            continue;
        }
        v2_devices.insert(id.clone());
        let head_result = transport
            .read_head(name)
            .and_then(|v| v.ok_or_else(|| "head disappeared".into()))
            .and_then(|(bytes, _)| {
                serde_json::from_slice::<Head>(&bytes).map_err(|e| e.to_string())
            })
            .and_then(|head| {
                if head.protocol != PROTOCOL
                    || head.device_id != id
                    || !valid_hash(&head.generation)
                {
                    Err("head 协议、设备或 generation 无效".into())
                } else {
                    Ok(head)
                }
            });
        let result = head_result.and_then(|head| {
            if id == local_id {
                read_device_generation(transport, &head)
                    .map(|(generation, hash, _)| ImportedDevice::Local(generation, hash))
            } else {
                load_device(transport, &head, &cache_dir())
                    .map(|loaded| ImportedDevice::Remote(loaded.package))
            }
        });
        match result {
            Ok(ImportedDevice::Local(generation, generation_hash)) => {
                // 本机设备只读 manifest 拿元数据;周期性深度审计校验远端 shard 完整性,
                // 失败不阻断设备列表,但记入 first_error 让 UI 保留旧快照。
                if let Err(e) =
                    audit_local_device(transport, &generation, &generation_hash, &cache_dir())
                {
                    data::log_event(format!("sync v2 audit device={id} failed: {e}"));
                    if first_error.is_none() {
                        first_error = Some(format!("本机 generation 远端审计失败: {e}"));
                    }
                }
                devices.push(RemoteDevice {
                    device_id: generation.device_id,
                    device_name: generation.device_name,
                    exported_at: generation.created_at,
                    session_count: generation.session_count,
                    is_local: true,
                });
            }
            Ok(ImportedDevice::Remote(pkg)) => {
                devices.push(RemoteDevice {
                    device_id: pkg.device_id.clone(),
                    device_name: pkg.device_name.clone(),
                    exported_at: pkg.exported_at,
                    session_count: pkg.sessions.len(),
                    is_local: false,
                });
                merged.sessions.extend(pkg.sessions);
                merged.turns.extend(pkg.turns);
                merged.turns_start = if merged.turns_start == 0 {
                    pkg.turns_start
                } else {
                    merged.turns_start.min(pkg.turns_start)
                };
                merged.turns_end = merged.turns_end.max(pkg.turns_end);
            }
            Err(e) => {
                data::log_event(format!("sync v2 import device={id} failed: {e}"));
                if first_error.is_none() {
                    first_error = Some(format!("读取设备 {id} 的 v2 generation 失败: {e}"));
                }
            }
        }
    }
    for name in files
        .iter()
        .filter(|n| n.starts_with("device-") && n.ends_with(".json"))
    {
        let id = name.trim_start_matches("device-").trim_end_matches(".json");
        if v2_devices.contains(id) || v3_devices.contains(id) {
            continue;
        }
        match transport
            .get(name)
            .and_then(|b| serde_json::from_slice::<DevicePackage>(&b).map_err(|e| e.to_string()))
        {
            Ok(pkg) if pkg.schema_version == 1 => {
                devices.push(RemoteDevice {
                    device_id: pkg.device_id.clone(),
                    device_name: pkg.device_name.clone(),
                    exported_at: pkg.exported_at,
                    session_count: pkg.sessions.len(),
                    is_local: pkg.device_id == local_id,
                });
                if pkg.device_id != local_id {
                    merged.sessions.extend(pkg.sessions);
                    merged.turns.extend(pkg.turns);
                    merged.turns_start = if merged.turns_start == 0 {
                        pkg.turns_start
                    } else {
                        merged.turns_start.min(pkg.turns_start)
                    };
                    merged.turns_end = merged.turns_end.max(pkg.turns_end);
                }
            }
            Ok(pkg) => {
                let error = format!(
                    "读取 v1 {name} 失败: 不支持 schema_version={}",
                    pkg.schema_version
                );
                data::log_event(&error);
                if first_error.is_none() {
                    first_error = Some(error);
                }
            }
            Err(e) => {
                if first_error.is_none() {
                    first_error = Some(format!("读取 v1 {name} 失败: {e}"));
                }
            }
        }
    }
    merged.turns.sort_by_key(|t| t.created_at);
    data::log_event(format!("sync v2 import {} heads={} devices={} shards_sequential=true sessions={} turns={} elapsed_ms={}", if first_error.is_some() { "partial" } else { "ok" }, v2_devices.len(), devices.len(), merged.sessions.len(), merged.turns.len(), started.elapsed().as_millis()));
    (merged, devices, first_error)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 测试进程内唯一即可:并行测试在同一纳秒刻度内启动时
    /// pid+时间戳会撞名,导致目录互相串用/被对方清理。
    fn unique_temp_dir(prefix: &str) -> PathBuf {
        use std::sync::atomic::{AtomicU64, Ordering};
        static COUNTER: AtomicU64 = AtomicU64::new(0);
        std::env::temp_dir().join(format!(
            "{prefix}-{}-{}",
            std::process::id(),
            COUNTER.fetch_add(1, Ordering::Relaxed)
        ))
    }

    fn temp_transport() -> (PathBuf, LocalTransport) {
        let dir = unique_temp_dir("devin-usage-metrics-v2");
        (dir.clone(), LocalTransport::new(dir))
    }

    #[test]
    fn chunk_roundtrip_and_hash() {
        let r = Record::Turn {
            record_id: "a".into(),
            value: TurnRec::default(),
        };
        let (z, raw) = encode(vec![r]).unwrap();
        assert_eq!(sha256_hex(&z).len(), 64);
        let mut d = zstd::stream::read::Decoder::new(Cursor::new(z)).unwrap();
        let mut json = Vec::new();
        d.read_to_end(&mut json).unwrap();
        assert_eq!(json.len() as u64, raw);
        let c: Chunk = serde_json::from_slice(&json).unwrap();
        assert_eq!(c.records.len(), 1);
    }
    #[test]
    fn stable_turn_id_distinguishes_same_timestamp() {
        let a = TurnRec {
            device_id: "d".into(),
            session_key: "s".into(),
            created_at: 1,
            input_tokens: 1.0,
            ..Default::default()
        };
        let mut b = a.clone();
        b.input_tokens = 2.0;
        assert_ne!(stable_turn_id(&a), stable_turn_id(&b));
    }

    #[test]
    fn agent_protocol_slugs_are_stable_and_not_display_labels() {
        assert_eq!(agent_slug(AgentKind::Claude), "claude_code");
        assert_eq!(agent_slug(AgentKind::Grok), "grok_build");
        assert_eq!(agent_slug(AgentKind::Pi), "pi_agent");
    }
    #[test]
    fn recursive_split_respects_limit() {
        let mut rs = Vec::new();
        for i in 0..300 {
            let mut s = SessionRec {
                device_id: "d".into(),
                key: format!("{i}"),
                title: "x".repeat(50_000),
                ..Default::default()
            };
            s.id = format!("{i}");
            rs.push(Record::Session {
                record_id: sha256_hex(format!("{i}").as_bytes()),
                value: s,
            });
        }
        let mut out = Vec::new();
        split_group(rs, String::new(), &mut out).unwrap();
        assert!(out.iter().all(|v| v.2.len() <= SHARD_LIMIT));
    }

    #[test]
    fn local_generation_roundtrip_fails_as_a_unit_when_shard_is_missing() {
        let (dir, transport) = temp_transport();
        let pkg = DevicePackage {
            schema_version: 1,
            device_id: "device-a".into(),
            device_name: "host".into(),
            exported_at: 1,
            turns_start: 1,
            turns_end: 2,
            sessions: vec![SessionRec {
                device_id: "device-a".into(),
                key: "session".into(),
                created_at: 1,
                ..Default::default()
            }],
            turns: vec![TurnRec {
                device_id: "device-a".into(),
                session_key: "session".into(),
                created_at: 1,
                input_tokens: 2.0,
                ..Default::default()
            }],
        };
        let mut package_records = records(&pkg);
        let hash = content_hash(&pkg, &mut package_records);
        let shards = make_shards(package_records).unwrap();
        for (reference, bytes) in &shards {
            transport.put_immutable(&reference.file, bytes).unwrap();
        }
        let generation = Generation {
            protocol: PROTOCOL,
            data_schema: DATA_SCHEMA,
            device_id: pkg.device_id.clone(),
            device_name: pkg.device_name.clone(),
            created_at: 1,
            parent: None,
            turns_start: pkg.turns_start,
            turns_end: pkg.turns_end,
            shards: shards.iter().map(|entry| entry.0.clone()).collect(),
            session_count: 1,
            turn_count: 1,
            content_hash: hash.clone(),
            source_hash: hash,
        };
        let manifest = serde_json::to_vec(&generation).unwrap();
        let generation_hash = sha256_hex(&manifest);
        transport
            .put_immutable(&format!("v2-generation-{generation_hash}.json"), &manifest)
            .unwrap();
        let head = Head {
            protocol: PROTOCOL,
            sequence: 1,
            generation: generation_hash,
            previous_generation: None,
            updated_at: 1,
            device_id: pkg.device_id.clone(),
        };
        let cache = temp_cache();
        let loaded = load_device(&transport, &head, &cache).unwrap();
        assert_eq!(
            (loaded.package.sessions.len(), loaded.package.turns.len()),
            (1, 1)
        );

        let fallback_head = Head {
            generation: "0".repeat(64),
            previous_generation: Some(head.generation.clone()),
            ..head.clone()
        };
        let fallback = load_device(&transport, &fallback_head, &cache).unwrap();
        assert!(fallback.fell_back);
        assert_eq!(fallback.generation, head.generation);
        assert_eq!(fallback.package.sessions.len(), 1);

        // 删掉远端 shard 和本地缓存副本,完整加载必须失败;只读 manifest 元数据仍可
        fs::remove_file(dir.join(&generation.shards[0].file)).unwrap();
        let _ = fs::remove_file(cache.join(format!("{}.zst", generation.shards[0].hash)));
        let (metadata, manifest_hash, metadata_fell_back) =
            read_device_generation(&transport, &head).unwrap();
        assert!(!metadata_fell_back);
        assert_eq!(manifest_hash, head.generation);
        assert_eq!(metadata.session_count, 1);
        assert!(load_device(&transport, &head, &cache).is_err());
        fs::remove_dir_all(dir).unwrap();
        let _ = fs::remove_dir_all(cache);
    }

    #[test]
    fn local_head_cas_rejects_stale_revision() {
        let (dir, transport) = temp_transport();
        transport.cas_head("head.json", b"one", None).unwrap();
        let (_, revision) = transport.read_head("head.json").unwrap().unwrap();
        transport
            .cas_head("head.json", b"two", revision.as_deref())
            .unwrap();
        assert_eq!(
            transport.cas_head("head.json", b"stale", revision.as_deref()),
            Err("CAS_CONFLICT".into())
        );
        assert_eq!(transport.get("head.json").unwrap(), b"two");
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn weak_etag_is_rejected_for_cas() {
        // 强 ETag 通过(含带引号/前后空白变体)
        assert!(ensure_strong_revision(Some("\"abc123\"")).is_ok());
        assert!(ensure_strong_revision(Some("  \"abc123\"  ")).is_ok());
        assert!(ensure_strong_revision(Some("abc123")).is_ok());
        // weak ETag 必须大写 W/ 前缀才拒绝(RFC 7232 大小写敏感)
        assert!(ensure_strong_revision(Some("W/\"abc123\"")).is_err());
        assert!(ensure_strong_revision(Some("  W/\"abc123\"")).is_err());
        // 小写 w/ 不是合法 weak 标记,按普通强 ETag 处理
        assert!(ensure_strong_revision(Some("w/\"abc123\"")).is_ok());
        // 无 ETag 拒绝
        assert!(ensure_strong_revision(None).is_err());
    }

    /// 构造一个真实发布到 transport 的设备,返回 (generation, head)。
    fn publish_device(transport: &LocalTransport, device_id: &str) -> (Generation, Head) {
        let pkg = DevicePackage {
            schema_version: 1,
            device_id: device_id.into(),
            device_name: "host".into(),
            exported_at: 1,
            turns_start: 1,
            turns_end: 2,
            sessions: vec![SessionRec {
                device_id: device_id.into(),
                key: "session".into(),
                created_at: 1,
                ..Default::default()
            }],
            turns: vec![TurnRec {
                device_id: device_id.into(),
                session_key: "session".into(),
                created_at: 1,
                input_tokens: 2.0,
                ..Default::default()
            }],
        };
        let mut package_records = records(&pkg);
        let hash = content_hash(&pkg, &mut package_records);
        let shards = make_shards(package_records).unwrap();
        for (reference, bytes) in &shards {
            transport.put_immutable(&reference.file, bytes).unwrap();
        }
        let generation = Generation {
            protocol: PROTOCOL,
            data_schema: DATA_SCHEMA,
            device_id: pkg.device_id.clone(),
            device_name: pkg.device_name.clone(),
            created_at: 1,
            parent: None,
            turns_start: pkg.turns_start,
            turns_end: pkg.turns_end,
            shards: shards.iter().map(|entry| entry.0.clone()).collect(),
            session_count: 1,
            turn_count: 1,
            content_hash: hash.clone(),
            source_hash: hash,
        };
        let manifest = serde_json::to_vec(&generation).unwrap();
        let generation_hash = sha256_hex(&manifest);
        transport
            .put_immutable(&format!("v2-generation-{generation_hash}.json"), &manifest)
            .unwrap();
        let head = Head {
            protocol: PROTOCOL,
            sequence: 1,
            generation: generation_hash,
            previous_generation: None,
            updated_at: 1,
            device_id: device_id.into(),
        };
        (generation, head)
    }

    /// 每个测试专属的临时缓存目录。测试共享相同的 device-a 内容,因此 shard
    /// 哈希相同;注入独立缓存目录才能彻底隔离,不触碰真实全局缓存。
    fn temp_cache() -> PathBuf {
        unique_temp_dir("devin-usage-metrics-v2cache")
    }

    #[test]
    fn noop_self_heal_detects_missing_remote_shard() {
        let (dir, transport) = temp_transport();
        let (generation, _head) = publish_device(&transport, "device-a");
        // 所有 shard 在时存在性检查通过
        assert_eq!(remote_shards_present(&transport, &generation), Ok(true));
        // 删掉一个远端 shard(本地缓存不动)后,存在性检查必须发现
        fs::remove_file(dir.join(&generation.shards[0].file)).unwrap();
        assert_eq!(remote_shards_present(&transport, &generation), Ok(false));
        fs::remove_dir_all(dir).unwrap();
    }

    #[test]
    fn audit_detects_remote_shard_corruption_and_repairs_cache() {
        let (dir, transport) = temp_transport();
        let cache_dir = temp_cache();
        let (generation, head) = publish_device(&transport, "device-a");
        let shard = generation.shards[0].clone();
        let cache = cache_dir.join(format!("{}.zst", shard.hash));

        // 内存态审计状态,不触碰真实用户配置目录
        let state = std::cell::RefCell::new(None::<(String, i64)>);
        let read_state = || state.borrow().clone();
        let write_state = |g: &str, at: i64| {
            *state.borrow_mut() = Some((g.to_owned(), at));
        };
        let audit = || {
            audit_local_device_with(
                &transport,
                &generation,
                &head.generation,
                &cache_dir,
                read_state,
                write_state,
            )
        };

        // 首次审计(无状态)通过,下载并写入缓存与状态
        audit().unwrap();
        assert!(cache.exists(), "审计应已把 shard 写入缓存");
        assert!(state.borrow().is_some(), "审计应写入状态");

        // 内容未变且未到期时,第二次直接跳过(不重新下载)
        audit().unwrap();

        // 篡改远端 shard 内容(文件仍在),本地缓存仍是好的——模拟"缓存完好但远端坏了"
        let good_bytes = fs::read(dir.join(&shard.file)).unwrap();
        fs::write(dir.join(&shard.file), b"corrupted-not-zstd").unwrap();

        // 强制到期重审计:把状态时间改成过去
        state.borrow_mut().as_mut().unwrap().1 = now() - AUDIT_INTERVAL_SECS - 1;
        // 审计必须用本地缓存覆盖修复远端,而不是只报错
        audit().unwrap();
        assert_eq!(
            fs::read(dir.join(&shard.file)).unwrap(),
            good_bytes,
            "审计应已用本地缓存修复远端损坏对象"
        );

        // 本地也没有可靠副本时才报错
        fs::write(dir.join(&shard.file), b"corrupted-again").unwrap();
        let _ = fs::remove_file(&cache);
        state.borrow_mut().as_mut().unwrap().1 = now() - AUDIT_INTERVAL_SECS - 1;
        let result = audit();
        assert!(result.is_err(), "远端损坏且本地无副本必须报错");
        assert!(
            result.unwrap_err().contains("无可用副本"),
            "报错应指明无法修复"
        );

        fs::remove_dir_all(dir).unwrap();
        let _ = fs::remove_dir_all(cache_dir);
    }

    #[test]
    fn audit_repairs_missing_remote_shard_from_cache() {
        let (dir, transport) = temp_transport();
        let cache_dir = temp_cache();
        let (generation, head) = publish_device(&transport, "device-a");
        let shard = generation.shards[0].clone();
        let cache = cache_dir.join(format!("{}.zst", shard.hash));

        let state = std::cell::RefCell::new(None::<(String, i64)>);
        let read_state = || state.borrow().clone();
        let write_state = |g: &str, at: i64| {
            *state.borrow_mut() = Some((g.to_owned(), at));
        };
        let audit = || {
            audit_local_device_with(
                &transport,
                &generation,
                &head.generation,
                &cache_dir,
                read_state,
                write_state,
            )
        };
        // 首次审计:远端完好,写入本地缓存
        audit().unwrap();
        let good_bytes = fs::read(&cache).unwrap();

        // 远端对象被删(文件不存在),本地缓存完好——审计应重新上传修复
        fs::remove_file(dir.join(&shard.file)).unwrap();
        state.borrow_mut().as_mut().unwrap().1 = now() - AUDIT_INTERVAL_SECS - 1;
        audit().unwrap();
        assert_eq!(
            fs::read(dir.join(&shard.file)).unwrap(),
            good_bytes,
            "审计应已把缺失的远端对象从本地缓存补回"
        );

        fs::remove_dir_all(dir).unwrap();
        let _ = fs::remove_dir_all(cache_dir);
    }

    #[test]
    fn audit_reruns_when_state_timestamp_is_in_the_future() {
        let (dir, transport) = temp_transport();
        let cache_dir = temp_cache();
        let (generation, head) = publish_device(&transport, "device-a");

        // 状态键匹配但时间戳在未来(时钟回拨/状态文件异常):必须立即重跑,
        // 否则审计会一直跳过到系统时间追上它。
        let state =
            std::cell::RefCell::new(Some((head.generation.clone(), now() + 365 * 24 * 3600)));
        let read_state = || state.borrow().clone();
        let write_state = |g: &str, at: i64| {
            *state.borrow_mut() = Some((g.to_owned(), at));
        };
        audit_local_device_with(
            &transport,
            &generation,
            &head.generation,
            &cache_dir,
            read_state,
            write_state,
        )
        .unwrap();
        // 重跑后状态时间被改写为当前时间(不再是未来值)
        assert!(state.borrow().as_ref().unwrap().1 <= now());

        fs::remove_dir_all(dir).unwrap();
        let _ = fs::remove_dir_all(cache_dir);
    }

    #[test]
    fn audit_reruns_when_generation_hash_changes() {
        let (dir, transport) = temp_transport();
        let cache_dir = temp_cache();
        let (generation, head) = publish_device(&transport, "device-a");

        // 状态记录的是真实 manifest SHA;head 指向另一个 manifest 时必须重跑,
        // 即使两个 manifest 的 content_hash 相同。
        let state = std::cell::RefCell::new(Some(("f".repeat(64), now())));
        let read_state = || state.borrow().clone();
        let write_state = |g: &str, at: i64| {
            *state.borrow_mut() = Some((g.to_owned(), at));
        };
        audit_local_device_with(
            &transport,
            &generation,
            &head.generation,
            &cache_dir,
            read_state,
            write_state,
        )
        .unwrap();
        assert_eq!(state.borrow().as_ref().unwrap().0, head.generation);

        fs::remove_dir_all(dir).unwrap();
        let _ = fs::remove_dir_all(cache_dir);
    }

    #[test]
    fn audit_state_path_varies_by_destination_and_device() {
        let a = audit_state_path_for("local:/sync-a", "device-a");
        let b = audit_state_path_for("local:/sync-b", "device-a");
        let c = audit_state_path_for("local:/sync-a", "device-b");
        let d = audit_state_path_for("webdav:https://example.com/|user", "device-a");
        if let (Some(a), Some(b), Some(c), Some(d)) = (a, b, c, d) {
            assert_ne!(a, b, "destination 不同必须分文件");
            assert_ne!(a, c, "device 不同必须分文件");
            assert_ne!(a, d, "后端不同必须分文件");
        }
        // dirs::config_dir() 不可用时全部为 None,无法断言
    }
}
