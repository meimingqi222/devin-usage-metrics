#![allow(unexpected_cfgs)]
// On Windows, hide the console window in release builds. In debug builds we
// keep the console so println!/eprintln! output is still visible while developing.
#![cfg_attr(
    all(target_os = "windows", not(debug_assertions)),
    windows_subsystem = "windows"
)]

mod text_input;
mod updater_ui;

use agg::{build_buckets_for_device, window_for, Bucket, PeriodKind};
use chrono::TimeZone;
use data::{AgentKind, LoadedData};
use devin_usage_metrics::{agg, cli, data, i18n, pricing, quota, sync};
use gpui::{
    actions, div, point, prelude::*, px, relative, rgb, size, Animation, AnimationExt as _, App,
    Application, Bounds, ClipboardItem, Context, Entity, FocusHandle, KeyBinding, KeyDownEvent,
    MouseButton, Render, SharedString, Task, TitlebarOptions, Window, WindowBounds,
    WindowControlArea, WindowOptions,
};
use rayon::prelude::*;
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};
use text_input::TextInput;

actions!(main, [Quit]);

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
const C_COST: u32 = 0xfbbf24;
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

#[derive(Clone, Copy, PartialEq, Debug)]
enum Tab {
    Usage,
    Sessions,
    Quota,
}

/// 左侧边栏可折叠的分区。
#[derive(Clone, Copy, PartialEq, Debug)]
enum SidebarSection {
    Agents,
    Devices,
    Sync,
}

/// 订阅配额卡片：一个账号一条，结果在后台查询完成后整体替换。
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
    total: f64,
}

struct Root {
    data: Arc<LoadedData>,
    loaded_agents: HashMap<AgentKind, Arc<LoadedData>>,
    agent: AgentKind,
    tab: Tab,
    period: PeriodKind,
    page: usize,
    buckets: Vec<Bucket>,
    sessions_rows: Vec<SessionRow>,
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
    section_sync_open: bool,
    /// 自动同步定时任务
    sync_tick_task: Option<Task<()>>,
    /// 正在进行的一次导出+导入（与 load_task 分开，避免点重新加载打断同步）
    sync_task: Option<Task<()>>,
    /// 应用自动更新
    update: updater_ui::UpdateState,
    update_prefs: i18n::UpdatePrefs,
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

impl Root {
    fn switch_agent(&mut self, target_agent: AgentKind, cx: &mut Context<Self>) {
        if self.agent == target_agent && self.has_loaded {
            cx.notify();
            return;
        }

        self.agent = target_agent;
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
        self.buckets = build_buckets_for_device(
            &self.data,
            self.period,
            self.agent,
            self.device_filter.as_deref(),
            self.page,
        );
    }

    fn switch_tab(&mut self, kind: Tab, cx: &mut Context<Self>) {
        self.tab = kind;
        // 首次进入 Quota Tab 时自动拉取一次
        if kind == Tab::Quota && !self.quota_loading && self.quota_updated_at.is_empty() {
            self.start_quota_load(cx);
        }
        cx.notify();
    }

    /// 切换设备筛选。None = 汇总所有设备。
    fn switch_device(&mut self, device_id: Option<String>, cx: &mut Context<Self>) {
        if self.device_filter == device_id {
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
            SidebarSection::Sync => self.section_sync_open = !self.section_sync_open,
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
                    sync::record_transient_failure(error);
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

    /// 后台发现账号并逐个查询配额（含必要的 token 刷新），完成后整体替换卡片。
    fn start_quota_load(&mut self, cx: &mut Context<Self>) {
        self.quota_load_id += 1;
        let load_id = self.quota_load_id;
        self.quota_loading = true;
        self.quota_message = None;
        cx.notify();

        let load = cx.background_executor().spawn(async move {
            let accounts = quota::discover_accounts();
            accounts
                .iter()
                .map(|account| QuotaCard {
                    key: account.key.clone(),
                    provider: account.provider,
                    label: account.label.clone(),
                    result: quota::fetch_account(account),
                })
                .collect::<Vec<_>>()
        });
        self.quota_task = Some(cx.spawn(async move |this, cx| {
            let cards = load.await;
            this.update(cx, |this, cx| {
                if this.quota_load_id != load_id {
                    return;
                }
                this.quota_loading = false;
                this.quota_cards = cards;
                this.quota_updated_at = chrono::Local::now().format("%H:%M:%S").to_string();
                if this.tab == Tab::Quota {
                    cx.notify();
                }
                this.quota_tick(cx);
            })
            .ok();
        }));
    }

    /// 每 30 秒重绘一次，驱动重置倒计时；离开 Quota Tab 或清空后自动停止。
    fn quota_tick(&mut self, cx: &mut Context<Self>) {
        let timer = cx.background_executor().timer(Duration::from_secs(30));
        self.quota_tick_task = Some(cx.spawn(async move |this, cx| {
            timer.await;
            this.update(cx, |this, cx| {
                if this.tab == Tab::Quota && !this.quota_cards.is_empty() {
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
        self.start_quota_load(cx);
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
                let total = s.input_tokens + s.output_tokens + s.cached_tokens;
                // 与每日汇总一致：当前加载窗口内按实际 turn 模型逐轮计价。
                let cost_summary = agg::cost_summary_for_turns(
                    turns_by_session
                        .get(s.key.as_str())
                        .into_iter()
                        .flatten()
                        .copied(),
                    &s.display_model(),
                );
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
                    id_short: truncate(&s.id, 18),
                    title_short: truncate(&s.title, 48),
                    model_text: truncate(&model_text, 30),
                    cost_str,
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
        let mut sorted_agents = AgentKind::ALL.to_vec();
        sorted_agents.sort_by_key(|kind| if kind.is_installed() { 0 } else { 1 });

        let items = sorted_agents.into_iter().map(|kind| {
            let is_selected = kind == self.agent;
            let installed = kind.is_installed();
            let count = self
                .loaded_agents
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
        });

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
        let sync_header = section_header(SidebarSection::Sync, "同步", self.section_sync_open);

        let sync_config_btn = div()
            .id("sync-config-open")
            .px_2()
            .py_1()
            .rounded_md()
            .text_xs()
            .text_color(rgb(MUTED))
            .cursor_pointer()
            .hover(|h| h.bg(rgb(PANEL2)))
            .child(i18n::t(i18n::Key::SyncConfigConfigure))
            .on_click(cx.listener(|this, _, window, cx| {
                this.open_sync_config(window, cx);
            }));

        // 可滚动内容区
        let content = div()
            .id("sidebar-scroll")
            .flex_1()
            .min_w(px(0.))
            .overflow_scroll()
            .flex()
            .flex_col()
            .child(agents_header)
            .when(self.section_agents_open, |d| {
                d.child(div().flex().flex_col().pl(px(12.)).pr_2().children(items))
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
            })
            .child(sync_header)
            .when(self.section_sync_open, |d| {
                d.child(
                    div()
                        .flex()
                        .flex_col()
                        .pl(px(12.))
                        .pr_2()
                        .child(sync_config_btn),
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

        div()
            .id("sidebar")
            .flex_shrink_0()
            .w(px(160.))
            .h_full()
            .flex()
            .flex_col()
            // 顶部留出 macOS 红绿灯按钮的悬浮空间
            .pt(px(40.))
            .bg(rgb(PANEL))
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
            .child(content)
            .child(sync_toggle)
            .child(self.update_version_row(cx))
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

    /// 同步设置弹窗：只保留 API 地址和 GitHub 登录。
    fn sync_config_modal(&self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
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

        let card = div()
            .w(px(360.))
            .p_4()
            .rounded_lg()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .shadow_md()
            .flex()
            .flex_col()
            .gap_3()
            .child(
                div()
                    .text_sm()
                    .font_weight(gpui::FontWeight::BOLD)
                    .text_color(rgb(TEXT))
                    .child(i18n::t(i18n::Key::SyncConfigTitle)),
            )
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
                            .on_click(cx.listener(|this, _, _, cx| this.start_github_login(cx)))
                    })
                    .child(login_label),
            )
            .when(
                self.github_login_busy && self.github_login_code.is_some(),
                |card| {
                    card.child(
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
                    )
                },
            )
            .when(logged_in.is_some(), |d| {
                d.child(
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
                )
            })
            .child(
                div()
                    .text_xs()
                    .text_color(rgb(MUTED))
                    .child("同一 GitHub 账号的设备会合并到同一份同步数据"),
            )
            .child(
                div()
                    .flex()
                    .justify_end()
                    .gap_2()
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
        let tab = self.tab;
        let period = self.period;
        let page = self.page;
        let show_period = tab == Tab::Usage;
        let show_quota = tab == Tab::Quota;
        let loaded_at = self.loaded_at.clone();
        let quota_updated_at = self.quota_updated_at.clone();

        let tab_btn = |kind: Tab, name: &'static str| {
            let active = tab == kind;
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

        // 顶栏语言切换：与周期按钮同款的紧凑两段式
        let lang_btn = |target: i18n::Lang| {
            let active = i18n::lang() == target;
            div()
                .id(SharedString::from(format!("lang-{}", target.short_label())))
                .px_2()
                .py(px(2.))
                .rounded_sm()
                .text_xs()
                .cursor_pointer()
                .when(active, |d| d.bg(rgb(PANEL2)).text_color(rgb(TEXT)))
                .when(!active, |d| {
                    d.text_color(rgb(MUTED)).hover(|h| h.bg(rgb(PANEL2)))
                })
                .child(target.short_label())
                .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
                .on_click(cx.listener(move |this, _, _, cx| {
                    i18n::set_lang(target);
                    this.rebuild_buckets();
                    this.rebuild_sessions();
                    cx.notify();
                }))
        };

        let prev_page = div()
            .id("prev-page")
            .px_2()
            .py(px(2.))
            .rounded_sm()
            .text_xs()
            .when(page > 0, |d| {
                d.cursor_pointer()
                    .text_color(rgb(MUTED))
                    .hover(|h| h.bg(rgb(PANEL2)))
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
            .child(i18n::t(i18n::Key::PrevPage));

        let next_page = div()
            .id("next-page")
            .px_2()
            .py(px(2.))
            .rounded_sm()
            .text_xs()
            .cursor_pointer()
            .text_color(rgb(MUTED))
            .hover(|h| h.bg(rgb(PANEL2)))
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .child(i18n::t(i18n::Key::NextPage))
            .on_click(cx.listener(|this, _, _, cx| {
                this.page = this.page.saturating_add(1);
                if this.page_needs_load() {
                    this.start_load(true, cx);
                } else {
                    this.rebuild_buckets();
                    cx.notify();
                }
            }));

        div()
            .flex()
            .w_full()
            .min_w(px(0.))
            .items_center()
            .gap_3()
            .px_4()
            .h(px(48.))
            // 一体化标题栏：顶栏区域负责窗口拖拽，内部按钮点击不受影响
            .window_control_area(WindowControlArea::Drag)
            .child(tab_btn(Tab::Usage, i18n::t(i18n::Key::Usage)))
            .child(tab_btn(Tab::Sessions, i18n::t(i18n::Key::Sessions)))
            .child(tab_btn(Tab::Quota, i18n::t(i18n::Key::QuotaTab)))
            .when(show_period, |d| {
                d.child(
                    div()
                        .flex()
                        .gap_1()
                        .pl_3()
                        .border_l_1()
                        .border_color(rgb(BORDER))
                        .child(period_btn(PeriodKind::Day))
                        .child(period_btn(PeriodKind::Week))
                        .child(period_btn(PeriodKind::Month)),
                )
                .child(
                    div()
                        .flex()
                        .gap_1()
                        .pl_3()
                        .border_l_1()
                        .border_color(rgb(BORDER))
                        .child(prev_page)
                        .child(next_page),
                )
            })
            .when(show_quota, |d| {
                d.child(
                    div()
                        .flex()
                        .gap_1()
                        .pl_3()
                        .border_l_1()
                        .border_color(rgb(BORDER))
                        .child(
                            div()
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
                                        this.start_quota_load(cx);
                                    }
                                })),
                        ),
                )
            })
            .child(div().flex_1())
            .child(
                div()
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
                    })),
            )
            // 多设备同步开关已移到侧边栏底部
            .when(!show_quota, |d| {
                d.child(
                    div()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child(i18n::tf(i18n::Key::DataAsOf, &[&loaded_at])),
                )
            })
            .when(show_quota && !quota_updated_at.is_empty(), |d| {
                d.child(
                    div()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .child(i18n::tf(i18n::Key::QuotaUpdated, &[&quota_updated_at])),
                )
            })
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap_1()
                    .pl_3()
                    .border_l_1()
                    .border_color(rgb(BORDER))
                    .child(lang_btn(i18n::Lang::Zh))
                    .child(lang_btn(i18n::Lang::En)),
            )
            // Windows 没有系统标题栏按钮（窗口样式不含 WS_CAPTION），
            // 在顶栏右端自绘 最小化/最大化/关闭
            .when(cfg!(target_os = "windows"), |bar| {
                bar.child(self.windows_caption_buttons(window))
            })
    }

    /// Windows 专用的窗口控制按钮。不要挂 on_click：这些区域会被系统
    /// 命中为 HTMINBUTTON/HTMAXBUTTON/HTCLOSE，松开鼠标时由 GPUI 执行
    /// 最小化/最大化/WM_CLOSE（关闭已通过 on_window_should_close 退出）。
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
            .min_w(px(140.))
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
        let (mut ti, mut to, mut tc, mut turns, mut cost) = (0.0, 0.0, 0.0, 0u32, 0.0);
        let mut sessions = std::collections::BTreeSet::new();
        for b in buckets {
            ti += b.input;
            to += b.output;
            tc += b.cached;
            turns += b.turns;
            cost += b.cost;
            sessions.extend(b.session_keys.iter().cloned());
        }
        let total = ti + to + tc;

        let cards = div()
            .flex()
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
                i18n::t(i18n::Key::StatCached),
                fmt_tokens(tc),
                C_CACHED,
            ))
            .child(Self::stat_card(
                i18n::t(i18n::Key::StatCost),
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

    fn bucket_table(&self, _cx: &mut Context<Self>) -> impl IntoElement {
        let header = div()
            .flex()
            .w_full()
            .min_w(px(0.))
            .gap_2()
            .px_2()
            .py_1()
            .text_xs()
            .text_color(rgb(MUTED))
            .border_b_1()
            .border_color(rgb(BORDER))
            .child(cell(i18n::t(i18n::Key::ThPeriod), 90.))
            .child(cell_r(i18n::t(i18n::Key::ThSessions), 60.))
            .child(cell_r(i18n::t(i18n::Key::ThTurns), 52.))
            .child(cell_r(i18n::t(i18n::Key::ThInput), 70.))
            .child(cell_r(i18n::t(i18n::Key::ThOutput), 70.))
            .child(cell_r(i18n::t(i18n::Key::ThCache), 76.))
            .child(cell_r(i18n::t(i18n::Key::ThTotal), 76.))
            .child(cell_r(i18n::t(i18n::Key::ThCost), 70.))
            .child(
                div()
                    .flex_1()
                    .min_w(px(0.))
                    .overflow_hidden()
                    .whitespace_nowrap()
                    .text_ellipsis()
                    .child(i18n::t(i18n::Key::ThModelMix)),
            );
        let mut rows: Vec<gpui::Div> = Vec::new();
        for b in self.buckets.iter().rev() {
            let mut models: Vec<(String, f64)> = b
                .by_model
                .iter()
                .map(|(m, u)| (m.clone(), u.total()))
                .collect();
            models.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));
            let model_bits: Vec<gpui::Div> = models
                .iter()
                .filter(|(_, t)| *t > 0.0)
                .map(|(m, t)| {
                    div()
                        .flex()
                        .items_center()
                        .gap_1()
                        .mr_2()
                        .child(
                            div()
                                .w(px(7.))
                                .h(px(7.))
                                .rounded_full()
                                .bg(rgb(model_color(m))),
                        )
                        .child(format!("{} {}", m, fmt_tokens(*t)))
                })
                .collect();
            let empty = b.total() <= 0.0;
            rows.push(
                div()
                    .flex()
                    .w_full()
                    .min_w(px(0.))
                    .gap_2()
                    .px_2()
                    .py_1()
                    .text_xs()
                    .text_color(if empty {
                        rgba(MUTED, 0.5)
                    } else {
                        rgba(TEXT, 1.0)
                    })
                    .border_b_1()
                    .border_color(rgba(BORDER, 0.5))
                    .child(div().w(px(90.)).child(format!("{} {}", b.label, b.sub)))
                    .child(cell_r(&format!("{}", b.session_keys.len()), 60.))
                    .child(cell_r(&format!("{}", b.turns), 52.))
                    .child(cell_r(&fmt_tokens(b.input), 70.))
                    .child(cell_r(&fmt_tokens(b.output), 70.))
                    .child(cell_r(&fmt_tokens(b.cached), 76.))
                    .child(cell_r(&fmt_tokens(b.total()), 76.))
                    .child(cell_r(&pricing::fmt_cost(b.cost), 70.))
                    .child(
                        div()
                            .flex_1()
                            .min_w(px(0.))
                            .overflow_hidden()
                            .flex()
                            .flex_wrap()
                            .children(model_bits),
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
            .child(header)
            .children(rows)
    }

    fn sessions_view(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let detail = self
            .selected
            .as_ref()
            .map(|key| self.session_detail(key, cx));
        let header = div()
            .flex()
            .gap_2()
            .px_2()
            .py_1()
            .text_xs()
            .text_color(rgb(MUTED))
            .border_b_1()
            .border_color(rgb(BORDER))
            .child(cell(i18n::t(i18n::Key::ThLastActive), 84.))
            .child(cell(i18n::t(i18n::Key::ThSession), 130.))
            .child(div().flex_1().child(i18n::t(i18n::Key::ThTitle)))
            .child(cell(i18n::t(i18n::Key::ThMode), 70.))
            .child(cell(i18n::t(i18n::Key::ThModel), 190.))
            .child(cell_r(i18n::t(i18n::Key::ThMsgs), 48.))
            .child(cell_r(i18n::t(i18n::Key::ThTokens), 72.))
            .child(cell_r(i18n::t(i18n::Key::ThWindowCost), 64.));
        let mut rows: Vec<gpui::AnyElement> = Vec::new();
        for r in &self.sessions_rows {
            let s = &r.session;
            let key = s.key.clone();
            let is_sel = self.selected.as_deref() == Some(key.as_str());
            let adaptive = s.selected_model == "adaptive";
            let mut row = div()
                .id(SharedString::from(format!("srow-{}", key)))
                .flex()
                .gap_2()
                .px_2()
                .py_1()
                .text_xs()
                .cursor_pointer()
                .items_center()
                .hover(|h| h.bg(rgb(PANEL2)))
                .when(is_sel, |d| d.bg(rgb(PANEL2)))
                .border_b_1()
                .border_color(rgba(BORDER, 0.4))
                .child(cell(&r.time_str, 84.))
                .child(
                    div()
                        .w(px(130.))
                        .flex()
                        .gap_1()
                        .items_center()
                        .child(
                            div()
                                .w(px(6.))
                                .h(px(6.))
                                .rounded_full()
                                .bg(rgb(model_color(&s.display_model()))),
                        )
                        .child(r.id_short.clone()),
                )
                .child(div().flex_1().child(r.title_short.clone()))
                .child(cell(&s.agent_mode, 70.))
                .child(
                    div()
                        .w(px(190.))
                        .text_color(if adaptive { rgb(ACCENT) } else { rgb(TEXT) })
                        .child(r.model_text.clone()),
                )
                .child(cell_r(&format!("{:.0}", s.agent_messages), 48.))
                .child(cell_r(&fmt_tokens(r.total), 72.))
                .child(cell_r(&r.cost_str, 64.))
                .on_click(cx.listener(move |this, _, _, cx| {
                    this.selected = Some(key.clone());
                    cx.notify();
                }));
            if self.agent == AgentKind::Devin && s.source == "cli-next" {
                row = row.child(div().text_xs().text_color(rgba(MUTED, 0.7)).child("next"));
            }
            rows.push(row.into_any_element());
        }
        let mut body = div()
            .id("sessions-scroll")
            .size_full()
            .min_w(px(0.))
            .overflow_scroll()
            .p_4()
            .flex()
            .flex_col()
            .gap_3();
        if let Some(d) = detail {
            body = body.child(d);
        }
        let empty = rows.is_empty();
        body.child(
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
                .child(header)
                .when(empty, |panel| {
                    panel.child(
                        div()
                            .p_4()
                            .text_sm()
                            .text_color(rgb(MUTED))
                            .child(i18n::tf(i18n::Key::SessionsNotFound, &[self.agent.label()])),
                    )
                })
                .children(rows),
        )
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
        let total = s.input_tokens + s.output_tokens + s.cached_tokens;
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

        div()
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
                        &fmt_tokens(s.input_tokens),
                        &fmt_tokens(s.output_tokens),
                        &fmt_tokens(s.cached_tokens),
                        &fmt_tokens(total),
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
        combined.sessions.append(&mut agent_data.sessions);
        combined.turns.append(&mut agent_data.turns);
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
    div().w(px(w)).child(text.to_string())
}

fn cell_r(text: &str, w: f32) -> gpui::Div {
    div().w(px(w)).text_right().child(text.to_string())
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

        let content = match self.tab {
            Tab::Usage => self.usage_view(cx).into_any_element(),
            Tab::Sessions => self.sessions_view(cx).into_any_element(),
            Tab::Quota => self.quota_view(cx).into_any_element(),
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

#[cfg(target_os = "macos")]
fn set_macos_dock_icon() {
    use objc::{class, msg_send, sel, sel_impl};
    const ICON_PNG: &[u8] = include_bytes!("../assets/icon.png");

    unsafe {
        let app: *mut objc::runtime::Object = msg_send![class!(NSApplication), sharedApplication];
        if app.is_null() {
            return;
        }
        let data: *mut objc::runtime::Object = msg_send![
            class!(NSData),
            dataWithBytes: ICON_PNG.as_ptr() as *const std::ffi::c_void
            length: ICON_PNG.len()
        ];
        if data.is_null() {
            return;
        }
        let image: *mut objc::runtime::Object = msg_send![class!(NSImage), alloc];
        let image: *mut objc::runtime::Object = msg_send![image, initWithData: data];
        if !image.is_null() {
            let _: () = msg_send![app, setApplicationIconImage: image];
            let _: () = msg_send![image, release];
        }
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

    Application::new().run(move |cx: &mut App| {
        // 价格表后台自动更新（对标 OpenCode）：启动后在后台检查一次，
        // 新价格在下次数据加载时生效；失败静默保留旧数据。
        pricing::ensure_fresh_async();
        #[cfg(target_os = "macos")]
        set_macos_dock_icon();

        // Bind platform-specific quit shortcut
        #[cfg(target_os = "macos")]
        cx.bind_keys([KeyBinding::new("cmd-q", Quit, None)]);
        #[cfg(not(target_os = "macos"))]
        cx.bind_keys([KeyBinding::new("ctrl-q", Quit, None)]);
        cx.on_action(|_: &Quit, cx| cx.quit());
        text_input::bind_keys(cx);

        let bounds = Bounds::centered(None, size(px(1180.), px(760.)), cx);
        cx.open_window(
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(bounds)),
                // 隐藏系统标题栏，内容延伸到窗口顶部（红绿灯悬浮在侧边栏上），
                // 实现 macOS 一体化工具栏外观
                titlebar: Some(TitlebarOptions {
                    title: Some("Agent Usage Metrics".into()),
                    appears_transparent: true,
                    traffic_light_position: Some(point(px(16.), px(16.))),
                }),
                ..Default::default()
            },
            |window, cx| {
                // Quit app when window is closed (clicking the red X button)
                window.on_window_should_close(cx, |_, cx| {
                    cx.quit();
                    true // allow the window to close
                });

                cx.new(|cx| {
                    // 读取同步配置：决定启动时是否导入远程数据、是否启动定时任务
                    let sync_cfg = sync::read_config();
                    let local_id = data::device_id();
                    let mut root = Root {
                        data: Arc::new(LoadedData::default()),
                        loaded_agents: HashMap::new(),
                        agent: AgentKind::Devin,
                        tab: Tab::Usage,
                        period: PeriodKind::Day,
                        page: 0,
                        buckets: Vec::new(),
                        sessions_rows: Vec::new(),
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
                        modal_url_input: cx.new(|cx| TextInput::new(cx, "", "", false)),
                        modal_focus: cx.focus_handle(),
                        section_agents_open: true,
                        section_devices_open: true,
                        section_sync_open: true,
                        sync_tick_task: None,
                        sync_task: None,
                        update: updater_ui::UpdateState::default(),
                        update_prefs: i18n::load_update_prefs(),
                    };
                    root.rebuild_buckets();
                    root.rebuild_sessions();
                    root.start_load(false, cx);
                    root.start_update_scheduler(cx);
                    root
                })
            },
        )
        .unwrap();

        cx.activate(true);
    });
}
