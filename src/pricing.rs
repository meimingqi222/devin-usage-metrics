//! 模型定价表与费用计算。
//!
//! 两个 JSON 快照在编译期嵌入二进制（离线兜底）：
//! - `devin-model-pricing.json` — Devin 官方发布的模型价格表
//!   (https://docs.devinenterprise.com/desktop/models)，`model_uid` 与
//!   sessions.db 中的 `model` 字段完全一致，覆盖所有思考等级变体。
//! - `models-dev-pricing.json` — 从 models.dev 提取的非 Devin 模型价格子集，
//!   覆盖 Claude / GPT / Gemini 等家族的裸 model id，供 Amp、Claude Code、
//!   Codex、Antigravity 使用。
//!
//! `models-dev` 部分会自动更新（对标 OpenCode 的 models-dev.ts）：
//! 启动时先加载内嵌快照，再用本地缓存覆盖；缓存缺失或超过 24h 时从
//! 上游（与 OpenCode 默认同源）拉取刷新，原子写文件，失败静默保留旧数据
//! 并记 1h 退避（离线环境不会每次启动都付出网络超时）。
//! GUI 启动后在后台检查一次（新价格下次数据加载时生效），
//! CLI 每次运行时检查一次。
//!
//! 所有价格均以"美元 / 百万 token"为单位。

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{OnceLock, RwLock};
use std::time::Duration;

use serde::Deserialize;

const DEVIN_PRICING_JSON: &str = include_str!("devin-model-pricing.json");
const MODELS_DEV_PRICING_JSON: &str = include_str!("models-dev-pricing.json");
const GPT_5_6_LONG_CONTEXT_THRESHOLD: f64 = 272_000.0;

/// models.dev 价格源，与 OpenCode 默认同源。
/// 可用 `DEVIN_USAGE_MODELS_URL` 覆盖地址，`DEVIN_USAGE_MODELS_PATH` 覆盖本地缓存路径，
/// `DEVIN_USAGE_DISABLE_MODELS_FETCH=1` 完全禁用网络更新（纯内嵌快照）。
const MODELS_DEV_URL: &str = "https://models.opencode.ai/api.json";
/// 本地价格缓存有效期：超过后下次检查时触发刷新。
const MODELS_DEV_CACHE_TTL_SECS: u64 = 24 * 3600;
/// 拉取失败后的退避间隔：避免离线环境下每次启动都付出完整网络超时。
const MODELS_DEV_RETRY_BACKOFF_SECS: u64 = 3600;

/// 单个模型的定价信息，所有字段均为"美元 / 百万 token"。
#[derive(Debug, Clone, Default, Deserialize)]
pub struct Pricing {
    /// 输入 token 单价
    pub i: f64,
    /// 输出 token 单价
    pub o: f64,
    /// 缓存写入 token 单价（Devin 不区分 TTL，统一按此费率计算）
    #[serde(default)]
    pub cw: f64,
    /// 缓存读取 token 单价
    #[serde(default)]
    pub cr: f64,
    /// 人类可读的模型标签（仅 Devin 表有）
    #[serde(default)]
    pub l: Option<String>,
}

impl Pricing {
    /// 根据各类 token 数量计算美元费用。
    /// - `input` 是不含 cache_creation 的纯输入 token
    /// - `cached` 对应缓存读取（cache_read）
    /// - `cache_creation` 对应缓存写入，按 `cw`（cache write 5m）费率计费
    ///
    /// 注意：Claude 的 1h cache 实际费率是 `input × 2`，但由于本地数据
    /// 不区分 5m/1h（Devin/Amp/Codex 均无此区分），这里统一用 `cw` 费率。
    /// Claude 的 1h vs 5m 差异在 `cost_with_cache_breakdown` 中处理。
    pub fn cost(&self, input: f64, output: f64, cached: f64) -> f64 {
        (input / 1_000_000.0) * self.i
            + (output / 1_000_000.0) * self.o
            + (cached / 1_000_000.0) * self.cr
    }

    /// 带缓存写入的完整费用计算。
    /// `cache_creation` 是缓存写入 token 总量，按 `cw` 费率计费。
    pub fn cost_with_cache_write(
        &self,
        input: f64,
        output: f64,
        cached: f64,
        cache_creation: f64,
    ) -> f64 {
        self.cost(input, output, cached) + (cache_creation / 1_000_000.0) * self.cw
    }

    /// Claude 专用：区分 5m 和 1h cache creation 的费用计算。
    /// 1h cache 的费率是 `input × 2`（`CACHE_CREATE_1H_INPUT_MULTIPLIER = 2.0`），
    /// 5m cache 的费率是 `cw`。
    pub fn cost_claude_cache(
        &self,
        input: f64,
        output: f64,
        cached: f64,
        cc_5m: f64,
        cc_1h: f64,
    ) -> f64 {
        let cw_1h = self.i * 2.0; // 1h cache = input × 2
        (input / 1_000_000.0) * self.i
            + (output / 1_000_000.0) * self.o
            + (cached / 1_000_000.0) * self.cr
            + (cc_5m / 1_000_000.0) * self.cw
            + (cc_1h / 1_000_000.0) * cw_1h
    }
}

/// GPT-5.6 标准（非 Fast）模型超过 272K prompt 后使用的整轮费率。
/// prompt 包含普通输入、cache read 与 cache write。
fn gpt_5_6_long_context_pricing(model: &str) -> Option<Pricing> {
    let model = model.trim().to_ascii_lowercase();
    if model.contains("-priority") || model.contains(" thinking fast") || model.contains("-fast") {
        return None;
    }

    let rates = if model.contains("gpt-5-6-luna")
        || model.contains("gpt-5.6-luna")
        || model.contains("gpt-5.6 luna")
    {
        (0.4, 1.8, 0.04, 0.5)
    } else if model.contains("gpt-5-6-sol")
        || model.contains("gpt-5.6-sol")
        || model.contains("gpt-5.6 sol")
    {
        (10.0, 45.0, 1.0, 12.5)
    } else if model.contains("gpt-5-6-terra")
        || model.contains("gpt-5.6-terra")
        || model.contains("gpt-5.6 terra")
    {
        (4.0, 18.0, 0.4, 5.0)
    } else {
        return None;
    };

    Some(Pricing {
        i: rates.0,
        o: rates.1,
        cr: rates.2,
        cw: rates.3,
        l: None,
    })
}

/// 嵌入的定价表，运行期只读。
/// `models_dev` 用锁包装，支持后台刷新热更新（读多写少，日常查找无影响）。
pub struct PricingTable {
    devin: HashMap<String, Pricing>,
    /// Devin 表的 label → Pricing 反向索引（如 "GPT-5.6 Luna XHigh Thinking" → 定价）
    devin_labels: HashMap<String, Pricing>,
    models_dev: RwLock<HashMap<String, Pricing>>,
}

impl PricingTable {
    /// 加载编译期嵌入的两张定价表。
    pub fn instance() -> &'static PricingTable {
        static TABLE: OnceLock<PricingTable> = OnceLock::new();
        TABLE.get_or_init(|| {
            let devin = parse_pricing_json(DEVIN_PRICING_JSON);
            // 构建 label → Pricing 反向索引，Devin 的 real_model 字段存的是 label
            let devin_labels: HashMap<String, Pricing> = devin
                .values()
                .filter_map(|p| {
                    p.l.as_ref()
                        .map(|label| (label.to_ascii_lowercase(), p.clone()))
                })
                .collect();
            let mut models_dev = parse_pricing_json(MODELS_DEV_PRICING_JSON);
            // 自动更新的价格缓存覆盖内嵌快照：新模型（如 gemini-3.8-flash）
            // 无需发版就能计价；无缓存或禁用更新时退回内嵌快照。
            if !models_fetch_disabled() {
                if let Some(path) = models_dev_cache_path() {
                    if let Some(cached) = load_cached_models_dev(&path) {
                        models_dev.extend(cached);
                    }
                }
            }
            PricingTable {
                devin,
                devin_labels,
                models_dev: RwLock::new(models_dev),
            }
        })
    }

    /// 按模型名查找定价，返回 owned 副本（内部用锁支持后台热更新）。
    /// 查找顺序：
    /// 1. Devin 表精确匹配（model_uid 完全一致）
    /// 2. models.dev 表精确匹配
    /// 3. Devin label 精确匹配（如 "GPT-5.6 Luna XHigh Thinking"）
    /// 4. 去掉 provider 前缀后重试（如 "accounts/fireworks/models/kimi-k2p5" → "kimi-k2p5"）
    /// 5. 去掉日期后缀后重试（如 "claude-haiku-4-5-20251001" → "claude-haiku-4-5"）
    /// 6. models.dev 表前缀匹配（如 "gpt-5.1-max" → "gpt-5.1"）
    /// 7. 反向前缀匹配
    pub fn find(&self, model: &str) -> Option<Pricing> {
        let normalized = model.trim().to_ascii_lowercase();
        self.find_normalized(&normalized)
    }

    fn models_dev_exact(&self, key: &str) -> Option<Pricing> {
        self.models_dev.read().ok()?.get(key).cloned()
    }

    fn find_normalized(&self, model: &str) -> Option<Pricing> {
        if model.is_empty() {
            return None;
        }

        // 1. Devin 表精确匹配（model_uid）
        if let Some(p) = self.devin.get(model) {
            return Some(p.clone());
        }

        // 2. models.dev 表精确匹配
        if let Some(p) = self.models_dev_exact(model) {
            return Some(p);
        }

        // 3. Devin label 精确匹配
        if let Some(p) = self.devin_labels.get(model) {
            return Some(p.clone());
        }

        // 4. 去掉 provider 前缀后重试
        let stripped_prefix = strip_provider_prefix(model);
        if stripped_prefix != model {
            if let Some(p) = self.find_normalized(stripped_prefix) {
                return Some(p);
            }
            // 同时尝试 Fireworks p→. 归一化
            let normalized = normalize_fireworks_p(stripped_prefix);
            if normalized != stripped_prefix {
                if let Some(p) = self.find_normalized(&normalized) {
                    return Some(p);
                }
            }
        }

        // 4b. 尝试 Fireworks p→. 归一化（即使没有 provider 前缀）
        let normalized = normalize_fireworks_p(model);
        if normalized != model {
            if let Some(p) = self.find_normalized(&normalized) {
                return Some(p);
            }
        }

        // 5. 去掉日期后缀后重试
        let stripped_date = strip_date_suffix(model);
        if stripped_date != model {
            if let Some(p) = self.devin.get(stripped_date) {
                return Some(p.clone());
            }
            if let Some(p) = self.models_dev_exact(stripped_date) {
                return Some(p);
            }
            if let Some(p) = self.devin_labels.get(stripped_date) {
                return Some(p.clone());
            }
        }

        // Flash 没有已确认的独立价格，不能通过前缀静默套用 glm-5.3。
        if stripped_date == "glm-5.3-flash" {
            return None;
        }

        // 6. models.dev 表前缀匹配 — 找最长的键作为前缀
        if let Ok(guard) = self.models_dev.read() {
            if let Some(p) = guard
                .iter()
                .filter(|(key, _)| model.starts_with(key.as_str()))
                .max_by_key(|(key, _)| key.len())
                .map(|(_, v)| v.clone())
            {
                return Some(p);
            }
        }

        // 6b. Devin label 前缀匹配 — 如 "GLM-5.2 High" 匹配 label "GLM-5.2"
        if let Some((_, p)) = self
            .devin_labels
            .iter()
            .filter(|(label, _)| model.starts_with(label.as_str()))
            .max_by_key(|(label, _)| label.len())
        {
            return Some(p.clone());
        }

        // 7. 反向前缀匹配 — 键以模型名开头
        self.models_dev.read().ok().and_then(|guard| {
            guard
                .iter()
                .filter(|(key, _)| key.starts_with(stripped_date))
                .max_by_key(|(key, _)| key.len())
                .map(|(_, v)| v.clone())
        })
    }
}

fn parse_pricing_json(json: &str) -> HashMap<String, Pricing> {
    match serde_json::from_str::<HashMap<String, Pricing>>(json) {
        Ok(map) => map
            .into_iter()
            .map(|(key, value)| (key.to_ascii_lowercase(), value))
            .collect(),
        Err(e) => {
            eprintln!("WARN 定价表解析失败: {e}");
            HashMap::new()
        }
    }
}

/// 无告警版本，供缓存形态探测用（嵌套的原始 api.json 走拍平解析必然失败，
/// 是预期内的分支，不应打 WARN 吓到用户）。
fn try_parse_flat_pricing(json: &str) -> HashMap<String, Pricing> {
    serde_json::from_str::<HashMap<String, Pricing>>(json)
        .map(|map| {
            map.into_iter()
                .map(|(key, value)| (key.to_ascii_lowercase(), value))
                .collect()
        })
        .unwrap_or_default()
}

/// 去掉模型名末尾的日期后缀。
/// - `-YYYYMMDD`（Anthropic 风格，如 `claude-haiku-4-5-20251001`）
/// - `-YYYY-MM-DD`（如 `claude-sonnet-4-2025-05-14`）
fn strip_date_suffix(model: &str) -> &str {
    let bytes = model.as_bytes();
    // -YYYY-MM-DD（长度 11）
    if bytes.len() > 11 {
        let s = &bytes[bytes.len() - 11..];
        if s[0] == b'-'
            && s[1..5].iter().all(u8::is_ascii_digit)
            && s[5] == b'-'
            && s[6..8].iter().all(u8::is_ascii_digit)
            && s[8] == b'-'
            && s[9..].iter().all(u8::is_ascii_digit)
        {
            return &model[..model.len() - 11];
        }
    }
    // -YYYYMMDD（长度 9）
    if bytes.len() > 9 {
        let s = &bytes[bytes.len() - 9..];
        if s[0] == b'-' && s[1..].iter().all(u8::is_ascii_digit) {
            return &model[..model.len() - 9];
        }
    }
    model
}

/// 去掉 provider 前缀，返回裸模型名。
/// 如 `accounts/fireworks/models/kimi-k2p5` → `kimi-k2p5`
/// `openai/gpt-5.1` → `gpt-5.1`
/// `@cf/moonshotai/kimi-k2.6` → `kimi-k2.6`
fn strip_provider_prefix(model: &str) -> &str {
    // 取最后一个 `/` 之后的部分
    if let Some(pos) = model.rfind('/') {
        return &model[pos + 1..];
    }
    model
}

/// 将 Fireworks AI 风格的 `p` 记法转换为点号。
/// 如 `kimi-k2p5` → `kimi-k2.5`，`glm-5p2` → `glm-5.2`
/// 规则：字母或数字 + `p` + 数字 → 字母或数字 + `.` + 数字
fn normalize_fireworks_p(model: &str) -> String {
    let bytes = model.as_bytes();
    let mut result = String::with_capacity(model.len());
    let mut i = 0;
    while i < bytes.len() {
        // 当前字符是 `p`，前一个字符是字母或数字，后一个字符是数字
        if i > 0
            && i + 1 < bytes.len()
            && bytes[i] == b'p'
            && bytes[i - 1].is_ascii_alphanumeric()
            && bytes[i + 1].is_ascii_digit()
        {
            result.push('.');
        } else {
            result.push(bytes[i] as char);
        }
        i += 1;
    }
    result
}

/// 是否禁用价格表网络更新（离线/气隙环境用，退回纯内嵌快照）。
fn models_fetch_disabled() -> bool {
    matches!(
        std::env::var("DEVIN_USAGE_DISABLE_MODELS_FETCH").as_deref(),
        Ok("1") | Ok("true")
    )
}

fn models_dev_url() -> String {
    std::env::var("DEVIN_USAGE_MODELS_URL")
        .ok()
        .filter(|s| !s.trim().is_empty())
        .unwrap_or_else(|| MODELS_DEV_URL.into())
}

/// 本地价格缓存路径（对标 OpenCode 存到全局 cache 下的 models.json）。
/// 可用 `DEVIN_USAGE_MODELS_PATH` 覆盖（测试用）。
pub fn models_dev_cache_path() -> Option<PathBuf> {
    if let Ok(p) = std::env::var("DEVIN_USAGE_MODELS_PATH") {
        if !p.trim().is_empty() {
            return Some(PathBuf::from(p));
        }
    }
    let base = dirs::cache_dir().or_else(dirs::data_dir)?;
    Some(base.join("devin-usage-metrics").join("models-dev.json"))
}

fn cache_is_fresh(path: &std::path::Path) -> bool {
    std::fs::metadata(path)
        .and_then(|m| m.modified())
        .ok()
        .and_then(|t| t.elapsed().ok())
        .map(|age| age.as_secs() < MODELS_DEV_CACHE_TTL_SECS)
        .unwrap_or(false)
}

/// 上次拉取失败的时间戳文件（与缓存同目录）。存在且在退避期内则跳过拉取，
/// 离线环境不会每次启动都付出完整网络超时；`force` 可绕过退避。
fn retry_marker_path(cache: &std::path::Path) -> PathBuf {
    let name = cache
        .file_name()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_else(|| "models-dev.json".into());
    cache.with_file_name(format!("{name}.retry"))
}

fn retry_backoff_active(marker: &std::path::Path) -> bool {
    std::fs::read_to_string(marker)
        .ok()
        .and_then(|s| s.trim().parse::<i64>().ok())
        .map(|at| {
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_secs() as i64 - at < MODELS_DEV_RETRY_BACKOFF_SECS as i64)
                .unwrap_or(false)
        })
        .unwrap_or(false)
}

fn mark_fetch_failed(marker: &std::path::Path) {
    if let Some(parent) = marker.parent() {
        std::fs::create_dir_all(parent).ok();
    }
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0);
    std::fs::write(marker, now.to_string()).ok();
}

/// 把 models.dev/api.json（provider → models → cost 嵌套结构）拍平成
/// `{model_id: Pricing}`。`provider/model` 形式的 id 会额外登记去前缀别名，
/// 与 `find()` 的去 provider 前缀逻辑配合。
/// 阶梯价（`tiers` / `context_over_200k`）暂按平价处理，与旧快照口径一致。
pub fn extract_models_dev_pricing(value: &serde_json::Value) -> HashMap<String, Pricing> {
    let mut out = HashMap::new();
    let Some(providers) = value.as_object() else {
        return out;
    };
    for provider in providers.values() {
        let Some(models) = provider.get("models").and_then(|m| m.as_object()) else {
            continue;
        };
        for (id, model) in models {
            let Some(cost) = model.get("cost") else {
                continue;
            };
            let num = |k: &str| {
                cost.get(k)
                    .and_then(serde_json::Value::as_f64)
                    .unwrap_or(0.0)
            };
            let p = Pricing {
                i: num("input"),
                o: num("output"),
                cw: num("cache_write"),
                cr: num("cache_read"),
                l: None,
            };
            // 全零条目多为按 plan 计价的占位（如 token-plan），跳过以免污染。
            if p.i == 0.0 && p.o == 0.0 && p.cw == 0.0 && p.cr == 0.0 {
                continue;
            }
            let key = id.to_ascii_lowercase();
            if let Some(suffix) = key.rsplit('/').next() {
                if !suffix.is_empty() && suffix != key.as_str() {
                    // 别名不覆盖裸 id（裸 id 的 provider 定价更可信）。
                    out.entry(suffix.to_owned()).or_insert_with(|| p.clone());
                }
            }
            out.insert(key, p);
        }
    }
    out
}

/// 读取本地价格缓存，兼容两种形态：自动更新存的原始 api.json（嵌套），
/// 以及拍平后的快照格式。不可用时返回 None（调用方退回内嵌快照）。
fn load_cached_models_dev(path: &std::path::Path) -> Option<HashMap<String, Pricing>> {
    let text = std::fs::read_to_string(path).ok()?;
    // 先试拍平快照；嵌套结构按 Pricing 解析必然失败（字段缺失），再走提取。
    let flat = try_parse_flat_pricing(&text);
    if !flat.is_empty() {
        return Some(flat);
    }
    let value: serde_json::Value = serde_json::from_str(&text).ok()?;
    let extracted = extract_models_dev_pricing(&value);
    (!extracted.is_empty()).then_some(extracted)
}

fn fetch_models_dev_json(url: &str) -> Result<String, String> {
    let agent = ureq::Agent::config_builder()
        .timeout_global(Some(Duration::from_secs(10)))
        .build()
        .new_agent();
    let mut body = agent
        .get(url)
        .header("User-Agent", "devin-usage-metrics")
        .call()
        .map_err(|e| match e {
            ureq::Error::StatusCode(code) => format!("HTTP {code}"),
            other => other.to_string(),
        })?
        .into_body();
    body.read_to_string()
        .map_err(|e: ureq::Error| e.to_string())
}

/// 同目录临时文件 + rename，保证并发读取方永远看不到半截文件。
fn save_cache_atomic(path: &std::path::Path, text: &str) -> std::io::Result<()> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0);
    let tmp = path.with_extension(format!("tmp-{}-{nanos}", std::process::id()));
    let result = (|| {
        std::fs::write(&tmp, text)?;
        std::fs::rename(&tmp, path)
    })();
    if result.is_err() {
        std::fs::remove_file(&tmp).ok();
    } else {
        cleanup_stale_tmps(path);
    }
    result
}

/// 进程被杀可能留下 `*.tmp-*` 孤儿文件，成功写入后顺手清理同前缀的旧文件。
fn cleanup_stale_tmps(path: &std::path::Path) {
    let (Some(parent), Some(stem)) = (path.parent(), path.file_stem().and_then(|s| s.to_str()))
    else {
        return;
    };
    let prefix = format!("{stem}.tmp-");
    let Ok(entries) = std::fs::read_dir(parent) else {
        return;
    };
    for entry in entries.flatten() {
        if entry.file_name().to_string_lossy().starts_with(&prefix) && entry.path() != path {
            std::fs::remove_file(entry.path()).ok();
        }
    }
}

/// 拉取最新价格并热更新内存表 + 本地缓存。离线/失败时静默保留旧数据。
/// 返回 true 表示内存表已更新。`force` 跳过新鲜度检查。
pub fn refresh_models_dev(force: bool) -> bool {
    if models_fetch_disabled() {
        return false;
    }
    let Some(path) = models_dev_cache_path() else {
        return false;
    };
    if !force && cache_is_fresh(&path) {
        return false;
    }
    let marker = retry_marker_path(&path);
    if !force && retry_backoff_active(&marker) {
        return false;
    }
    let url = models_dev_url();
    let text = match fetch_models_dev_json(&url) {
        Ok(t) => t,
        Err(e) => {
            eprintln!("WARN 价格表更新失败（{url}）: {e}，继续使用本地数据");
            mark_fetch_failed(&marker);
            return false;
        }
    };
    let value: serde_json::Value = match serde_json::from_str(&text) {
        Ok(v) => v,
        Err(e) => {
            eprintln!("WARN 价格表解析失败: {e}");
            mark_fetch_failed(&marker);
            return false;
        }
    };
    let fetched = extract_models_dev_pricing(&value);
    if fetched.is_empty() {
        eprintln!("WARN 价格表更新内容为空，已忽略");
        mark_fetch_failed(&marker);
        return false;
    }
    // 缓存写失败不影响内存更新（下次启动会重试拉取）。
    save_cache_atomic(&path, &text).ok();
    std::fs::remove_file(&marker).ok();
    let count = fetched.len();
    if let Ok(mut guard) = PricingTable::instance().models_dev.write() {
        guard.extend(fetched);
    }
    eprintln!("INFO 价格表已更新（{count} 个模型）");
    true
}

/// GUI 启动时调用：在后台线程检查一次，调用后立即返回，不阻塞界面。
/// 拉取到的新价格在下次数据加载时生效（启动时即决定本次视图的价格表，
/// 常驻期间价格源变化不频繁，不做周期轮询）。
pub fn ensure_fresh_async() {
    std::thread::Builder::new()
        .name("pricing-refresh".into())
        .spawn(|| {
            refresh_models_dev(false);
        })
        .ok();
}

/// 计算单轮对话的费用（美元）。模型未知时返回 None。
/// 包含 cache_creation 费用（按 cw 费率）。
pub fn turn_cost(
    model: &str,
    input: f64,
    output: f64,
    cached: f64,
    cache_creation: f64,
) -> Option<f64> {
    PricingTable::instance()
        .find(model)
        .map(|p| p.cost_with_cache_write(input, output, cached, cache_creation))
}

/// 计算单次请求费用。与聚合用量不同，这里会按该请求的 prompt 总量应用
/// GPT-5.6 的 272K 长上下文阶梯价。
pub fn single_turn_cost(
    model: &str,
    input: f64,
    output: f64,
    cached: f64,
    cache_creation: f64,
) -> Option<f64> {
    let base = PricingTable::instance().find(model)?;
    let prompt = input + cached + cache_creation;
    if prompt > GPT_5_6_LONG_CONTEXT_THRESHOLD {
        if let Some(long_context) = gpt_5_6_long_context_pricing(model) {
            return Some(long_context.cost_with_cache_write(input, output, cached, cache_creation));
        }
    }
    Some(base.cost_with_cache_write(input, output, cached, cache_creation))
}

/// Claude 专用：区分 5m/1h cache creation 的费用计算。
pub fn turn_cost_claude(
    model: &str,
    input: f64,
    output: f64,
    cached: f64,
    cc_5m: f64,
    cc_1h: f64,
) -> Option<f64> {
    PricingTable::instance()
        .find(model)
        .map(|p| p.cost_claude_cache(input, output, cached, cc_5m, cc_1h))
}

/// 格式化美元金额为简洁字符串。
pub fn fmt_cost(usd: f64) -> String {
    if usd <= 0.0 {
        "—".into()
    } else if usd < 0.01 {
        format!("${:.4}", usd)
    } else if usd < 1000.0 {
        format!("${:.2}", usd)
    } else {
        format!("${:.0}", usd)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn devin_models_are_priced() {
        let table = PricingTable::instance();
        // Devin 的思考等级变体 — 同一基础模型，不同思考等级
        let high = table
            .find("gpt-5-6-luna-high")
            .expect("luna-high 必须有定价");
        let xhigh = table
            .find("gpt-5-6-luna-xhigh")
            .expect("luna-xhigh 必须有定价");
        assert_eq!(high.i, 0.2);
        assert_eq!(high.o, 1.2);
        assert_eq!(high.cr, 0.02);
        // 不同思考等级的 per-token 价格相同
        assert_eq!(high.i, xhigh.i);
        assert_eq!(high.o, xhigh.o);
    }

    #[test]
    fn devin_swe_models_are_priced() {
        let table = PricingTable::instance();
        let swe = table.find("swe-1-7").expect("swe-1-7 必须有定价");
        assert_eq!(swe.i, 0.5);
        assert_eq!(swe.o, 2.5);
        assert_eq!(swe.cr, 0.2);
    }

    #[test]
    fn devin_adaptive_is_priced() {
        let table = PricingTable::instance();
        let adaptive = table.find("adaptive").expect("adaptive 必须有定价");
        assert_eq!(adaptive.i, 0.5);
        assert_eq!(adaptive.o, 2.0);
    }

    #[test]
    fn claude_date_suffix_is_stripped() {
        let table = PricingTable::instance();
        // Amp 存储带日期后缀的模型名
        let p = table
            .find("claude-haiku-4-5-20251001")
            .expect("应通过去日期后缀匹配 claude-haiku-4-5");
        assert_eq!(p.i, 1.0);
        assert_eq!(p.o, 5.0);
    }

    #[test]
    fn gpt_5_1_max_falls_back_to_gpt_5_1() {
        let table = PricingTable::instance();
        let p = table
            .find("gpt-5.1-max")
            .expect("应通过前缀匹配回退到 gpt-5.1");
        assert_eq!(p.i, 1.25);
    }

    #[test]
    fn unknown_model_returns_none() {
        let table = PricingTable::instance();
        assert!(table.find("nonexistent-model-xyz").is_none());
    }

    #[test]
    fn cost_calculation_is_correct() {
        let p = Pricing {
            i: 1.0,
            o: 5.0,
            cr: 0.1,
            cw: 0.0,
            l: None,
        };
        // 1M input ($1) + 2M output ($10) + 10M cached ($1) = $12
        let cost = p.cost(1_000_000.0, 2_000_000.0, 10_000_000.0);
        assert!((cost - 12.0).abs() < 1e-9);
    }

    #[test]
    fn cost_with_cache_write_is_correct() {
        let p = Pricing {
            i: 5.0,
            o: 25.0,
            cr: 0.5,
            cw: 6.25,
            l: None,
        };
        // claude-opus-5: 530 input + 216178 output + 32217103 cache_read + 662052 cache_creation(1h)
        // ccusage 算法：1h cache 按 input×2=10 计费
        let cost = p.cost_claude_cache(530.0, 216178.0, 32217103.0, 0.0, 662052.0);
        // 验证与 ccusage 的 $28.1361715 一致
        assert!(
            (cost - 28.1361715).abs() < 1e-4,
            "claude-opus-5 1h cache 费用应为 $28.14，实际 {cost}"
        );
    }

    #[test]
    fn luna_long_context_uses_per_turn_threshold_and_rates() {
        let at_threshold =
            single_turn_cost("gpt-5-6-luna-high", 3.0, 1_000.0, 271_000.0, 997.0).unwrap();
        let expected_standard =
            (3.0 * 0.2 + 1_000.0 * 1.2 + 271_000.0 * 0.02 + 997.0 * 0.25) / 1_000_000.0;
        assert!((at_threshold - expected_standard).abs() < 1e-12);

        let above_threshold =
            single_turn_cost("gpt-5-6-luna-high", 3.0, 1_000.0, 271_000.0, 998.0).unwrap();
        let expected_long =
            (3.0 * 0.4 + 1_000.0 * 1.8 + 271_000.0 * 0.04 + 998.0 * 0.5) / 1_000_000.0;
        assert!((above_threshold - expected_long).abs() < 1e-12);
    }

    #[test]
    fn exact_fast_label_keeps_fast_pricing_above_272k() {
        let cost = single_turn_cost(
            "GPT-5.6 Luna High Thinking Fast",
            0.0,
            1_000_000.0,
            300_000.0,
            0.0,
        )
        .unwrap();
        // Fast 是独立定价，不应被非 Fast label 前缀或长上下文阶梯覆盖。
        assert!((cost - 2.412).abs() < 1e-12);
    }

    #[test]
    fn luna_max_and_fast_max_are_priced() {
        let table = PricingTable::instance();
        let standard = table.find("gpt-5-6-luna-max").expect("Luna Max 必须有定价");
        assert_eq!(
            (standard.i, standard.o, standard.cw, standard.cr),
            (0.2, 1.2, 0.25, 0.02)
        );
        let fast = table
            .find("gpt-5-6-luna-max-priority")
            .expect("Luna Max Fast 必须有定价");
        assert_eq!((fast.i, fast.o, fast.cw, fast.cr), (0.4, 2.4, 0.5, 0.04));
    }

    #[test]
    fn lookup_is_case_insensitive_but_glm_flash_does_not_fall_back() {
        let table = PricingTable::instance();
        assert!(table.find("CLAUDE-OPUS-5").is_some());
        assert!(table.find("GLM-5.2 High").is_some());
        assert!(table.find("GLM-5.3-Flash").is_none());
        assert!(table.find("glm-5.3-flash").is_none());
    }

    #[test]
    fn strip_date_suffix_works() {
        assert_eq!(
            strip_date_suffix("claude-haiku-4-5-20251001"),
            "claude-haiku-4-5"
        );
        assert_eq!(
            strip_date_suffix("claude-sonnet-4-2025-05-14"),
            "claude-sonnet-4"
        );
        assert_eq!(strip_date_suffix("gpt-5.1"), "gpt-5.1");
        assert_eq!(strip_date_suffix("adaptive"), "adaptive");
    }

    #[test]
    fn devin_label_lookup_works() {
        let table = PricingTable::instance();
        // Devin 的 real_model 字段存的是 label，不是 model_uid
        let p = table
            .find("GPT-5.6 Luna XHigh Thinking")
            .expect("应通过 label 匹配 gpt-5-6-luna-xhigh");
        assert_eq!(p.i, 0.2);
        assert_eq!(p.o, 1.2);
    }

    #[test]
    fn devin_swe_1_7_max_is_priced() {
        let table = PricingTable::instance();
        let p = table.find("swe-1-7-max").expect("swe-1-7-max 应有定价");
        assert_eq!(p.i, 0.5);
        assert_eq!(p.o, 2.5);
        // 也通过 label 查找
        let p2 = table
            .find("SWE-1.7 Max")
            .expect("SWE-1.7 Max label 应有定价");
        assert_eq!(p2.i, 0.5);
    }

    #[test]
    fn devin_glm_5_2_high_label_prefix_match() {
        let table = PricingTable::instance();
        // "GLM-5.2 High" 不在定价表中，但 "GLM-5.2" 是它的前缀
        let p = table
            .find("GLM-5.2 High")
            .expect("应通过 label 前缀匹配 GLM-5.2");
        assert_eq!(p.i, 1.4);
        assert_eq!(p.o, 4.4);
    }

    #[test]
    fn provider_prefix_is_stripped() {
        let table = PricingTable::instance();
        // Amp 使用带 provider 前缀的模型名，且 Fireworks 用 p 代替点号
        let p = table
            .find("accounts/fireworks/models/kimi-k2p5")
            .expect("应通过去 provider 前缀 + p→. 归一化匹配 kimi-k2.5");
        assert_eq!(p.i, 0.6);
    }

    #[test]
    fn zcode_models_are_priced() {
        let table = PricingTable::instance();
        assert!(table.find("deepseek-v4-flash").is_some());
        assert!(table.find("deepseek-v4-pro").is_some());
        assert!(table.find("glm-5.2").is_some());
        assert!(table.find("kimi-k2.7").is_some());
        assert!(table.find("kimi-k3").is_some());
        assert!(table.find("grok-4.6").is_some());
        assert!(table.find("gk-4.6").is_some());
    }

    #[test]
    fn strip_provider_prefix_works() {
        assert_eq!(strip_provider_prefix("openai/gpt-5.1"), "gpt-5.1");
        assert_eq!(
            strip_provider_prefix("accounts/fireworks/models/kimi-k2p5"),
            "kimi-k2p5"
        );
        assert_eq!(strip_provider_prefix("gpt-5.1"), "gpt-5.1");
    }

    #[test]
    fn extract_flattens_nested_api_and_registers_alias() {
        let v: serde_json::Value = serde_json::from_str(
            r#"{
                "google": {"models": {"gemini-3.8-flash": {"cost": {"input": 0.75, "output": 3.75, "cache_read": 0.075, "cache_write": 0.08333}}}},
                "nano-gpt": {"models": {"google/gemini-3.8-flash": {"cost": {"input": 0.8, "output": 4.0}}}},
                "plan": {"models": {"free-model": {"cost": {"input": 0, "output": 0}}}},
                "nocost": {"models": {"mystery": {"limit": {}}}}
            }"#,
        )
        .unwrap();
        let m = extract_models_dev_pricing(&v);
        let p = m.get("gemini-3.8-flash").expect("应提取到 3.8-flash");
        assert_eq!((p.i, p.o, p.cr, p.cw), (0.75, 3.75, 0.075, 0.08333));
        // provider/ 前缀形态保留原键，裸 id 别名不覆盖已有条目
        assert!(m.get("google/gemini-3.8-flash").is_some());
        // 全零占位与无 cost 条目应被跳过
        assert!(m.get("free-model").is_none());
        assert!(m.get("mystery").is_none());
    }

    #[test]
    fn cached_file_loads_both_flat_and_nested_shapes() {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let dir = std::env::temp_dir().join(format!("dum-pricing-{}-{nanos}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        // 嵌套形态（自动更新缓存的实际存储格式）
        let nested = dir.join("nested.json");
        std::fs::write(
            &nested,
            r#"{"google":{"models":{"gemini-3.8-flash":{"cost":{"input":0.75,"output":3.75}}}}}"#,
        )
        .unwrap();
        let m = load_cached_models_dev(&nested).expect("嵌套缓存应可加载");
        assert_eq!(m.get("gemini-3.8-flash").map(|p| p.i), Some(0.75));
        // 拍平形态（历史快照格式）
        let flat = dir.join("flat.json");
        std::fs::write(
            &flat,
            r#"{"gemini-3.8-flash":{"i":0.75,"o":3.75,"cw":0.08,"cr":0.07}}"#,
        )
        .unwrap();
        let m = load_cached_models_dev(&flat).expect("拍平缓存应可加载");
        assert_eq!(m.get("gemini-3.8-flash").map(|p| p.o), Some(3.75));
        // 缺失/损坏的缓存不影响启动（退回内嵌快照）
        assert!(load_cached_models_dev(&dir.join("missing.json")).is_none());
        assert!(!cache_is_fresh(&dir.join("missing.json")));
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn fetch_failure_backoff_roundtrip() {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let dir =
            std::env::temp_dir().join(format!("dum-pricing-retry-{}-{nanos}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let marker = retry_marker_path(&dir.join("models-dev.json"));
        // 无标记 → 不退避，照常拉取
        assert!(!retry_backoff_active(&marker));
        mark_fetch_failed(&marker);
        // 刚失败过 → 退避期内跳过拉取
        assert!(retry_backoff_active(&marker));
        // 很久以前的时间戳 → 退避结束
        std::fs::write(&marker, "1").unwrap();
        assert!(!retry_backoff_active(&marker));
        // 损坏内容 → 不触发退避（下次照常拉取，而非永久跳过）
        std::fs::write(&marker, "garbage").unwrap();
        assert!(!retry_backoff_active(&marker));
        std::fs::remove_dir_all(&dir).ok();
    }
}
