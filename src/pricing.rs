//! 模型定价表与费用计算。
//!
//! JSON 快照在编译期嵌入二进制（离线兜底）：
//! - `devin-pro-pricing.json` — 官方 Self-serve/Pro 价格，Devin 用量专用；
//!   从 https://docs.devin.ai/desktop/models.md 在线刷新，缓存 24h。
//! - `devin-model-pricing.json` — Devin 官方发布的模型价格表
//!   (https://docs.devinenterprise.com/desktop/models)，`model_uid` 与
//!   sessions.db 中的 `model` 字段完全一致，覆盖所有思考等级变体。
//! - `models-dev-pricing.json` — models.dev 的供应商完整报价与可确认的裸模型别名，
//!   保留来源、完整模型 ID、缓存单价缺失标记与上下文阶梯。
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

use serde::{Deserialize, Serialize};

const DEVIN_PRICING_JSON: &str = include_str!("devin-model-pricing.json");
const DEVIN_PRO_PRICING_JSON: &str = include_str!("devin-pro-pricing.json");
const DEVIN_PRO_URL: &str = "https://docs.devin.ai/desktop/models.md";
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
#[derive(Debug, Clone, Default, Deserialize, Serialize)]
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
    /// 模型的上下文窗口（token），来自 models.dev 的 `limit.context`。
    /// 压缩建议用它做上界：一个全局设置对池内所有模型生效，不能超过
    /// 窗口最小的那个，否则建议出模型装不下的阈值。
    #[serde(default)]
    pub ctx: Option<u64>,
    /// The actual catalog provider, retained even for a bare default alias.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source_model: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tiers: Vec<ContextTier>,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub missing_cache_read: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub missing_cache_write: bool,
}

#[derive(Debug, Clone, PartialEq, Deserialize, Serialize)]
pub struct ContextTier {
    pub above: u64,
    pub i: f64,
    pub o: f64,
    pub cw: f64,
    pub cr: f64,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub missing_cache_read: bool,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub missing_cache_write: bool,
}

impl Pricing {
    /// Thresholds are per request, including cache reads and writes, never daily totals.
    pub fn for_prompt(&self, prompt: f64) -> Self {
        let mut price = self.clone();
        if let Some(tier) = self
            .tiers
            .iter()
            .filter(|t| prompt > t.above as f64)
            .max_by_key(|t| t.above)
        {
            price.i = tier.i;
            price.o = tier.o;
            price.cw = tier.cw;
            price.cr = tier.cr;
            price.missing_cache_read = tier.missing_cache_read;
            price.missing_cache_write = tier.missing_cache_write;
        }
        price
    }

    pub fn supports_cache_usage(&self, read: f64, write: f64) -> bool {
        (read <= 0.0 || !self.missing_cache_read) && (write <= 0.0 || !self.missing_cache_write)
    }

    /// 根据各类 token 数量计算美元费用。
    /// - `input` 是不含 cache_creation 的纯输入 token
    /// - `cached` 对应缓存读取（cache_read）
    /// - `cache_creation` 对应缓存写入，按 `cw`（cache write 5m）费率计费
    ///
    /// 注意：Claude 的 1h cache 实际费率是 `input × 2`，但由于本地数据
    /// 不区分 5m/1h（Devin/Amp/Codex 均无此区分），这里统一用 `cw` 费率。
    /// Claude 的 1h vs 5m 差异在 `cost_with_cache_breakdown` 中处理。
    pub fn cost(&self, input: f64, output: f64, cached: f64) -> f64 {
        let p = self.for_prompt(input + cached);
        (input * p.i + output * p.o + cached * p.cr) / 1_000_000.0
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
        let p = self.for_prompt(input + cached + cache_creation);
        (input * p.i + output * p.o + cached * p.cr + cache_creation * p.cw) / 1_000_000.0
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
        let p = self.for_prompt(input + cached + cc_5m + cc_1h);
        let cw_1h = p.i * 2.0; // 1h cache = input × 2
        (input / 1_000_000.0) * p.i
            + (output / 1_000_000.0) * p.o
            + (cached / 1_000_000.0) * p.cr
            + (cc_5m / 1_000_000.0) * p.cw
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
        (8.0, 30.0, 0.8, 10.0)
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
        ctx: None,
        ..Default::default()
    })
}

/// 嵌入的定价表，运行期只读。
/// `models_dev` 用锁包装，支持后台刷新热更新（读多写少，日常查找无影响）。
pub struct PricingTable {
    devin: HashMap<String, Pricing>,
    /// Devin 表的 label → Pricing 反向索引（如 "GPT-5.6 Luna XHigh Thinking" → 定价）
    devin_labels: HashMap<String, Pricing>,
    devin_pro: RwLock<HashMap<String, Pricing>>,
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
            #[cfg_attr(test, allow(unused_mut))]
            let mut models_dev = parse_pricing_json(MODELS_DEV_PRICING_JSON);
            let mut devin_pro = parse_pricing_json(DEVIN_PRO_PRICING_JSON);
            if !cfg!(test) && !models_fetch_disabled() {
                if let Some(cached) = devin_pro_cache_path()
                    .and_then(|path| std::fs::read_to_string(path).ok())
                    .and_then(|text| extract_devin_pro_pricing(&text).ok())
                {
                    devin_pro = cached;
                }
            }
            // 自动更新的价格缓存覆盖内嵌快照：新模型（如 gemini-3.8-flash）
            // 无需发版就能计价；无缓存或禁用更新时退回内嵌快照。
            // 单元测试禁用覆盖以保证确定性（覆盖逻辑由 load_cached 等单测覆盖）。
            #[cfg(not(test))]
            if !models_fetch_disabled() {
                if let Some(path) = models_dev_cache_path() {
                    if let Some(cached) = load_cached_models_dev(&path) {
                        models_dev = cached;
                    }
                }
            }
            PricingTable {
                devin,
                devin_labels,
                devin_pro: RwLock::new(devin_pro),
                models_dev: RwLock::new(models_dev),
            }
        })
    }

    /// 按模型名查找定价，返回 owned 副本（内部用锁支持后台热更新）。
    /// Legacy table inspection, including Devin enterprise model UIDs and labels.
    /// Usage billing must choose `find_api` or `find_devin_pro` explicitly.
    pub fn find(&self, model: &str) -> Option<Pricing> {
        let normalized = model.trim().to_ascii_lowercase();
        self.find_normalized(&normalized)
    }

    /// API-equivalent prices must never fall back to the Devin enterprise table.
    pub fn find_api(&self, model: &str) -> Option<Pricing> {
        let model = model.trim().to_ascii_lowercase();
        let prices = self.models_dev.read().ok()?;
        lookup_api_price(&prices, &model)
    }

    /// 模型的上下文窗口（token）。压缩建议用它做上界，避免建议出模型
    /// 装不下的阈值。Devin 表不带窗口信息，返回 None。
    pub fn context_window(&self, model: &str) -> Option<u64> {
        self.find(model).and_then(|p| p.ctx)
    }

    /// Devin Self-serve/Pro prices; missing entries never use enterprise prices.
    pub fn find_devin_pro(&self, model: &str) -> Option<Pricing> {
        let normalized = model.trim().to_ascii_lowercase();
        let model = strip_provider_prefix(&normalized);
        let aliases = [model.to_string(), model.replace('.', "-")];
        let prices = self.devin_pro.read().ok()?;
        for alias in &aliases {
            if let Some(price) = prices.get(alias) {
                return Some(price.clone());
            }
        }
        prices
            .values()
            .filter_map(|price| {
                let label = price.l.as_ref()?.to_ascii_lowercase();
                (model == label || model.strip_prefix(&label).is_some_and(is_effort_suffix))
                    .then_some((label.len(), price))
            })
            .max_by_key(|(len, _)| *len)
            .map(|(_, price)| price.clone())
    }

    fn find_normalized(&self, model: &str) -> Option<Pricing> {
        if model.is_empty() {
            return None;
        }

        // 1. Devin 表精确匹配（model_uid）
        if let Some(p) = self.devin.get(model) {
            return Some(p.clone());
        }

        if let Some(p) = self.find_api(model) {
            return Some(p);
        }

        // 3. Devin label 精确匹配
        if let Some(p) = self.devin_labels.get(model) {
            return Some(p.clone());
        }

        // 5. 去掉日期后缀后重试
        let stripped_date = strip_date_suffix(model);
        if stripped_date != model {
            if let Some(p) = self.devin.get(stripped_date) {
                return Some(p.clone());
            }
            if let Some(p) = self.devin_labels.get(stripped_date) {
                return Some(p.clone());
            }
        }

        // 6b. Devin label 前缀匹配 — 如 "GLM-5.2 High" 匹配 label "GLM-5.2"
        if let Some((_, p)) = self
            .devin_labels
            .iter()
            .filter(|(label, _)| {
                model
                    .strip_prefix(label.as_str())
                    .is_some_and(is_effort_suffix)
            })
            .max_by_key(|(label, _)| label.len())
        {
            return Some(p.clone());
        }

        None
    }
}

fn is_effort_suffix(suffix: &str) -> bool {
    matches!(
        suffix,
        "-low"
            | "-medium"
            | "-high"
            | "-xhigh"
            | "-max"
            | "-thinking"
            | "-think"
            | " low"
            | " medium"
            | " high"
            | " xhigh"
            | " max"
            | " low thinking"
            | " medium thinking"
            | " high thinking"
            | " xhigh thinking"
            | " max thinking"
    ) || suffix
        .strip_prefix("-thinking:")
        .is_some_and(|n| !n.is_empty() && n.bytes().all(|b| b.is_ascii_digit()))
}

fn normalize_provider_model(model: &str) -> String {
    // Fireworks log names use an API resource path rather than a provider/model id.
    if let Some(id) = model.strip_prefix("accounts/fireworks/models/") {
        return format!("fireworks-ai/{}", normalize_fireworks_p(id));
    }
    if let Some(id) = model.strip_prefix("fireworks/") {
        return format!("fireworks-ai/{}", normalize_fireworks_p(id));
    }
    if let Some(id) = model.strip_prefix("fireworks-ai/") {
        return format!("fireworks-ai/{}", normalize_fireworks_p(id));
    }
    let (prefix, id) = model
        .rsplit_once('/')
        .map(|(prefix, id)| (format!("{prefix}/"), id))
        .unwrap_or_else(|| (String::new(), model));
    if let Some(id) = id.strip_prefix("gk-") {
        return format!("{prefix}grok-{id}");
    }
    if let Some(version) = id.strip_prefix("gpt-") {
        let mut parts = version.splitn(3, '-');
        let major = parts.next().unwrap_or("");
        let minor = parts.next().unwrap_or("");
        if !major.is_empty()
            && !minor.is_empty()
            && major.bytes().all(|b| b.is_ascii_digit())
            && minor.bytes().all(|b| b.is_ascii_digit())
        {
            let suffix = parts.next().map(|s| format!("-{s}")).unwrap_or_default();
            return format!("{prefix}gpt-{major}.{minor}{suffix}");
        }
    }
    model.to_owned()
}

fn lookup_api_price(prices: &HashMap<String, Pricing>, model: &str) -> Option<Pricing> {
    if let Some(price) = prices.get(model) {
        return Some(price.clone());
    }
    let model = normalize_provider_model(model);
    if let Some(price) = prices.get(&model) {
        return Some(price.clone());
    }
    let dated = strip_date_suffix(&model);
    if let Some(price) = prices.get(dated) {
        return Some(price.clone());
    }
    // 仅合法的 effort 后缀能回退；从最长前缀开始查表，避免每轮扫描整张价格表。
    // 不缓存查找结果，后台更新报价后下一次查询即可使用新价格。
    for (index, _) in dated.char_indices().rev() {
        if !is_effort_suffix(&dated[index..]) {
            continue;
        }
        let key = &dated[..index];
        let base = strip_provider_prefix(key);
        if base.starts_with("gpt-") || base.starts_with("claude-") {
            if let Some(price) = prices.get(key) {
                return Some(price.clone());
            }
        }
    }
    None
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

fn devin_pro_cache_path() -> Option<PathBuf> {
    if let Ok(path) = std::env::var("DEVIN_USAGE_PRO_PRICING_PATH") {
        if !path.trim().is_empty() {
            return Some(PathBuf::from(path));
        }
    }
    Some(
        dirs::cache_dir()
            .or_else(dirs::data_dir)?
            .join("devin-usage-metrics")
            .join("devin-pro-models.md"),
    )
}

#[derive(Deserialize)]
struct DevinPriceRow {
    tier: String,
    model_uid: String,
    label: String,
    input_cost_per_million_usd: f64,
    output_cost_per_million_usd: f64,
    cache_write_cost_per_million_usd: f64,
    cache_read_cost_per_million_usd: f64,
}

fn extract_devin_pro_pricing(text: &str) -> Result<HashMap<String, Pricing>, String> {
    let marker = "export const modelCostData = ";
    let data = text.split_once(marker).ok_or("missing modelCostData")?.1;
    let mut deserializer = serde_json::Deserializer::from_str(data);
    let rows =
        Vec::<DevinPriceRow>::deserialize(&mut deserializer).map_err(|error| error.to_string())?;
    let mut prices = HashMap::new();
    for row in rows.into_iter().filter(|row| row.tier == "TEAMS_TIER_PRO") {
        let price = Pricing {
            i: row.input_cost_per_million_usd,
            o: row.output_cost_per_million_usd,
            cw: row.cache_write_cost_per_million_usd,
            cr: row.cache_read_cost_per_million_usd,
            l: Some(row.label),
            ctx: None,
            ..Default::default()
        };
        if row.model_uid.trim().is_empty()
            || [price.i, price.o, price.cw, price.cr]
                .iter()
                .any(|rate| !rate.is_finite() || *rate < 0.)
            || prices
                .insert(row.model_uid.to_ascii_lowercase(), price)
                .is_some()
        {
            return Err("invalid or duplicate Pro model price".into());
        }
    }
    if prices.is_empty() {
        return Err("no Pro model prices".into());
    }
    Ok(prices)
}

/// Refresh official Self-serve/Pro data, preserving the last good table on failure.
pub fn refresh_devin_pro(force: bool) -> bool {
    if models_fetch_disabled() {
        return false;
    }
    let Some(path) = devin_pro_cache_path() else {
        return false;
    };
    let marker = retry_marker_path(&path);
    if !force && (cache_is_fresh(&path) || retry_backoff_active(&marker)) {
        return false;
    }
    let url = std::env::var("DEVIN_USAGE_PRO_PRICING_URL").unwrap_or_else(|_| DEVIN_PRO_URL.into());
    let result = fetch_models_dev_json(&url)
        .and_then(|text| extract_devin_pro_pricing(&text).map(|prices| (text, prices)));
    let (text, prices) = match result {
        Ok(result) => result,
        Err(error) => {
            eprintln!("WARN Devin Pro price refresh failed: {error}; keeping cached prices");
            mark_fetch_failed(&marker);
            return false;
        }
    };
    let Ok(mut table) = PricingTable::instance().devin_pro.write() else {
        return false;
    };
    *table = prices;
    save_cache_atomic(&path, &text).ok();
    std::fs::remove_file(marker).ok();
    true
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

struct CatalogQuote {
    provider: String,
    model: String,
    owner: Option<String>,
    price: Pricing,
}

/// Preserve billing providers. Bare aliases require an origin quote or agreement
/// between all candidates; conflicting reseller quotes never overwrite an origin.
pub fn extract_models_dev_pricing(value: &serde_json::Value) -> HashMap<String, Pricing> {
    let mut out = HashMap::new();
    let mut aliases: std::collections::BTreeMap<String, Vec<CatalogQuote>> =
        std::collections::BTreeMap::new();
    let Some(providers) = value.as_object() else {
        return out;
    };
    for (provider_id, provider) in providers {
        let Some(models) = provider.get("models").and_then(|m| m.as_object()) else {
            continue;
        };
        for (id, model) in models {
            let Some(mut p) = parse_api_price(model) else {
                continue;
            };
            let provider_id = provider_id.to_ascii_lowercase();
            p.source = Some(provider_id.clone());
            let key = id.to_ascii_lowercase();
            p.source_model = Some(key.clone());
            out.insert(format!("{provider_id}/{key}"), p.clone());
            if provider_id == "fireworks-ai" && key.starts_with("accounts/fireworks/models/") {
                out.insert(normalize_provider_model(&key), p.clone());
            }
            let owner = model
                .get("canonical_model_id")
                .and_then(|v| v.as_str())
                .and_then(|id| id.split_once('/'))
                .map(|(owner, _)| owner.to_ascii_lowercase());
            let alias = strip_provider_prefix(&key);
            if !alias.is_empty() {
                aliases
                    .entry(alias.to_owned())
                    .or_default()
                    .push(CatalogQuote {
                        provider: provider_id,
                        model: key,
                        owner,
                        price: p,
                    });
            }
        }
    }
    for (alias, mut candidates) in aliases {
        if out.contains_key(&alias) {
            continue;
        }
        candidates.sort_by(|a, b| (&a.provider, &a.model).cmp(&(&b.provider, &b.model)));
        let origin = origin_provider(strip_provider_prefix(&alias));
        let mut authoritative: Vec<_> = candidates
            .iter()
            .filter(|quote| Some(quote.provider.as_str()) == origin)
            .collect();
        if authoritative.is_empty() {
            authoritative = candidates
                .iter()
                .filter(|quote| {
                    candidates
                        .iter()
                        .any(|candidate| candidate.owner.as_ref() == Some(&quote.provider))
                })
                .collect();
        }
        let has_origin = !authoritative.is_empty();
        let choices: Vec<_> = if has_origin {
            authoritative
        } else {
            candidates.iter().collect()
        };
        let first = &choices[0].price;
        if choices.iter().all(|quote| same_rates(first, &quote.price))
            && (first.i != 0.0
                || first.o != 0.0
                || first.cw != 0.0
                || first.cr != 0.0
                || has_origin)
        {
            out.insert(alias, first.clone());
        }
    }
    out
}

/// Defaults for families whose origin provider is absent from canonical metadata,
/// or has distinct global/regional APIs. This is identity policy, never a rate table.
fn origin_provider(model: &str) -> Option<&'static str> {
    let prefixes: &[(&str, &[&str])] = &[
        ("anthropic", &["claude-"]),
        (
            "openai",
            &["gpt-", "chatgpt-", "o1", "o3", "o4", "text-embedding-"],
        ),
        ("google", &["gemini-", "gemma-"]),
        ("xai", &["grok-", "gk-"]),
        ("deepseek", &["deepseek-"]),
        ("moonshotai", &["kimi-", "moonshot-"]),
        ("minimax", &["minimax-"]),
        ("zai", &["glm-"]),
        ("alibaba", &["qwen"]),
        (
            "mistral",
            &[
                "mistral-",
                "open-mistral-",
                "magistral-",
                "codestral-",
                "devstral-",
                "ministral-",
                "pixtral-",
            ],
        ),
        ("cohere", &["command-", "embed-"]),
        ("xiaomi", &["mimo-"]),
    ];
    prefixes
        .iter()
        .find(|(_, prefixes)| prefixes.iter().any(|p| model.starts_with(p)))
        .map(|(provider, _)| *provider)
}

fn same_rates(a: &Pricing, b: &Pricing) -> bool {
    (
        a.i,
        a.o,
        a.cr,
        a.cw,
        a.missing_cache_read,
        a.missing_cache_write,
    ) == (
        b.i,
        b.o,
        b.cr,
        b.cw,
        b.missing_cache_read,
        b.missing_cache_write,
    ) && a.tiers == b.tiers
}

fn parse_api_price(model: &serde_json::Value) -> Option<Pricing> {
    let cost = model.get("cost")?.as_object()?;
    let rate = |key: &str| {
        cost.get(key)
            .and_then(|v| v.as_f64())
            .filter(|n| n.is_finite() && *n >= 0.0)
    };
    let optional_rate = |key: &str| {
        if cost.contains_key(key) {
            rate(key)
        } else {
            Some(0.0)
        }
    };
    let mut p = Pricing {
        i: rate("input")?,
        o: rate("output")?,
        cw: optional_rate("cache_write")?,
        cr: optional_rate("cache_read")?,
        ctx: model
            .pointer("/limit/context")
            .and_then(|v| v.as_u64())
            .filter(|n| *n > 0),
        missing_cache_read: !cost.contains_key("cache_read"),
        missing_cache_write: !cost.contains_key("cache_write"),
        ..Default::default()
    };
    if let Some(tiers) = cost.get("tiers") {
        for tier in tiers.as_array()? {
            if tier.pointer("/tier/type")?.as_str()? != "context" {
                return None;
            }
            let above = tier.pointer("/tier/size")?.as_u64()?;
            if above == 0 {
                return None;
            }
            let tier_rate = |key: &str, base| match tier.get(key) {
                None => Some(base),
                Some(v) => v.as_f64().filter(|n| n.is_finite() && *n >= 0.0),
            };
            p.tiers.push(ContextTier {
                above,
                i: tier_rate("input", p.i)?,
                o: tier_rate("output", p.o)?,
                cw: tier_rate("cache_write", p.cw)?,
                cr: tier_rate("cache_read", p.cr)?,
                missing_cache_read: p.missing_cache_read && tier.get("cache_read").is_none(),
                missing_cache_write: p.missing_cache_write && tier.get("cache_write").is_none(),
            });
        }
        p.tiers.sort_by_key(|t| t.above);
        if p.tiers.windows(2).any(|w| w[0].above == w[1].above) {
            return None;
        }
    } else if cost.contains_key("context_over_200k") {
        // The legacy name is also used for 272K models; without an explicit
        // threshold it cannot safely tell us when to apply the higher rate.
        return None;
    }
    Some(p)
}

/// Legacy flattened caches discarded provider identity and cannot be trusted.
/// Raw catalogs and flattened snapshots with complete provenance remain supported.
fn load_cached_models_dev(path: &std::path::Path) -> Option<HashMap<String, Pricing>> {
    let text = std::fs::read_to_string(path).ok()?;
    // 先试拍平快照；嵌套结构按 Pricing 解析必然失败（字段缺失），再走提取。
    let flat = try_parse_flat_pricing(&text);
    if !flat.is_empty() {
        return flat
            .values()
            .all(|p| {
                p.source.as_deref().is_some_and(|s| !s.is_empty())
                    && p.source_model.as_deref().is_some_and(|s| !s.is_empty())
                    && [p.i, p.o, p.cr, p.cw]
                        .iter()
                        .all(|r| r.is_finite() && *r >= 0.0)
            })
            .then_some(flat);
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
    if !force && cache_is_fresh(&path) && load_cached_models_dev(&path).is_some() {
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
        *guard = fetched;
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
            refresh_devin_pro(false);
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
        .find_api(model)
        .filter(|p| {
            p.for_prompt(input + cached + cache_creation)
                .supports_cache_usage(cached, cache_creation)
        })
        .map(|p| p.cost_with_cache_write(input, output, cached, cache_creation))
}

/// Antigravity API-equivalent estimate, preserving exact provider quotes first.
/// Internal Flash suffixes use the base quote. Missing Google Flash cache-write
/// prices estimate first processing at the input rate, not zero; storage-duration
/// charges cannot be inferred from local token records and are not included.
pub fn turn_cost_antigravity(
    model: &str,
    input: f64,
    output: f64,
    cached: f64,
    cache_creation: f64,
) -> Option<f64> {
    let model = model.trim().to_ascii_lowercase();
    let table = PricingTable::instance();
    let price = table.find_api(&model).or_else(|| {
        let suffix = model.strip_prefix("gemini-3.8-flash-")?;
        // These are separate product families, not internal routing suffixes.
        if suffix.is_empty()
            || ["lite", "tts", "cyber"].iter().any(|family| {
                suffix == *family
                    || suffix
                        .strip_prefix(family)
                        .is_some_and(|rest| rest.starts_with('-'))
            })
        {
            return None;
        }
        table.find_api("gemini-3.8-flash")
    })?;
    let price = price.for_prompt(input + cached + cache_creation);
    if price.missing_cache_write
        && price.source.as_deref() == Some("google")
        && price.source_model.as_deref() == Some("gemini-3.8-flash")
    {
        return price
            .supports_cache_usage(cached, 0.0)
            .then(|| price.cost(input + cache_creation, output, cached));
    }
    price
        .supports_cache_usage(cached, cache_creation)
        .then(|| price.cost_with_cache_write(input, output, cached, cache_creation))
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
    devin_pro_turn_cost(
        model,
        input,
        output,
        cached,
        cache_creation,
        chrono::Utc::now().timestamp(),
    )
}

/// Pro promotions use the usage date, so old free SWE-2 turns remain free.
pub fn devin_pro_turn_cost(
    model: &str,
    input: f64,
    output: f64,
    cached: f64,
    cache_creation: f64,
    created_at: i64,
) -> Option<f64> {
    let mut base = PricingTable::instance().find_devin_pro(model)?;
    let normalized = model.trim().to_ascii_lowercase();
    let label = base.l.as_deref().unwrap_or("").to_ascii_lowercase();
    if normalized.starts_with("swe-2-") || label.starts_with("swe-2 ") {
        let date = chrono::DateTime::from_timestamp(created_at, 0)?.date_naive();
        if date <= chrono::NaiveDate::from_ymd_opt(2026, 10, 15)? {
            return Some(0.);
        }
        // Published list price after the promotion. A newer nonzero official
        // price takes precedence over the initial zero-price snapshot.
        if base.i == 0. && base.o == 0. && base.cr == 0. && base.cw == 0. {
            base.i = 3.;
            base.o = 15.;
            base.cr = 0.3;
        }
    }
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
        .find_api(model)
        .filter(|p| {
            p.for_prompt(input + cached + cc_5m + cc_1h)
                .supports_cache_usage(cached, cc_5m)
        })
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
    fn pro_prices_keep_zero_and_do_not_use_enterprise_fallback() {
        let table = PricingTable::instance();
        assert_eq!(table.find_devin_pro("swe-2-high").unwrap().i, 0.);
        assert_eq!(table.find("swe-2-high").unwrap().i, 0.75);
        let opus = table.find_devin_pro("claude-opus-5-5-medium").unwrap();
        assert_eq!((opus.i, opus.o, opus.cw, opus.cr), (4., 20., 5., 0.2));
        assert!(table.find_devin_pro("unknown-model").is_none());
    }

    #[test]
    fn swe_pro_promotion_uses_turn_date_and_expiry() {
        let day = |year, month, day| {
            chrono::NaiveDate::from_ymd_opt(year, month, day)
                .unwrap()
                .and_hms_opt(12, 0, 0)
                .unwrap()
                .and_utc()
                .timestamp()
        };
        for model in ["swe-2-high", "swe-2-medium", "swe-2-max", "SWE-2 High"] {
            assert_eq!(
                devin_pro_turn_cost(model, 1e6, 1e6, 1e6, 0., day(2026, 10, 15)),
                Some(0.)
            );
            let after = devin_pro_turn_cost(model, 1e6, 1e6, 1e6, 0., day(2026, 10, 16)).unwrap();
            assert!((after - 18.3).abs() < 1e-9);
        }
    }

    #[test]
    fn official_document_parser_filters_tiers_and_validates_rates() {
        let row = |tier, model, rate| {
            serde_json::json!({
                "tier": tier, "model_uid": model, "label": model,
                "input_cost_per_million_usd": rate,
                "output_cost_per_million_usd": rate,
                "cache_write_cost_per_million_usd": 0.,
                "cache_read_cost_per_million_usd": 0.
            })
        };
        let document = |rows: Vec<serde_json::Value>| {
            format!(
                "# Models\nexport const modelCostData = {};\nexport const Component = () => null;",
                serde_json::to_string(&rows).unwrap()
            )
        };
        let pro = row("TEAMS_TIER_PRO", "swe-2-high", 0.);
        let prices = extract_devin_pro_pricing(&document(vec![
            row("TEAMS_TIER_ENTERPRISE_SAAS", "swe-2-high", 0.75),
            pro.clone(),
        ]))
        .unwrap();
        assert_eq!(prices.len(), 1);
        assert_eq!(prices["swe-2-high"].i, 0.);
        assert!(extract_devin_pro_pricing(&document(vec![pro.clone(), pro])).is_err());
        assert!(
            extract_devin_pro_pricing(&document(vec![row("TEAMS_TIER_PRO", "bad", -1.)])).is_err()
        );
        assert!(extract_devin_pro_pricing("invalid document").is_err());
        assert!(extract_devin_pro_pricing("export const modelCostData = [];").is_err());
    }

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
            ctx: None,
            ..Default::default()
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
            ctx: None,
            ..Default::default()
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
    fn lookup_is_case_insensitive_and_flash_requires_its_own_quote() {
        let table = PricingTable::instance();
        assert!(table.find("CLAUDE-OPUS-5").is_some());
        assert!(table.find("GLM-5.2 High").is_some());
        // This catalog now contains a real Flash quote; it must use that exact quote.
        let flash = table.find_api("GLM-5.3-Flash").unwrap();
        assert_eq!(flash.source.as_deref(), Some("zai"));
        assert!(table.find_api("glm-5.3-unknown-flash").is_none());
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
    fn fireworks_resource_names_keep_the_billing_provider() {
        let table = PricingTable::instance();
        // Amp 使用带 provider 前缀的模型名，且 Fireworks 用 p 代替点号
        let p = table
            .find_api("accounts/fireworks/models/kimi-k3")
            .expect("Fireworks resource names must retain the provider");
        assert_eq!(p.source.as_deref(), Some("fireworks-ai"));
        assert_eq!(
            normalize_provider_model("accounts/fireworks/models/kimi-k2p5"),
            "fireworks-ai/kimi-k2.5"
        );
    }

    #[test]
    fn zcode_models_are_priced() {
        let table = PricingTable::instance();
        assert!(table.find("deepseek-v4-flash").is_some());
        assert!(table.find("deepseek-v4-pro").is_some());
        assert!(table.find("glm-5.2").is_some());
        // A shortened version id is not evidence for the price of a longer id.
        assert!(table.find_api("kimi-k2.7").is_none());
        assert!(table.find("kimi-k3").is_some());
        assert!(table.find("grok-4.6").is_some());
        assert!(table.find_api("gk-4.6").is_some());
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
    fn exact_provider_ids_win_before_normalization_and_max_is_not_universal() {
        let prices = extract_models_dev_pricing(&serde_json::json!({"proxy": {"models": {
            "gpt-5-1": {"cost": {"input": 9, "output": 90}},
            "gpt-5.1": {"cost": {"input": 2, "output": 20}},
            "qwen3": {"cost": {"input": 1, "output": 10}}
        }}}));
        assert_eq!(lookup_api_price(&prices, "proxy/gpt-5-1").unwrap().i, 9.0);
        assert_eq!(lookup_api_price(&prices, "proxy/gpt-5.1").unwrap().i, 2.0);
        assert!(lookup_api_price(&prices, "proxy/qwen3-max").is_none());
        let snapshot = parse_pricing_json(MODELS_DEV_PRICING_JSON);
        for (id, quote) in snapshot.iter().filter(|(id, _)| id.contains('/')) {
            let resolved = lookup_api_price(&snapshot, id).unwrap();
            assert!(same_rates(quote, &resolved), "wrong rates for {id}");
            assert_eq!(quote.source, resolved.source, "wrong channel for {id}");
            assert_eq!(
                quote.source_model, resolved.source_model,
                "wrong deployment for {id}"
            );
        }
    }

    #[test]
    fn effort_lookup_preserves_longest_match_and_provider_boundaries() {
        let mut prices = HashMap::new();
        for (key, rate) in [
            ("proxy/claude-sonnet", 1.0),
            ("proxy/claude-sonnet-high", 2.0),
            ("claude-sonnet", 3.0),
            ("proxy/gpt-5.1", 4.0),
            ("proxy/gemini-3.8-flash", 5.0),
        ] {
            let mut price = PricingTable::instance().find_api("gpt-5.1").unwrap();
            price.i = rate;
            prices.insert(key.to_string(), price);
        }
        for (model, expected) in [
            ("proxy/claude-sonnet-high", Some(2.0)),
            ("proxy/claude-sonnet-high-thinking", Some(2.0)),
            ("proxy/claude-sonnet high thinking", Some(1.0)),
            ("proxy/claude-sonnet-thinking:8192", Some(1.0)),
            ("proxy/claude-sonnet-20251001", Some(1.0)),
            ("proxy/gpt-5-1-xhigh", Some(4.0)),
            ("proxy/claude-sonnet-thinking:", None),
            ("proxy/claude-sonnet-thinking:abc", None),
            ("other/claude-sonnet-high", None),
            ("proxy/gemini-3.8-flash-high", None),
            ("proxy/claude-sonnet-未知", None),
            ("", None),
        ] {
            assert_eq!(
                lookup_api_price(&prices, model).map(|p| p.i),
                expected,
                "{model}"
            );
        }
        // 同一个别名在报价更新后必须立即使用新价格，不能缓存旧值或未命中结果。
        prices.get_mut("proxy/claude-sonnet").unwrap().i = 9.0;
        assert_eq!(
            lookup_api_price(&prices, "proxy/claude-sonnet-max")
                .unwrap()
                .i,
            9.0
        );
        assert!(lookup_api_price(&prices, "proxy/gpt-new-high").is_none());
        let quote = prices["proxy/gpt-5.1"].clone();
        prices.insert("proxy/gpt-new".into(), quote);
        assert_eq!(
            lookup_api_price(&prices, "proxy/gpt-new-high").unwrap().i,
            4.0
        );
    }

    #[test]
    fn context_tiers_use_full_prompt_and_keep_unspecified_rates() {
        let p = parse_api_price(&serde_json::json!({"cost": {
            "input": 2, "output": 10, "cache_read": 0.1, "cache_write": 2.5,
            "tiers": [
                {"tier": {"type": "context", "size": 100}, "input": 4, "output": 15},
                {"tier": {"type": "context", "size": 200}, "input": 8, "output": 30, "cache_read": 0.2, "cache_write": 10}
            ], "context_over_200k": {"input": 999}
        }})).unwrap();
        assert_eq!(p.for_prompt(100.0).i, 2.0);
        assert_eq!(p.for_prompt(101.0).i, 4.0);
        assert_eq!(p.for_prompt(200.0).i, 4.0);
        assert_eq!(p.for_prompt(201.0).i, 8.0);
        assert!(
            (p.cost_with_cache_write(1.0, 2.0, 90.0, 10.0) - (4.0 + 30.0 + 9.0 + 25.0) / 1e6).abs()
                < 1e-12
        );
        assert!(
            (p.cost_claude_cache(1.0, 2.0, 90.0, 5.0, 5.0)
                - (4.0 + 30.0 + 9.0 + 12.5 + 40.0) / 1e6)
                .abs()
                < 1e-12
        );
    }

    #[test]
    fn malformed_or_unspecified_rates_are_not_assumed_free() {
        for cost in [
            serde_json::json!({"input": -1, "output": 2}),
            serde_json::json!({"input": 1}),
            serde_json::json!({"input": 1, "output": 2, "cache_read": "bad"}),
            serde_json::json!({"input": 1, "output": 2, "context_over_200k": {"input": 3}}),
            serde_json::json!({"input": 1, "output": 2, "tiers": [{"tier": {"type": "unknown", "size": 1}}]}),
        ] {
            assert!(parse_api_price(&serde_json::json!({"cost": cost})).is_none());
        }
        let p = parse_api_price(&serde_json::json!({"cost": {"input": 1, "output": 2}})).unwrap();
        assert!(p.supports_cache_usage(0.0, 0.0));
        assert!(!p.supports_cache_usage(1.0, 0.0));
        assert!(!p.supports_cache_usage(0.0, 1.0));
        let zero = parse_api_price(&serde_json::json!({"cost": {"input": 0, "output": 0, "cache_read": 0, "cache_write": 0}})).unwrap();
        assert!(zero.supports_cache_usage(1.0, 1.0));
        assert_eq!(zero.cost_with_cache_write(1.0, 1.0, 1.0, 1.0), 0.0);
    }

    #[test]
    fn api_lookup_preserves_channels_and_never_uses_devin_or_unknown_variants() {
        let table = PricingTable::instance();
        assert_eq!(
            table.find_api("anthropic/claude-sonnet-5-5").unwrap().cr,
            0.1
        );
        assert_eq!(table.find_api("vivgrid/claude-sonnet-5-5").unwrap().cr, 0.2);
        for model in [
            "unknown-provider/claude-sonnet-5-5",
            "adaptive",
            "swe-1-7-high",
            "claude-opus",
            "gpt-5.1-fast",
            "gpt-5.1-free",
        ] {
            assert!(
                table.find_api(model).is_none(),
                "wrong price inferred for {model}"
            );
        }
    }

    #[test]
    fn full_ids_distinguish_deployments_within_one_provider() {
        let prices = extract_models_dev_pricing(&serde_json::json!({"reseller": {"models": {
            "vendor/novel-1": {"cost": {"input": 1, "output": 2}},
            "TEE/novel-1": {"cost": {"input": 3, "output": 6}}
        }}}));
        assert_eq!(prices["reseller/vendor/novel-1"].i, 1.0);
        assert_eq!(prices["reseller/tee/novel-1"].i, 3.0);
        assert!(!prices.contains_key("novel-1"));
        assert!(!prices.contains_key("vendor/novel-1"));
        assert!(!prices.contains_key("tee/novel-1"));
    }

    #[test]
    fn canonical_identity_finds_an_origin_without_using_a_reseller_quote() {
        let prices = extract_models_dev_pricing(&serde_json::json!({
            "lab": {"models": {"novel-1": {"cost": {"input": 1, "output": 2}}}},
            "reseller": {"models": {"novel-1": {"canonical_model_id": "lab/novel-1", "cost": {"input": 9, "output": 18}}}}
        }));
        assert_eq!(prices["novel-1"].source.as_deref(), Some("lab"));
        assert_eq!(prices["reseller/novel-1"].i, 9.0);
    }

    #[test]
    fn catalog_keeps_provider_prices_and_rejects_ambiguous_defaults() {
        let v = serde_json::json!({
            "openai": {"models": {"gpt-test": {"cost": {"input": 2, "output": 10}}}},
            "google": {"models": {"gemini-test": {"cost": {"input": 1, "output": 5}}}},
            "reseller": {"models": {
                "gpt-test": {"cost": {"input": 9, "output": 90}},
                "google/gemini-test": {"cost": {"input": 7, "output": 70}},
                "mystery": {"cost": {"input": 2, "output": 4}},
                "free-model": {"cost": {"input": 0, "output": 0}}
            }},
            "other": {"models": {"mystery": {"cost": {"input": 3, "output": 6}}}}
        });
        let prices = extract_models_dev_pricing(&v);
        assert_eq!(prices["gpt-test"].i, 2.0);
        assert_eq!(prices["reseller/gpt-test"].i, 9.0);
        assert_eq!(prices["reseller/google/gemini-test"].i, 7.0);
        assert_eq!(prices["google/gemini-test"].i, 1.0);
        assert!(!prices.contains_key("mystery"));
        assert_eq!(prices["reseller/free-model"].i, 0.0);
    }

    #[test]
    fn unknown_model_variants_do_not_inherit_a_different_price() {
        let table = PricingTable::instance();
        for model in [
            "claude-opus",
            "gpt-5.1-fast",
            "gpt-5.1-new-version",
            "gpt-5.1-free",
        ] {
            assert!(
                table.find(model).is_none(),
                "unexpected fallback for {model}"
            );
        }
    }

    #[test]
    fn anthropic_prices_win_over_resellers_for_claude() {
        let v = serde_json::json!({
            "anthropic": {"models": {
                "claude-sonnet-5-5": {"cost": {"input": 2, "output": 10, "cache_read": 0.1, "cache_write": 2.5}},
                "claude-opus-5-5": {"cost": {"input": 4, "output": 20, "cache_read": 0.2, "cache_write": 5}}
            }},
            "vivgrid": {"models": {
                "claude-sonnet-5-5": {"cost": {"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5}}
            }},
            "venice": {"models": {
                "claude-opus-5-5": {"cost": {"input": 4.8, "output": 24, "cache_read": 0.24, "cache_write": 6}}
            }}
        });
        let prices = extract_models_dev_pricing(&v);
        let sonnet = &prices["claude-sonnet-5-5"];
        assert_eq!(
            (sonnet.i, sonnet.o, sonnet.cr, sonnet.cw),
            (2.0, 10.0, 0.1, 2.5)
        );
        let opus = &prices["claude-opus-5-5"];
        assert_eq!((opus.i, opus.o, opus.cr, opus.cw), (4.0, 20.0, 0.2, 5.0));
        // The reported excess is exactly Sonnet's 45,338,785 cache reads at +$0.10/M.
        assert!((sonnet.cost(0.0, 0.0, 45_338_785.0) - 4.5338785).abs() < 1e-9);
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
        assert!(m.contains_key("google/gemini-3.8-flash"));
        // 全零占位与无 cost 条目应被跳过
        assert!(!m.contains_key("free-model"));
        assert!(!m.contains_key("mystery"));
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
            r#"{"gemini-3.8-flash":{"i":0.75,"o":3.75,"cw":0.08,"cr":0.07,"source":"google","source_model":"gemini-3.8-flash"}}"#,
        )
        .unwrap();
        let m = load_cached_models_dev(&flat).expect("拍平缓存应可加载");
        assert_eq!(m.get("gemini-3.8-flash").map(|p| p.o), Some(3.75));
        std::fs::write(&flat, r#"{"gemini-3.8-flash":{"i":0.8,"o":4}}"#).unwrap();
        assert!(
            load_cached_models_dev(&flat).is_none(),
            "legacy reseller prices must not override the corrected snapshot"
        );
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
