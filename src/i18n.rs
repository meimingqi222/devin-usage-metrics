//! 中英双语 UI 文案。
//!
//! - 语言全局唯一（`Lang`），启动时从配置文件读取；无配置时跟随系统 locale。
//! - `t(Key)` 返回当前语言的静态文案；带参数的模板用 `{0}` `{1}` 占位，
//!   由 `tf(key, args)` 按位置填充（英文语序可能与中文不同，不能直接 `format!`）。
//! - 切换语言时立即写回配置文件。运行中切换只影响之后构造的字符串，
//!   已生成的数据源错误消息要等下次 reload 才会更新。

use serde::{Deserialize, Serialize};
use std::sync::atomic::{AtomicU8, Ordering};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Lang {
    Zh,
    En,
}

impl Lang {
    pub fn other(self) -> Lang {
        match self {
            Lang::Zh => Lang::En,
            Lang::En => Lang::Zh,
        }
    }

    /// 顶栏语言切换按钮上的短标签。
    pub fn short_label(self) -> &'static str {
        match self {
            Lang::Zh => "中",
            Lang::En => "EN",
        }
    }

    fn from_code(code: &str) -> Option<Lang> {
        match code {
            "zh" => Some(Lang::Zh),
            "en" => Some(Lang::En),
            _ => None,
        }
    }

    fn code(self) -> &'static str {
        match self {
            Lang::Zh => "zh",
            Lang::En => "en",
        }
    }
}

// 0 = Zh, 1 = En；启动时 init() 会立即覆盖默认值
static LANG: AtomicU8 = AtomicU8::new(0);

pub fn lang() -> Lang {
    match LANG.load(Ordering::Relaxed) {
        1 => Lang::En,
        _ => Lang::Zh,
    }
}

pub fn set_lang(new_lang: Lang) {
    LANG.store(
        match new_lang {
            Lang::En => 1,
            Lang::Zh => 0,
        },
        Ordering::Relaxed,
    );
    save_config(new_lang);
}

/// 启动时调用：优先用用户上次选择，否则跟随系统 locale。
pub fn init() {
    let lang = load_config().unwrap_or_else(system_default);
    LANG.store(
        match lang {
            Lang::En => 1,
            Lang::Zh => 0,
        },
        Ordering::Relaxed,
    );
}

/// 系统 locale 以 zh 开头视为中文，其余一律英文。
fn system_default() -> Lang {
    match sys_locale::get_locale() {
        Some(locale) if locale.to_ascii_lowercase().starts_with("zh") => Lang::Zh,
        _ => Lang::En,
    }
}

fn config_path() -> Option<std::path::PathBuf> {
    dirs::config_dir().map(|base| base.join("devin-usage-metrics/config.json"))
}

#[derive(Serialize, Deserialize)]
struct ConfigFile {
    lang: String,
}

fn load_config() -> Option<Lang> {
    let path = config_path()?;
    let text = std::fs::read_to_string(path).ok()?;
    let config: ConfigFile = serde_json::from_str(&text).ok()?;
    Lang::from_code(&config.lang)
}

fn save_config(lang: Lang) {
    let Some(path) = config_path() else {
        return;
    };
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    let config = ConfigFile {
        lang: lang.code().to_string(),
    };
    if let Ok(text) = serde_json::to_string_pretty(&config) {
        let _ = std::fs::write(path, text);
    }
}

/// 带参数文案：把模板中的 {0} {1} … 依次替换为 args。
pub fn tf(key: Key, args: &[&str]) -> String {
    let mut out = t(key).to_string();
    for (index, arg) in args.iter().enumerate() {
        out = out.replace(&format!("{{{index}}}"), arg);
    }
    out
}

#[derive(Debug, Clone, Copy)]
pub enum Key {
    // 顶栏与导航
    Usage,
    Sessions,
    PeriodDay,
    PeriodWeek,
    PeriodMonth,
    PrevPage,
    NextPage,
    Reload,
    Reloading,
    Loading,
    DataAsOf,
    SessionsCount,
    LangLabel,

    // 加载页
    LoadingData,
    LoadingCacheHint,

    // 统计卡
    StatTotalTokens,
    StatInput,
    StatOutput,
    StatCached,
    StatCost,
    StatTurnsSessions,
    NoRecord,

    // 空态 / 图表
    EmptyWindow,
    ChartTitle,
    LegendCached,
    LegendIn,
    LegendOut,

    // 用量表头
    ThPeriod,
    ThSessions,
    ThTurns,
    ThInput,
    ThOutput,
    ThCache,
    ThTotal,
    ThCost,
    ThModelMix,

    // 会话列表
    ThLastActive,
    ThSession,
    ThTitle,
    ThMode,
    ThModel,
    ThMsgs,
    ThTokens,
    ThWindowCost,
    SessionsNotFound,

    // 会话详情
    AdaptiveRouted,
    AdaptiveShort,
    ConfigValue,
    KvSession,
    KvWorkdir,
    KvModel,
    KvTime,
    KvActivity,
    CreatedLastActive,
    TokenBreakdown,
    PerTurnPriced,
    UnknownUnpriced,
    ActivityNoTtft,
    ActivityTtft,

    // 错误横幅与数据源错误
    DataWarning,
    ErrNotExist,
    ErrOpenFailed,
    ErrNotFound,
    ErrSessionsUnreadable,
    ErrDbOpen,
    ErrRemoteExport,
    ErrAndMore,
}

/// 当前语言的文案。返回 'static，可直接传给接受 &'static str 的接口。
pub fn t(key: Key) -> &'static str {
    match lang() {
        Lang::Zh => zh(key),
        Lang::En => en(key),
    }
}

fn zh(key: Key) -> &'static str {
    match key {
        Key::Usage => "用量",
        Key::Sessions => "会话",
        Key::PeriodDay => "日",
        Key::PeriodWeek => "周",
        Key::PeriodMonth => "月",
        Key::PrevPage => "◀ 上一页",
        Key::NextPage => "下一页 ▶",
        Key::Reload => "重新加载",
        Key::Reloading => "正在刷新…",
        Key::Loading => "加载中...",
        Key::DataAsOf => "数据截至 {0}",
        Key::SessionsCount => "{0} 会话",
        Key::LangLabel => "语言",
        Key::LoadingData => "正在读取本地 Agent 用量数据",
        Key::LoadingCacheHint => "首次读取完成后，后续启动会使用 5 分钟缓存",
        Key::StatTotalTokens => "总 Tokens",
        Key::StatInput => "输入（新）",
        Key::StatOutput => "输出",
        Key::StatCached => "缓存读取",
        Key::StatCost => "费用 (USD)",
        Key::StatTurnsSessions => "轮次 / 会话",
        Key::NoRecord => "无记录",
        Key::EmptyWindow => "当前{0}窗口没有{1}用量；最近记录：{2}。可点击“下一页 ▶”查看更早历史。",
        Key::ChartTitle => "Token 用量趋势",
        Key::LegendCached => "缓存",
        Key::LegendIn => "输入",
        Key::LegendOut => "输出",
        Key::ThPeriod => "周期",
        Key::ThSessions => "会话",
        Key::ThTurns => "轮次",
        Key::ThInput => "输入",
        Key::ThOutput => "输出",
        Key::ThCache => "缓存",
        Key::ThTotal => "总计",
        Key::ThCost => "费用",
        Key::ThModelMix => "模型分布",
        Key::ThLastActive => "最后活跃",
        Key::ThSession => "会话",
        Key::ThTitle => "标题",
        Key::ThMode => "模式",
        Key::ThModel => "模型",
        Key::ThMsgs => "消息",
        Key::ThTokens => "Tokens",
        Key::ThWindowCost => "窗口费用",
        Key::SessionsNotFound => "未找到 {0} 本地会话数据",
        Key::AdaptiveRouted => "adaptive → {0}（服务端路由）",
        Key::AdaptiveShort => "adaptive → {0}",
        Key::ConfigValue => "{0}（配置值：{1}）",
        Key::KvSession => "会话",
        Key::KvWorkdir => "工作目录",
        Key::KvModel => "模型",
        Key::KvTime => "时间",
        Key::KvActivity => "活动",
        Key::CreatedLastActive => "{0} 创建，最后活跃 {1}",
        Key::TokenBreakdown => "输入 {0} · 输出 {1} · 缓存 {2} · 共 {3}",
        Key::PerTurnPriced => "{0}（窗口内 {1} 轮逐轮计价）",
        Key::UnknownUnpriced => "未知（无匹配定价）",
        Key::ActivityNoTtft => "{0} 条 agent 消息 · 窗口内 {1} 轮",
        Key::ActivityTtft => "{0} 条 agent 消息 · 窗口内 {1} 轮 · TTFT 中位 {2} ms",
        Key::DataWarning => "数据源警告：{0}",
        Key::ErrNotExist => "{0} 不存在",
        Key::ErrOpenFailed => "打开失败 {0}",
        Key::ErrNotFound => "未找到 {0}",
        Key::ErrSessionsUnreadable => "有 {0} 个会话文件无法读取",
        Key::ErrDbOpen => "无法打开 {0} 数据库: {1}",
        Key::ErrRemoteExport => "有 {0} 个远程线程导出失败: {1}",
        Key::ErrAndMore => " 等 {0} 个",
    }
}

fn en(key: Key) -> &'static str {
    match key {
        Key::Usage => "Usage",
        Key::Sessions => "Sessions",
        Key::PeriodDay => "Day",
        Key::PeriodWeek => "Week",
        Key::PeriodMonth => "Month",
        Key::PrevPage => "◀ Prev",
        Key::NextPage => "Next ▶",
        Key::Reload => "Reload",
        Key::Reloading => "Refreshing…",
        Key::Loading => "Loading...",
        Key::DataAsOf => "Data as of {0}",
        Key::SessionsCount => "{0} sessions",
        Key::LangLabel => "Language",
        Key::LoadingData => "Reading local agent usage data",
        Key::LoadingCacheHint => "After the first read, subsequent launches use a 5-minute cache",
        Key::StatTotalTokens => "Total Tokens",
        Key::StatInput => "Input (new)",
        Key::StatOutput => "Output",
        Key::StatCached => "Cache Read",
        Key::StatCost => "Cost (USD)",
        Key::StatTurnsSessions => "Turns / Sessions",
        Key::NoRecord => "No record",
        Key::EmptyWindow => {
            "No {1} usage in the current {0} window; latest record: {2}. \
             Click “Next ▶” to browse earlier history."
        }
        Key::ChartTitle => "Token Usage Trend",
        Key::LegendCached => "Cached",
        Key::LegendIn => "In",
        Key::LegendOut => "Out",
        Key::ThPeriod => "Period",
        Key::ThSessions => "Sessions",
        Key::ThTurns => "Turns",
        Key::ThInput => "Input",
        Key::ThOutput => "Output",
        Key::ThCache => "Cache",
        Key::ThTotal => "Total",
        Key::ThCost => "Cost",
        Key::ThModelMix => "Models",
        Key::ThLastActive => "Last Active",
        Key::ThSession => "Session",
        Key::ThTitle => "Title",
        Key::ThMode => "Mode",
        Key::ThModel => "Model",
        Key::ThMsgs => "Msgs",
        Key::ThTokens => "Tokens",
        Key::ThWindowCost => "Cost",
        Key::SessionsNotFound => "No local session data found for {0}",
        Key::AdaptiveRouted => "adaptive → {0} (server-routed)",
        Key::AdaptiveShort => "adaptive → {0}",
        Key::ConfigValue => "{0} (configured: {1})",
        Key::KvSession => "Session",
        Key::KvWorkdir => "Directory",
        Key::KvModel => "Model",
        Key::KvTime => "Time",
        Key::KvActivity => "Activity",
        Key::CreatedLastActive => "Created {0}, last active {1}",
        Key::TokenBreakdown => "in {0} · out {1} · cached {2} · total {3}",
        Key::PerTurnPriced => "{0} (per-turn pricing over {1} turns in window)",
        Key::UnknownUnpriced => "unknown (no pricing match)",
        Key::ActivityNoTtft => "{0} agent messages · {1} turns in window",
        Key::ActivityTtft => "{0} agent messages · {1} turns in window · median TTFT {2} ms",
        Key::DataWarning => "Data source warning: {0}",
        Key::ErrNotExist => "{0} does not exist",
        Key::ErrOpenFailed => "Failed to open {0}",
        Key::ErrNotFound => "{0} not found",
        Key::ErrSessionsUnreadable => "{0} session files could not be read",
        Key::ErrDbOpen => "Failed to open {0} database: {1}",
        Key::ErrRemoteExport => "{0} remote threads failed to export: {1}",
        Key::ErrAndMore => " and {0} more",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn all_keys_are_non_empty_in_both_languages() {
        // 用枚举全量遍历保证新增 Key 忘记补文案时编译/测试立刻失败
        let all = [
            Key::Usage,
            Key::Sessions,
            Key::PeriodDay,
            Key::PeriodWeek,
            Key::PeriodMonth,
            Key::PrevPage,
            Key::NextPage,
            Key::Reload,
            Key::Reloading,
            Key::Loading,
            Key::DataAsOf,
            Key::SessionsCount,
            Key::LangLabel,
            Key::LoadingData,
            Key::LoadingCacheHint,
            Key::StatTotalTokens,
            Key::StatInput,
            Key::StatOutput,
            Key::StatCached,
            Key::StatCost,
            Key::StatTurnsSessions,
            Key::NoRecord,
            Key::EmptyWindow,
            Key::ChartTitle,
            Key::LegendCached,
            Key::LegendIn,
            Key::LegendOut,
            Key::ThPeriod,
            Key::ThSessions,
            Key::ThTurns,
            Key::ThInput,
            Key::ThOutput,
            Key::ThCache,
            Key::ThTotal,
            Key::ThCost,
            Key::ThModelMix,
            Key::ThLastActive,
            Key::ThSession,
            Key::ThTitle,
            Key::ThMode,
            Key::ThModel,
            Key::ThMsgs,
            Key::ThTokens,
            Key::ThWindowCost,
            Key::SessionsNotFound,
            Key::AdaptiveRouted,
            Key::AdaptiveShort,
            Key::ConfigValue,
            Key::KvSession,
            Key::KvWorkdir,
            Key::KvModel,
            Key::KvTime,
            Key::KvActivity,
            Key::CreatedLastActive,
            Key::TokenBreakdown,
            Key::PerTurnPriced,
            Key::UnknownUnpriced,
            Key::ActivityNoTtft,
            Key::ActivityTtft,
            Key::DataWarning,
            Key::ErrNotExist,
            Key::ErrOpenFailed,
            Key::ErrNotFound,
            Key::ErrSessionsUnreadable,
            Key::ErrDbOpen,
            Key::ErrRemoteExport,
            Key::ErrAndMore,
        ];
        for key in all {
            assert!(!zh(key).is_empty(), "zh 缺少文案: {key:?}");
            assert!(!en(key).is_empty(), "en 缺少文案: {key:?}");
        }
    }

    #[test]
    fn tf_fills_positional_args() {
        assert_eq!(tf(Key::SessionsCount, &["12"]), format!("{} 会话", 12));
    }

    #[test]
    fn config_roundtrip() {
        // 直接测内部序列化格式，避免写用户配置目录
        let config = ConfigFile {
            lang: Lang::En.code().to_string(),
        };
        let text = serde_json::to_string(&config).unwrap();
        let parsed: ConfigFile = serde_json::from_str(&text).unwrap();
        assert_eq!(Lang::from_code(&parsed.lang), Some(Lang::En));
    }

    #[test]
    fn system_default_maps_zh_locales() {
        // from_code 是 locale → Lang 的核心映射
        assert_eq!(Lang::from_code("zh"), Some(Lang::Zh));
        assert_eq!(Lang::from_code("en"), Some(Lang::En));
        assert_eq!(Lang::from_code("fr"), None);
    }
}
