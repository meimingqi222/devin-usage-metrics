#![allow(unexpected_cfgs)]
// On Windows, hide the console window in release builds. In debug builds we
// keep the console so println!/eprintln! output is still visible while developing.
#![cfg_attr(
    all(target_os = "windows", not(debug_assertions)),
    windows_subsystem = "windows"
)]

mod compaction_ui;
mod model_distribution;
mod text_input;
mod tray;
mod tray_controller;
mod tray_model;
mod updater_ui;
mod usage_table;

use agg::{build_buckets_for_device, format_window_range, window_for, Bucket, PeriodKind};
use chrono::TimeZone;
use data::{AgentKind, LoadedData};
use devin_usage_metrics::compaction_config::current_thresholds;
use devin_usage_metrics::{agg, cli, compaction, data, i18n, pricing, quota, sync};
use gpui::{
    div, point, prelude::*, px, relative, rgb, size, Animation, AnimationExt as _, AnyElement, App,
    Application, Bounds, ClipboardItem, Context, Entity, FocusHandle, KeyDownEvent, MouseButton,
    Render, SharedString, Task, TitlebarOptions, Window, WindowBounds, WindowControlArea,
    WindowHandle, WindowOptions,
};
use rayon::prelude::*;
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};
use text_input::TextInput;

const BG: u32 = 0x101014;
const PANEL: u32 = 0x1a1a21;
const PANEL2: u32 = 0x22222b;
const BORDER: u32 = 0x2c2c38;
const TEXT: u32 = 0xe6e6ee;
const MUTED: u32 = 0x8f8fa3;
const ACCENT: u32 = 0x4cc2ff;
const C_IN: u32 = 0x4cc2ff;
const C_OUT: u32 = 0x3ddc97;
const C_CACHED: u32 = 0x8b7cf6;
/// 缓存写入：与缓存读取同色系但更浅，让堆叠图里两者可区分
const C_CACHE_WRITE: u32 = 0xc4b5fd;
const C_COST: u32 = 0xfbbf24;
const RED: u32 = 0xf87171;
const GREEN: u32 = 0x3ddc97;
/// 同步采集涵盖 CLI、SQLite 与大量 JSONL 文件。限制外层并行度以重叠 I/O，
/// 同时让各数据源内部的 Rayon 任务共享同一个池，避免过度抢占磁盘和 CPU。
const SYNC_COLLECTION_CONCURRENCY: usize = 4;

const MODEL_COLORS: [u32; 10] = [
    0xf472b6, 0xfbbf24, 0x60a5fa, 0x34d399, 0xa78bfa, 0xfb923c, 0xf87171, 0x4ade80, 0x22d3ee,
    0xfacc15,
];

fn fmt_tokens(n: f64) -> String {
    if n >= 1e9 {
        format!("{:.2}B", n / 1e9)
    } else if n >= 1e6 {
        format!("{:.1}M", n / 1e6)
    } else if n >= 1e3 {
        format!("{:.1}K", n / 1e3)
    } else {
        format!("{:.0}", n)
    }
}

fn fmt_ts(sec: i64) -> String {
    chrono::Local
        .timestamp_opt(sec, 0)
        .single()
        .map(|t| t.format("%m-%d %H:%M").to_string())
        .unwrap_or_else(|| "-".into())
}

fn rgba(hex: u32, alpha: f32) -> gpui::Hsla {
    let mut c = gpui::Hsla::from(rgb(hex));
    c.a = alpha;
    c
}

fn model_color(name: &str) -> u32 {
    let mut h: usize = 0;
    for b in name.bytes() {
        h = h.wrapping_mul(31).wrapping_add(b as usize);
    }
    MODEL_COLORS[h % MODEL_COLORS.len()]
}

#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
enum ViewMode {
    #[default]
    AgentHistory,
    LiveQuota,
}

#[derive(Clone, Copy, PartialEq, Eq, Debug, Default)]
enum Tab {
    #[default]
    Usage,
    Sessions,
    Compaction,
}

/// 左侧边栏可折叠的分区。
#[derive(Clone, Copy, PartialEq, Debug)]
enum SidebarSection {
    Agents,
    Devices,
}

#[derive(Clone, Copy, PartialEq, Eq, Default, Debug)]
enum SettingsTab {
    #[default]
    Sync,
    General,
}

/// 订阅配额卡片：一个账号一条，后台查询完成后逐条更新。
#[derive(Clone)]
struct QuotaCard {
    key: String,
    provider: quota::Provider,
    label: String,
    result: Result<quota::QuotaResult, String>,
}

/// 会话列表的一行，在数据变化时预计算好展示所需的字符串，
/// 避免每次 render 都重新排序、查定价表、截断标题。
struct SessionRow {
    session: data::SessionRec,
    time_str: String,
    id_short: String,
    title_short: String,
    model_text: String,
    cost_str: String,
    cost_val: Option<f64>,
    total: f64,
}

/// 会话级 token 合计（input + output + cache_read + cache_creation）。
///
/// 口径与定价一致：这四类 token 正是计费公式的输入，漏掉缓存写入会让会话列表
/// 与每日汇总、CLI 三处对不上。
///
/// 优先用 `sessions.metadata.response_dimensions` 里的累计值；但 Devin 常常不
/// 写这个字段（本机 85 个会话里 22 个在窗口内有真实用量而 metadata 合计为 0），
/// 直接取用会把这些会话显示成 0。此时回退到窗口内实际 turn 的合计。
fn session_token_total(session: &data::SessionRec, turns: &[&data::TurnRec]) -> f64 {
    let from_meta = session.input_tokens
        + session.output_tokens
        + session.cached_tokens
        + session.cache_creation_tokens;
    if from_meta > 0.0 || turns.is_empty() {
        return from_meta;
    }
    turns
        .iter()
        .map(|turn| {
            turn.input_tokens
                + turn.output_tokens
                + turn.cache_read_tokens
                + turn.cache_creation_tokens
        })
        .sum()
}

struct Root {
    data: Arc<LoadedData>,
    loaded_agents: HashMap<AgentKind, Arc<LoadedData>>,
    agent: AgentKind,
    view_mode: ViewMode,
    tab: Tab,
    period: PeriodKind,
    page: usize,
    buckets: Vec<Bucket>,
    expanded_buckets: std::collections::HashSet<i64>,
    sessions_rows: Vec<SessionRow>,
    compaction_report: Option<compaction::Report>,
    compaction_key: Option<(usize, AgentKind, PeriodKind, usize, Option<String>)>,
    compaction_task: Option<Task<()>>,
    compaction_config_task: Option<Task<()>>,
    compaction_notice: Option<(bool, String)>,
    compaction_details_open: bool,
    selected: Option<String>,
    loaded_at: String,
    loading: bool,
    has_loaded: bool,
    load_task: Option<Task<()>>,
    load_id: u64,
    quota_cards: Vec<QuotaCard>,
    quota_loading: bool,
    quota_load_id: u64,
    quota_task: Option<Task<()>>,
    quota_tick_task: Option<Task<()>>,
    quota_updated_at: String,
    quota_message: Option<String>,
    /// 用户是否已显式同意读取 macOS Keychain（持久化）
    quota_keychain_allowed: bool,
    /// 本次会话内「暂不」隐藏提示
    quota_keychain_dismissed: bool,
    // 多设备同步
    /// None = 汇总所有设备；Some(id) = 只看指定设备
    device_filter: Option<String>,
    /// 从同步目录导入的其他设备数据（全部 agent）
    remote_data: Arc<LoadedData>,
    /// 同步目录中发现的设备列表
    known_devices: Vec<sync::RemoteDevice>,
    /// 是否正在导出/导入
    sync_busy: bool,
    /// 用来丢弃过期的同步任务结果（关掉同步或新一轮开始时递增）
    sync_id: u64,
    /// 上次同步时间（HH:MM:SS）
    sync_last_at: String,
    /// 上次同步完成的单调时钟时间，用于判断刷新后是否需要补同步。
    sync_last_completed: Option<Instant>,
    /// 同步失败原因（成功时为 None）
    sync_message: Option<String>,
    /// 连续临时失败次数，用于指数退避；成功后清零。
    sync_retry_attempt: u8,
    /// 同步是否已开启（持久化在 sync.json）
    sync_enabled: bool,
    /// 自建同步 API 地址；用户只需填写这一项。
    sync_api_url: String,
    /// GitHub Device Flow 的进行状态。
    github_login_busy: bool,
    github_login_id: u64,
    github_login_code: Option<String>,
    github_login_code_copied: bool,
    github_login_task: Option<Task<()>>,
    /// 同步设置弹窗是否打开
    sync_config_open: bool,
    /// 弹窗输入框（内容在打开时从配置填入，保存时读回）
    modal_url_input: Entity<TextInput>,
    /// 弹窗的键盘焦点句柄（Esc 关闭）
    modal_focus: FocusHandle,
    /// 左侧边栏各分区展开状态
    section_agents_open: bool,
    section_devices_open: bool,
    section_other_agents_open: bool,
    /// 设置弹窗当前激活的 Tab
    settings_tab: SettingsTab,
    /// 是否隐藏无活动日期
    hide_empty_buckets: bool,
    /// 自动同步定时任务
    sync_tick_task: Option<Task<()>>,
    /// 正在进行的一次导出+导入（与 load_task 分开，避免点重新加载打断同步）
    sync_task: Option<Task<()>>,
    /// 应用自动更新
    update: updater_ui::UpdateState,
    update_prefs: i18n::UpdatePrefs,
    // 会话管理
    session_search_input: Entity<TextInput>,
    session_prefs: i18n::SessionPrefs,
    sessions_collapsed_projects: std::collections::HashSet<String>,
    sessions_show_hidden: bool,
    session_feedback: Option<(String, std::time::Instant)>,
}

fn open_external_url(url: &str) -> Result<(), String> {
    #[cfg(target_os = "windows")]
    let result = std::process::Command::new("explorer.exe").arg(url).spawn();
    #[cfg(target_os = "macos")]
    let result = std::process::Command::new("open").arg(url).spawn();
    #[cfg(all(not(target_os = "windows"), not(target_os = "macos")))]
    let result = std::process::Command::new("xdg-open").arg(url).spawn();
    result
        .map(|_| ())
        .map_err(|e| format!("无法打开浏览器: {e}"))
}

fn open_folder(path: &str) {
    let p = std::path::Path::new(path);
    if !p.exists() {
        return;
    }
    #[cfg(target_os = "windows")]
    let _ = std::process::Command::new("explorer.exe").arg(path).spawn();
    #[cfg(target_os = "macos")]
    let _ = std::process::Command::new("open").arg(path).spawn();
    #[cfg(all(not(target_os = "windows"), not(target_os = "macos")))]
    let _ = std::process::Command::new("xdg-open").arg(path).spawn();
}

impl Root {
    fn set_session_feedback(&mut self, text: impl Into<String>, cx: &mut Context<Self>) {
        self.session_feedback = Some((text.into(), Instant::now()));
        cx.notify();
    }

    fn copy_to_clipboard(&mut self, text: String, feedback: &'static str, cx: &mut Context<Self>) {
        cx.write_to_clipboard(ClipboardItem::new_string(text));
        self.set_session_feedback(feedback, cx);
    }

    fn toggle_project_collapsed(&mut self, project: &str, cx: &mut Context<Self>) {
        if self.sessions_collapsed_projects.contains(project) {
            self.sessions_collapsed_projects.remove(project);
        } else {
            self.sessions_collapsed_projects.insert(project.to_string());
        }
        cx.notify();
    }

    fn hide_session(&mut self, key: &str, cx: &mut Context<Self>) {
        self.session_prefs
            .hidden_session_keys
            .insert(key.to_string());
        i18n::save_session_prefs(&self.session_prefs);
        self.set_session_feedback(i18n::t(i18n::Key::SessionsHide), cx);
    }

    fn restore_session(&mut self, key: &str, cx: &mut Context<Self>) {
        self.session_prefs.hidden_session_keys.remove(key);
        i18n::save_session_prefs(&self.session_prefs);
        self.set_session_feedback(i18n::t(i18n::Key::SessionsRestore), cx);
    }

    fn switch_to_quota(&mut self, cx: &mut Context<Self>) {
        if self.view_mode == ViewMode::LiveQuota {
            cx.notify();
            return;
        }
        self.view_mode = ViewMode::LiveQuota;
        if !self.quota_loading && self.quota_updated_at.is_empty() {
            self.start_quota_load(false, cx);
        }
        cx.notify();
    }

    fn switch_agent(&mut self, target_agent: AgentKind, cx: &mut Context<Self>) {
        let was_quota = self.view_mode == ViewMode::LiveQuota;
        self.view_mode = ViewMode::AgentHistory;
        if !was_quota && self.agent == target_agent && self.has_loaded {
            cx.notify();
            return;
        }

        self.agent = target_agent;
        if !self.agent.supports_compaction() && self.tab == Tab::Compaction {
            self.tab = Tab::Usage;
        }
        self.selected = None;
        self.page = 0;

        if let Some(cached_data) = self.loaded_agents.get(&target_agent) {
            self.data = cached_data.clone();
            self.rebuild_buckets();
            self.rebuild_sessions();
            cx.notify();
            return;
        }

        self.start_load(false, cx);
    }

    fn start_load(&mut self, force: bool, cx: &mut Context<Self>) {
        // 序号递增后，上一个加载任务完成时会因 load_id 不匹配而丢弃结果，
        // 不会覆盖当前 Agent 的界面数据（GPUI 0.2 的 Task 没有 cancel 接口）
        self.load_id += 1;
        let load_id = self.load_id;
        self.loading = true;
        self.selected = None;
        cx.notify();

        let (start, end) = window_for(self.period, self.page);
        let agent = self.agent;
        let previous = self.data.clone();
        let load = cx.background_executor().spawn(async move {
            if force {
                data::reload_agent_from(previous, start, end, agent)
            } else {
                data::load_agent(agent, start, end)
            }
        });
        self.load_task = Some(cx.spawn(async move |this, cx| {
            let data = load.await;
            this.update(cx, |this, cx| {
                // 已有更新的加载任务时，直接丢弃本次结果
                if this.load_id != load_id {
                    return;
                }
                // 合并远程设备中该 agent 的数据
                let merged = this.merge_remote_for_agent(data, agent);
                let arc_data = Arc::new(merged);
                this.loaded_agents.insert(agent, arc_data.clone());
                this.loading = false;
                // 加载期间用户可能已切换到其他 Agent，只缓存结果，不覆盖当前界面
                if this.agent == agent {
                    this.data = arc_data;
                    this.has_loaded = true;
                    this.loaded_at = chrono::Local::now().format("%H:%M:%S").to_string();
                    this.rebuild_buckets();
                    this.rebuild_sessions();
                    cx.notify();
                }
                // 刷新完成后，如果距离上次同步已超过 5 分钟，则补做一次同步。
                // 首次加载因没有上次同步时间，也会立即同步一次。
                let refresh_sync_due = this
                    .sync_last_completed
                    .map(|at| at.elapsed() >= Duration::from_secs(5 * 60))
                    .unwrap_or(true);
                if this.sync_enabled && !this.sync_busy && refresh_sync_due {
                    this.start_sync(cx);
                }
            })
            .ok();
        }));
    }

    /// 把远程数据中指定 agent 的部分合并进本机数据。
    fn merge_remote_for_agent(&self, local: LoadedData, agent: AgentKind) -> LoadedData {
        if self.remote_data.sessions.is_empty() && self.remote_data.turns.is_empty() {
            return local;
        }
        let remote_agent = LoadedData {
            sessions: self
                .remote_data
                .sessions
                .iter()
                .filter(|s| s.agent == agent)
                .cloned()
                .collect(),
            turns: self
                .remote_data
                .turns
                .iter()
                .filter(|t| t.agent == agent)
                .cloned()
                .collect(),
            ..Default::default()
        };
        sync::merge_local_with_remote(&local, &remote_agent)
    }

    fn rebuild_buckets(&mut self) {
        self.expanded_buckets.clear();
        self.buckets = build_buckets_for_device(
            &self.data,
            self.period,
            self.agent,
            self.device_filter.as_deref(),
            self.page,
        );
    }

    fn switch_tab(&mut self, kind: Tab, cx: &mut Context<Self>) {
        self.view_mode = ViewMode::AgentHistory;
        self.tab = kind;
        cx.notify();
    }

    /// 切换设备筛选。None = 汇总所有设备。
    fn switch_device(&mut self, device_id: Option<String>, cx: &mut Context<Self>) {
        let was_quota = self.view_mode == ViewMode::LiveQuota;
        self.view_mode = ViewMode::AgentHistory;
        if !was_quota && self.device_filter == device_id {
            cx.notify();
            return;
        }
        self.device_filter = device_id;
        self.rebuild_buckets();
        self.rebuild_sessions();
        cx.notify();
    }

    fn build_sync_config(&self) -> sync::SyncConfig {
        sync::SyncConfig {
            enabled: self.sync_enabled,
            api_url: self.sync_api_url.clone(),
        }
    }

    /// 开启/关闭自动同步。切换后立即持久化，开启时立即执行一次同步并启动定时任务。
    fn toggle_sync(&mut self, cx: &mut Context<Self>) {
        if !self.sync_enabled && !sync::github_sync_session_is_valid() {
            self.sync_message = Some("请先在同步设置中登录 GitHub".into());
            cx.notify();
            return;
        }
        self.sync_enabled = !self.sync_enabled;
        sync::save_config(&self.build_sync_config());

        if self.sync_enabled {
            // 开启：立即同步一次，然后启动定时任务
            self.start_sync(cx);
        } else {
            // 关闭：停止定时任务，丢弃进行中的同步结果
            self.sync_id += 1;
            self.sync_tick_task = None;
            self.sync_task = None;
            self.sync_busy = false;
            sync::clear_import_cache();
            self.remote_data = Arc::new(LoadedData::default());
            self.known_devices = Vec::new();
            self.sync_message = None;
            self.sync_last_at.clear();
            self.sync_last_completed = None;
            self.sync_retry_attempt = 0;
            self.remerge_all_agents();
            self.rebuild_buckets();
            self.rebuild_sessions();
        }
        cx.notify();
    }

    fn fill_modal_inputs(&mut self, cfg: &sync::SyncConfig, cx: &mut Context<Self>) {
        self.modal_url_input.update(cx, |input, cx| {
            input.set_placeholder("https://sync.example.com");
            input.set_text(cfg.api_url.clone(), cx);
        });
    }

    fn read_modal_api_url(&self, cx: &App) -> String {
        self.modal_url_input.read(cx).text()
    }

    /// 打开同步设置弹窗，从当前配置初始化编辑值。
    fn open_sync_config(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let cfg = sync::read_config();
        self.sync_api_url = cfg.api_url.clone();
        self.fill_modal_inputs(&cfg, cx);
        self.sync_config_open = true;
        window.focus(&self.modal_focus);
        cx.notify();
    }

    /// 关闭弹窗并丢弃修改（重新加载已保存的配置）。
    fn cancel_sync_config(&mut self, cx: &mut Context<Self>) {
        let cfg = sync::read_config();
        self.github_login_id += 1;
        self.github_login_busy = false;
        self.github_login_code = None;
        self.github_login_code_copied = false;
        self.github_login_task = None;
        self.sync_api_url = cfg.api_url.clone();
        self.fill_modal_inputs(&cfg, cx);
        self.sync_config_open = false;
        cx.notify();
    }

    /// 保存弹窗中的同步设置。
    fn save_sync_config(&mut self, cx: &mut Context<Self>) {
        let url = self.read_modal_api_url(cx);
        let url = url.trim().trim_end_matches('/').to_string();
        let api_changed = self.sync_api_url != url;
        if api_changed {
            sync::delete_github_sync_session();
        }
        self.sync_api_url = url;
        if api_changed && self.sync_enabled {
            self.toggle_sync(cx);
            self.sync_message = Some("同步 API 已更新，请重新登录 GitHub 后再开启同步".into());
        } else {
            sync::save_config(&self.build_sync_config());
        }
        self.sync_config_open = false;
        if self.sync_enabled && !api_changed {
            self.start_sync(cx);
        }
        cx.notify();
    }

    fn start_github_login(&mut self, cx: &mut Context<Self>) {
        let api_url = self.read_modal_api_url(cx);
        let api_url = api_url.trim().trim_end_matches('/').to_string();
        if api_url.is_empty() {
            self.sync_message = Some("请先填写同步 API 地址".into());
            cx.notify();
            return;
        }
        if self.sync_api_url != api_url {
            sync::delete_github_sync_session();
        }
        self.sync_api_url = api_url.clone();
        sync::save_config(&self.build_sync_config());
        self.github_login_id += 1;
        let login_id = self.github_login_id;
        self.github_login_busy = true;
        self.github_login_code = None;
        self.github_login_code_copied = false;
        self.sync_message = Some("正在请求 GitHub 登录…".into());
        let begin = cx
            .background_executor()
            .spawn(async move { sync::begin_github_login(&api_url) });
        self.github_login_task = Some(cx.spawn(async move |this, cx| {
            let result = begin.await;
            this.update(cx, |this, cx| {
                if this.github_login_id != login_id {
                    return;
                }
                match result {
                    Ok(authorization) => {
                        this.github_login_code = Some(authorization.user_code.clone());
                        this.github_login_code_copied = false;
                        this.sync_message = Some(format!(
                            "请在浏览器完成 GitHub 登录，验证码：{}",
                            authorization.user_code
                        ));
                        if let Err(error) = open_external_url(&authorization.verification_uri) {
                            this.sync_message = Some(format!(
                                "请手动打开 {}，验证码：{}（{error}）",
                                authorization.verification_uri, authorization.user_code
                            ));
                        }
                        let wait_api_url = this.sync_api_url.clone();
                        let wait = cx.background_executor().spawn(async move {
                            sync::wait_for_github_login(&wait_api_url, &authorization)
                        });
                        this.github_login_task = Some(cx.spawn(async move |this, cx| {
                            let session = wait.await;
                            this.update(cx, |this, cx| {
                                if this.github_login_id != login_id {
                                    return;
                                }
                                this.github_login_busy = false;
                                this.github_login_code = None;
                                this.github_login_code_copied = false;
                                match session {
                                    Ok(session) => match sync::save_github_sync_session(&session) {
                                        Ok(()) => {
                                            this.sync_message =
                                                Some(format!("已登录 GitHub：{}", session.login));
                                            if this.sync_enabled {
                                                this.start_sync(cx);
                                            }
                                        }
                                        Err(error) => this.sync_message = Some(error),
                                    },
                                    Err(error) => this.sync_message = Some(error),
                                }
                                cx.notify();
                            })
                            .ok();
                        }));
                    }
                    Err(error) => {
                        this.github_login_busy = false;
                        this.sync_message = Some(error);
                    }
                }
                cx.notify();
            })
            .ok();
        }));
        // 后台请求可能要等待网络超时；立即刷新弹窗，让用户知道点击已生效。
        cx.notify();
    }

    fn copy_github_login_code(&mut self, cx: &mut Context<Self>) {
        let Some(code) = self.github_login_code.clone() else {
            return;
        };
        cx.write_to_clipboard(ClipboardItem::new_string(code));
        self.github_login_code_copied = true;
        cx.notify();
    }

    fn logout_github(&mut self, cx: &mut Context<Self>) {
        self.github_login_id += 1;
        self.github_login_busy = false;
        self.github_login_code = None;
        self.github_login_code_copied = false;
        self.github_login_task = None;
        sync::delete_github_sync_session();
        self.sync_enabled = false;
        self.sync_id += 1;
        self.sync_tick_task = None;
        self.sync_task = None;
        self.sync_busy = false;
        self.remote_data = Arc::new(LoadedData::default());
        self.known_devices = Vec::new();
        self.sync_last_at.clear();
        self.sync_message = Some("已退出 GitHub 同步".into());
        sync::save_config(&self.build_sync_config());
        self.remerge_all_agents();
        self.rebuild_buckets();
        self.rebuild_sessions();
        cx.notify();
    }

    /// 折叠/展开侧边栏分区。
    fn toggle_section(&mut self, section: SidebarSection, cx: &mut Context<Self>) {
        match section {
            SidebarSection::Agents => self.section_agents_open = !self.section_agents_open,
            SidebarSection::Devices => self.section_devices_open = !self.section_devices_open,
        }
        cx.notify();
    }

    /// Esc 关闭同步设置弹窗。捕获阶段处理，这样输入框聚焦时也能关掉。
    fn handle_modal_key(&mut self, event: &KeyDownEvent, cx: &mut Context<Self>) {
        if event.keystroke.key.eq_ignore_ascii_case("escape") {
            self.cancel_sync_config(cx);
            cx.stop_propagation();
        }
    }

    /// 启动自动同步定时任务：每 SYNC_INTERVAL_SECS 秒自动导出+导入。
    /// 仅在 sync_enabled 为 true 时持续运行；关闭后任务自然停止。
    fn sync_tick(&mut self, cx: &mut Context<Self>) {
        self.sync_tick_after(Duration::from_secs(sync::SYNC_INTERVAL_SECS), cx);
    }

    fn sync_tick_after(&mut self, delay: Duration, cx: &mut Context<Self>) {
        let timer = cx.background_executor().timer(delay);
        self.sync_tick_task = Some(cx.spawn(async move |this, cx| {
            timer.await;
            this.update(cx, |this, cx| {
                // 只有同步仍处于开启状态且没有正在进行的同步时，才触发下一轮
                if this.sync_enabled && !this.sync_busy {
                    this.start_sync(cx);
                }
            })
            .ok();
        }));
    }

    fn sync_toggle_label(&self) -> &'static str {
        if !self.sync_enabled {
            i18n::t(i18n::Key::SyncOff)
        } else if self.sync_busy {
            i18n::t(i18n::Key::SyncExporting)
        } else if self.sync_message.is_some() {
            i18n::t(i18n::Key::SyncFailed)
        } else if !self.sync_last_at.is_empty() {
            i18n::t(i18n::Key::SyncExported)
        } else {
            i18n::t(i18n::Key::SyncOn)
        }
    }

    fn sync_status_detail(&self) -> Option<String> {
        if !self.sync_enabled || self.sync_busy {
            return None;
        }
        if let Some(msg) = &self.sync_message {
            return Some(msg.clone());
        }
        if !self.sync_last_at.is_empty() {
            return Some(i18n::tf(i18n::Key::SyncLastExport, &[&self.sync_last_at]));
        }
        None
    }

    /// 触发一次完整的同步：导出本机数据 → 导入其他设备数据 → 重新合并已加载的 agent。
    /// 完成后若同步仍开启，自动安排下一次定时同步。
    fn start_sync(&mut self, cx: &mut Context<Self>) {
        if self.sync_busy {
            return;
        }
        if let Some(remaining) = sync::cooldown_remaining() {
            self.sync_message = Some(format!(
                "服务暂时限流，将在约 {} 分钟后重试",
                remaining.as_secs().div_ceil(60)
            ));
            self.sync_tick_after(remaining, cx);
            cx.notify();
            return;
        }
        self.sync_id += 1;
        let sync_id = self.sync_id;
        self.sync_busy = true;
        self.sync_message = None;
        cx.notify();

        // 导出本机已安装的全部 agent，时间窗口固定为最近 12 个月，
        // 与界面上的日/周/月选择无关，避免窄窗口覆盖掉远端更早的数据。
        let (start, end) = window_for(PeriodKind::Month, 0);
        let sync_task = cx.background_executor().spawn(async move {
            let export_data = collect_local_export(start, end);
            sync::sync_cycle(export_data)
        });

        self.sync_task = Some(cx.spawn(async move |this, cx| {
            // 导出和导入共用一个 HTTP 连接池；导出失败时仍会继续导入。
            let (export_result, (remote, devices, import_error)) = sync_task.await;

            this.update(cx, |this, cx| {
                if this.sync_id != sync_id {
                    if !this.sync_enabled {
                        sync::clear_import_cache();
                    }
                    return;
                }
                this.sync_busy = false;

                this.sync_message = match (export_result.as_ref(), import_error.as_ref()) {
                    (Err(e), _) => Some(e.clone()),
                    (Ok(_), Some(e)) => Some(e.clone()),
                    (Ok(_), None) => None,
                };
                let sync_succeeded = this.sync_message.is_none();
                if sync_succeeded {
                    this.sync_last_completed = Some(Instant::now());
                    this.sync_last_at = chrono::Local::now().format("%H:%M:%S").to_string();
                }
                let retryable_failure = this
                    .sync_message
                    .as_deref()
                    .is_some_and(sync::is_retryable_error);
                if let Some(error) = this.sync_message.as_deref() {
                    if this.sync_retry_attempt >= 3 {
                        sync::record_transient_failure(error);
                    }
                } else {
                    sync::clear_transient_failure();
                    this.sync_retry_attempt = 0;
                }

                // 导入失败时保留上次成功的远程数据，避免设备列表被清空。
                if import_error.is_none() {
                    this.remote_data = Arc::new(remote);
                    this.known_devices = devices;
                    this.remerge_all_agents();
                    this.rebuild_buckets();
                    this.rebuild_sessions();
                }

                // 临时故障采用有限次数的指数退避，避免网络恢复前每小时才重试，
                // 也避免持续重试打满服务端；非临时错误仍按正常周期重试。
                if this.sync_enabled {
                    if let Some(remaining) = sync::cooldown_remaining() {
                        this.sync_tick_after(remaining, cx);
                    } else if retryable_failure && this.sync_retry_attempt < 3 {
                        let delays = [30, 120, 600];
                        let delay = delays[this.sync_retry_attempt as usize];
                        this.sync_retry_attempt += 1;
                        this.sync_tick_after(Duration::from_secs(delay), cx);
                    } else {
                        this.sync_retry_attempt = 0;
                        this.sync_tick(cx);
                    }
                }
                cx.notify();
            })
            .ok();
        }));
    }

    /// 用最新的 remote_data 重新合并所有已加载 agent 的缓存数据。
    /// 本机部分从合并前的数据中提取（device_id 为空或等于本机 ID 的记录）。
    fn remerge_all_agents(&mut self) {
        let local_id = data::device_id();
        let mut updated: HashMap<AgentKind, Arc<LoadedData>> = HashMap::new();
        for (agent, arc) in &self.loaded_agents {
            // 提取本机部分
            let local_only = LoadedData {
                sessions: arc
                    .sessions
                    .iter()
                    .filter(|s| s.device_id.is_empty() || s.device_id == local_id)
                    .cloned()
                    .collect(),
                turns: arc
                    .turns
                    .iter()
                    .filter(|t| t.device_id.is_empty() || t.device_id == local_id)
                    .cloned()
                    .collect(),
                turns_start: arc.turns_start,
                turns_end: arc.turns_end,
                ..Default::default()
            };
            let merged = self.merge_remote_for_agent(local_only, *agent);
            updated.insert(*agent, Arc::new(merged));
        }
        self.loaded_agents = updated;
        // 刷新当前 agent 的显示数据
        if let Some(arc) = self.loaded_agents.get(&self.agent) {
            self.data = arc.clone();
        }
    }

    /// 后台发现账号并并发查询配额（含必要的 token 刷新），逐条更新卡片。
    /// `force=true`（手动刷新）绕过 5 分钟结果缓存，但仍尊重 429 冷却。
    fn start_quota_load(&mut self, force: bool, cx: &mut Context<Self>) {
        self.quota_load_id += 1;
        let load_id = self.quota_load_id;
        self.quota_loading = true;
        self.quota_message = None;
        let allow_keychain = self.quota_keychain_allowed;
        cx.notify();

        let (updates, progress) = smol::channel::unbounded();
        let load = cx.background_executor().spawn(async move {
            let started = std::time::Instant::now();
            let accounts = quota::discover_accounts(allow_keychain);
            data::log_event(format!(
                "quota discovery elapsed_ms={} accounts={}",
                started.elapsed().as_millis(),
                accounts.len()
            ));
            // 各账号并发查询；返回顺序与 accounts 一致，卡片顺序不变
            let results =
                quota::fetch_accounts_with_progress(&accounts, force, |index, account, result| {
                    let _ = updates.try_send((
                        index,
                        QuotaCard {
                            key: account.key.clone(),
                            provider: account.provider,
                            label: account.label.clone(),
                            result: result.clone(),
                        },
                    ));
                });
            accounts
                .iter()
                .zip(results)
                .map(|(account, result)| QuotaCard {
                    key: account.key.clone(),
                    provider: account.provider,
                    label: account.label.clone(),
                    result,
                })
                .collect::<Vec<_>>()
        });
        self.quota_task = Some(cx.spawn(async move |this, cx| {
            let mut completed = std::collections::BTreeMap::new();
            while let Ok((index, card)) = progress.recv().await {
                completed.insert(index, card);
                if this
                    .update(cx, |this, cx| {
                        if this.quota_load_id != load_id {
                            return;
                        }
                        // 已完成账号按发现顺序展示；刷新时其余账号保留上次结果。
                        let mut cards: Vec<QuotaCard> = completed.values().cloned().collect();
                        cards.extend(
                            this.quota_cards
                                .iter()
                                .filter(|old| !completed.values().any(|new| new.key == old.key))
                                .cloned(),
                        );
                        this.quota_cards = cards;
                        cx.notify();
                    })
                    .is_err()
                {
                    return;
                }
            }
            let cards = load.await;
            this.update(cx, |this, cx| {
                if this.quota_load_id != load_id {
                    return;
                }
                this.quota_loading = false;
                this.quota_cards = cards;
                this.quota_updated_at = chrono::Local::now().format("%H:%M:%S").to_string();
                if this.view_mode == ViewMode::LiveQuota {
                    cx.notify();
                }
                this.quota_tick(cx);
            })
            .ok();
        }));
    }

    /// 用户在 UI 中显式同意后，才允许发现/读取 Keychain 账号。
    fn allow_quota_keychain(&mut self, cx: &mut Context<Self>) {
        quota::set_keychain_allowed(true);
        self.quota_keychain_allowed = true;
        self.quota_keychain_dismissed = false;
        self.start_quota_load(false, cx);
    }

    /// 本次会话内隐藏钥匙串提示，不写入持久化偏好。
    fn dismiss_quota_keychain(&mut self, cx: &mut Context<Self>) {
        self.quota_keychain_dismissed = true;
        cx.notify();
    }

    fn show_quota_keychain_banner(&self) -> bool {
        cfg!(target_os = "macos")
            && !self.quota_keychain_allowed
            && !self.quota_keychain_dismissed
            && quota::macos_may_have_keychain_accounts_cached()
    }

    /// 每 30 秒重绘一次，驱动重置倒计时；离开 LiveQuota 页面或清空后自动停止。
    fn quota_tick(&mut self, cx: &mut Context<Self>) {
        let timer = cx.background_executor().timer(Duration::from_secs(30));
        self.quota_tick_task = Some(cx.spawn(async move |this, cx| {
            timer.await;
            this.update(cx, |this, cx| {
                if this.view_mode == ViewMode::LiveQuota && !this.quota_cards.is_empty() {
                    cx.notify();
                    this.quota_tick(cx);
                }
            })
            .ok();
        }));
    }

    fn remove_quota_account(&mut self, key: &str, cx: &mut Context<Self>) {
        if let Some(idx) = key
            .strip_prefix("devin-")
            .and_then(|s| s.parse::<usize>().ok())
        {
            quota::remove_devin_account(idx);
        }
        self.start_quota_load(false, cx);
    }

    /// 按当前 Agent + 设备筛选预计算会话列表（排序、截断、费用、模型展示名）。
    fn rebuild_sessions(&mut self) {
        let device = self.device_filter.clone();
        // 先按 session_key 分组，费用合计从 O(会话数 × 轮次数) 降到 O(会话数 + 轮次数)
        let mut turns_by_session: HashMap<&str, Vec<&data::TurnRec>> = HashMap::new();
        for turn in self
            .data
            .turns
            .iter()
            .filter(|t| t.agent == self.agent)
            .filter(|t| device.as_deref().is_none_or(|d| t.device_id == d))
        {
            turns_by_session
                .entry(turn.session_key.as_str())
                .or_default()
                .push(turn);
        }

        let mut rows: Vec<SessionRow> = self
            .data
            .sessions
            .iter()
            .filter(|s| s.agent == self.agent)
            .filter(|s| device.as_deref().is_none_or(|d| s.device_id == d))
            .map(|s| {
                let session_turns: Vec<&data::TurnRec> = turns_by_session
                    .get(s.key.as_str())
                    .into_iter()
                    .flatten()
                    .copied()
                    .collect();
                let total = session_token_total(s, &session_turns);
                // 与每日汇总一致：当前加载窗口内按实际 turn 模型逐轮计价。
                let cost_summary =
                    agg::cost_summary_for_turns(session_turns.iter().copied(), &s.display_model());
                let cost_str = match cost_summary.cost {
                    Some(cost) if cost_summary.is_partial() => {
                        i18n::tf(i18n::Key::CliPartialCostShort, &[&pricing::fmt_cost(cost)])
                    }
                    Some(cost) => pricing::fmt_cost(cost),
                    None => "—".into(),
                };
                let adaptive = s.selected_model == "adaptive";
                let model_text = if adaptive {
                    i18n::tf(i18n::Key::AdaptiveShort, &[&s.display_model()])
                } else if s.selected_model.is_empty() {
                    s.display_model()
                } else {
                    s.selected_model.clone()
                };
                SessionRow {
                    time_str: fmt_ts(s.last_activity_at),
                    id_short: truncate(&s.id, 16),
                    title_short: truncate(&s.title, 80),
                    model_text: truncate(&model_text, 30),
                    cost_str,
                    cost_val: cost_summary.cost,
                    total,
                    session: s.clone(),
                }
            })
            .collect();
        rows.sort_by_key(|row| std::cmp::Reverse(row.session.last_activity_at));
        rows.truncate(500);
        self.sessions_rows = rows;
    }

    /// 判断当前窗口内是否已经有目标 Agent 的数据。
    /// 不仅检查总范围是否覆盖，还要检查该 Agent 在 [start, end) 内是否有会话重叠。
    fn has_data_for(&self, start: i64, end: i64, agent: AgentKind) -> bool {
        if self.data.turns_start == 0 && self.data.turns_end == 0 {
            return false;
        }
        if start < self.data.turns_start || end > self.data.turns_end {
            return false;
        }
        self.data
            .sessions
            .iter()
            .any(|s| s.agent == agent && s.created_at < end && s.last_activity_at >= start)
    }

    fn page_needs_load(&self) -> bool {
        let (start, end) = window_for(self.period, self.page);
        start < self.data.turns_start
            || end > self.data.turns_end
            || !self.has_data_for(start, end, self.agent)
    }

    /// 左侧边栏：应用名 + 全部 agent 的常驻切换列表。
    /// 已安装的 agent 排在上方（正常样式），未安装的排在下方（灰色样式）。
    /// 左侧边栏：应用名 + 可滚动/可折叠的分区（agents、设备、同步设置）。
    fn sidebar(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let is_history_mode = self.view_mode == ViewMode::AgentHistory;
        let render_agent_item =
            |kind: AgentKind,
             this_agent: AgentKind,
             loaded_agents: &HashMap<AgentKind, Arc<LoadedData>>| {
                let is_selected = is_history_mode && kind == this_agent;
                let installed = kind.is_installed();
                let count = loaded_agents
                    .get(&kind)
                    .map(|d| d.sessions.iter().filter(|s| s.agent == kind).count());

                div()
                    .id(SharedString::from(format!("agent-{}", kind.label())))
                    .flex()
                    .items_center()
                    .gap_2()
                    .px_2()
                    .py_2()
                    .rounded_md()
                    .cursor_pointer()
                    .when(is_selected, |d| d.bg(rgba(ACCENT, 0.18)))
                    .when(!is_selected, |d| d.hover(|h| h.bg(rgb(PANEL2))))
                    .when(!installed, |d| d.opacity(0.45))
                    .child(
                        div()
                            .w(px(7.))
                            .h(px(7.))
                            .rounded_full()
                            .bg(rgb(if installed {
                                model_color(kind.label())
                            } else {
                                MUTED
                            })),
                    )
                    .child(
                        div()
                            .flex_1()
                            .text_sm()
                            .font_weight(if is_selected {
                                gpui::FontWeight::BOLD
                            } else {
                                gpui::FontWeight::MEDIUM
                            })
                            .text_color(if is_selected {
                                rgb(ACCENT)
                            } else if installed {
                                rgb(TEXT)
                            } else {
                                rgb(MUTED)
                            })
                            .child(kind.label()),
                    )
                    .children(
                        count.map(|c| div().text_xs().text_color(rgb(MUTED)).child(c.to_string())),
                    )
                    .on_click(cx.listener(move |this, _, _, cx| {
                        this.switch_agent(kind, cx);
                    }))
            };

        let mut active_kinds = Vec::new();
        let mut other_kinds = Vec::new();
        for kind in AgentKind::ALL {
            let has_sessions = self
                .loaded_agents
                .get(&kind)
                .map(|d| d.sessions.iter().any(|s| s.agent == kind))
                .unwrap_or(false);
            if kind.is_installed() || has_sessions || kind == self.agent {
                active_kinds.push(kind);
            } else {
                other_kinds.push(kind);
            }
        }

        let active_items = active_kinds
            .into_iter()
            .map(|kind| render_agent_item(kind, self.agent, &self.loaded_agents));

        let other_count = other_kinds.len();
        let other_arrow = if self.section_other_agents_open {
            "▾"
        } else {
            "▸"
        };
        let other_toggle_btn = if other_count > 0 {
            Some(
                div()
                    .id("toggle-more-agents")
                    .px_2()
                    .py_1()
                    .rounded_md()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .cursor_pointer()
                    .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                    .child(format!(
                        "{other_arrow} {}",
                        i18n::tf(i18n::Key::MoreAgents, &[&other_count.to_string()])
                    ))
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.section_other_agents_open = !this.section_other_agents_open;
                        cx.notify();
                    })),
            )
        } else {
            None
        };
        let other_items = if self.section_other_agents_open {
            other_kinds
                .into_iter()
                .map(|kind| render_agent_item(kind, self.agent, &self.loaded_agents))
                .collect::<Vec<_>>()
        } else {
            Vec::new()
        };

        // ── 设备选择区 ──
        let local_id = data::device_id();
        let local_name = data::device_name();
        let current_filter = self.device_filter.clone();

        // 构建设备列表：本机 + 远程（去重）
        let mut device_items: Vec<(String, String, bool)> =
            vec![(local_id.clone(), local_name, true)];
        for d in &self.known_devices {
            if !d.is_local && !device_items.iter().any(|(id, _, _)| id == &d.device_id) {
                device_items.push((d.device_id.clone(), d.device_name.clone(), false));
            }
        }

        // 各设备选项
        let device_btns = device_items.into_iter().map(|(id, name, is_local)| {
            let id_clone = id.clone();
            let selected = current_filter.as_deref() == Some(id.as_str());
            div()
                .id(SharedString::from(format!("device-{id}")))
                .flex()
                .items_center()
                .gap_2()
                .px_2()
                .py_1()
                .rounded_md()
                .cursor_pointer()
                .when(selected, |d| d.bg(rgba(ACCENT, 0.18)))
                .when(!selected, |d| d.hover(|h| h.bg(rgb(PANEL2))))
                .child(
                    div()
                        .w(px(7.))
                        .h(px(7.))
                        .rounded_full()
                        .bg(rgb(if is_local { 0x3ddc97 } else { 0x4cc2ff })),
                )
                .child(
                    div()
                        .flex_1()
                        .w(px(0.))
                        .min_w(px(0.))
                        .overflow_hidden()
                        .whitespace_nowrap()
                        .text_sm()
                        .font_weight(if selected {
                            gpui::FontWeight::BOLD
                        } else {
                            gpui::FontWeight::MEDIUM
                        })
                        .text_color(if selected { rgb(ACCENT) } else { rgb(TEXT) })
                        .child(truncate(&name, 14)),
                )
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.switch_device(Some(id_clone.clone()), cx);
                }))
        });

        let all_selected = current_filter.is_none();
        let all_btn = div()
            .id("device-all")
            .flex()
            .items_center()
            .gap_2()
            .px_2()
            .py_1()
            .rounded_md()
            .cursor_pointer()
            .when(all_selected, |d| d.bg(rgba(ACCENT, 0.18)))
            .when(!all_selected, |d| d.hover(|h| h.bg(rgb(PANEL2))))
            .child(div().w(px(7.)).h(px(7.)))
            .child(
                div()
                    .flex_1()
                    .text_sm()
                    .font_weight(if all_selected {
                        gpui::FontWeight::BOLD
                    } else {
                        gpui::FontWeight::MEDIUM
                    })
                    .text_color(if all_selected { rgb(ACCENT) } else { rgb(TEXT) })
                    .child(i18n::t(i18n::Key::AllDevices)),
            )
            .on_click(cx.listener(|this, _, _, cx| {
                this.switch_device(None, cx);
            }));

        let section_header = |section: SidebarSection, title: &'static str, open: bool| {
            let arrow = if open { "▾" } else { "▸" };
            div()
                .id(SharedString::from(format!("section-{section:?}")))
                .pt_3()
                .pb_1()
                .px_3()
                .text_xs()
                .font_weight(gpui::FontWeight::MEDIUM)
                .text_color(rgb(MUTED))
                .cursor_pointer()
                .hover(|h| h.text_color(rgb(TEXT)))
                .child(format!("{arrow}  {title}"))
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.toggle_section(section, cx);
                }))
        };

        let agents_header = section_header(
            SidebarSection::Agents,
            i18n::t(i18n::Key::AgentsSection),
            self.section_agents_open,
        );
        let devices_header = section_header(
            SidebarSection::Devices,
            i18n::t(i18n::Key::DevicesSection),
            self.section_devices_open,
        );

        // 可滚动内容区：只有数据筛选（代理、设备），不再混杂设置入口
        let content = div()
            .id("sidebar-scroll")
            .flex_1()
            .min_w(px(0.))
            .overflow_scroll()
            .flex()
            .flex_col()
            .child(agents_header)
            .when(self.section_agents_open, |d| {
                d.child(
                    div()
                        .flex()
                        .flex_col()
                        .pl(px(12.))
                        .pr_2()
                        .children(active_items)
                        .children(other_toggle_btn)
                        .children(other_items),
                )
            })
            .child(devices_header)
            .when(self.section_devices_open, |d| {
                d.child(
                    div()
                        .flex()
                        .flex_col()
                        .pl(px(12.))
                        .pr_2()
                        .child(all_btn)
                        .children(device_btns),
                )
            });

        // 同步开关（移到侧边栏底部），下面一行显示上次时间或失败原因
        let sync_failed = self.sync_enabled && self.sync_message.is_some();
        let sync_color = if sync_failed {
            rgb(0xf87171)
        } else if self.sync_enabled {
            rgb(0x3ddc97)
        } else {
            rgb(MUTED)
        };
        let sync_detail = self.sync_status_detail();
        let sync_toggle = div()
            .mt_auto()
            .mx_2()
            .mb_2()
            .flex()
            .flex_col()
            .child(
                div()
                    .id("sync-toggle")
                    .px_3()
                    .py_2()
                    .rounded_md()
                    .text_xs()
                    .flex()
                    .items_center()
                    .gap_2()
                    .cursor_pointer()
                    .text_color(sync_color)
                    .hover(|h| h.bg(rgb(PANEL2)))
                    .child(div().w(px(7.)).h(px(7.)).rounded_full().bg(sync_color))
                    .child(self.sync_toggle_label())
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.toggle_sync(cx);
                    })),
            )
            .children(sync_detail.map(|detail| {
                div()
                    .px_3()
                    .pt(px(1.))
                    .text_xs()
                    .text_color(if sync_failed {
                        rgb(0xf87171)
                    } else {
                        rgb(MUTED)
                    })
                    .child(detail)
            }));

        // 统一集中设置入口
        let settings_btn = div()
            .id("settings-open")
            .flex()
            .items_center()
            .gap_1()
            .px_2()
            .py(px(2.))
            .h(px(24.))
            .rounded_md()
            .cursor_pointer()
            .text_color(rgb(MUTED))
            .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_center()
                    .w(px(14.))
                    .h(px(14.))
                    .text_xs()
                    .line_height(px(14.))
                    .child("⚙"),
            )
            .child(
                div()
                    .text_xs()
                    .line_height(px(16.))
                    .child(i18n::t(i18n::Key::Settings)),
            )
            .on_click(cx.listener(|this, _, window, cx| {
                this.open_sync_config(window, cx);
            }));

        let footer_row = div()
            .id("sidebar-footer")
            .mx_2()
            .mb_2()
            .flex()
            .items_center()
            .justify_between()
            .child(settings_btn)
            .child(self.update_version_row(cx));

        let quota_active = self.view_mode == ViewMode::LiveQuota;
        let quota_nav_btn = div()
            .id("nav-live-quota")
            .mx_2()
            .mb_2()
            .px_2()
            .py(px(6.))
            .rounded_md()
            .cursor_pointer()
            .flex()
            .items_center()
            .justify_between()
            .when(quota_active, |d| d.bg(rgba(ACCENT, 0.18)))
            .when(!quota_active, |d| d.hover(|h| h.bg(rgb(PANEL2))))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_2()
                    .child(
                        div()
                            .w(px(7.))
                            .h(px(7.))
                            .rounded_full()
                            .bg(rgb(if quota_active { ACCENT } else { 0x3ddc97 })),
                    )
                    .child(
                        div()
                            .text_sm()
                            .font_weight(if quota_active {
                                gpui::FontWeight::BOLD
                            } else {
                                gpui::FontWeight::MEDIUM
                            })
                            .text_color(if quota_active { rgb(ACCENT) } else { rgb(TEXT) })
                            .child(i18n::t(i18n::Key::LiveQuotaNav)),
                    ),
            )
            .children(if !self.quota_cards.is_empty() {
                Some(
                    div()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child(self.quota_cards.len().to_string()),
                )
            } else {
                None
            })
            .on_click(cx.listener(|this, _, _, cx| {
                this.switch_to_quota(cx);
            }));

        div()
            .id("sidebar")
            .flex_shrink_0()
            .w(px(160.))
            .h_full()
            .flex()
            .flex_col()
            .bg(rgb(PANEL))
            // 左上角平台差异：macOS 红绿灯悬浮在侧边栏顶部，标题需下移让位；
            // Windows 没有系统按钮，标题上移为与右侧工具栏同高的 48px 行。
            .when(cfg!(target_os = "macos"), |d| {
                d.child(
                    div()
                        .w_full()
                        .h(px(40.))
                        .window_control_area(WindowControlArea::Drag),
                )
                .child(
                    div()
                        .px_4()
                        .pb_2()
                        .text_sm()
                        .font_weight(gpui::FontWeight::BOLD)
                        .text_color(rgb(TEXT))
                        .window_control_area(WindowControlArea::Drag)
                        .child("Agent Usage Metrics"),
                )
            })
            .when(!cfg!(target_os = "macos"), |d| {
                d.child(
                    div()
                        .w_full()
                        .h(px(48.))
                        .px_4()
                        .flex()
                        .items_center()
                        .overflow_hidden()
                        .window_control_area(WindowControlArea::Drag)
                        .child(
                            div()
                                .text_xs()
                                .whitespace_nowrap()
                                .font_weight(gpui::FontWeight::BOLD)
                                .text_color(rgb(TEXT))
                                .child("Agent Usage Metrics"),
                        ),
                )
            })
            .child(quota_nav_btn)
            .child(div().mx_3().mb_1().border_b_1().border_color(rgb(BORDER)))
            .child(content)
            .child(sync_toggle)
            .child(footer_row)
    }

    /// 弹窗中的文本输入框。
    fn sync_config_input(
        &self,
        input: Entity<TextInput>,
        window: &Window,
        cx: &mut Context<Self>,
    ) -> impl IntoElement {
        let focused = input.read(cx).is_focused(window);
        let focus_input = input.clone();
        div()
            .w_full()
            .px_3()
            .py_2()
            .rounded_md()
            .text_xs()
            .cursor_text()
            .when(focused, |d| {
                d.bg(rgb(PANEL2)).border_1().border_color(rgb(ACCENT))
            })
            .when(!focused, |d| {
                d.bg(rgb(0x101014)).hover(|h| h.bg(rgb(PANEL2)))
            })
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(move |_, _, window, cx| {
                    focus_input.read(cx).focus(window);
                    cx.stop_propagation();
                }),
            )
            .child(input)
    }

    /// 集中设置弹窗：包含多设备同步与常规偏好（语言等）。
    fn sync_config_modal(&self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let tab_btn = |tab: SettingsTab, label: &'static str| {
            let is_sel = self.settings_tab == tab;
            div()
                .id(SharedString::from(format!("settings-tab-{tab:?}")))
                .px_3()
                .py_1()
                .rounded_sm()
                .text_xs()
                .cursor_pointer()
                .when(is_sel, |d| {
                    d.bg(rgba(ACCENT, 0.2))
                        .text_color(rgb(ACCENT))
                        .font_weight(gpui::FontWeight::BOLD)
                })
                .when(!is_sel, |d| {
                    d.text_color(rgb(MUTED))
                        .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                })
                .child(label)
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.settings_tab = tab;
                    cx.notify();
                }))
        };

        let title_bar = div()
            .flex()
            .items_center()
            .justify_between()
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_2()
                    .text_sm()
                    .font_weight(gpui::FontWeight::BOLD)
                    .text_color(rgb(TEXT))
                    .child("⚙")
                    .child(i18n::t(i18n::Key::Settings)),
            )
            .child(
                div()
                    .id("modal-close-x")
                    .px_2()
                    .py_1()
                    .rounded_sm()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .cursor_pointer()
                    .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                    .child("✕")
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.cancel_sync_config(cx);
                    })),
            );

        let nav_tabs = div()
            .flex()
            .gap_1()
            .pb_2()
            .border_b_1()
            .border_color(rgb(BORDER))
            .child(tab_btn(SettingsTab::Sync, i18n::t(i18n::Key::SettingsSync)))
            .child(tab_btn(
                SettingsTab::General,
                i18n::t(i18n::Key::SettingsGeneral),
            ));

        let content_body: AnyElement = match self.settings_tab {
            SettingsTab::Sync => {
                let url_input = self.sync_config_input(self.modal_url_input.clone(), window, cx);
                let session = sync::load_github_sync_session();
                let logged_in = session
                    .as_ref()
                    .filter(|session| session.expires_at > chrono::Utc::now().timestamp());
                let login_label = if self.github_login_busy {
                    "等待 GitHub 授权…"
                } else if let Some(session) = logged_in {
                    if session.login.is_empty() {
                        "已登录 GitHub"
                    } else {
                        "重新登录 GitHub"
                    }
                } else {
                    "登录 GitHub"
                };
                let login_status = if self.github_login_busy {
                    self.github_login_code
                        .as_ref()
                        .map(|code| {
                            if self.github_login_code_copied {
                                format!("验证码 {code} 已复制，请到浏览器粘贴")
                            } else {
                                format!("浏览器已打开，请输入验证码：{code}")
                            }
                        })
                        .unwrap_or_else(|| "正在准备浏览器登录…".into())
                } else if let Some(session) = logged_in {
                    format!("已登录 GitHub：{}", session.login)
                } else {
                    "未登录；同一 GitHub 账号的设备会自动同步".into()
                };

                let mut sync_box = div()
                    .flex()
                    .flex_col()
                    .gap_3()
                    .child(
                        div()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .child("同步 API 地址"),
                    )
                    .child(url_input)
                    .child(div().text_xs().text_color(rgb(MUTED)).child(login_status))
                    .child(
                        div()
                            .id("modal-github-login")
                            .px_3()
                            .py_2()
                            .rounded_sm()
                            .text_center()
                            .text_xs()
                            .text_color(rgb(0x0a0a0c))
                            .bg(rgb(ACCENT))
                            .when(!self.github_login_busy, |button| {
                                button
                                    .cursor_pointer()
                                    .hover(|h| h.bg(rgb(0x6ad4ff)))
                                    .on_click(
                                        cx.listener(|this, _, _, cx| this.start_github_login(cx)),
                                    )
                            })
                            .child(login_label),
                    );

                if self.github_login_busy && self.github_login_code.is_some() {
                    sync_box = sync_box.child(
                        div()
                            .id("modal-github-copy-code")
                            .px_3()
                            .py_1()
                            .rounded_sm()
                            .text_center()
                            .text_xs()
                            .text_color(rgb(TEXT))
                            .bg(rgb(PANEL2))
                            .border_1()
                            .border_color(rgb(BORDER))
                            .cursor_pointer()
                            .hover(|h| h.bg(rgb(0x30303b)))
                            .child(if self.github_login_code_copied {
                                "验证码已复制"
                            } else {
                                "复制验证码"
                            })
                            .on_click(
                                cx.listener(|this, _, _, cx| this.copy_github_login_code(cx)),
                            ),
                    );
                }

                if logged_in.is_some() {
                    sync_box = sync_box.child(
                        div()
                            .id("modal-github-logout")
                            .px_3()
                            .py_1()
                            .rounded_sm()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .cursor_pointer()
                            .hover(|h| h.bg(rgb(PANEL2)))
                            .child("退出 GitHub")
                            .on_click(cx.listener(|this, _, _, cx| this.logout_github(cx))),
                    );
                }

                sync_box = sync_box.child(
                    div()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child("同一 GitHub 账号的设备会合并到同一份同步数据"),
                );

                sync_box.into_any_element()
            }
            SettingsTab::General => {
                let cur_lang = i18n::lang();
                let lang_btn = |lang: i18n::Lang, label: &'static str| {
                    let active = cur_lang == lang;
                    div()
                        .id(SharedString::from(format!(
                            "settings-lang-{}",
                            lang.short_label()
                        )))
                        .px_3()
                        .py_1()
                        .rounded_sm()
                        .text_xs()
                        .cursor_pointer()
                        .when(active, |d| {
                            d.bg(rgba(ACCENT, 0.2))
                                .text_color(rgb(ACCENT))
                                .font_weight(gpui::FontWeight::BOLD)
                        })
                        .when(!active, |d| {
                            d.text_color(rgb(MUTED))
                                .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                        })
                        .child(label)
                        .on_click(cx.listener(move |this, _, _, cx| {
                            i18n::set_lang(lang);
                            tray_controller::TrayController::refresh_language(cx);
                            this.rebuild_buckets();
                            this.rebuild_sessions();
                            cx.notify();
                        }))
                };

                div()
                    .flex()
                    .flex_col()
                    .gap_4()
                    .py_1()
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .gap_2()
                            .child(
                                div()
                                    .text_xs()
                                    .font_weight(gpui::FontWeight::MEDIUM)
                                    .text_color(rgb(MUTED))
                                    .child(i18n::t(i18n::Key::Language)),
                            )
                            .child(
                                div()
                                    .flex()
                                    .gap_2()
                                    .child(lang_btn(i18n::Lang::Zh, "简体中文"))
                                    .child(lang_btn(i18n::Lang::En, "English")),
                            ),
                    )
                    .child(
                        div()
                            .flex()
                            .flex_col()
                            .gap_1()
                            .pt_2()
                            .border_t_1()
                            .border_color(rgb(BORDER))
                            .child(div().text_xs().text_color(rgb(MUTED)).child("关于本应用"))
                            .child(
                                div()
                                    .text_xs()
                                    .font_weight(gpui::FontWeight::MEDIUM)
                                    .text_color(rgb(TEXT))
                                    .child("Agent Usage Metrics v0.2.1"),
                            )
                            .child(
                                div()
                                    .text_xs()
                                    .text_color(rgb(MUTED))
                                    .child("本地 Coding Agent 用量与配额监控工具"),
                            ),
                    )
                    .into_any_element()
            }
        };

        let card = div()
            .w(px(380.))
            .p_4()
            .rounded_lg()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .shadow_md()
            .flex()
            .flex_col()
            .gap_3()
            .child(title_bar)
            .child(nav_tabs)
            .child(content_body)
            .child(
                div()
                    .flex()
                    .justify_end()
                    .gap_2()
                    .pt_2()
                    .border_t_1()
                    .border_color(rgb(BORDER))
                    .child(
                        div()
                            .id("modal-cancel")
                            .px_4()
                            .py_1()
                            .rounded_sm()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .cursor_pointer()
                            .hover(|h| h.bg(rgb(PANEL2)))
                            .child(i18n::t(i18n::Key::SyncConfigCancel))
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.cancel_sync_config(cx);
                            })),
                    )
                    .child(
                        div()
                            .id("modal-save")
                            .px_4()
                            .py_1()
                            .rounded_sm()
                            .text_xs()
                            .text_color(rgb(0x0a0a0c))
                            .bg(rgb(ACCENT))
                            .cursor_pointer()
                            .hover(|h| h.bg(rgb(0x6ad4ff)))
                            .child(i18n::t(i18n::Key::SyncConfigSave))
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.save_sync_config(cx);
                            })),
                    ),
            );

        div()
            .id("sync-config-overlay")
            .absolute()
            .top(px(0.))
            .bottom(px(0.))
            .left(px(0.))
            .right(px(0.))
            .bg(rgba(0x000000, 0.55))
            .flex()
            .items_center()
            .justify_center()
            .track_focus(&self.modal_focus)
            .capture_key_down(cx.listener(|this, event: &KeyDownEvent, _window, cx| {
                this.handle_modal_key(event, cx);
            }))
            .child(card)
    }
    fn top_bar(&self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let is_quota_mode = self.view_mode == ViewMode::LiveQuota;
        let tab = self.tab;
        let period = self.period;
        let page = self.page;
        let show_period = !is_quota_mode && matches!(tab, Tab::Usage | Tab::Compaction);
        let loaded_at = self.loaded_at.clone();
        let quota_updated_at = self.quota_updated_at.clone();

        let tab_btn = |kind: Tab, name: &'static str| {
            let active = !is_quota_mode && tab == kind;
            div()
                .id(SharedString::from(format!("tab-{kind:?}")))
                .px_3()
                .py_1()
                .rounded_md()
                .text_sm()
                .cursor_pointer()
                .when(active, |d| d.bg(rgb(ACCENT)).text_color(rgb(0x0a0a0c)))
                .when(!active, |d| {
                    d.text_color(rgb(MUTED)).hover(|h| h.bg(rgb(PANEL2)))
                })
                .child(name)
                // Windows 会把 Drag 区域整体命中为 HTCAPTION，按钮的 mousedown
                // 需要阻断传播，否则点击会进入系统标题栏拖拽循环而失效
                .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.switch_tab(kind, cx);
                }))
        };

        let period_btn = |kind: PeriodKind| {
            let active = period == kind;
            div()
                .id(SharedString::from(format!("period-{kind:?}")))
                .px_2()
                .py(px(2.))
                .rounded_sm()
                .text_xs()
                .cursor_pointer()
                .when(active, |d| d.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                .when(!active, |d| {
                    d.text_color(rgb(MUTED)).hover(|h| h.bg(rgb(PANEL2)))
                })
                .child(kind.label())
                .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.period = kind;
                    this.page = 0;
                    if this.page_needs_load() {
                        this.start_load(true, cx);
                    } else {
                        this.rebuild_buckets();
                        cx.notify();
                    }
                }))
        };

        let range_label = format_window_range(self.period, self.page);

        // ◀ 更早历史（增加 page 偏移）
        let earlier_btn = div()
            .id("history-earlier")
            .px_2()
            .py(px(2.))
            .rounded_sm()
            .text_xs()
            .cursor_pointer()
            .text_color(rgb(MUTED))
            .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .child("◀")
            .on_click(cx.listener(|this, _, _, cx| {
                this.page = this.page.saturating_add(1);
                if this.page_needs_load() {
                    this.start_load(true, cx);
                } else {
                    this.rebuild_buckets();
                    cx.notify();
                }
            }));

        let range_indicator = div()
            .px_2()
            .py(px(2.))
            .rounded_sm()
            .text_xs()
            .text_color(rgb(TEXT))
            .font_weight(gpui::FontWeight::MEDIUM)
            .child(range_label);

        // ▶ 更近历史 / 向今天（减少 page 偏移；page == 0 时置灰禁用）
        let later_btn = div()
            .id("history-later")
            .px_2()
            .py(px(2.))
            .rounded_sm()
            .text_xs()
            .when(page > 0, |d| {
                d.cursor_pointer()
                    .text_color(rgb(MUTED))
                    .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.page = this.page.saturating_sub(1);
                        if this.page_needs_load() {
                            this.start_load(true, cx);
                        } else {
                            this.rebuild_buckets();
                            cx.notify();
                        }
                    }))
            })
            .when(page == 0, |d| {
                d.text_color(rgba(MUTED, 0.3)).cursor_default()
            })
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .child("▶");

        let jump_today_btn = if page > 0 {
            Some(
                div()
                    .id("jump-today")
                    .px_2()
                    .py(px(2.))
                    .rounded_sm()
                    .text_xs()
                    .cursor_pointer()
                    .text_color(rgb(ACCENT))
                    .hover(|h| h.bg(rgba(ACCENT, 0.15)))
                    .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                    .child(i18n::t(i18n::Key::JumpToday))
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.page = 0;
                        if this.page_needs_load() {
                            this.start_load(true, cx);
                        } else {
                            this.rebuild_buckets();
                            cx.notify();
                        }
                    })),
            )
        } else {
            None
        };

        let reload_btn = div()
            .id("reload")
            .px_2()
            .py(px(2.))
            .rounded_sm()
            .text_xs()
            .flex()
            .items_center()
            .gap_2()
            .when(!self.loading, |d| {
                d.cursor_pointer()
                    .text_color(rgb(MUTED))
                    .hover(|h| h.bg(rgb(PANEL2)))
            })
            .when(self.loading, |d| d.text_color(rgba(MUTED, 0.55)))
            .child(if self.loading {
                loading_dots(5., 1.).into_any_element()
            } else {
                div().into_any_element()
            })
            .child(if self.loading {
                i18n::t(i18n::Key::Reloading)
            } else {
                i18n::t(i18n::Key::Reload)
            })
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .on_click(cx.listener(|this, _, _, cx| {
                if !this.loading {
                    this.start_load(true, cx);
                }
            }));

        let quota_refresh_btn = div()
            .id("quota-refresh")
            .px_2()
            .py(px(2.))
            .rounded_sm()
            .text_xs()
            .flex()
            .items_center()
            .gap_2()
            .when(!self.quota_loading, |b| {
                b.cursor_pointer()
                    .text_color(rgb(MUTED))
                    .hover(|h| h.bg(rgb(PANEL2)))
            })
            .when(self.quota_loading, |b| b.text_color(rgba(MUTED, 0.55)))
            .child(if self.quota_loading {
                loading_dots(5., 1.).into_any_element()
            } else {
                div().into_any_element()
            })
            .child(if self.quota_loading {
                i18n::t(i18n::Key::QuotaRefreshing)
            } else {
                i18n::t(i18n::Key::QuotaRefresh)
            })
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .on_click(cx.listener(|this, _, _, cx| {
                if !this.quota_loading {
                    this.start_quota_load(true, cx);
                }
            }));

        let right_status = if is_quota_mode {
            div()
                .flex()
                .items_center()
                .gap_3()
                .flex_shrink_0()
                .child(quota_refresh_btn)
                .when(!quota_updated_at.is_empty(), |d| {
                    d.child(
                        div()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .child(i18n::tf(i18n::Key::QuotaUpdated, &[&quota_updated_at])),
                    )
                })
        } else {
            div()
                .flex()
                .items_center()
                .gap_3()
                .flex_shrink_0()
                .child(reload_btn)
                .child(
                    div()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child(i18n::tf(i18n::Key::DataAsOf, &[&loaded_at])),
                )
        };

        let left_controls = if is_quota_mode {
            div()
                .flex()
                .items_center()
                .gap_3()
                .min_w(px(0.))
                .overflow_hidden()
                .child(
                    div()
                        .text_sm()
                        .font_weight(gpui::FontWeight::BOLD)
                        .text_color(rgb(TEXT))
                        .child(i18n::t(i18n::Key::LiveQuotaTitle)),
                )
                .child(
                    div()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child(i18n::t(i18n::Key::LiveQuotaSubtitle)),
                )
        } else {
            div()
                .flex()
                .items_center()
                .gap_2()
                .min_w(px(0.))
                .overflow_hidden()
                .child(tab_btn(Tab::Usage, i18n::t(i18n::Key::Usage)))
                .child(tab_btn(Tab::Sessions, i18n::t(i18n::Key::Sessions)))
                .when(self.agent.supports_compaction(), |d| {
                    d.child(tab_btn(Tab::Compaction, i18n::t(i18n::Key::Compaction)))
                })
                .when(show_period, |d| {
                    d.child(
                        div()
                            .flex()
                            .gap_1()
                            .pl_2()
                            .border_l_1()
                            .border_color(rgb(BORDER))
                            .child(period_btn(PeriodKind::Day))
                            .child(period_btn(PeriodKind::Week))
                            .child(period_btn(PeriodKind::Month)),
                    )
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap_1()
                            .pl_2()
                            .border_l_1()
                            .border_color(rgb(BORDER))
                            .child(earlier_btn)
                            .child(range_indicator)
                            .child(later_btn)
                            .children(jump_today_btn),
                    )
                })
        };

        let right_bar = div()
            .flex()
            .items_center()
            .gap_3()
            .flex_shrink_0()
            .child(right_status)
            .when(cfg!(target_os = "windows"), |bar| {
                bar.child(self.windows_caption_buttons(window))
            });

        div()
            .flex()
            .w_full()
            .min_w(px(0.))
            .items_center()
            .pl_4()
            .pr(if cfg!(target_os = "windows") {
                px(0.)
            } else {
                px(16.)
            })
            .h(px(48.))
            // 一体化标题栏：顶栏区域负责窗口拖拽，内部按钮点击不受影响
            .window_control_area(WindowControlArea::Drag)
            .child(left_controls)
            .child(div().flex_1().min_w(px(8.)))
            .child(right_bar)
    }

    /// Windows 专用的窗口控制按钮。不要挂 on_click：这些区域会被系统
    /// 命中为 HTMINBUTTON/HTMAXBUTTON/HTCLOSE，松开鼠标时由 GPUI 执行
    /// 最小化/最大化/WM_CLOSE（关闭通过 on_window_should_close 转为托盘驻留）。
    /// `.occlude()` 把按钮区域从父级的 Drag 命中测试中挖出来，否则整条
    /// 顶栏都会被当作标题栏拖拽区。
    fn windows_caption_buttons(&self, window: &mut Window) -> impl IntoElement {
        // Win11 为 Segoe Fluent Icons；若需支持 Win10 改为 "Segoe MDL2 Assets"
        const ICON_FONT: &str = "Segoe Fluent Icons";
        let caption_button =
            |id: &'static str, icon: &str, area: WindowControlArea, danger: bool| {
                div()
                    .id(id)
                    .occlude()
                    .window_control_area(area)
                    .flex()
                    .items_center()
                    .justify_center()
                    .w(px(46.))
                    .h_full()
                    .text_size(px(10.))
                    .font_family(ICON_FONT)
                    .text_color(rgb(MUTED))
                    .hover(|s| {
                        if danger {
                            s.bg(rgb(0xe81123)).text_color(rgb(0xffffff))
                        } else {
                            s.bg(rgb(PANEL2)).text_color(rgb(TEXT))
                        }
                    })
                    .child(icon.to_string())
            };
        div()
            .flex()
            .flex_shrink_0()
            .h_full()
            .child(caption_button(
                "win-min",
                "\u{e921}",
                WindowControlArea::Min,
                false,
            ))
            .child(if window.is_maximized() {
                caption_button("win-restore", "\u{e923}", WindowControlArea::Max, false)
            } else {
                caption_button("win-max", "\u{e922}", WindowControlArea::Max, false)
            })
            .child(caption_button(
                "win-close",
                "\u{e8bb}",
                WindowControlArea::Close,
                true,
            ))
    }

    fn loading_view(&self) -> impl IntoElement {
        div()
            .size_full()
            .bg(rgb(BG))
            .flex()
            .flex_col()
            .items_center()
            .justify_center()
            .gap_4()
            .child(loading_dots(10., 2.))
            .child(
                div()
                    .text_lg()
                    .font_weight(gpui::FontWeight::SEMIBOLD)
                    .text_color(rgb(TEXT))
                    .child(i18n::t(i18n::Key::LoadingData)),
            )
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child(i18n::t(i18n::Key::LoadingCacheHint)),
            )
    }

    fn stat_card(label: &'static str, value: String, color: u32) -> impl IntoElement {
        div()
            .flex_1()
            .min_w(px(120.))
            .flex()
            .flex_col()
            .gap_1()
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child(label.to_string()),
            )
            .child(
                div()
                    .text_lg()
                    .font_weight(gpui::FontWeight::BOLD)
                    .text_color(rgb(color))
                    .child(value),
            )
    }

    fn usage_view(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let buckets = &self.buckets;
        let (mut ti, mut to, mut tc, mut tw, mut turns, mut cost) = (0.0, 0.0, 0.0, 0.0, 0u32, 0.0);
        let mut sessions = std::collections::BTreeSet::new();
        for b in buckets {
            ti += b.input;
            to += b.output;
            tc += b.cached;
            tw += b.cache_write;
            turns += b.turns;
            cost += b.cost;
            sessions.extend(b.session_keys.iter().cloned());
        }
        // 四类 token 全部计入总计：与定价公式、CLI 表格保持一致（见 agg::Bucket::total）
        let total = ti + to + tc + tw;

        // 7 张卡在默认窗口下刚好一行；窗口变窄时换行而不是被裁掉。
        let cards = div()
            .flex()
            .flex_wrap()
            .w_full()
            .min_w(px(0.))
            .gap_2()
            .child(Self::stat_card(
                i18n::t(i18n::Key::StatTotalTokens),
                fmt_tokens(total),
                TEXT,
            ))
            .child(Self::stat_card(
                i18n::t(i18n::Key::StatInput),
                fmt_tokens(ti),
                C_IN,
            ))
            .child(Self::stat_card(
                i18n::t(i18n::Key::StatOutput),
                fmt_tokens(to),
                C_OUT,
            ))
            .child(Self::stat_card(
                i18n::t(i18n::Key::StatCacheWrite),
                fmt_tokens(tw),
                C_CACHE_WRITE,
            ))
            .child(Self::stat_card(
                i18n::t(i18n::Key::StatCached),
                fmt_tokens(tc),
                C_CACHED,
            ))
            .child(Self::stat_card(
                i18n::t(if self.agent == AgentKind::Devin {
                    i18n::Key::StatCostPro
                } else {
                    i18n::Key::StatCost
                }),
                pricing::fmt_cost(cost),
                C_COST,
            ))
            .child(Self::stat_card(
                i18n::t(i18n::Key::StatTurnsSessions),
                format!("{turns} / {}", sessions.len()),
                MUTED,
            ));

        let latest_activity = self
            .data
            .sessions
            .iter()
            .filter(|session| session.agent == self.agent)
            .map(|session| session.last_activity_at)
            .max()
            .map(fmt_ts)
            .unwrap_or_else(|| i18n::t(i18n::Key::NoRecord).into());
        let empty_message = i18n::tf(
            i18n::Key::EmptyWindow,
            &[self.period.label(), self.agent.label(), &latest_activity],
        );
        let chart = self.chart();
        let table = self.bucket_table(cx);
        let range_str = format_window_range(self.period, self.page);
        let period_indicator = div()
            .flex()
            .items_center()
            .gap_2()
            .text_xs()
            .text_color(rgb(MUTED))
            .child(div().w(px(6.)).h(px(6.)).rounded_full().bg(rgb(ACCENT)))
            .child(i18n::tf(i18n::Key::PeriodRangeLabel, &[&range_str]));

        // 统计卡与图表固定，仅底部明细表格独立滚动
        div()
            .id("usage-layout")
            .flex_1()
            .min_w(px(0.))
            .min_h(px(0.))
            .p_4()
            .flex()
            .flex_col()
            .gap_3()
            .overflow_hidden()
            .child(period_indicator)
            .child(cards)
            .when(turns == 0, |d| {
                d.child(
                    div()
                        .w_full()
                        .min_w(px(0.))
                        .px_3()
                        .py_2()
                        .rounded_md()
                        .bg(rgb(PANEL))
                        .border_1()
                        .border_color(rgb(BORDER))
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child(empty_message.clone()),
                )
            })
            .child(chart)
            .child(
                div()
                    .id("bucket-table-scroll")
                    .flex_1()
                    .min_w(px(0.))
                    .min_h(px(0.))
                    .overflow_scroll()
                    .child(table),
            )
    }

    fn chart(&self) -> impl IntoElement {
        let max = self
            .buckets
            .iter()
            .map(|b| b.total())
            .fold(1.0f64, f64::max);
        let bar_area = px(140.);
        let bar_area_f: f32 = bar_area.into();
        let mut cols: Vec<gpui::Div> = Vec::new();
        for b in &self.buckets {
            let total = b.total();
            let scale = |v: f64| {
                if v <= 0.0 {
                    px(0.)
                } else {
                    px(((v / max) as f32 * bar_area_f).max(2.0))
                }
            };
            let h_in = scale(b.input);
            let h_out = scale(b.output);
            let h_ca = scale(b.cached);
            let h_cw = scale(b.cache_write);
            let label_color = if total <= 0.0 { MUTED } else { TEXT };
            cols.push(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .flex()
                    .flex_col()
                    .items_center()
                    .gap_1()
                    .child(
                        div()
                            .w_full()
                            .min_w(px(0.))
                            .text_center()
                            .whitespace_nowrap()
                            .text_xs()
                            .text_color(rgb(label_color))
                            .child(if total > 0.0 {
                                fmt_tokens(total)
                            } else {
                                "·".into()
                            }),
                    )
                    .child(
                        div()
                            .w(px(26.))
                            .h(bar_area)
                            .flex()
                            .flex_col()
                            .justify_end()
                            .child(div().w_full().h(h_cw).bg(rgba(C_CACHE_WRITE, 0.9)))
                            .child(div().w_full().h(h_ca).bg(rgba(C_CACHED, 0.9)))
                            .child(div().w_full().h(h_in).bg(rgba(C_IN, 0.9)))
                            .child(div().w_full().h(h_out).bg(rgba(C_OUT, 0.9))),
                    )
                    .child(
                        div()
                            .w_full()
                            .min_w(px(0.))
                            .text_center()
                            .whitespace_nowrap()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .child(b.label.clone()),
                    ),
            );
        }
        div()
            .w_full()
            .min_w(px(0.))
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .flex()
            .flex_col()
            .gap_2()
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_3()
                    .child(
                        div()
                            .text_sm()
                            .font_weight(gpui::FontWeight::SEMIBOLD)
                            .text_color(rgb(TEXT))
                            .child(i18n::t(i18n::Key::ChartTitle)),
                    )
                    .child(legend(i18n::t(i18n::Key::LegendCacheWrite), C_CACHE_WRITE))
                    .child(legend(i18n::t(i18n::Key::LegendCached), C_CACHED))
                    .child(legend(i18n::t(i18n::Key::LegendIn), C_IN))
                    .child(legend(i18n::t(i18n::Key::LegendOut), C_OUT)),
            )
            .child(
                div()
                    .w_full()
                    .min_w(px(0.))
                    .flex()
                    .items_end()
                    .gap_1()
                    .children(cols),
            )
    }

    fn bucket_table(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let header = usage_table::usage_row(
            i18n::t(i18n::Key::ThPeriod).to_string(),
            [
                i18n::t(i18n::Key::ThSessions).to_string(),
                i18n::t(i18n::Key::ThTurns).to_string(),
                i18n::t(i18n::Key::ThInput).to_string(),
                i18n::t(i18n::Key::ThOutput).to_string(),
                i18n::t(i18n::Key::CliCacheCreate).to_string(),
                i18n::t(i18n::Key::ThCache).to_string(),
                i18n::t(i18n::Key::ThTotal).to_string(),
                i18n::t(i18n::Key::ThCost).to_string(),
            ],
            div()
                .flex_1()
                .min_w(px(0.))
                .child(i18n::t(i18n::Key::ThModelMix)),
            false,
        )
        .text_color(rgb(MUTED));
        let mut rows = Vec::new();
        for bucket in self.buckets.iter().rev() {
            if self.hide_empty_buckets && bucket.total() == 0.0 && bucket.turns == 0 {
                continue;
            }
            let mut models: Vec<_> = bucket
                .by_model
                .iter()
                .filter(|(_, usage)| usage.total() > 0.)
                .collect();
            models.sort_by(|left, right| {
                right
                    .1
                    .total()
                    .total_cmp(&left.1.total())
                    .then_with(|| left.0.cmp(right.0))
            });
            let can_expand = models.len() > 1;
            let expanded = can_expand && self.expanded_buckets.contains(&bucket.start);
            let key = bucket.start;
            let label_text = if bucket.sub.is_empty() {
                bucket.label.clone()
            } else {
                format!("{} {}", bucket.label, bucket.sub)
            };
            let label = div()
                .flex()
                .items_center()
                .gap_1()
                .child(
                    div()
                        .w(px(10.))
                        .flex_none()
                        .text_center()
                        .child(if !can_expand {
                            ""
                        } else if expanded {
                            "▾"
                        } else {
                            "▸"
                        }),
                )
                .child(
                    div()
                        .flex_1()
                        .min_w(px(0.))
                        .whitespace_nowrap()
                        .child(label_text),
                );
            let distribution = if models.is_empty() {
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child("-")
            } else if models.len() == 1 {
                let (model, usage) = &models[0];
                let label = format!("{} {}", model, fmt_tokens(usage.total()));
                model_distribution::model_summary_single(label, model_color(model))
            } else {
                let bucket_total = bucket.total();
                let segments: Vec<(u32, f32)> = models
                    .iter()
                    .map(|(model, usage)| {
                        let ratio = if bucket_total > 0. {
                            (usage.total() / bucket_total) as f32
                        } else {
                            0.0
                        };
                        (model_color(model), ratio)
                    })
                    .collect();
                let (top_model, top_usage) = &models[0];
                let top_pct = if bucket_total > 0. {
                    ((top_usage.total() / bucket_total) * 100.0).round() as u32
                } else {
                    0
                };
                model_distribution::model_summary_stacked(
                    segments,
                    top_model.to_string(),
                    model_color(top_model),
                    format!("{top_pct}%"),
                    models.len() - 1,
                )
            };
            rows.push(
                usage_table::usage_row(
                    label,
                    [
                        bucket.session_keys.len().to_string(),
                        bucket.turns.to_string(),
                        fmt_tokens(bucket.input),
                        fmt_tokens(bucket.output),
                        fmt_tokens(bucket.cache_write),
                        fmt_tokens(bucket.cached),
                        fmt_tokens(bucket.total()),
                        pricing::fmt_cost(bucket.cost),
                    ],
                    distribution,
                    false,
                )
                .id(SharedString::from(format!("bucket-{key}")))
                .text_color(if bucket.total() > 0. {
                    rgba(TEXT, 1.)
                } else {
                    rgba(MUTED, 0.5)
                })
                .when(can_expand, |row| {
                    row.cursor_pointer()
                        .hover(|row| row.bg(rgb(PANEL2)))
                        .on_click(cx.listener(move |root, _, _, cx| {
                            if !root.expanded_buckets.remove(&key) {
                                root.expanded_buckets.insert(key);
                            }
                            cx.notify();
                        }))
                })
                .into_any_element(),
            );
            if expanded {
                let bucket_total = bucket.total();
                let sub_headers = [
                    i18n::t(i18n::Key::ThModel).to_string(),
                    i18n::t(i18n::Key::ThShare).to_string(),
                    i18n::t(i18n::Key::ThTurns).to_string(),
                    i18n::t(i18n::Key::ThInput).to_string(),
                    i18n::t(i18n::Key::ThOutput).to_string(),
                    i18n::t(i18n::Key::CliCacheCreate).to_string(),
                    i18n::t(i18n::Key::CliCacheRead).to_string(),
                    i18n::t(i18n::Key::ThTotal).to_string(),
                    i18n::t(i18n::Key::ThCost).to_string(),
                ];
                let breakdown_rows: Vec<usage_table::ModelBreakdownItem> = models
                    .iter()
                    .map(|(model, usage)| {
                        let pct = if bucket_total > 0. {
                            (usage.total() / bucket_total) * 100.0
                        } else {
                            0.0
                        };
                        let share = if pct >= 0.1 || pct == 0.0 {
                            format!("{:.1}%", pct)
                        } else {
                            "<0.1%".to_string()
                        };
                        let cost = if usage.cost > 0. {
                            pricing::fmt_cost(usage.cost)
                        } else if usage.priced {
                            "$0.00".into()
                        } else {
                            "—".into()
                        };
                        usage_table::ModelBreakdownItem {
                            name: model.to_string(),
                            color: model_color(model),
                            share,
                            turns: usage.turns.to_string(),
                            input: fmt_tokens(usage.input),
                            output: fmt_tokens(usage.output),
                            cache_creation: fmt_tokens(usage.cache_write),
                            cached: fmt_tokens(usage.cached),
                            total: fmt_tokens(usage.total()),
                            cost,
                        }
                    })
                    .collect();

                let subpanel = usage_table::model_breakdown_panel(
                    i18n::tf(i18n::Key::ModelBreakdownTitle, &[&bucket.label]),
                    i18n::tf(i18n::Key::ModelCountSummary, &[&models.len().to_string()]),
                    sub_headers,
                    breakdown_rows,
                );
                rows.push(subpanel.into_any_element());
            }
        }

        let table_toolbar = div()
            .flex()
            .items_center()
            .justify_between()
            .pb_2()
            .child(
                div()
                    .text_xs()
                    .font_weight(gpui::FontWeight::BOLD)
                    .text_color(rgb(TEXT))
                    .child(i18n::t(i18n::Key::ThPeriod)),
            )
            .child(
                div()
                    .id("toggle-hide-empty")
                    .px_2()
                    .py(px(2.))
                    .rounded_sm()
                    .text_xs()
                    .cursor_pointer()
                    .flex()
                    .items_center()
                    .gap_1()
                    .when(self.hide_empty_buckets, |d| {
                        d.text_color(rgb(ACCENT)).bg(rgba(ACCENT, 0.15))
                    })
                    .when(!self.hide_empty_buckets, |d| {
                        d.text_color(rgb(MUTED))
                            .hover(|h| h.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                    })
                    .child(if self.hide_empty_buckets {
                        "☑"
                    } else {
                        "☐"
                    })
                    .child(i18n::t(i18n::Key::HideEmptyDays))
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.hide_empty_buckets = !this.hide_empty_buckets;
                        cx.notify();
                    })),
            );

        if rows.is_empty() {
            rows.push(
                div()
                    .py_6()
                    .text_center()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child("当前周期无活动记录")
                    .into_any_element(),
            );
        }

        div()
            .w_full()
            .min_w(px(0.))
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .flex()
            .flex_col()
            .child(table_toolbar)
            .child(header)
            .children(rows)
    }

    fn update_compaction(&mut self, cx: &mut Context<Self>) {
        let key = (
            Arc::as_ptr(&self.data) as usize,
            self.agent,
            self.period,
            self.page,
            self.device_filter.clone(),
        );
        if self.compaction_key.as_ref() == Some(&key) {
            return;
        }
        self.compaction_key = Some(key.clone());
        self.compaction_report = None;
        self.compaction_notice = None;
        let data = self.data.clone();
        let agent = self.agent;
        let device = self.device_filter.clone();
        let (start, end) = window_for(self.period, self.page);
        // Measure the advice against the thresholds set now, not only against
        // where compactions happened to land.
        let models = data
            .turns
            .iter()
            .filter(|t| t.agent == agent)
            .map(|t| t.model.trim().to_ascii_lowercase())
            .collect::<std::collections::BTreeSet<_>>()
            .into_iter();
        let work = cx.background_executor().spawn(async move {
            let current = current_thresholds(agent, models);
            compaction::analyze(&data, agent, device.as_deref(), start, end, &current)
        });
        self.compaction_task = Some(cx.spawn(async move |this, cx| {
            let report = work.await;
            this.update(cx, |this, cx| {
                if this.compaction_key.as_ref() == Some(&key) {
                    this.compaction_report = Some(report);
                    cx.notify();
                }
            })
            .ok();
        }));
    }

    fn compaction_view(&self, cx: &mut Context<Self>) -> impl IntoElement {
        use compaction::Status;
        use i18n::{t, tf, Key};
        let mut body = div()
            .id("compaction-scroll")
            .size_full()
            .min_w(px(0.))
            .overflow_y_scroll()
            .p_4()
            .flex()
            .flex_col()
            .gap_3()
            .child(
                div()
                    .text_lg()
                    .font_weight(gpui::FontWeight::BOLD)
                    .child(t(Key::Compaction)),
            )
            .child(
                div()
                    .text_sm()
                    .text_color(rgb(MUTED))
                    .whitespace_normal()
                    .child(t(Key::CompactionIntro)),
            );
        if !matches!(self.agent, AgentKind::Claude | AgentKind::Codex) {
            return body.child(
                div()
                    .text_sm()
                    .text_color(rgb(MUTED))
                    .whitespace_normal()
                    .child(t(Key::CompactionUnsupported)),
            );
        }
        if let Some((ok, message)) = &self.compaction_notice {
            body = body.child(
                div()
                    .p_3()
                    .rounded_md()
                    .bg(rgb(PANEL2))
                    .text_sm()
                    .text_color(rgb(if *ok { C_OUT } else { C_COST }))
                    .whitespace_normal()
                    .child(message.clone()),
            );
        }
        // Saved local changes do not depend on the current transcript range.
        if let Ok(models) =
            devin_usage_metrics::compaction_config::Target::pending_local(self.agent)
        {
            for model in models {
                let displayed = self.compaction_report.as_ref().is_some_and(|report| {
                    report.recommendations.iter().any(|r| {
                        (r.device_id.is_empty() || r.device_id == data::device_id())
                            && devin_usage_metrics::compaction_config::Target::local(
                                self.agent, &r.model,
                            )
                            .is_ok_and(|target| {
                                target.key
                                    == if self.agent == AgentKind::Claude {
                                        format!("modelSettings.{model}.autoCompactWindow")
                                    } else {
                                        "model_auto_compact_token_limit".into()
                                    }
                            })
                    })
                });
                if !displayed {
                    body = body.child(
                        div()
                            .p_4()
                            .rounded_md()
                            .bg(rgb(PANEL))
                            .child(self.compaction_config_panel("", &model, None, cx)),
                    );
                }
            }
        }
        let Some(report) = &self.compaction_report else {
            return body.child(
                div()
                    .text_sm()
                    .text_color(rgb(MUTED))
                    .child(t(Key::Loading)),
            );
        };
        if report.skipped > 0 {
            body = body.child(
                div()
                    .text_sm()
                    .text_color(rgb(C_COST))
                    .whitespace_normal()
                    .child(tf(Key::CompactionSkipped, &[&report.skipped.to_string()])),
            );
        }
        if report.recommendations.is_empty() {
            body = body.child(
                div()
                    .text_sm()
                    .text_color(rgb(MUTED))
                    .whitespace_normal()
                    .child(t(Key::CompactionEmpty)),
            );
        }
        for r in &report.recommendations {
            let device = if r.device_name.is_empty() {
                &r.device_id
            } else {
                &r.device_name
            };
            let mut card = div()
                .w_full()
                .min_w(px(0.))
                .flex_none()
                .p_4()
                .rounded_md()
                .bg(rgb(PANEL))
                .border_1()
                .border_color(rgb(BORDER))
                .flex()
                .flex_col()
                .gap_3();

            // 头部标题与样本统计
            card = card.child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap_2()
                            .child(
                                div()
                                    .text_sm()
                                    .font_weight(gpui::FontWeight::BOLD)
                                    .text_color(rgb(TEXT))
                                    .child(r.model.clone()),
                            )
                            .child(
                                div()
                                    .px_2()
                                    .py(px(1.))
                                    .rounded_sm()
                                    .bg(rgb(PANEL2))
                                    .text_xs()
                                    .text_color(rgb(MUTED))
                                    .child(device.to_string()),
                            ),
                    )
                    .child(div().text_xs().text_color(rgb(MUTED)).child(tf(
                        Key::CompactionSample,
                        &[
                            &r.calls.to_string(),
                            &r.streams.to_string(),
                            &r.compactions.to_string(),
                        ],
                    ))),
            );

            // 核心结论 Hero 横幅 (结论先行，清晰明确)
            let keeping = r.status == Status::Keep;

            let (
                hero_icon,
                hero_icon_bg,
                hero_icon_color,
                hero_title,
                hero_badge,
                hero_badge_bg,
                hero_badge_color,
                hero_desc,
            ) = match r.status {
                Status::Keep => {
                    let desc = if r.sensitivity_failed {
                        tf(
                            Key::CompactSensitivity,
                            &[&format!("{:.1}", r.cautious_saving * 100.)],
                        )
                    } else if r.candidate < r.current {
                        tf(
                            Key::CompactionKeepMarginal,
                            &[
                                &format!("{:.1}", r.candidate_saving * 100.),
                                &r.candidate_compactions
                                    .saturating_sub(r.baseline_compactions)
                                    .to_string(),
                            ],
                        )
                    } else {
                        t(Key::CompactionKeep).into()
                    };
                    (
                        "✓",
                        rgba(C_OUT, 0.12),
                        C_OUT,
                        format!(
                            "{} ({})",
                            t(Key::CompactKeepTitle),
                            fmt_tokens(r.current as f64)
                        ),
                        t(Key::CompactOptimal),
                        rgba(C_OUT, 0.15),
                        C_OUT,
                        desc,
                    )
                }
                Status::Recommend => (
                    "⚡",
                    rgba(ACCENT, 0.12),
                    ACCENT,
                    format!(
                        "{} ({})",
                        t(Key::CompactChangeTitle),
                        fmt_tokens(r.recommended as f64)
                    ),
                    t(Key::CompactRecommendBadge),
                    rgba(ACCENT, 0.15),
                    ACCENT,
                    format!(
                        "检测到调优至 {} 预计可节省 {:.1}% 费用，且返工增量在安全范围内。",
                        fmt_tokens(r.recommended as f64),
                        r.candidate_saving * 100.,
                    ),
                ),
                Status::NotEnoughCalls => (
                    "ℹ",
                    rgba(MUTED, 0.12),
                    MUTED,
                    t(Key::CompactionFewCalls).into(),
                    "样本不足",
                    rgba(MUTED, 0.15),
                    MUTED,
                    t(Key::CompactionFewCalls).into(),
                ),
                Status::NotEnoughCompactions => (
                    "ℹ",
                    rgba(MUTED, 0.12),
                    MUTED,
                    t(Key::CompactionFewCuts).into(),
                    "样本不足",
                    rgba(MUTED, 0.15),
                    MUTED,
                    t(Key::CompactionFewCuts).into(),
                ),
                Status::UnknownPricing => (
                    "!",
                    rgba(C_COST, 0.12),
                    C_COST,
                    t(Key::CompactionNoPrice).into(),
                    "缺少定价",
                    rgba(C_COST, 0.15),
                    C_COST,
                    t(Key::CompactionNoPrice).into(),
                ),
            };

            let hero_card = div()
                .p_3()
                .rounded_md()
                .bg(rgb(PANEL2))
                .border_1()
                .border_color(rgb(BORDER))
                .flex()
                .items_start()
                .gap_3()
                .child(
                    div()
                        .w(px(32.))
                        .h(px(32.))
                        .rounded_md()
                        .bg(hero_icon_bg)
                        .text_color(rgb(hero_icon_color))
                        .flex()
                        .items_center()
                        .justify_center()
                        .font_weight(gpui::FontWeight::BOLD)
                        .child(hero_icon),
                )
                .child(
                    div()
                        .flex_1()
                        .min_w(px(0.))
                        .flex()
                        .flex_col()
                        .gap_1()
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap_2()
                                .child(
                                    div()
                                        .text_sm()
                                        .font_weight(gpui::FontWeight::BOLD)
                                        .text_color(rgb(TEXT))
                                        .child(hero_title),
                                )
                                .child(
                                    div()
                                        .px_2()
                                        .py(px(1.))
                                        .rounded_sm()
                                        .bg(hero_badge_bg)
                                        .text_xs()
                                        .font_weight(gpui::FontWeight::BOLD)
                                        .text_color(rgb(hero_badge_color))
                                        .child(hero_badge),
                                ),
                        )
                        .child(
                            div()
                                .text_xs()
                                .text_color(rgb(MUTED))
                                .whitespace_normal()
                                .child(hero_desc),
                        ),
                );

            card = card.child(hero_card);

            if matches!(r.status, Status::Recommend | Status::Keep) {
                let baseline_label = if r.baseline_inferred {
                    Key::CompactBaselineInferred
                } else {
                    Key::CompactBaselineConfigured
                };
                let diff_cuts = r
                    .candidate_compactions
                    .saturating_sub(r.baseline_compactions);
                let stat_matrix = div()
                    .flex()
                    .flex_wrap()
                    .w_full()
                    .gap_2()
                    .child(compaction_ui::metric_card(
                        t(baseline_label),
                        fmt_tokens(r.current as f64),
                        Some(pricing::fmt_cost(r.baseline_cost)),
                        TEXT,
                    ))
                    .child(compaction_ui::metric_card(
                        t(if keeping {
                            Key::CompactCandidate
                        } else {
                            Key::CompactionRecommended
                        }),
                        fmt_tokens(if keeping { r.candidate } else { r.recommended } as f64),
                        Some(pricing::fmt_cost(if keeping {
                            r.candidate_cost
                        } else {
                            r.recommended_cost
                        })),
                        if keeping { MUTED } else { ACCENT },
                    ))
                    .child(compaction_ui::metric_card(
                        t(if keeping {
                            Key::CompactCandidateSaving
                        } else {
                            Key::CompactionSaving
                        }),
                        format!("{:.1}%", r.candidate_saving * 100.),
                        Some(format!(
                            "~{}",
                            pricing::fmt_cost(r.baseline_cost * r.candidate_saving.max(0.0))
                        )),
                        if r.candidate_saving >= 0.03 {
                            C_OUT
                        } else {
                            MUTED
                        },
                    ))
                    .child(compaction_ui::metric_card(
                        t(Key::CompactImpactTitle),
                        format!("+{} 次", diff_cuts),
                        Some(format!(
                            "{} → {} 次",
                            r.baseline_compactions, r.candidate_compactions
                        )),
                        if diff_cuts > 100 { C_COST } else { MUTED },
                    ));
                card = card.child(stat_matrix);

                // 本机配置设置卡片 (精致单行)
                card = card.child(self.compaction_config_panel(
                    &r.device_id,
                    &r.model,
                    (r.status == Status::Recommend).then_some(r.recommended),
                    cx,
                ));

                // 成本曲线
                if r.curve.len() > 1 {
                    card = card.child(compaction_ui::curve(r));
                }

                // 渐进式披露：折叠面板收纳算法依据与样本明细
                let details_open = self.compaction_details_open;
                let mut drawer = div()
                    .w_full()
                    .rounded_md()
                    .bg(rgb(PANEL2))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .flex()
                    .flex_col();

                drawer = drawer.child(
                    div()
                        .id("compaction-details-toggle")
                        .px_3()
                        .py_2()
                        .flex()
                        .items_center()
                        .justify_between()
                        .cursor_pointer()
                        .hover(|h| h.bg(rgb(PANEL)))
                        .on_click(cx.listener(|this, _, _, cx| {
                            this.compaction_details_open = !this.compaction_details_open;
                            cx.notify();
                        }))
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap_2()
                                .text_xs()
                                .text_color(rgb(MUTED))
                                .child(if details_open { "▼" } else { "▶" })
                                .child(format!(
                                    "{} ({} 次调用样本)",
                                    t(Key::CompactDetails),
                                    r.calls
                                )),
                        )
                        .child(
                            div()
                                .text_xs()
                                .text_color(rgb(MUTED))
                                .child(if details_open {
                                    "收起"
                                } else {
                                    "展开明细"
                                }),
                        ),
                );

                if details_open {
                    let mut details_body = div()
                        .px_3()
                        .py_3()
                        .border_t_1()
                        .border_color(rgb(BORDER))
                        .flex()
                        .flex_col()
                        .gap_2()
                        .text_xs()
                        .text_color(rgb(MUTED));

                    let params_grid = div()
                        .flex()
                        .flex_wrap()
                        .gap_2()
                        .child(
                            div()
                                .flex_1()
                                .min_w(px(100.))
                                .p_2()
                                .rounded_sm()
                                .bg(rgb(BG))
                                .child(div().text_xs().text_color(rgb(MUTED)).child("单次调用增长"))
                                .child(
                                    div()
                                        .text_xs()
                                        .font_weight(gpui::FontWeight::BOLD)
                                        .text_color(rgb(TEXT))
                                        .child(fmt_tokens(r.growth as f64)),
                                ),
                        )
                        .child(
                            div()
                                .flex_1()
                                .min_w(px(100.))
                                .p_2()
                                .rounded_sm()
                                .bg(rgb(BG))
                                .child(div().text_xs().text_color(rgb(MUTED)).child("压缩后保留"))
                                .child(
                                    div()
                                        .text_xs()
                                        .font_weight(gpui::FontWeight::BOLD)
                                        .text_color(rgb(TEXT))
                                        .child(fmt_tokens(r.after as f64)),
                                ),
                        )
                        .child(
                            div()
                                .flex_1()
                                .min_w(px(100.))
                                .p_2()
                                .rounded_sm()
                                .bg(rgb(BG))
                                .child(div().text_xs().text_color(rgb(MUTED)).child("平均估计返工"))
                                .child(
                                    div()
                                        .text_xs()
                                        .font_weight(gpui::FontWeight::BOLD)
                                        .text_color(rgb(TEXT))
                                        .child(fmt_tokens(r.rework as f64)),
                                ),
                        )
                        .child(
                            div()
                                .flex_1()
                                .min_w(px(100.))
                                .p_2()
                                .rounded_sm()
                                .bg(rgb(BG))
                                .child(div().text_xs().text_color(rgb(MUTED)).child("疑似压缩事件"))
                                .child(
                                    div()
                                        .text_xs()
                                        .font_weight(gpui::FontWeight::BOLD)
                                        .text_color(rgb(TEXT))
                                        .child(format!("{} 次", r.compactions)),
                                ),
                        );

                    details_body = details_body.child(params_grid);

                    details_body = details_body.child(div().whitespace_normal().child(tf(
                        Key::CompactEvidence,
                        &[
                            &r.priced_calls.to_string(),
                            &r.compactions.to_string(),
                            &r.baseline_compactions.to_string(),
                        ],
                    )));

                    if r.unpriced > 0 {
                        details_body = details_body.child(
                            div()
                                .text_color(rgb(C_COST))
                                .whitespace_normal()
                                .child(tf(Key::CompactionUnpriced, &[&r.unpriced.to_string()])),
                        );
                    }

                    if r.baseline_inferred {
                        details_body = details_body
                            .child(div().whitespace_normal().child(t(Key::CompactInferredNote)));
                    }

                    details_body = details_body
                        .child(div().whitespace_normal().child(t(Key::CompactionCaveat)));

                    drawer = drawer.child(details_body);
                }

                card = card.child(drawer);
            } else {
                card = card.child(self.compaction_config_panel(&r.device_id, &r.model, None, cx));
            }

            body = body.child(card);
        }
        body
    }

    fn render_session_header(has_modes: bool) -> gpui::Div {
        let mut header = div()
            .w_full()
            .min_w(px(0.))
            .flex_none()
            .h(px(26.))
            .flex()
            .gap_2()
            .px_2()
            .items_center()
            .text_xs()
            .text_color(rgb(MUTED))
            .border_b_1()
            .border_color(rgb(BORDER))
            .child(cell(i18n::t(i18n::Key::ThLastActive), 80.))
            .child(
                div()
                    .w(px(110.))
                    .flex_none()
                    .min_w(px(0.))
                    .truncate()
                    .child(i18n::t(i18n::Key::ThSession)),
            )
            .child(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .overflow_hidden()
                    .truncate()
                    .child(i18n::t(i18n::Key::ThTitle)),
            );
        if has_modes {
            header = header.child(cell(i18n::t(i18n::Key::ThMode), 60.));
        }
        header
            .child(cell(i18n::t(i18n::Key::ThModel), 140.))
            .child(cell_r(i18n::t(i18n::Key::ThMsgs), 42.))
            .child(cell_r(i18n::t(i18n::Key::ThTokens), 65.))
            .child(cell_r(i18n::t(i18n::Key::ThWindowCost), 55.))
            .child(div().w(px(105.)).flex_none())
    }

    fn render_session_row(
        &self,
        r: &SessionRow,
        is_hidden: bool,
        has_modes: bool,
        cx: &mut Context<Self>,
    ) -> gpui::AnyElement {
        let s = &r.session;
        let key = s.key.clone();
        let is_sel = self.selected.as_deref() == Some(key.as_str());
        let adaptive = s.selected_model == "adaptive";
        let resume_cmd = s.resume_command();

        let mut row = div()
            .id(SharedString::from(format!("srow-{}", key)))
            .w_full()
            .min_w(px(0.))
            .flex_none()
            .h(px(28.))
            .flex()
            .gap_2()
            .px_2()
            .text_xs()
            .cursor_pointer()
            .items_center()
            .hover(|h| h.bg(rgb(PANEL2)))
            .when(is_sel, |d| d.bg(rgb(PANEL2)))
            .border_b_1()
            .border_color(rgba(BORDER, 0.4))
            .child(cell(&r.time_str, 80.))
            .child(
                div()
                    .w(px(110.))
                    .flex_none()
                    .flex()
                    .gap_1()
                    .items_center()
                    .min_w(px(0.))
                    .child(
                        div()
                            .w(px(6.))
                            .h(px(6.))
                            .flex_none()
                            .rounded_full()
                            .bg(rgb(model_color(&s.display_model()))),
                    )
                    .child(div().min_w(px(0.)).truncate().child(r.id_short.clone()))
                    .when(
                        self.agent == AgentKind::Devin && s.source == "cli-next",
                        |d| {
                            d.child(
                                div()
                                    .flex_none()
                                    .text_xs()
                                    .text_color(rgba(MUTED, 0.7))
                                    .child("next"),
                            )
                        },
                    ),
            )
            .child(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .overflow_hidden()
                    .truncate()
                    .child(r.title_short.clone()),
            );

        if has_modes {
            row = row.child(cell(&s.agent_mode, 60.));
        }

        let mut row = row
            .child(
                div()
                    .w(px(140.))
                    .flex_none()
                    .min_w(px(0.))
                    .truncate()
                    .text_color(if adaptive { rgb(ACCENT) } else { rgb(TEXT) })
                    .child(r.model_text.clone()),
            )
            .child(cell_r(&format!("{:.0}", s.agent_messages), 42.))
            .child(cell_r(&fmt_tokens(r.total), 65.))
            .child(cell_r(&r.cost_str, 55.));

        // 操作按钮列
        let key_copy = key.clone();
        let sid = s.id.clone();
        let key_toggle = key.clone();
        let mut actions = div()
            .w(px(105.))
            .flex_none()
            .flex()
            .items_center()
            .justify_end()
            .gap_1();

        // 1. 复制 ID 按钮
        actions = actions.child(
            div()
                .id(SharedString::from(format!("btn-copy-id-{}", key)))
                .px(px(4.))
                .py(px(1.))
                .rounded_sm()
                .bg(rgb(PANEL))
                .border_1()
                .border_color(rgb(BORDER))
                .text_color(rgb(MUTED))
                .cursor_pointer()
                .hover(|h| h.text_color(rgb(TEXT)).border_color(rgb(ACCENT)))
                .child("ID")
                .on_click(cx.listener({
                    let sid = sid.clone();
                    move |this, _, _, cx| {
                        this.copy_to_clipboard(sid.clone(), i18n::t(i18n::Key::SessionsCopied), cx);
                        cx.stop_propagation();
                    }
                })),
        );

        // 2. 续读命令
        if let Some(cmd) = resume_cmd {
            actions = actions.child(
                div()
                    .id(SharedString::from(format!("btn-resume-{}", key)))
                    .px(px(4.))
                    .py(px(1.))
                    .rounded_sm()
                    .bg(rgb(PANEL))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .text_color(rgb(ACCENT))
                    .cursor_pointer()
                    .hover(|h| h.bg(rgba(ACCENT, 0.15)).border_color(rgb(ACCENT)))
                    .child(i18n::t(i18n::Key::SessionsResume))
                    .on_click(cx.listener({
                        let cmd = cmd.clone();
                        move |this, _, _, cx| {
                            this.copy_to_clipboard(
                                cmd.clone(),
                                i18n::t(i18n::Key::SessionsCopied),
                                cx,
                            );
                            cx.stop_propagation();
                        }
                    })),
            );
        }

        // 3. 移入回收站 / 恢复
        actions = actions.child(
            div()
                .id(SharedString::from(format!("btn-hide-toggle-{}", key)))
                .px(px(4.))
                .py(px(1.))
                .rounded_sm()
                .bg(rgb(PANEL))
                .border_1()
                .border_color(rgb(BORDER))
                .text_color(if is_hidden { rgb(GREEN) } else { rgb(MUTED) })
                .cursor_pointer()
                .hover(|h| {
                    if is_hidden {
                        h.text_color(rgb(GREEN)).border_color(rgb(GREEN))
                    } else {
                        h.text_color(rgb(RED)).border_color(rgb(RED))
                    }
                })
                .child(if is_hidden {
                    i18n::t(i18n::Key::SessionsRestore)
                } else {
                    i18n::t(i18n::Key::SessionsHide)
                })
                .on_click(cx.listener({
                    let key_toggle = key_toggle.clone();
                    move |this, _, _, cx| {
                        if is_hidden {
                            this.restore_session(&key_toggle, cx);
                        } else {
                            this.hide_session(&key_toggle, cx);
                        }
                        cx.stop_propagation();
                    }
                })),
        );

        row = row.child(actions);

        row.on_click(cx.listener(move |this, _, _, cx| {
            this.selected = Some(key_copy.clone());
            cx.notify();
        }))
        .into_any_element()
    }

    fn session_search_box(&self, window: &Window, cx: &mut Context<Self>) -> impl IntoElement {
        let input = self.session_search_input.clone();
        let focused = input.read(cx).is_focused(window);
        let focus_input = input.clone();
        let has_text = !input.read(cx).text().is_empty();
        let clear_input = input.clone();

        div()
            .w(px(240.))
            .h(px(28.))
            .px_2()
            .rounded_md()
            .flex()
            .items_center()
            .gap_1()
            .text_xs()
            .cursor_text()
            .when(focused, |d| {
                d.bg(rgb(PANEL2)).border_1().border_color(rgb(ACCENT))
            })
            .when(!focused, |d| {
                d.bg(rgb(0x101014))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .hover(|h| h.bg(rgb(PANEL2)))
            })
            .on_mouse_down(
                MouseButton::Left,
                cx.listener(move |_, _, window, cx| {
                    focus_input.read(cx).focus(window);
                    cx.stop_propagation();
                }),
            )
            .child(div().flex_1().min_w(px(0.)).child(input))
            .when(has_text, |d| {
                d.child(
                    div()
                        .id("clear-search-query")
                        .flex_none()
                        .px_1()
                        .cursor_pointer()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .hover(|h| h.text_color(rgb(TEXT)))
                        .child("✕")
                        .on_click(cx.listener(move |_, _, _, cx| {
                            clear_input.update(cx, |input, cx| {
                                input.set_text("", cx);
                            });
                        })),
                )
            })
    }

    fn sessions_view(&self, window: &Window, cx: &mut Context<Self>) -> impl IntoElement {
        let detail = self
            .selected
            .as_ref()
            .map(|key| self.session_detail(key, cx));
        let has_modes = self
            .sessions_rows
            .iter()
            .any(|r| !r.session.agent_mode.trim().is_empty());

        let search_query = self
            .session_search_input
            .read(cx)
            .text()
            .trim()
            .to_lowercase();

        let hidden_subagent_count = self
            .sessions_rows
            .iter()
            .filter(|r| {
                !self
                    .session_prefs
                    .hidden_session_keys
                    .contains(&r.session.key)
                    && r.session.is_subagent_or_review()
            })
            .count();

        let hidden_total_count = self.session_prefs.hidden_session_keys.len();

        // 过滤会话行
        let mut filtered_rows: Vec<&SessionRow> = Vec::new();
        for r in &self.sessions_rows {
            let s = &r.session;
            let is_hidden = self.session_prefs.hidden_session_keys.contains(&s.key);
            if self.sessions_show_hidden != is_hidden {
                continue;
            }
            if !self.sessions_show_hidden
                && self.session_prefs.hide_subagents
                && s.is_subagent_or_review()
            {
                continue;
            }
            if !search_query.is_empty() {
                let matches = s.title.to_lowercase().contains(&search_query)
                    || s.id.to_lowercase().contains(&search_query)
                    || s.working_directory.to_lowercase().contains(&search_query)
                    || s.display_model().to_lowercase().contains(&search_query);
                if !matches {
                    continue;
                }
            }
            filtered_rows.push(r);
        }

        // 收集项目中所有不重复的项目名
        let mut all_project_names: Vec<String> = Vec::new();
        for r in &filtered_rows {
            let p_name = r.session.project_name();
            let p_name = if p_name.is_empty() {
                i18n::t(i18n::Key::SessionsUnspecifiedProject).to_string()
            } else {
                p_name
            };
            if !all_project_names.contains(&p_name) {
                all_project_names.push(p_name);
            }
        }

        // Toolbar 控制栏
        let group_by_project = self.session_prefs.group_by_project;
        let hide_subagents = self.session_prefs.hide_subagents;
        let show_hidden = self.sessions_show_hidden;

        let toggle_group_btn = div()
            .id("toggle-session-grouping")
            .px_2()
            .py_1()
            .rounded_md()
            .text_xs()
            .cursor_pointer()
            .flex()
            .items_center()
            .gap_1()
            .when(group_by_project, |d| {
                d.bg(rgb(PANEL2))
                    .text_color(rgb(ACCENT))
                    .border_1()
                    .border_color(rgb(ACCENT))
            })
            .when(!group_by_project, |d| {
                d.bg(rgb(PANEL))
                    .text_color(rgb(MUTED))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .hover(|h| h.text_color(rgb(TEXT)).bg(rgb(PANEL2)))
            })
            .child(if group_by_project {
                i18n::t(i18n::Key::SessionsGroupByProject).to_string()
            } else {
                i18n::t(i18n::Key::SessionsFlatView).to_string()
            })
            .on_click(cx.listener(|this, _, _, cx| {
                this.session_prefs.group_by_project = !this.session_prefs.group_by_project;
                i18n::save_session_prefs(&this.session_prefs);
                cx.notify();
            }));

        let collapse_all_btn = if group_by_project && !all_project_names.is_empty() {
            Some(
                div()
                    .id("collapse-all-projects")
                    .px_2()
                    .py_1()
                    .rounded_md()
                    .text_xs()
                    .cursor_pointer()
                    .bg(rgb(PANEL))
                    .text_color(rgb(MUTED))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .hover(|h| h.text_color(rgb(TEXT)).bg(rgb(PANEL2)))
                    .child(i18n::t(i18n::Key::SessionsCollapseAll))
                    .on_click(cx.listener({
                        let all_projects = all_project_names.clone();
                        move |this, _, _, cx| {
                            for p in &all_projects {
                                this.sessions_collapsed_projects.insert(p.clone());
                            }
                            cx.notify();
                        }
                    })),
            )
        } else {
            None
        };

        let expand_all_btn = if group_by_project && !all_project_names.is_empty() {
            Some(
                div()
                    .id("expand-all-projects")
                    .px_2()
                    .py_1()
                    .rounded_md()
                    .text_xs()
                    .cursor_pointer()
                    .bg(rgb(PANEL))
                    .text_color(rgb(MUTED))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .hover(|h| h.text_color(rgb(TEXT)).bg(rgb(PANEL2)))
                    .child(i18n::t(i18n::Key::SessionsExpandAll))
                    .on_click(cx.listener(|this, _, _, cx| {
                        this.sessions_collapsed_projects.clear();
                        cx.notify();
                    })),
            )
        } else {
            None
        };

        let toggle_subagents_btn = div()
            .id("toggle-hide-subagents")
            .px_2()
            .py_1()
            .rounded_md()
            .text_xs()
            .cursor_pointer()
            .flex()
            .items_center()
            .gap_1()
            .when(hide_subagents, |d| {
                d.bg(rgb(PANEL2))
                    .text_color(rgb(ACCENT))
                    .border_1()
                    .border_color(rgb(ACCENT))
            })
            .when(!hide_subagents, |d| {
                d.bg(rgb(PANEL))
                    .text_color(rgb(MUTED))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .hover(|h| h.text_color(rgb(TEXT)).bg(rgb(PANEL2)))
            })
            .child(if hide_subagents && hidden_subagent_count > 0 {
                format!(
                    "{} ({})",
                    i18n::t(i18n::Key::SessionsHideSubagents),
                    hidden_subagent_count
                )
            } else if hide_subagents {
                i18n::t(i18n::Key::SessionsHideSubagents).to_string()
            } else {
                i18n::t(i18n::Key::SessionsShowSubagents).to_string()
            })
            .on_click(cx.listener(|this, _, _, cx| {
                this.session_prefs.hide_subagents = !this.session_prefs.hide_subagents;
                i18n::save_session_prefs(&this.session_prefs);
                cx.notify();
            }));

        let toggle_recycle_btn = div()
            .id("toggle-recycle-bin")
            .px_2()
            .py_1()
            .rounded_md()
            .text_xs()
            .cursor_pointer()
            .flex()
            .items_center()
            .gap_1()
            .when(show_hidden, |d| {
                d.bg(rgba(RED, 0.15))
                    .text_color(rgb(RED))
                    .border_1()
                    .border_color(rgba(RED, 0.4))
            })
            .when(!show_hidden, |d| {
                d.bg(rgb(PANEL))
                    .text_color(rgb(MUTED))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .hover(|h| h.text_color(rgb(TEXT)).bg(rgb(PANEL2)))
            })
            .child(if show_hidden {
                i18n::t(i18n::Key::SessionsActive).to_string()
            } else {
                i18n::tf(
                    i18n::Key::SessionsRecycleBin,
                    &[&hidden_total_count.to_string()],
                )
            })
            .on_click(cx.listener(|this, _, _, cx| {
                this.sessions_show_hidden = !this.sessions_show_hidden;
                cx.notify();
            }));

        let feedback_el = if let Some((ref text, at)) = self.session_feedback {
            if at.elapsed().as_secs() < 3 {
                Some(
                    div()
                        .px_2()
                        .py_1()
                        .rounded_md()
                        .bg(rgba(GREEN, 0.15))
                        .text_color(rgb(GREEN))
                        .border_1()
                        .border_color(rgba(GREEN, 0.3))
                        .text_xs()
                        .child(format!("✓ {}", text)),
                )
            } else {
                None
            }
        } else {
            None
        };

        let mut toolbar = div()
            .w_full()
            .min_w(px(0.))
            .flex_none()
            .flex()
            .items_center()
            .gap_2()
            .child(self.session_search_box(window, cx))
            .child(toggle_group_btn);

        if let Some(btn) = collapse_all_btn {
            toolbar = toolbar.child(btn);
        }
        if let Some(btn) = expand_all_btn {
            toolbar = toolbar.child(btn);
        }

        toolbar = toolbar
            .child(toggle_subagents_btn)
            .child(toggle_recycle_btn);

        if let Some(fb) = feedback_el {
            toolbar = toolbar.child(fb);
        }

        let mut body = div()
            .id("sessions-scroll")
            .flex_1()
            .min_w(px(0.))
            .min_h(px(0.))
            .overflow_y_scroll()
            .p_4()
            .flex()
            .flex_col()
            .gap_3()
            .child(toolbar);

        if let Some(d) = detail {
            body = body.child(d);
        }

        if filtered_rows.is_empty() {
            let msg = if show_hidden {
                i18n::t(i18n::Key::SessionsRecycleBin).to_string()
            } else if !search_query.is_empty() {
                format!("没有找到与 \"{}\" 匹配的会话", search_query)
            } else {
                i18n::tf(i18n::Key::SessionsNotFound, &[self.agent.label()])
            };
            return body.child(
                div()
                    .w_full()
                    .min_w(px(0.))
                    .flex_none()
                    .p_8()
                    .rounded_md()
                    .bg(rgb(PANEL))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .flex()
                    .items_center()
                    .justify_center()
                    .text_sm()
                    .text_color(rgb(MUTED))
                    .child(msg),
            );
        }

        if group_by_project {
            struct Group<'a> {
                name: String,
                working_dir: String,
                rows: Vec<&'a SessionRow>,
                total_tokens: f64,
                total_cost: Option<f64>,
            }
            let mut groups: Vec<Group> = Vec::new();
            for r in filtered_rows {
                let p_name = r.session.project_name();
                let p_name = if p_name.is_empty() {
                    i18n::t(i18n::Key::SessionsUnspecifiedProject).to_string()
                } else {
                    p_name
                };
                if let Some(g) = groups.iter_mut().find(|g| g.name == p_name) {
                    g.rows.push(r);
                    g.total_tokens += r.total;
                    if let Some(c) = r.cost_val {
                        *g.total_cost.get_or_insert(0.0) += c;
                    }
                    if g.working_dir.is_empty() && !r.session.working_directory.is_empty() {
                        g.working_dir = r.session.working_directory.clone();
                    }
                } else {
                    groups.push(Group {
                        name: p_name,
                        working_dir: r.session.working_directory.clone(),
                        rows: vec![r],
                        total_tokens: r.total,
                        total_cost: r.cost_val,
                    });
                }
            }

            for group in groups {
                let is_collapsed = self.sessions_collapsed_projects.contains(&group.name);
                let p_name_clone = group.name.clone();
                let working_dir_clone = group.working_dir.clone();
                let arrow = if is_collapsed { "▸" } else { "▾" };

                let mut card = div()
                    .id(SharedString::from(format!("project-card-{}", group.name)))
                    .w_full()
                    .min_w(px(0.))
                    .flex_none()
                    .rounded_md()
                    .bg(rgb(PANEL))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .flex()
                    .flex_col();

                let mut card_header = div()
                    .id(SharedString::from(format!("project-header-{}", group.name)))
                    .w_full()
                    .min_w(px(0.))
                    .flex_none()
                    .h(px(36.))
                    .px_3()
                    .py_2()
                    .bg(rgb(PANEL2))
                    .border_b_1()
                    .border_color(if is_collapsed {
                        rgba(0, 0.0)
                    } else {
                        rgba(BORDER, 1.0)
                    })
                    .flex()
                    .items_center()
                    .gap_2()
                    .cursor_pointer()
                    .hover(|h| h.bg(rgb(0x2a2a35)))
                    .on_click(cx.listener({
                        let p_name = p_name_clone.clone();
                        move |this, _, _, cx| {
                            this.toggle_project_collapsed(&p_name, cx);
                        }
                    }))
                    .child(div().text_xs().text_color(rgb(MUTED)).child(arrow))
                    .child(
                        div()
                            .text_sm()
                            .font_weight(gpui::FontWeight::BOLD)
                            .text_color(rgb(TEXT))
                            .child(group.name.clone()),
                    );

                if !group.working_dir.is_empty() {
                    card_header = card_header.child(
                        div()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .truncate()
                            .max_w(px(320.))
                            .child(group.working_dir.clone()),
                    );
                }

                card_header = card_header
                    .child(div().flex_1())
                    .child(
                        div()
                            .px_2()
                            .py(px(1.))
                            .rounded_sm()
                            .bg(rgb(PANEL))
                            .border_1()
                            .border_color(rgb(BORDER))
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .child(format!("{} 会话", group.rows.len())),
                    )
                    .child(
                        div()
                            .px_2()
                            .py(px(1.))
                            .rounded_sm()
                            .bg(rgb(PANEL))
                            .border_1()
                            .border_color(rgb(BORDER))
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .child(format!("{} Tokens", fmt_tokens(group.total_tokens))),
                    );

                if let Some(cost) = group.total_cost {
                    card_header = card_header.child(
                        div()
                            .px_2()
                            .py(px(1.))
                            .rounded_sm()
                            .bg(rgb(PANEL))
                            .border_1()
                            .border_color(rgb(BORDER))
                            .text_xs()
                            .text_color(rgb(ACCENT))
                            .child(pricing::fmt_cost(cost)),
                    );
                }

                if !working_dir_clone.is_empty() {
                    card_header = card_header.child(
                        div()
                            .id(SharedString::from(format!("open-dir-{}", group.name)))
                            .px_2()
                            .py(px(1.))
                            .rounded_sm()
                            .bg(rgb(PANEL))
                            .border_1()
                            .border_color(rgb(BORDER))
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .cursor_pointer()
                            .hover(|h| h.text_color(rgb(TEXT)).border_color(rgb(ACCENT)))
                            .child(i18n::t(i18n::Key::SessionsOpenFolder))
                            .on_click(cx.listener({
                                let wdir = working_dir_clone.clone();
                                move |_, _, _, cx| {
                                    open_folder(&wdir);
                                    cx.stop_propagation();
                                }
                            })),
                    );
                }

                card = card.child(card_header);

                if !is_collapsed {
                    let mut rows_elements: Vec<gpui::AnyElement> = Vec::new();
                    for r in group.rows {
                        rows_elements.push(self.render_session_row(r, show_hidden, has_modes, cx));
                    }
                    card = card
                        .child(Self::render_session_header(has_modes))
                        .children(rows_elements);
                }

                body = body.child(card);
            }
        } else {
            // 平铺视图
            let mut rows_elements: Vec<gpui::AnyElement> = Vec::new();
            for r in filtered_rows {
                rows_elements.push(self.render_session_row(r, show_hidden, has_modes, cx));
            }
            body = body.child(
                div()
                    .w_full()
                    .min_w(px(0.))
                    .flex_none()
                    .p_3()
                    .rounded_md()
                    .bg(rgb(PANEL))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .flex()
                    .flex_col()
                    .child(Self::render_session_header(has_modes))
                    .children(rows_elements),
            );
        }

        body
    }

    fn session_detail(&self, key: &str, cx: &mut Context<Self>) -> gpui::Div {
        let Some(s) = self.data.sessions.iter().find(|s| s.key == key) else {
            return div();
        };
        let turns: Vec<&data::TurnRec> = self
            .data
            .turns
            .iter()
            .filter(|t| t.session_key == key)
            .collect();
        let mut ttfts: Vec<f64> = turns
            .iter()
            .map(|t| t.ttft_ms)
            .filter(|ttft| *ttft > 0.0)
            .collect();
        ttfts.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
        let ttft_med = ttfts.get(ttfts.len() / 2).copied().unwrap_or(0.0);
        let token_total = session_token_total(s, &turns);
        let meta_total =
            s.input_tokens + s.output_tokens + s.cached_tokens + s.cache_creation_tokens;
        let (t_in, t_out, t_cached, t_cache_write) = if meta_total > 0.0 || turns.is_empty() {
            (
                s.input_tokens,
                s.output_tokens,
                s.cached_tokens,
                s.cache_creation_tokens,
            )
        } else {
            (
                turns.iter().map(|t| t.input_tokens).sum(),
                turns.iter().map(|t| t.output_tokens).sum(),
                turns.iter().map(|t| t.cache_read_tokens).sum(),
                turns.iter().map(|t| t.cache_creation_tokens).sum(),
            )
        };
        let cost_summary = agg::cost_summary_for_turns(turns.iter().copied(), &s.display_model());
        let adaptive = s.selected_model == "adaptive";
        let model_text = if adaptive {
            i18n::tf(i18n::Key::AdaptiveRouted, &[&s.display_model()])
        } else {
            s.display_model()
        };
        let model_detail = if adaptive {
            i18n::tf(i18n::Key::ConfigValue, &[&model_text, &s.selected_model])
        } else {
            model_text
        };

        let kv = |k: &str, v: String| {
            div()
                .flex()
                .gap_2()
                .text_xs()
                .child(div().w(px(84.)).text_color(rgb(MUTED)).child(k.to_string()))
                .child(div().flex_1().text_color(rgb(TEXT)).child(v))
        };

        let is_hidden = self.session_prefs.hidden_session_keys.contains(key);
        let resume_cmd = s.resume_command();
        let s_id = s.id.clone();
        let s_key = key.to_string();
        let workdir = s.working_directory.clone();

        let mut actions_bar = div()
            .mt_2()
            .pt_2()
            .border_t_1()
            .border_color(rgba(BORDER, 0.4))
            .flex()
            .items_center()
            .gap_2();

        // 复制 ID 按钮
        actions_bar = actions_bar.child(
            div()
                .id(SharedString::from(format!("detail-copy-id-{}", s_key)))
                .px_2()
                .py_1()
                .rounded_md()
                .bg(rgb(PANEL2))
                .border_1()
                .border_color(rgb(BORDER))
                .text_xs()
                .cursor_pointer()
                .hover(|h| h.bg(rgb(0x2a2a35)).border_color(rgb(ACCENT)))
                .child(i18n::t(i18n::Key::SessionsCopyId))
                .on_click(cx.listener({
                    let s_id = s_id.clone();
                    move |this, _, _, cx| {
                        this.copy_to_clipboard(
                            s_id.clone(),
                            i18n::t(i18n::Key::SessionsCopied),
                            cx,
                        );
                    }
                })),
        );

        // 续读命令
        if let Some(cmd) = resume_cmd {
            actions_bar = actions_bar.child(
                div()
                    .id(SharedString::from(format!("detail-resume-{}", s_key)))
                    .px_2()
                    .py_1()
                    .rounded_md()
                    .bg(rgb(PANEL2))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .text_xs()
                    .cursor_pointer()
                    .hover(|h| h.bg(rgb(0x2a2a35)).border_color(rgb(ACCENT)))
                    .child(i18n::t(i18n::Key::SessionsResume))
                    .on_click(cx.listener({
                        let cmd = cmd.clone();
                        move |this, _, _, cx| {
                            this.copy_to_clipboard(
                                cmd.clone(),
                                i18n::t(i18n::Key::SessionsCopied),
                                cx,
                            );
                        }
                    })),
            );
        }

        // 打开目录
        if !workdir.is_empty() {
            actions_bar = actions_bar.child(
                div()
                    .id(SharedString::from(format!("detail-open-dir-{}", s_key)))
                    .px_2()
                    .py_1()
                    .rounded_md()
                    .bg(rgb(PANEL2))
                    .border_1()
                    .border_color(rgb(BORDER))
                    .text_xs()
                    .cursor_pointer()
                    .hover(|h| h.bg(rgb(0x2a2a35)).border_color(rgb(ACCENT)))
                    .child(i18n::t(i18n::Key::SessionsOpenFolder))
                    .on_click(cx.listener({
                        let workdir = workdir.clone();
                        move |_, _, _, _| {
                            open_folder(&workdir);
                        }
                    })),
            );
        }

        // 移入回收站 / 恢复
        actions_bar = actions_bar.child(
            div()
                .id(SharedString::from(format!("detail-toggle-hide-{}", s_key)))
                .px_2()
                .py_1()
                .rounded_md()
                .bg(rgb(PANEL2))
                .border_1()
                .border_color(rgb(BORDER))
                .text_xs()
                .cursor_pointer()
                .hover(|h| h.bg(rgb(0x2a2a35)).border_color(rgb(ACCENT)))
                .child(if is_hidden {
                    i18n::t(i18n::Key::SessionsRestore)
                } else {
                    i18n::t(i18n::Key::SessionsHide)
                })
                .on_click(cx.listener({
                    let s_key = s_key.clone();
                    move |this, _, _, cx| {
                        if is_hidden {
                            this.restore_session(&s_key, cx);
                        } else {
                            this.hide_session(&s_key, cx);
                        }
                    }
                })),
        );

        div()
            .w_full()
            .min_w(px(0.))
            .flex_none()
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgba(ACCENT, 0.4))
            .flex()
            .flex_col()
            .gap_1()
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_2()
                    .child(
                        div()
                            .text_sm()
                            .font_weight(gpui::FontWeight::SEMIBOLD)
                            .child(if s.title.is_empty() {
                                s.id.clone()
                            } else {
                                s.title.clone()
                            }),
                    )
                    .child(div().flex_1())
                    .child(
                        div()
                            .id("close-detail")
                            .px_2()
                            .text_xs()
                            .cursor_pointer()
                            .text_color(rgb(MUTED))
                            .hover(|h| h.text_color(rgb(TEXT)))
                            .child("✕")
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.selected = None;
                                cx.notify();
                            })),
                    ),
            )
            .child(kv(
                i18n::t(i18n::Key::KvSession),
                format!("{} · {} · {}", s.id, s.source, s.agent_mode),
            ))
            .child(kv(
                i18n::t(i18n::Key::KvWorkdir),
                s.working_directory.clone(),
            ))
            .child(kv(i18n::t(i18n::Key::KvModel), model_detail))
            .child(kv(
                i18n::t(i18n::Key::KvTime),
                i18n::tf(
                    i18n::Key::CreatedLastActive,
                    &[&fmt_ts(s.created_at), &fmt_ts(s.last_activity_at)],
                ),
            ))
            .child(kv(
                i18n::t(i18n::Key::ThTokens),
                i18n::tf(
                    i18n::Key::TokenBreakdown,
                    &[
                        &fmt_tokens(t_in),
                        &fmt_tokens(t_out),
                        &fmt_tokens(t_cache_write),
                        &fmt_tokens(t_cached),
                        &fmt_tokens(token_total),
                    ],
                ),
            ))
            .child(kv(
                i18n::t(i18n::Key::ThWindowCost),
                match cost_summary.cost {
                    Some(c) if cost_summary.is_partial() => i18n::tf(
                        i18n::Key::CliPartialCostDetail,
                        &[
                            &pricing::fmt_cost(c),
                            &cost_summary.priced_turns.to_string(),
                            &cost_summary.total_turns.to_string(),
                        ],
                    ),
                    Some(c) => i18n::tf(
                        i18n::Key::PerTurnPriced,
                        &[&pricing::fmt_cost(c), &cost_summary.total_turns.to_string()],
                    ),
                    None => i18n::t(i18n::Key::UnknownUnpriced).into(),
                },
            ))
            .child(kv(
                i18n::t(i18n::Key::KvActivity),
                if ttfts.is_empty() {
                    i18n::tf(
                        i18n::Key::ActivityNoTtft,
                        &[
                            &format!("{:.0}", s.agent_messages),
                            &turns.len().to_string(),
                        ],
                    )
                } else {
                    i18n::tf(
                        i18n::Key::ActivityTtft,
                        &[
                            &format!("{:.0}", s.agent_messages),
                            &turns.len().to_string(),
                            &format!("{:.0}", ttft_med),
                        ],
                    )
                },
            ))
            .child(actions_bar)
    }

    /// 订阅用量视图：每个账号一张卡片，展示各配额窗口的用量进度条与重置倒计时。
    fn quota_view(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let mut body = div()
            .id("quota-scroll")
            .size_full()
            .min_w(px(0.))
            .overflow_scroll()
            .p_4()
            .flex()
            .flex_col()
            .gap_3();

        if let Some(msg) = &self.quota_message {
            body = body.child(div().text_xs().text_color(rgb(0xf87171)).child(msg.clone()));
        }

        if self.show_quota_keychain_banner() {
            body = body.child(self.quota_keychain_banner(cx));
        }

        if self.quota_loading && self.quota_cards.is_empty() {
            return body.child(
                div()
                    .p_4()
                    .flex()
                    .items_center()
                    .gap_3()
                    .child(loading_dots(6., 1.5))
                    .child(
                        div()
                            .text_sm()
                            .text_color(rgb(MUTED))
                            .child(i18n::t(i18n::Key::QuotaLoading)),
                    ),
            );
        }

        if self.quota_cards.is_empty() {
            return body.child(
                div()
                    .p_4()
                    .text_sm()
                    .text_color(rgb(MUTED))
                    .child(i18n::t(i18n::Key::QuotaNoAccounts)),
            );
        }

        let cards: Vec<gpui::Div> = self
            .quota_cards
            .iter()
            .map(|card| self.quota_card(card, cx))
            .collect();
        body.child(div().flex().flex_wrap().gap_3().children(cards))
    }

    /// macOS：解释为何需要钥匙串，并提供显式同意入口。同意前不读 Keychain。
    fn quota_keychain_banner(&self, cx: &mut Context<Self>) -> gpui::Div {
        div()
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .flex()
            .flex_col()
            .gap_2()
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child(i18n::t(i18n::Key::QuotaKeychainNotice)),
            )
            .child(
                div()
                    .flex()
                    .gap_2()
                    .child(
                        div()
                            .id("quota-keychain-allow")
                            .px_3()
                            .py_1()
                            .rounded_md()
                            .text_xs()
                            .font_weight(gpui::FontWeight::SEMIBOLD)
                            .bg(rgb(ACCENT))
                            .text_color(rgb(0x0a0a0c))
                            .cursor_pointer()
                            .hover(|h| h.bg(rgb(0x6ad4ff)))
                            .child(i18n::t(i18n::Key::QuotaKeychainAllow))
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.allow_quota_keychain(cx);
                            })),
                    )
                    .child(
                        div()
                            .id("quota-keychain-dismiss")
                            .px_3()
                            .py_1()
                            .rounded_md()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .cursor_pointer()
                            .hover(|h| h.bg(rgb(PANEL2)))
                            .child(i18n::t(i18n::Key::QuotaKeychainDismiss))
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.dismiss_quota_keychain(cx);
                            })),
                    ),
            )
    }

    fn quota_card(&self, card: &QuotaCard, cx: &mut Context<Self>) -> gpui::Div {
        let removable = card.key.starts_with("devin-") && card.key != "devin-cli";
        let key = card.key.clone();
        let mut header = div()
            .flex()
            .items_center()
            .gap_2()
            .child(
                div()
                    .w(px(7.))
                    .h(px(7.))
                    .rounded_full()
                    .bg(rgb(provider_color(card.provider))),
            )
            .child(
                div()
                    .text_sm()
                    .font_weight(gpui::FontWeight::SEMIBOLD)
                    .text_color(rgb(TEXT))
                    .child(card.provider.label()),
            )
            .child(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .overflow_hidden()
                    .whitespace_nowrap()
                    .text_ellipsis()
                    .child(card.label.clone()),
            );
        if removable {
            header = header.child(
                div()
                    .id(SharedString::from(format!("quota-rm-{}", card.key)))
                    .px_1()
                    .text_xs()
                    .cursor_pointer()
                    .text_color(rgb(MUTED))
                    .hover(|h| h.text_color(rgb(0xf87171)))
                    .child(i18n::t(i18n::Key::QuotaRemove))
                    .on_click(cx.listener(move |this, _, _, cx| {
                        this.remove_quota_account(&key, cx);
                    })),
            );
        }

        let mut d = div()
            .w(px(320.))
            .p_3()
            .rounded_md()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .flex()
            .flex_col()
            .gap_2()
            .child(header);

        match &card.result {
            Err(e) => {
                d = d.child(
                    div()
                        .text_xs()
                        .text_color(rgb(0xf87171))
                        .child(i18n::tf(i18n::Key::QuotaError, &[e])),
                );
            }
            Ok(result) => {
                if let Some(plan) = &result.plan {
                    let plan_color = if plan == "API MODE" { 0x34d399 } else { ACCENT };
                    d = d.child(
                        div().flex().child(
                            div()
                                .px_2()
                                .py(px(1.))
                                .rounded_sm()
                                .bg(rgb(PANEL2))
                                .text_xs()
                                .text_color(rgb(plan_color))
                                .child(plan.clone()),
                        ),
                    );
                }
                for w in &result.windows {
                    let color = pct_color(w.used_percent);
                    let bar_pct = w.used_percent.clamp(0.0, 100.0) as f32 / 100.0;
                    let mut win = div()
                        .flex()
                        .flex_col()
                        .gap_1()
                        .child(
                            div()
                                .flex()
                                .items_center()
                                .gap_2()
                                .child(
                                    div()
                                        .flex_1()
                                        .min_w(px(0.))
                                        .text_xs()
                                        .text_color(rgb(TEXT))
                                        .child(window_label_text(&w.label)),
                                )
                                .child(
                                    div()
                                        .text_xs()
                                        .font_weight(gpui::FontWeight::SEMIBOLD)
                                        .text_color(rgb(color))
                                        .child(format!("{:.0}%", w.used_percent)),
                                ),
                        )
                        .child(
                            div()
                                .w_full()
                                .h(px(6.))
                                .rounded_full()
                                .bg(rgb(PANEL2))
                                .child(
                                    div()
                                        .h_full()
                                        .w(relative(bar_pct))
                                        .rounded_full()
                                        .bg(rgb(color)),
                                ),
                        );
                    if let Some(ts) = w.resets_at {
                        let countdown = quota::fmt_countdown(ts);
                        if !countdown.is_empty() {
                            win = win.child(
                                div()
                                    .text_xs()
                                    .text_color(rgb(MUTED))
                                    .child(i18n::tf(i18n::Key::QuotaResetsIn, &[&countdown])),
                            );
                        }
                    }
                    d = d.child(win);
                }
                if result.windows.is_empty() && result.plan.as_deref() != Some("API MODE") {
                    d = d.child(
                        div()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .child(i18n::t(i18n::Key::NoRecord)),
                    );
                }
                if let Some(extra) = &result.extra {
                    d = d.child(
                        div()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .child(i18n::tf(i18n::Key::QuotaExtra, &[extra])),
                    );
                }
                if let Some(stale) = &result.stale {
                    // 缓存/过期数据：限流冷却中，或本次查询失败后回退到上次结果
                    let age = quota::fmt_duration(stale.age_secs.max(60) as i64);
                    let text = match &stale.reason {
                        quota::StaleReason::RateLimited { retry_in_secs } => i18n::tf(
                            i18n::Key::QuotaStaleRateLimited,
                            &[&age, &quota::fmt_duration((*retry_in_secs).max(60) as i64)],
                        ),
                        quota::StaleReason::Error(err) => {
                            i18n::tf(i18n::Key::QuotaStaleError, &[&age, err])
                        }
                    };
                    d = d.child(div().text_xs().text_color(rgb(0xfbbf24)).child(text));
                }
            }
        }
        d
    }
}

fn provider_color(provider: quota::Provider) -> u32 {
    match provider {
        quota::Provider::Claude => 0xd97757,
        quota::Provider::Codex => 0x3ddc97,
        quota::Provider::Grok => 0xe6e6ee,
        quota::Provider::Devin => 0x4cc2ff,
        quota::Provider::Antigravity => 0x4285f4,
    }
}

/// 进度条阈值配色（按剩余用量）：剩余≥66% 绿、33~66% 黄、<33% 红。
fn pct_color(used_percent: f64) -> u32 {
    let remaining = 100.0 - used_percent;
    if remaining >= 66.0 {
        0x34d399
    } else if remaining >= 33.0 {
        0xfbbf24
    } else {
        0xf87171
    }
}

fn window_label_text(label: &quota::WindowLabel) -> String {
    match label {
        quota::WindowLabel::FiveHour => i18n::t(i18n::Key::QuotaWindowFiveHour).into(),
        quota::WindowLabel::SevenDay => i18n::t(i18n::Key::QuotaWindowSevenDay).into(),
        quota::WindowLabel::SevenDayOauthApps => i18n::t(i18n::Key::QuotaWindowSevenDayApps).into(),
        quota::WindowLabel::SevenDayOpus => i18n::t(i18n::Key::QuotaWindowSevenDayOpus).into(),
        quota::WindowLabel::SevenDaySonnet => i18n::t(i18n::Key::QuotaWindowSevenDaySonnet).into(),
        quota::WindowLabel::Daily => i18n::t(i18n::Key::QuotaWindowDaily).into(),
        quota::WindowLabel::Weekly => i18n::t(i18n::Key::QuotaWindowWeekly).into(),
        quota::WindowLabel::Monthly => i18n::t(i18n::Key::QuotaWindowMonthly).into(),
        quota::WindowLabel::Custom(name) => name.clone(),
    }
}

/// 为本机同步包收集数据。每个 Agent 都通过带新鲜度检查的加载入口读取，
/// 避免长期运行时反复导出启动时的内存快照。
fn collect_local_export(start: i64, end: i64) -> LoadedData {
    let started = Instant::now();
    let local_id = data::device_id();
    let mut combined = LoadedData {
        turns_start: start,
        turns_end: end,
        ..Default::default()
    };
    let agents: Vec<_> = AgentKind::ALL
        .into_iter()
        .filter(|kind| kind.is_installed())
        .collect();
    let agent_count = agents.len();
    let parallelism = SYNC_COLLECTION_CONCURRENCY.min(agents.len().max(1));
    let load_agent = |kind: AgentKind| {
        let mut agent_data = data::load_agent_for_sync(kind, start, end);
        agent_data
            .sessions
            .retain(|s| s.device_id.is_empty() || s.device_id == local_id);
        agent_data
            .turns
            .retain(|t| t.device_id.is_empty() || t.device_id == local_id);
        agent_data
    };
    let loaded: Vec<_> = rayon::ThreadPoolBuilder::new()
        .num_threads(parallelism)
        .build()
        .map(|pool| {
            pool.install(|| {
                agents
                    .par_iter()
                    .copied()
                    .map(|kind| (kind, load_agent(kind)))
                    .collect()
            })
        })
        // 线程池无法创建时仍保证同步可用，只回退为原有串行行为。
        .unwrap_or_else(|_| {
            agents
                .iter()
                .copied()
                .map(|kind| (kind, load_agent(kind)))
                .collect()
        });
    let fingerprints: Option<Vec<_>> = loaded
        .iter()
        .map(|(kind, data)| {
            (!data.sync_content_hash.is_empty()).then_some((*kind, data.sync_content_hash.clone()))
        })
        .collect();
    for (_, mut agent_data) in loaded {
        append_export_source(&mut combined, &mut agent_data);
    }
    if let Some(fingerprints) = fingerprints {
        if let Ok(bytes) =
            serde_json::to_vec(&(local_id, data::device_name(), start, end, fingerprints))
        {
            combined.sync_content_hash = format!("{:x}", Sha256::digest(bytes));
        }
    }
    data::log_event(format!(
        "sync local collection agents={} parallelism={} sessions={} turns={} elapsed_ms={}",
        agent_count,
        parallelism,
        combined.sessions.len(),
        combined.turns.len(),
        started.elapsed().as_millis()
    ));
    combined
}

fn append_export_source(combined: &mut LoadedData, source: &mut LoadedData) {
    combined.sessions.append(&mut source.sessions);
    combined.turns.append(&mut source.turns);
    combined.errors.append(&mut source.errors);
}

fn legend(name: &'static str, color: u32) -> impl IntoElement {
    div()
        .flex()
        .items_center()
        .gap_1()
        .text_xs()
        .text_color(rgb(MUTED))
        .child(div().w(px(8.)).h(px(8.)).rounded_sm().bg(rgb(color)))
        .child(name.to_string())
}

fn cell(text: &str, w: f32) -> gpui::Div {
    div()
        .w(px(w))
        .flex_none()
        .min_w(px(0.))
        .truncate()
        .child(text.to_string())
}

fn cell_r(text: &str, w: f32) -> gpui::Div {
    div()
        .w(px(w))
        .flex_none()
        .text_right()
        .min_w(px(0.))
        .truncate()
        .child(text.to_string())
}

fn truncate(s: &str, n: usize) -> String {
    let mut out = String::new();
    for (i, c) in s.chars().enumerate() {
        if i >= n {
            out.push('…');
            break;
        }
        out.push(c);
    }
    out
}

fn loading_dots(size: f32, gap: f32) -> impl IntoElement {
    let dots = (0..3).map(|index| {
        let offset = index as f32 / 3.0;
        div()
            .w(px(size))
            .h(px(size))
            .rounded_full()
            .bg(rgba(ACCENT, 0.25))
            .with_animation(
                SharedString::from(format!("loading-dot-{size}-{index}")),
                Animation::new(Duration::from_millis(900)).repeat(),
                move |dot, delta| {
                    let phase = ((delta + offset) % 1.0) * std::f32::consts::TAU;
                    let opacity = 0.25 + 0.75 * (phase.sin() * 0.5 + 0.5);
                    dot.bg(rgba(ACCENT, opacity))
                },
            )
    });
    div().flex().items_center().gap(px(gap)).children(dots)
}

impl Render for Root {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        if self.loading && !self.has_loaded {
            return self.loading_view().into_any_element();
        }
        if self.view_mode == ViewMode::AgentHistory
            && self.tab == Tab::Compaction
            && self.agent.supports_compaction()
        {
            self.update_compaction(cx);
        }

        let content = match self.view_mode {
            ViewMode::LiveQuota => self.quota_view(cx).into_any_element(),
            ViewMode::AgentHistory => match self.tab {
                Tab::Usage => self.usage_view(cx).into_any_element(),
                Tab::Sessions => self.sessions_view(window, cx).into_any_element(),
                Tab::Compaction if self.agent.supports_compaction() => {
                    self.compaction_view(cx).into_any_element()
                }
                Tab::Compaction => self.usage_view(cx).into_any_element(),
            },
        };
        let errors: Vec<String> = self
            .data
            .errors
            .iter()
            .filter(|error| error.agent == self.agent)
            .map(|error| error.message.clone())
            .collect();
        let mut right = div()
            .flex_1()
            .min_w(px(0.))
            .overflow_hidden()
            .flex()
            .flex_col()
            .relative()
            .child(self.top_bar(window, cx));
        if self.view_mode == ViewMode::AgentHistory {
            for e in errors {
                right = right.child(
                    div()
                        .px_4()
                        .py_1()
                        .text_xs()
                        .text_color(rgb(0xf87171))
                        .child(i18n::tf(i18n::Key::DataWarning, &[&e])),
                );
            }
        }
        right = right.child(content);

        div()
            .size_full()
            .min_w(px(0.))
            .overflow_hidden()
            .bg(rgb(BG))
            .text_color(rgb(TEXT))
            .flex()
            .child(self.sidebar(cx))
            .child(right)
            .when(self.sync_config_open, |d| {
                d.child(self.sync_config_modal(window, cx))
            })
            .when(self.update.show_dialog, |d| d.child(self.update_dialog(cx)))
            .into_any_element()
    }
}

fn main() {
    // 语言必须在任何文案生成前确定：配置文件 > 系统 locale。
    // 必须在 --cli 分支之前，否则 CLI 会停在 LANG 的静态默认值上。
    i18n::init();

    let args: Vec<String> = std::env::args().skip(1).collect();
    if args.first().is_some_and(|arg| arg == "--cli") {
        if let Err(error) = cli::run(args.into_iter().skip(1).collect()) {
            eprintln!("{}", i18n::tf(i18n::Key::CliFatal, &[&error]));
            std::process::exit(2);
        }
        return;
    }

    struct MainWindowState(pub Option<WindowHandle<Root>>);
    impl gpui::Global for MainWindowState {}

    fn build_root(cx: &mut Context<Root>) -> Root {
        let sync_cfg = sync::read_config();
        let local_id = data::device_id();
        let mut root = Root {
            data: Arc::new(LoadedData::default()),
            loaded_agents: HashMap::new(),
            agent: AgentKind::Devin,
            view_mode: ViewMode::AgentHistory,
            tab: Tab::Usage,
            period: PeriodKind::Day,
            page: 0,
            buckets: Vec::new(),
            expanded_buckets: std::collections::HashSet::new(),
            sessions_rows: Vec::new(),
            compaction_report: None,
            compaction_key: None,
            compaction_task: None,
            compaction_config_task: None,
            compaction_notice: None,
            compaction_details_open: false,
            selected: None,
            loaded_at: i18n::t(i18n::Key::Loading).into(),
            loading: false,
            has_loaded: false,
            load_task: None,
            load_id: 0,
            quota_cards: Vec::new(),
            quota_loading: false,
            quota_load_id: 0,
            quota_task: None,
            quota_tick_task: None,
            quota_updated_at: String::new(),
            quota_message: None,
            quota_keychain_allowed: quota::keychain_allowed(),
            quota_keychain_dismissed: false,
            device_filter: Some(local_id),
            remote_data: Arc::new(LoadedData::default()),
            known_devices: Vec::new(),
            sync_busy: false,
            sync_id: 0,
            sync_last_at: String::new(),
            sync_last_completed: None,
            sync_message: None,
            sync_retry_attempt: 0,
            sync_enabled: sync_cfg.enabled && sync::github_sync_session_is_valid(),
            sync_api_url: sync_cfg.api_url,
            github_login_busy: false,
            github_login_id: 0,
            github_login_code: None,
            github_login_code_copied: false,
            github_login_task: None,
            sync_config_open: false,
            modal_url_input: cx.new(|cx| text_input::new(cx, "", "", false)),
            modal_focus: cx.focus_handle(),
            section_agents_open: true,
            section_devices_open: true,
            section_other_agents_open: false,
            settings_tab: SettingsTab::Sync,
            hide_empty_buckets: false,
            sync_tick_task: None,
            sync_task: None,
            update: updater_ui::UpdateState::default(),
            update_prefs: i18n::load_update_prefs(),
            session_search_input: cx.new(|cx| {
                text_input::new(cx, "", i18n::t(i18n::Key::SessionsSearchPlaceholder), false)
            }),
            session_prefs: i18n::load_session_prefs(),
            sessions_collapsed_projects: std::collections::HashSet::new(),
            sessions_show_hidden: false,
            session_feedback: None,
        };
        root.rebuild_buckets();
        root.rebuild_sessions();
        root.start_load(false, cx);
        root.start_update_scheduler(cx);
        let search_input = root.session_search_input.clone();
        cx.observe(&search_input, |_, _, cx| {
            cx.notify();
        })
        .detach();
        root
    }

    fn open_main_window(cx: &mut App) {
        // 必须先激活应用再创建普通窗口。GPUI 0.2.2 在后台创建窗口时会同步
        // resignKeyWindow，重入 window_did_change_key_status 并死锁。
        tray_controller::TrayController::activate_dashboard(cx);
        if cx.has_global::<MainWindowState>() {
            if let Some(handle) = cx.global::<MainWindowState>().0 {
                let res = handle.update(cx, |_, window, _| {
                    window.activate_window();
                    true
                });
                if res.is_ok() {
                    return;
                }
            }
        }

        let bounds = Bounds::centered(None, size(px(1240.), px(780.)), cx);
        let handle = cx.open_window(
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(bounds)),
                titlebar: Some(TitlebarOptions {
                    title: Some("Agent Usage Metrics".into()),
                    appears_transparent: true,
                    traffic_light_position: Some(point(px(16.), px(16.))),
                }),
                ..Default::default()
            },
            |window, cx| {
                gpui_app_host::desktop::install_close_handler(window, cx, |cx| {
                    if cx.has_global::<MainWindowState>() {
                        cx.global_mut::<MainWindowState>().0 = None;
                    }
                    tray_controller::TrayController::dashboard_closed(cx, |cx| {
                        cx.global::<MainWindowState>().0.is_some()
                    });
                });

                cx.new(build_root)
            },
        );

        if let Ok(handle) = handle {
            if cx.has_global::<MainWindowState>() {
                cx.global_mut::<MainWindowState>().0 = Some(handle);
            } else {
                cx.set_global(MainWindowState(Some(handle)));
            }
        }
    }

    let application = Application::new().with_assets(tray::EmbeddedAssets);
    application.on_reopen(|cx| {
        tray_controller::TrayController::open_dashboard(cx, Arc::new(open_main_window));
    });
    application.run(move |cx: &mut App| {
        // 价格表后台定期检查更新（对标 OpenCode），
        // 新价格在下次数据加载时生效；失败静默保留旧数据。
        pricing::ensure_fresh_async();
        #[cfg(target_os = "macos")]
        gpui_app_host::desktop::set_dock_icon(include_bytes!("../assets/icon.png"), cx);

        gpui_app_host::desktop::bind_actions(
            cx,
            gpui_app_host::desktop::DesktopLabels {
                app: "Agent Usage Metrics",
                hide: i18n::t(i18n::Key::DesktopHide),
                hide_others: i18n::t(i18n::Key::DesktopHideOthers),
                quit: i18n::t(i18n::Key::TrayQuit),
                window: i18n::t(i18n::Key::DesktopWindow),
                close: i18n::t(i18n::Key::DesktopClose),
            },
            |cx| {
                let Some(active) = cx.active_window() else {
                    return;
                };
                if tray_controller::TrayController::close_active(cx, active) {
                    return;
                }
                if cx
                    .try_global::<MainWindowState>()
                    .and_then(|state| state.0)
                    .is_some_and(|handle| gpui::AnyWindowHandle::from(handle) == active)
                {
                    gpui_app_host::desktop::close_window(active, cx, |cx| {
                        cx.global_mut::<MainWindowState>().0 = None;
                        tray_controller::TrayController::dashboard_closed(cx, |cx| {
                            cx.global::<MainWindowState>().0.is_some()
                        });
                    });
                }
            },
        );
        text_input::bind_keys(cx);

        cx.set_global(MainWindowState(None));

        let open_main_cb = Arc::new(|cx: &mut App| {
            open_main_window(cx);
        });

        if let Err(e) = tray_controller::TrayController::init(cx, open_main_cb) {
            eprintln!("Failed to initialize system tray: {e}");
        }

        open_main_window(cx);
    });
}

#[cfg(test)]
mod tests {
    use super::session_token_total;
    use devin_usage_metrics::data::{AgentKind, SessionRec, TurnRec};

    fn turn(input: f64, output: f64, cached: f64, cache_create: f64) -> TurnRec {
        TurnRec {
            agent: AgentKind::Devin,
            session_key: "cli/s1".into(),
            input_tokens: input,
            output_tokens: output,
            cache_read_tokens: cached,
            cache_creation_tokens: cache_create,
            ..Default::default()
        }
    }

    /// 回归：Devin 常常不写 `sessions.metadata.response_dimensions`，此时会话级
    /// 合计必须用窗口内 turn 的实际用量兜底，否则会话列表会把有真实用量的会话
    /// 显示成 0（本机实测 39 个窗口内会话里 22 个是 0，合计 3.17 亿 token）。
    /// 同时确认四类 token 都计入：缓存写入漏计会让会话列表比每日汇总和 CLI 偏低。
    #[test]
    fn session_total_falls_back_to_turns_when_metadata_is_missing() {
        let turns = [turn(10.0, 2.0, 3.0, 4.0), turn(1.0, 1.0, 1.0, 0.5)];
        let refs: Vec<&TurnRec> = turns.iter().collect();

        let empty_meta = SessionRec {
            agent: AgentKind::Devin,
            ..Default::default()
        };
        assert_eq!(session_token_total(&empty_meta, &refs), 22.5);

        // metadata 有值时以它为准：它覆盖整个会话，不随加载窗口变化。
        let with_meta = SessionRec {
            input_tokens: 100.0,
            cache_creation_tokens: 61_686.0,
            ..empty_meta.clone()
        };
        assert_eq!(session_token_total(&with_meta, &refs), 61_786.0);

        // 没有 turn 时保持 0，不编造用量。
        assert_eq!(session_token_total(&empty_meta, &[]), 0.0);
    }
}

#[cfg(test)]
mod sync_collection_tests {
    use super::*;

    #[test]
    fn successful_source_does_not_hide_another_sources_failure() {
        let mut combined = LoadedData::default();
        let mut successful = LoadedData {
            turns: vec![data::TurnRec::default()],
            ..Default::default()
        };
        let mut failed = LoadedData {
            errors: vec![data::DataError {
                agent: AgentKind::Devin,
                message: "database unavailable".into(),
            }],
            ..Default::default()
        };
        append_export_source(&mut combined, &mut successful);
        append_export_source(&mut combined, &mut failed);
        assert_eq!(combined.turns.len(), 1);
        assert_eq!(combined.errors.len(), 1);
        assert_eq!(combined.errors[0].message, "database unavailable");
    }
}
