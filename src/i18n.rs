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

fn default_true() -> bool {
    true
}

#[derive(Serialize, Deserialize)]
struct ConfigFile {
    #[serde(default)]
    lang: String,
    #[serde(default = "default_true")]
    auto_check_updates: bool,
    #[serde(default)]
    skipped_update_version: Option<String>,
    #[serde(default)]
    last_update_check_at: Option<i64>,
}

impl Default for ConfigFile {
    fn default() -> Self {
        Self {
            lang: String::new(),
            auto_check_updates: true,
            skipped_update_version: None,
            last_update_check_at: None,
        }
    }
}

/// 自动更新相关偏好，与语言一起存在 `config.json`。
#[derive(Clone, Debug)]
pub struct UpdatePrefs {
    pub auto_check_updates: bool,
    pub skipped_update_version: Option<String>,
    pub last_update_check_at: Option<i64>,
}

impl Default for UpdatePrefs {
    fn default() -> Self {
        Self {
            auto_check_updates: true,
            skipped_update_version: None,
            last_update_check_at: None,
        }
    }
}

fn load_config_file() -> ConfigFile {
    let Some(path) = config_path() else {
        return ConfigFile::default();
    };
    let Ok(text) = std::fs::read_to_string(path) else {
        return ConfigFile::default();
    };
    serde_json::from_str(&text).unwrap_or_default()
}

fn write_config_file(config: &ConfigFile) {
    let Some(path) = config_path() else {
        return;
    };
    if let Some(parent) = path.parent() {
        let _ = std::fs::create_dir_all(parent);
    }
    let Ok(text) = serde_json::to_string_pretty(config) else {
        return;
    };
    // 与 data.rs 的缓存写盘一致：先写临时文件再 rename，避免留下半截 config
    let temp = path.with_extension(format!("json.tmp-{}", std::process::id()));
    if std::fs::write(&temp, text).is_ok() {
        if replace_config_file(&temp, &path).is_err() {
            let _ = std::fs::remove_file(&temp);
        }
    } else {
        let _ = std::fs::remove_file(&temp);
    }
}

fn load_config() -> Option<Lang> {
    Lang::from_code(&load_config_file().lang)
}

fn save_config(lang: Lang) {
    let mut config = load_config_file();
    config.lang = lang.code().to_string();
    write_config_file(&config);
}

pub fn load_update_prefs() -> UpdatePrefs {
    let config = load_config_file();
    UpdatePrefs {
        auto_check_updates: config.auto_check_updates,
        skipped_update_version: config.skipped_update_version,
        last_update_check_at: config.last_update_check_at,
    }
}

pub fn save_update_prefs(prefs: &UpdatePrefs) {
    let mut config = load_config_file();
    config.auto_check_updates = prefs.auto_check_updates;
    config.skipped_update_version = prefs.skipped_update_version.clone();
    config.last_update_check_at = prefs.last_update_check_at;
    write_config_file(&config);
}

#[cfg(not(target_os = "windows"))]
fn replace_config_file(temp: &std::path::Path, path: &std::path::Path) -> std::io::Result<()> {
    std::fs::rename(temp, path)
}

#[cfg(target_os = "windows")]
fn replace_config_file(temp: &std::path::Path, path: &std::path::Path) -> std::io::Result<()> {
    // Windows 的 std::fs::rename 不覆盖已有文件。配置很小；删除旧文件后立即
    // 提升同目录临时文件，确保第二次及后续语言切换仍能持久化。
    match std::fs::rename(temp, path) {
        Ok(()) => Ok(()),
        Err(_) if path.exists() => {
            std::fs::remove_file(path)?;
            std::fs::rename(temp, path)
        }
        Err(error) => Err(error),
    }
}

/// 带参数文案：把模板中的 {0} {1} … 依次替换为 args。
///
/// 逐个 `replace` 实现，因此参数值本身不得含 `{0}` 这类占位符字面量，
/// 否则会被后续参数二次替换。当前调用方传的都是数字、路径与模型名。
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

    // 订阅用量（Quota）
    QuotaTab,
    QuotaRefresh,
    QuotaRefreshing,
    QuotaLoading,
    QuotaUpdated,
    QuotaNoAccounts,
    QuotaAddDevin,
    QuotaValidatingCookie,
    QuotaErrClipboard,
    QuotaErrCookieInvalid,
    QuotaRemove,
    QuotaWindowFiveHour,
    QuotaWindowSevenDay,
    QuotaWindowSevenDayApps,
    QuotaWindowSevenDayOpus,
    QuotaWindowSevenDaySonnet,
    QuotaWindowDaily,
    QuotaWindowWeekly,
    QuotaWindowMonthly,
    QuotaResetsIn,
    QuotaExtra,
    QuotaError,

    CliUnpricedModels,
    CliPartialCostShort,
    CliPartialCostDetail,
    CliUntilBeforeSince,
    CliNextDayFailed,
    CliMissingValue,
    CliInvalidDays,
    CliDaysPositive,
    CliUnsupportedFormat,
    CliUnknownArgument,
    CliUnsupportedAgent,
    CliInvalidDate,
    CliLocalDateFailed,
    CliFatal,
    CliHelp,
    CliDate,
    CliAgent,
    CliModels,
    CliInput,
    CliOutput,
    CliCacheCreate,
    CliCacheRead,
    CliTotalTokens,
    CliCostUsd,
    CliTotal,
    CliAll,

    // 多设备同步
    DevicesSection,
    AgentsSection,
    AllDevices,
    ThisDevice,
    SyncOn,
    SyncOff,
    SyncExporting,
    SyncExported,
    SyncFailed,
    SyncExportFailed,
    SyncImporting,
    SyncNoRemote,
    SyncLastExport,
    SyncRemoteCount,
    SyncDirLabel,
    SyncDirNotSet,
    SyncDirPick,
    SyncDirReset,
    // WebDAV 后端
    SyncBackendLocal,
    SyncBackendWebDav,
    SyncWebDavUrl,
    SyncWebDavUrlPlaceholder,
    SyncWebDavUsername,
    SyncWebDavUsernamePlaceholder,
    SyncWebDavPassword,
    SyncWebDavPaste,
    SyncWebDavPasswordSet,
    SyncWebDavPasswordNotSet,
    SyncWebDavPasswordClear,
    SyncConfigTitle,
    SyncConfigSave,
    SyncConfigCancel,
    SyncConfigConfigure,

    // 应用自动更新
    UpdateAvailableShort,
    UpdateChecking,
    UpdateDialogTitle,
    UpdateDownload,
    UpdateDownloading,
    UpdateVerifying,
    UpdateRestartInstall,
    UpdateInstalling,
    UpdateReadyHint,
    UpdateSkipVersion,
    UpdateLater,
    UpdateOpenRelease,
    UpdateRetry,
    UpdateFailed,
}

impl Key {
    /// 全部文案 key，供测试遍历。用 `&[Key]` 而非 `[Key; N]`，新增 key 时不必同步改长度。
    pub const ALL: &[Key] = &[
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
        Key::QuotaTab,
        Key::QuotaRefresh,
        Key::QuotaRefreshing,
        Key::QuotaLoading,
        Key::QuotaUpdated,
        Key::QuotaNoAccounts,
        Key::QuotaAddDevin,
        Key::QuotaValidatingCookie,
        Key::QuotaErrClipboard,
        Key::QuotaErrCookieInvalid,
        Key::QuotaRemove,
        Key::QuotaWindowFiveHour,
        Key::QuotaWindowSevenDay,
        Key::QuotaWindowSevenDayApps,
        Key::QuotaWindowSevenDayOpus,
        Key::QuotaWindowSevenDaySonnet,
        Key::QuotaWindowDaily,
        Key::QuotaWindowWeekly,
        Key::QuotaWindowMonthly,
        Key::QuotaResetsIn,
        Key::QuotaExtra,
        Key::QuotaError,
        Key::CliUnpricedModels,
        Key::CliPartialCostShort,
        Key::CliPartialCostDetail,
        Key::CliUntilBeforeSince,
        Key::CliNextDayFailed,
        Key::CliMissingValue,
        Key::CliInvalidDays,
        Key::CliDaysPositive,
        Key::CliUnsupportedFormat,
        Key::CliUnknownArgument,
        Key::CliUnsupportedAgent,
        Key::CliInvalidDate,
        Key::CliLocalDateFailed,
        Key::CliFatal,
        Key::CliHelp,
        Key::CliDate,
        Key::CliAgent,
        Key::CliModels,
        Key::CliInput,
        Key::CliOutput,
        Key::CliCacheCreate,
        Key::CliCacheRead,
        Key::CliTotalTokens,
        Key::CliCostUsd,
        Key::CliTotal,
        Key::CliAll,
        Key::DevicesSection,
        Key::AgentsSection,
        Key::AllDevices,
        Key::ThisDevice,
        Key::SyncOn,
        Key::SyncOff,
        Key::SyncExporting,
        Key::SyncExported,
        Key::SyncFailed,
        Key::SyncExportFailed,
        Key::SyncImporting,
        Key::SyncNoRemote,
        Key::SyncLastExport,
        Key::SyncRemoteCount,
        Key::SyncDirLabel,
        Key::SyncDirNotSet,
        Key::SyncDirPick,
        Key::SyncDirReset,
        Key::SyncBackendLocal,
        Key::SyncBackendWebDav,
        Key::SyncWebDavUrl,
        Key::SyncWebDavUrlPlaceholder,
        Key::SyncWebDavUsername,
        Key::SyncWebDavUsernamePlaceholder,
        Key::SyncWebDavPassword,
        Key::SyncWebDavPaste,
        Key::SyncWebDavPasswordSet,
        Key::SyncWebDavPasswordNotSet,
        Key::SyncWebDavPasswordClear,
        Key::SyncConfigTitle,
        Key::SyncConfigSave,
        Key::SyncConfigCancel,
        Key::SyncConfigConfigure,
        Key::UpdateAvailableShort,
        Key::UpdateChecking,
        Key::UpdateDialogTitle,
        Key::UpdateDownload,
        Key::UpdateDownloading,
        Key::UpdateVerifying,
        Key::UpdateRestartInstall,
        Key::UpdateInstalling,
        Key::UpdateReadyHint,
        Key::UpdateSkipVersion,
        Key::UpdateLater,
        Key::UpdateOpenRelease,
        Key::UpdateRetry,
        Key::UpdateFailed,
    ];
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
        Key::QuotaTab => "订阅用量",
        Key::QuotaRefresh => "刷新",
        Key::QuotaRefreshing => "正在查询…",
        Key::QuotaLoading => "正在查询订阅配额",
        Key::QuotaUpdated => "更新于 {0}",
        Key::QuotaNoAccounts =>
            "未检测到已登录账号。先在 claude / codex / grok CLI 登录，或粘贴 app.devin.ai 的 Cookie 添加 Devin 账号。",
        Key::QuotaAddDevin => "添加 Devin 账号（粘贴 Cookie）",
        Key::QuotaValidatingCookie => "正在验证 Cookie…",
        Key::QuotaErrClipboard => "剪贴板为空或无法读取",
        Key::QuotaErrCookieInvalid => "Cookie 验证失败：{0}",
        Key::QuotaRemove => "移除",
        Key::QuotaWindowFiveHour => "5 小时窗口",
        Key::QuotaWindowSevenDay => "7 天窗口",
        Key::QuotaWindowSevenDayApps => "7 天 · OAuth 应用",
        Key::QuotaWindowSevenDayOpus => "7 天 · Opus",
        Key::QuotaWindowSevenDaySonnet => "7 天 · Sonnet",
        Key::QuotaWindowDaily => "日配额",
        Key::QuotaWindowWeekly => "周配额",
        Key::QuotaWindowMonthly => "月配额",
        Key::QuotaResetsIn => "{0} 后重置",
        Key::QuotaExtra => "附加：{0}",
        Key::QuotaError => "查询失败：{0}",
        Key::CliUnpricedModels => "警告：以下模型未找到定价，Cost 未包含它们：{0}",
        Key::CliPartialCostShort => "≥{0}",
        Key::CliPartialCostDetail => "{0}（仅 {1}/{2} 轮可定价，实际费用不低于此值）",
        Key::CliUntilBeforeSince => "--until {0} 不能早于 --since {1}",
        Key::CliNextDayFailed => "无法计算 {0} 的下一天",
        Key::CliMissingValue => "{0} 缺少值",
        Key::CliInvalidDays => "无效的 --days：{0}",
        Key::CliDaysPositive => "--days 必须大于 0 且处于有效日期范围内",
        Key::CliUnsupportedFormat => "不支持的格式：{0}",
        Key::CliUnknownArgument => "未知参数：{0}",
        Key::CliUnsupportedAgent => "不支持的 Agent：{0}",
        Key::CliInvalidDate => "无效日期：{0}，应为 YYYY-MM-DD 或 YYYYMMDD",
        Key::CliLocalDateFailed => "无法解析本地日期：{0}",
        Key::CliFatal => "错误：{0}\n使用 --cli --help 查看帮助。",
        Key::CliHelp => {
            "Agent Usage Metrics CLI\n\n\
用法：\n  devin-usage-metrics --cli [选项]\n\n\
选项：\n  --agent <name>       Agent 或 all，默认 claude\n  --days <n>           最近 N 天，默认 30\n  --since <date>       起始日期（YYYY-MM-DD 或 YYYYMMDD）\n  --until <date>       结束日期，包含当天\n  --format table|csv   输出格式，默认 table\n  --by-agent           增加按 Agent 分项\n  --refresh            忽略缓存，重新读取本地数据\n  -h, --help           显示帮助\n\n\
示例：\n  devin-usage-metrics --cli --agent claude --since 2026-08-20 --until 2026-08-30 --refresh\n  devin-usage-metrics --cli --agent all --by-agent --since 2026-08-20 --until 2026-08-30\n  devin-usage-metrics --cli --agent claude --days 30 --format csv"
        }
        Key::CliDate => "日期",
        Key::CliAgent => "Agent",
        Key::CliModels => "模型",
        Key::CliInput => "输入",
        Key::CliOutput => "输出",
        Key::CliCacheCreate => "缓存写入",
        Key::CliCacheRead => "缓存读取",
        Key::CliTotalTokens => "总 Tokens",
        Key::CliCostUsd => "费用 (USD)",
        Key::CliTotal => "合计",
        Key::CliAll => "全部",
        // 多设备同步
        Key::DevicesSection => "设备",
        Key::AgentsSection => "代理",
        Key::AllDevices => "全部",
        Key::ThisDevice => "本机",
        Key::SyncOn => "同步已开",
        Key::SyncOff => "同步已关",
        Key::SyncExporting => "同步中…",
        Key::SyncExported => "已同步",
        Key::SyncFailed => "同步失败",
        Key::SyncExportFailed => "同步失败：{0}",
        Key::SyncImporting => "导入其他设备…",
        Key::SyncNoRemote => "暂无其他设备数据",
        Key::SyncLastExport => "上次同步 {0}",
        Key::SyncRemoteCount => "已发现 {0} 台其他设备",
        Key::SyncDirLabel => "同步目录",
        Key::SyncDirNotSet => "未配置（使用默认）",
        Key::SyncDirPick => "选择…",
        Key::SyncDirReset => "重置",
        // WebDAV 后端
        Key::SyncBackendLocal => "本地目录",
        Key::SyncBackendWebDav => "WebDAV",
        Key::SyncWebDavUrl => "WebDAV 地址",
        Key::SyncWebDavUrlPlaceholder => "https://dav.example.com/path/",
        Key::SyncWebDavUsername => "用户名",
        Key::SyncWebDavUsernamePlaceholder => "username",
        Key::SyncWebDavPasswordSet => "密码已设置",
        Key::SyncWebDavPasswordNotSet => "密码未设置",
        Key::SyncWebDavPasswordClear => "清除密码",
        Key::SyncWebDavPassword => "密码",
        Key::SyncWebDavPaste => "粘贴",
        Key::SyncConfigTitle => "同步设置",
        Key::SyncConfigSave => "保存",
        Key::SyncConfigCancel => "取消",
        Key::SyncConfigConfigure => "配置…",
        Key::UpdateAvailableShort => "有新版本",
        Key::UpdateChecking => "正在检查…",
        Key::UpdateDialogTitle => "应用更新",
        Key::UpdateDownload => "下载更新",
        Key::UpdateDownloading => "正在下载…",
        Key::UpdateVerifying => "正在校验…",
        Key::UpdateRestartInstall => "重启安装",
        Key::UpdateInstalling => "正在安装…",
        Key::UpdateReadyHint => "新版本已下载并校验通过，重启后即可完成安装。",
        Key::UpdateSkipVersion => "跳过此版本",
        Key::UpdateLater => "稍后",
        Key::UpdateOpenRelease => "打开发布页",
        Key::UpdateRetry => "重试",
        Key::UpdateFailed => "更新失败",
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
        Key::QuotaTab => "Quota",
        Key::QuotaRefresh => "Refresh",
        Key::QuotaRefreshing => "Querying…",
        Key::QuotaLoading => "Querying subscription quotas",
        Key::QuotaUpdated => "Updated {0}",
        Key::QuotaNoAccounts =>
            "No logged-in accounts detected. Log in via the claude / codex / grok CLI first, or paste an app.devin.ai cookie to add a Devin account.",
        Key::QuotaAddDevin => "Add Devin account (paste cookie)",
        Key::QuotaValidatingCookie => "Validating cookie…",
        Key::QuotaErrClipboard => "Clipboard is empty or unreadable",
        Key::QuotaErrCookieInvalid => "Cookie validation failed: {0}",
        Key::QuotaRemove => "Remove",
        Key::QuotaWindowFiveHour => "5-hour window",
        Key::QuotaWindowSevenDay => "7-day window",
        Key::QuotaWindowSevenDayApps => "7-day · OAuth apps",
        Key::QuotaWindowSevenDayOpus => "7-day · Opus",
        Key::QuotaWindowSevenDaySonnet => "7-day · Sonnet",
        Key::QuotaWindowDaily => "Daily quota",
        Key::QuotaWindowWeekly => "Weekly quota",
        Key::QuotaWindowMonthly => "Monthly quota",
        Key::QuotaResetsIn => "resets in {0}",
        Key::QuotaExtra => "Extra: {0}",
        Key::QuotaError => "Query failed: {0}",
        Key::CliUnpricedModels => {
            "Warning: no pricing found for these models; Cost excludes them: {0}"
        }
        Key::CliPartialCostShort => "≥{0}",
        Key::CliPartialCostDetail => {
            "{0} (only {1}/{2} turns are priced; actual cost is at least this amount)"
        }
        Key::CliUntilBeforeSince => "--until {0} cannot be earlier than --since {1}",
        Key::CliNextDayFailed => "Cannot calculate the day after {0}",
        Key::CliMissingValue => "{0} requires a value",
        Key::CliInvalidDays => "Invalid --days value: {0}",
        Key::CliDaysPositive => "--days must be positive and within the supported date range",
        Key::CliUnsupportedFormat => "Unsupported format: {0}",
        Key::CliUnknownArgument => "Unknown argument: {0}",
        Key::CliUnsupportedAgent => "Unsupported agent: {0}",
        Key::CliInvalidDate => "Invalid date: {0}; expected YYYY-MM-DD or YYYYMMDD",
        Key::CliLocalDateFailed => "Cannot resolve local date: {0}",
        Key::CliFatal => "Error: {0}\nRun --cli --help for usage.",
        Key::CliHelp => {
            "Agent Usage Metrics CLI\n\n\
Usage:\n  devin-usage-metrics --cli [options]\n\n\
Options:\n  --agent <name>       Agent or all; default: claude\n  --days <n>           Most recent N days; default: 30\n  --since <date>       Start date (YYYY-MM-DD or YYYYMMDD)\n  --until <date>       Inclusive end date\n  --format table|csv   Output format; default: table\n  --by-agent           Include per-agent rows\n  --refresh            Ignore cache and reload local data\n  -h, --help           Show help\n\n\
Examples:\n  devin-usage-metrics --cli --agent claude --since 2026-08-20 --until 2026-08-30 --refresh\n  devin-usage-metrics --cli --agent all --by-agent --since 2026-08-20 --until 2026-08-30\n  devin-usage-metrics --cli --agent claude --days 30 --format csv"
        }
        Key::CliDate => "Date",
        Key::CliAgent => "Agent",
        Key::CliModels => "Models",
        Key::CliInput => "Input",
        Key::CliOutput => "Output",
        Key::CliCacheCreate => "Cache Create",
        Key::CliCacheRead => "Cache Read",
        Key::CliTotalTokens => "Total Tokens",
        Key::CliCostUsd => "Cost (USD)",
        Key::CliTotal => "Total",
        Key::CliAll => "All",
        // 多设备同步
        Key::DevicesSection => "Devices",
        Key::AgentsSection => "Agents",
        Key::AllDevices => "All Devices",
        Key::ThisDevice => "This Device",
        Key::SyncOn => "Sync On",
        Key::SyncOff => "Sync Off",
        Key::SyncExporting => "Syncing…",
        Key::SyncExported => "Synced",
        Key::SyncFailed => "Sync failed",
        Key::SyncExportFailed => "Sync failed: {0}",
        Key::SyncImporting => "Importing other devices…",
        Key::SyncNoRemote => "No other device data",
        Key::SyncLastExport => "Last sync {0}",
        Key::SyncRemoteCount => "Found {0} other device(s)",
        Key::SyncDirLabel => "Sync folder",
        Key::SyncDirNotSet => "Not set (using default)",
        Key::SyncDirPick => "Browse…",
        Key::SyncDirReset => "Reset",
        // WebDAV backend
        Key::SyncBackendLocal => "Local folder",
        Key::SyncBackendWebDav => "WebDAV",
        Key::SyncWebDavUrl => "WebDAV URL",
        Key::SyncWebDavUrlPlaceholder => "https://dav.example.com/path/",
        Key::SyncWebDavUsername => "Username",
        Key::SyncWebDavUsernamePlaceholder => "username",
        Key::SyncWebDavPasswordSet => "Password set",
        Key::SyncWebDavPasswordNotSet => "Password not set",
        Key::SyncWebDavPasswordClear => "Clear password",
        Key::SyncWebDavPassword => "Password",
        Key::SyncWebDavPaste => "Paste",
        Key::SyncConfigTitle => "Sync settings",
        Key::SyncConfigSave => "Save",
        Key::SyncConfigCancel => "Cancel",
        Key::SyncConfigConfigure => "Configure…",
        Key::UpdateAvailableShort => "Update available",
        Key::UpdateChecking => "Checking…",
        Key::UpdateDialogTitle => "App update",
        Key::UpdateDownload => "Download",
        Key::UpdateDownloading => "Downloading…",
        Key::UpdateVerifying => "Verifying…",
        Key::UpdateRestartInstall => "Restart to install",
        Key::UpdateInstalling => "Installing…",
        Key::UpdateReadyHint => {
            "The update is downloaded and verified. Restart to finish installing."
        }
        Key::UpdateSkipVersion => "Skip this version",
        Key::UpdateLater => "Later",
        Key::UpdateOpenRelease => "Open release page",
        Key::UpdateRetry => "Retry",
        Key::UpdateFailed => "Update failed",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn all_keys_are_non_empty_in_both_languages() {
        // zh()/en() 的 match 不带 `_ =>`，编译器已保证每个 key 都有分支；
        // 这里只补查编译器查不到的那类错误：分支写了，但文案是空串。
        for &key in Key::ALL {
            assert!(!zh(key).is_empty(), "zh 缺少文案: {key:?}");
            assert!(!en(key).is_empty(), "en 缺少文案: {key:?}");
        }
    }

    #[test]
    fn both_languages_use_the_same_placeholders() {
        // tf() 按位置逐个替换，某一侧漏写 {n} 或写错下标会静默错位；
        // 占位符集合一致是 tf 能正确填充的前提，编译器查不到。
        for &key in Key::ALL {
            assert_eq!(
                placeholders(zh(key)),
                placeholders(en(key)),
                "{key:?} 的中英文占位符不一致"
            );
        }
    }

    fn placeholders(template: &str) -> Vec<u32> {
        let mut found: Vec<u32> = template
            .match_indices('{')
            .filter_map(|(index, _)| {
                template[index + 1..]
                    .chars()
                    .take_while(char::is_ascii_digit)
                    .collect::<String>()
                    .parse()
                    .ok()
            })
            .collect();
        found.sort_unstable();
        found.dedup();
        found
    }

    #[test]
    fn tf_fills_positional_args() {
        // 不依赖全局 LANG：只验证实参填进去了、占位符没残留
        let filled = tf(Key::SessionsCount, &["12"]);
        assert!(filled.contains("12"), "未填入实参: {filled}");
        assert!(!filled.contains("{0}"), "占位符未替换干净: {filled}");
    }

    #[test]
    fn config_roundtrip() {
        // 直接测内部序列化格式，避免写用户配置目录
        let config = ConfigFile {
            lang: Lang::En.code().to_string(),
            auto_check_updates: false,
            skipped_update_version: Some("0.2.0".into()),
            last_update_check_at: Some(1_700_000_000),
        };
        let text = serde_json::to_string(&config).unwrap();
        let parsed: ConfigFile = serde_json::from_str(&text).unwrap();
        assert_eq!(Lang::from_code(&parsed.lang), Some(Lang::En));
        assert!(!parsed.auto_check_updates);
        assert_eq!(parsed.skipped_update_version.as_deref(), Some("0.2.0"));
        assert_eq!(parsed.last_update_check_at, Some(1_700_000_000));
    }

    #[test]
    fn old_config_without_update_fields_defaults() {
        let parsed: ConfigFile = serde_json::from_str(r#"{"lang":"zh"}"#).unwrap();
        assert_eq!(Lang::from_code(&parsed.lang), Some(Lang::Zh));
        assert!(parsed.auto_check_updates);
        assert!(parsed.skipped_update_version.is_none());
        assert!(parsed.last_update_check_at.is_none());
    }

    #[test]
    fn system_default_maps_zh_locales() {
        // from_code 是 locale → Lang 的核心映射
        assert_eq!(Lang::from_code("zh"), Some(Lang::Zh));
        assert_eq!(Lang::from_code("en"), Some(Lang::En));
        assert_eq!(Lang::from_code("fr"), None);
    }
}
