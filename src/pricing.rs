//! 模型定价表与费用计算。
//!
//! 两个 JSON 快照在编译期嵌入二进制：
//! - `devin-model-pricing.json` — Devin 官方发布的模型价格表
//!   (https://docs.devinenterprise.com/desktop/models)，`model_uid` 与
//!   sessions.db 中的 `model` 字段完全一致，覆盖所有思考等级变体。
//! - `models-dev-pricing.json` — 从 models.dev 提取的非 Devin 模型价格子集，
//!   覆盖 Claude / GPT / Gemini 等家族的裸 model id，供 Amp、Claude Code、
//!   Codex、Antigravity 使用。
//!
//! 所有价格均以"美元 / 百万 token"为单位。

use std::collections::HashMap;
use std::sync::OnceLock;

use serde::Deserialize;

const DEVIN_PRICING_JSON: &str = include_str!("devin-model-pricing.json");
const MODELS_DEV_PRICING_JSON: &str = include_str!("models-dev-pricing.json");

/// 单个模型的定价信息，所有字段均为"美元 / 百万 token"。
#[derive(Debug, Clone, Default, Deserialize)]
pub struct Pricing {
    /// 输入 token 单价
    pub i: f64,
    /// 输出 token 单价
    pub o: f64,
    /// 缓存写入 token 单价（Devin 的 metrics 不区分缓存写入，此字段暂不参与计算）
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
        self.cost(input, output, cached)
            + (cache_creation / 1_000_000.0) * self.cw
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

/// 嵌入的定价表，运行期只读。
pub struct PricingTable {
    devin: HashMap<String, Pricing>,
    /// Devin 表的 label → Pricing 反向索引（如 "GPT-5.6 Luna XHigh Thinking" → 定价）
    devin_labels: HashMap<String, Pricing>,
    models_dev: HashMap<String, Pricing>,
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
                .filter_map(|p| p.l.as_ref().map(|label| (label.clone(), p.clone())))
                .collect();
            let models_dev = parse_pricing_json(MODELS_DEV_PRICING_JSON);
            PricingTable {
                devin,
                devin_labels,
                models_dev,
            }
        })
    }

    /// 按模型名查找定价。查找顺序：
    /// 1. Devin 表精确匹配（model_uid 完全一致）
    /// 2. models.dev 表精确匹配
    /// 3. Devin label 精确匹配（如 "GPT-5.6 Luna XHigh Thinking"）
    /// 4. 去掉 provider 前缀后重试（如 "accounts/fireworks/models/kimi-k2p5" → "kimi-k2p5"）
    /// 5. 去掉日期后缀后重试（如 "claude-haiku-4-5-20251001" → "claude-haiku-4-5"）
    /// 6. models.dev 表前缀匹配（如 "gpt-5.1-max" → "gpt-5.1"）
    /// 7. 反向前缀匹配
    pub fn find(&self, model: &str) -> Option<Pricing> {
        let model = model.trim();
        if model.is_empty() {
            return None;
        }

        // 1. Devin 表精确匹配（model_uid）
        if let Some(p) = self.devin.get(model) {
            return Some(p.clone());
        }

        // 2. models.dev 表精确匹配
        if let Some(p) = self.models_dev.get(model) {
            return Some(p.clone());
        }

        // 3. Devin label 精确匹配
        if let Some(p) = self.devin_labels.get(model) {
            return Some(p.clone());
        }

        // 4. 去掉 provider 前缀后重试
        let stripped_prefix = strip_provider_prefix(model);
        if stripped_prefix != model {
            if let Some(p) = self.find(stripped_prefix) {
                return Some(p);
            }
            // 同时尝试 Fireworks p→. 归一化
            let normalized = normalize_fireworks_p(stripped_prefix);
            if normalized != stripped_prefix {
                if let Some(p) = self.find(&normalized) {
                    return Some(p);
                }
            }
        }

        // 4b. 尝试 Fireworks p→. 归一化（即使没有 provider 前缀）
        let normalized = normalize_fireworks_p(model);
        if normalized != model {
            if let Some(p) = self.find(&normalized) {
                return Some(p);
            }
        }

        // 5. 去掉日期后缀后重试
        let stripped_date = strip_date_suffix(model);
        if stripped_date != model {
            if let Some(p) = self.devin.get(stripped_date) {
                return Some(p.clone());
            }
            if let Some(p) = self.models_dev.get(stripped_date) {
                return Some(p.clone());
            }
            if let Some(p) = self.devin_labels.get(stripped_date) {
                return Some(p.clone());
            }
        }

        // 6. models.dev 表前缀匹配 — 找最长的键作为前缀
        let prefix_match = self
            .models_dev
            .iter()
            .filter(|(key, _)| model.starts_with(key.as_str()))
            .max_by_key(|(key, _)| key.len())
            .map(|(_, v)| v.clone());
        if prefix_match.is_some() {
            return prefix_match;
        }

        // 6b. Devin label 前缀匹配 — 如 "GLM-5.2 High" 匹配 label "GLM-5.2"
        let label_prefix_match = self
            .devin_labels
            .iter()
            .filter(|(label, _)| model.starts_with(label.as_str()))
            .max_by_key(|(label, _)| label.len())
            .map(|(_, v)| v.clone());
        if label_prefix_match.is_some() {
            return label_prefix_match;
        }

        // 7. 反向前缀匹配 — 键以模型名开头
        let reverse_match = self
            .models_dev
            .iter()
            .filter(|(key, _)| key.starts_with(stripped_date))
            .max_by_key(|(key, _)| key.len())
            .map(|(_, v)| v.clone());
        reverse_match
    }

    /// 查找定价，返回定价和是否为"估算"（非精确匹配）。
    pub fn find_with_flag(&self, model: &str) -> Option<(Pricing, bool)> {
        let model = model.trim();
        if model.is_empty() {
            return None;
        }

        // 精确匹配
        if let Some(p) = self.devin.get(model) {
            return Some((p.clone(), false));
        }
        if let Some(p) = self.models_dev.get(model) {
            return Some((p.clone(), false));
        }
        if let Some(p) = self.devin_labels.get(model) {
            return Some((p.clone(), false));
        }

        // 模糊匹配
        let stripped_prefix = strip_provider_prefix(model);
        if stripped_prefix != model {
            if let Some((p, _)) = self.find_with_flag(stripped_prefix) {
                return Some((p, true));
            }
        }
        let stripped_date = strip_date_suffix(model);
        if let Some(p) = self.devin.get(stripped_date) {
            return Some((p.clone(), true));
        }
        if let Some(p) = self.models_dev.get(stripped_date) {
            return Some((p.clone(), true));
        }
        if let Some(p) = self.devin_labels.get(stripped_date) {
            return Some((p.clone(), true));
        }
        if let Some(p) = self.prefix_match(model).or_else(|| self.reverse_match(stripped_date)) {
            return Some((p, true));
        }
        None
    }

    fn prefix_match(&self, model: &str) -> Option<Pricing> {
        self.models_dev
            .iter()
            .filter(|(key, _)| model.starts_with(key.as_str()))
            .max_by_key(|(key, _)| key.len())
            .map(|(_, v)| v.clone())
    }

    fn reverse_match(&self, model: &str) -> Option<Pricing> {
        self.models_dev
            .iter()
            .filter(|(key, _)| key.starts_with(model))
            .max_by_key(|(key, _)| key.len())
            .map(|(_, v)| v.clone())
    }
}

fn parse_pricing_json(json: &str) -> HashMap<String, Pricing> {
    match serde_json::from_str::<HashMap<String, Pricing>>(json) {
        Ok(map) => map,
        Err(e) => {
            eprintln!("WARN 定价表解析失败: {e}");
            HashMap::new()
        }
    }
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
        let high = table.find("gpt-5-6-luna-high").expect("luna-high 必须有定价");
        let xhigh = table.find("gpt-5-6-luna-xhigh").expect("luna-xhigh 必须有定价");
        assert_eq!(high.i, 0.2);
        assert_eq!(high.o, 1.2);
        assert_eq!(high.cr, 0.02);
        // 不同思考等级的 per-token 价格相同
        assert_eq!(high.i, xhigh.i);
        assert_eq!(high.o, xhigh.o);
    }

    #[test]
    fn devin_swe_models_are_free() {
        let table = PricingTable::instance();
        let swe = table.find("swe-1-7").expect("swe-1-7 必须有定价");
        assert_eq!(swe.i, 0.0);
        assert_eq!(swe.o, 0.0);
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
    fn strip_date_suffix_works() {
        assert_eq!(strip_date_suffix("claude-haiku-4-5-20251001"), "claude-haiku-4-5");
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
    fn devin_swe_1_7_max_is_free() {
        let table = PricingTable::instance();
        let p = table.find("swe-1-7-max").expect("swe-1-7-max 应有定价");
        assert_eq!(p.i, 0.0);
        assert_eq!(p.o, 0.0);
        // 也通过 label 查找
        let p2 = table.find("SWE-1.7 Max").expect("SWE-1.7 Max label 应有定价");
        assert_eq!(p2.i, 0.0);
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
}
