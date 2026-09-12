//! 自动更新动作与对话框：检查、下载、校验、安装交接。
//!
//! 更新源为 GitHub Releases；开发态（cargo target）与用户关闭自动检查时
//! 不发起任何网络请求。发现新版本不会静默重启，必须由用户点安装。

use super::*;
use devin_usage_metrics::updater::{
    self, candidate_asset_names, current_target, download_to_file, evaluate_release,
    extract_update_zip, fetch_latest_release, fetch_text, is_skipped, parse_checksum_sidecar,
    sha256_file, DownloadProgress, UpdateStatus, CHECK_INTERVAL_MS, FIRST_CHECK_DELAY_MS,
};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};

/// 自动更新的界面状态。
pub struct UpdateState {
    pub status: UpdateStatus,
    pub show_dialog: bool,
    pub task: Option<Task<()>>,
    pub checking: bool,
    pub live_progress: Option<Arc<Mutex<DownloadProgress>>>,
}

impl Default for UpdateState {
    fn default() -> Self {
        let current_version = env!("CARGO_PKG_VERSION").to_string();
        Self {
            status: UpdateStatus::Idle { current_version },
            show_dialog: false,
            task: None,
            checking: false,
            live_progress: None,
        }
    }
}

impl Root {
    /// 启动延迟检查 + 周期重查。幂等。
    pub fn start_update_scheduler(&mut self, cx: &mut Context<Self>) {
        if updater::is_packaged_install() {
            updater::cleanup_previous_update_leftovers();
        }
        if self.update.task.is_some() || !updater::is_packaged_install() {
            return;
        }
        if !self.update_prefs.auto_check_updates {
            return;
        }
        self.update.task = Some(cx.spawn(async move |this, cx| {
            cx.background_executor()
                .timer(Duration::from_millis(FIRST_CHECK_DELAY_MS))
                .await;
            loop {
                let keep = this
                    .update(cx, |this, cx| {
                        this.check_for_updates_internal(false, cx);
                        this.update_prefs.auto_check_updates
                    })
                    .unwrap_or(false);
                if !keep {
                    return;
                }
                cx.background_executor()
                    .timer(Duration::from_millis(CHECK_INTERVAL_MS))
                    .await;
            }
        }));
    }

    pub fn check_for_updates_manual(&mut self, cx: &mut Context<Self>) {
        self.check_for_updates_internal(true, cx);
    }

    fn check_for_updates_internal(&mut self, manual: bool, cx: &mut Context<Self>) {
        if !updater::is_packaged_install() {
            return;
        }
        if !manual && !self.update_prefs.auto_check_updates {
            return;
        }
        if self.update.checking {
            return;
        }
        if matches!(
            self.update.status,
            UpdateStatus::Downloading { .. }
                | UpdateStatus::Verifying { .. }
                | UpdateStatus::Downloaded { .. }
                | UpdateStatus::Installing { .. }
        ) {
            return;
        }

        let current_version = env!("CARGO_PKG_VERSION").to_string();
        let skipped = self.update_prefs.skipped_update_version.clone();
        self.update.checking = true;
        self.update.status = UpdateStatus::Checking {
            current_version: current_version.clone(),
        };
        cx.notify();

        let work = cx.background_executor().spawn(async move {
            let release = fetch_latest_release(15_000)?;
            let candidates = candidate_asset_names(current_target());
            let status = evaluate_release(&current_version, &release, candidates)?;
            Ok::<UpdateStatus, String>(status)
        });

        cx.spawn(async move |this, cx| {
            let result = work.await;
            let _ = this.update(cx, |this, cx| {
                this.update.checking = false;
                this.update_prefs.last_update_check_at = Some(chrono::Utc::now().timestamp());
                i18n::save_update_prefs(&this.update_prefs);
                match result {
                    Ok(status) => {
                        if let UpdateStatus::Available { latest_version, .. } = &status {
                            if is_skipped(latest_version, skipped.as_deref()) {
                                this.update.status = UpdateStatus::NotAvailable {
                                    current_version: env!("CARGO_PKG_VERSION").to_string(),
                                };
                            } else {
                                this.update.status = status;
                                this.start_update_download(cx);
                            }
                        } else {
                            this.update.status = status;
                        }
                    }
                    Err(message) => {
                        this.update.status = UpdateStatus::Error {
                            current_version: env!("CARGO_PKG_VERSION").to_string(),
                            operation: updater::UpdateOperation::Check,
                            message,
                        };
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    pub fn open_update_dialog(&mut self, cx: &mut Context<Self>) {
        if !self.update.status.wants_attention() {
            return;
        }
        self.update.show_dialog = true;
        cx.notify();
    }

    pub fn dismiss_update_dialog(&mut self, cx: &mut Context<Self>) {
        self.update.show_dialog = false;
        cx.notify();
    }

    pub fn skip_this_update_version(&mut self, cx: &mut Context<Self>) {
        if let Some(v) = self.update.status.latest_version().map(|s| s.to_string()) {
            self.update_prefs.skipped_update_version = Some(v);
            i18n::save_update_prefs(&self.update_prefs);
        }
        self.update.show_dialog = false;
        if matches!(self.update.status, UpdateStatus::Available { .. }) {
            self.update.status = UpdateStatus::NotAvailable {
                current_version: env!("CARGO_PKG_VERSION").to_string(),
            };
        }
        cx.notify();
    }

    pub fn start_update_download(&mut self, cx: &mut Context<Self>) {
        let UpdateStatus::Available {
            current_version,
            latest_version,
            asset_name,
            asset_url,
            checksum_url,
            release_url,
            notes,
            ..
        } = self.update.status.clone()
        else {
            return;
        };
        let cache = match updater::update_cache_dir() {
            Some(c) => c,
            None => {
                self.update.status = UpdateStatus::Error {
                    current_version,
                    operation: updater::UpdateOperation::Download,
                    message: "update cache dir unavailable".into(),
                };
                cx.notify();
                return;
            }
        };

        self.update.status = UpdateStatus::Downloading {
            current_version: current_version.clone(),
            latest_version: latest_version.clone(),
            progress: Default::default(),
            asset_name: asset_name.clone(),
            asset_url: asset_url.clone(),
            checksum_url: checksum_url.clone(),
            release_url: release_url.clone(),
            notes: notes.clone(),
        };
        let progress = Arc::new(Mutex::new(DownloadProgress::default()));
        self.update.live_progress = Some(progress.clone());
        cx.notify();

        cx.spawn(async move |this, cx| loop {
            let downloading = this
                .update(cx, |this, _| {
                    matches!(this.update.status, UpdateStatus::Downloading { .. })
                })
                .unwrap_or(false);
            if !downloading {
                return;
            }
            let _ = this.update(cx, |_, cx| cx.notify());
            cx.background_executor()
                .timer(Duration::from_millis(250))
                .await;
        })
        .detach();

        let work = cx.background_executor().spawn(async move {
            let zip_path = cache.join(&asset_name);
            let sidecar_path = cache.join(format!("{asset_name}.sha256"));
            {
                let progress = progress.clone();
                download_to_file(&asset_url, &zip_path, &mut |transferred, total| {
                    if let Ok(mut p) = progress.lock() {
                        p.transferred = transferred;
                        p.total = total;
                        p.percent = match total {
                            Some(t) if t > 0 => {
                                (transferred as f32 / t as f32 * 100.0).clamp(0.0, 100.0)
                            }
                            _ => 0.0,
                        };
                    }
                })?;
            }
            let sidecar = fetch_text(&checksum_url, 20_000)?;
            std::fs::write(&sidecar_path, &sidecar).map_err(|e| e.to_string())?;
            let expected = parse_checksum_sidecar(&sidecar)
                .ok_or_else(|| "checksum sidecar is malformed".to_string())?;
            let actual = sha256_file(&zip_path).map_err(|e| e.to_string())?;
            if actual != expected {
                return Err(format!(
                    "checksum mismatch: expected {expected}, got {actual}"
                ));
            }
            let extracted = cache.join("extracted");
            let payload = extract_update_zip(&zip_path, &extracted)?;
            Ok::<PathBuf, String>(payload)
        });

        cx.spawn(async move |this, cx| {
            let result = work.await;
            let _ = this.update(cx, |this, cx| {
                this.update.live_progress = None;
                match result {
                    Ok(payload) => {
                        this.update.status = UpdateStatus::Downloaded {
                            current_version: env!("CARGO_PKG_VERSION").to_string(),
                            latest_version,
                            payload,
                        };
                    }
                    Err(message) => {
                        this.update.status = UpdateStatus::Error {
                            current_version: env!("CARGO_PKG_VERSION").to_string(),
                            operation: updater::UpdateOperation::Download,
                            message,
                        };
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    pub fn install_update_now(&mut self, cx: &mut Context<Self>) {
        let UpdateStatus::Downloaded {
            payload,
            latest_version,
            ..
        } = self.update.status.clone()
        else {
            return;
        };
        self.update.status = UpdateStatus::Installing {
            current_version: env!("CARGO_PKG_VERSION").to_string(),
            latest_version,
        };
        cx.notify();
        let work = cx
            .background_executor()
            .spawn(async move { updater::apply_update_and_restart(&payload) });
        cx.spawn(async move |this, cx| {
            let result = work.await;
            let _ = this.update(cx, |this, cx| match result {
                Ok(()) => {
                    cx.quit();
                }
                Err(message) => {
                    this.update.status = UpdateStatus::Error {
                        current_version: env!("CARGO_PKG_VERSION").to_string(),
                        operation: updater::UpdateOperation::Install,
                        message,
                    };
                    cx.notify();
                }
            });
        })
        .detach();
    }

    pub fn retry_update(&mut self, cx: &mut Context<Self>) {
        let op = match &self.update.status {
            UpdateStatus::Error { operation, .. } => *operation,
            _ => return,
        };
        match op {
            updater::UpdateOperation::Check
            | updater::UpdateOperation::Download
            | updater::UpdateOperation::Install => {
                self.update.status = UpdateStatus::Idle {
                    current_version: env!("CARGO_PKG_VERSION").to_string(),
                };
                self.check_for_updates_manual(cx);
            }
        }
    }

    pub fn open_update_release_page(&self, url: String) {
        updater::open_url(&url);
    }

    pub(super) fn update_version_row(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let version = env!("CARGO_PKG_VERSION");
        let status = &self.update.status;
        let attention = status.wants_attention();
        let checking = self.update.checking || matches!(status, UpdateStatus::Checking { .. });

        let label = match status {
            UpdateStatus::Available { latest_version, .. } => {
                format!(
                    "{} {latest_version}",
                    i18n::t(i18n::Key::UpdateAvailableShort)
                )
            }
            UpdateStatus::Downloading { .. } => i18n::t(i18n::Key::UpdateDownloading).to_string(),
            UpdateStatus::Verifying { .. } => i18n::t(i18n::Key::UpdateVerifying).to_string(),
            UpdateStatus::Downloaded { latest_version, .. } => {
                format!(
                    "{} {latest_version}",
                    i18n::t(i18n::Key::UpdateRestartInstall)
                )
            }
            UpdateStatus::Installing { .. } => i18n::t(i18n::Key::UpdateInstalling).to_string(),
            UpdateStatus::Error { .. } => i18n::t(i18n::Key::UpdateFailed).to_string(),
            _ if checking => i18n::t(i18n::Key::UpdateChecking).to_string(),
            _ => format!("v{version}"),
        };

        let mut row = div()
            .id("sidebar-update-row")
            .mx_2()
            .mb_2()
            .flex()
            .items_center()
            .justify_between()
            .px_3()
            .py_2()
            .rounded_md()
            .child(div().text_xs().child(SharedString::from(label)));

        if attention {
            row = row
                .bg(rgba(ACCENT, 0.18))
                .text_color(rgb(ACCENT))
                .cursor_pointer()
                .child(div().text_xs().child("›"))
                .on_click(cx.listener(|this, _, _, cx| {
                    if matches!(this.update.status, UpdateStatus::Downloaded { .. }) {
                        this.install_update_now(cx);
                        return;
                    }
                    this.open_update_dialog(cx);
                }));
        } else {
            row = row
                .text_color(rgb(MUTED))
                .when(!checking, |d| {
                    d.hover(|h| h.bg(rgb(PANEL2))).cursor_pointer()
                })
                .on_click(cx.listener(|this, _, _, cx| {
                    if this.update.checking || !updater::is_packaged_install() {
                        return;
                    }
                    this.check_for_updates_manual(cx);
                }));
        }

        row
    }

    pub(super) fn update_dialog(&self, cx: &mut Context<Self>) -> impl IntoElement {
        let status = self.update.status.clone();

        let (title, body, primary_label, primary_enabled) = match &status {
            UpdateStatus::Available {
                latest_version,
                notes,
                release_url,
                ..
            } => {
                let note = if notes.trim().is_empty() {
                    release_url.clone()
                } else {
                    truncate(notes.trim(), 240)
                };
                (
                    format!("v{latest_version}"),
                    note,
                    i18n::t(i18n::Key::UpdateDownload).to_string(),
                    true,
                )
            }
            UpdateStatus::Downloading { progress, .. } => {
                let live = self
                    .update
                    .live_progress
                    .as_ref()
                    .and_then(|p| p.lock().ok().map(|p| p.percent));
                let percent = live.unwrap_or(progress.percent);
                (
                    i18n::t(i18n::Key::UpdateDownloading).to_string(),
                    format!("{percent:.0}%"),
                    i18n::t(i18n::Key::UpdateDownloading).to_string(),
                    false,
                )
            }
            UpdateStatus::Verifying { .. } => (
                i18n::t(i18n::Key::UpdateVerifying).to_string(),
                String::new(),
                i18n::t(i18n::Key::UpdateVerifying).to_string(),
                false,
            ),
            UpdateStatus::Downloaded { latest_version, .. } => (
                format!("v{latest_version}"),
                i18n::t(i18n::Key::UpdateReadyHint).to_string(),
                i18n::t(i18n::Key::UpdateRestartInstall).to_string(),
                true,
            ),
            UpdateStatus::Installing { .. } => (
                i18n::t(i18n::Key::UpdateInstalling).to_string(),
                String::new(),
                i18n::t(i18n::Key::UpdateInstalling).to_string(),
                false,
            ),
            UpdateStatus::Error { message, .. } => (
                i18n::t(i18n::Key::UpdateFailed).to_string(),
                message.clone(),
                i18n::t(i18n::Key::UpdateRetry).to_string(),
                true,
            ),
            _ => (
                i18n::t(i18n::Key::UpdateChecking).to_string(),
                String::new(),
                i18n::t(i18n::Key::UpdateLater).to_string(),
                false,
            ),
        };

        let show_skip = matches!(status, UpdateStatus::Available { .. });
        let release_url = match &status {
            UpdateStatus::Available { release_url, .. }
            | UpdateStatus::Downloading { release_url, .. } => Some(release_url.clone()),
            _ => None,
        };

        let card = div()
            .w(px(420.))
            .p_4()
            .rounded_lg()
            .bg(rgb(PANEL))
            .border_1()
            .border_color(rgb(BORDER))
            .flex()
            .flex_col()
            .gap_4()
            .child(
                div()
                    .flex()
                    .flex_col()
                    .child(
                        div()
                            .text_sm()
                            .font_weight(gpui::FontWeight::BOLD)
                            .text_color(rgb(TEXT))
                            .child(i18n::t(i18n::Key::UpdateDialogTitle)),
                    )
                    .child(div().text_xs().text_color(rgb(MUTED)).child(title)),
            )
            .when(!body.is_empty(), |d| {
                d.child(
                    div()
                        .text_xs()
                        .text_color(rgb(MUTED))
                        .max_h(px(120.))
                        .overflow_hidden()
                        .child(body),
                )
            })
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_between()
                    .gap_2()
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap_2()
                            .when(show_skip, |d| {
                                d.child(
                                    div()
                                        .id("update-skip")
                                        .px_2()
                                        .py_1()
                                        .rounded_sm()
                                        .text_xs()
                                        .text_color(rgb(MUTED))
                                        .cursor_pointer()
                                        .hover(|h| h.bg(rgb(PANEL2)))
                                        .child(i18n::t(i18n::Key::UpdateSkipVersion))
                                        .on_click(cx.listener(|this, _, _, cx| {
                                            this.skip_this_update_version(cx);
                                        })),
                                )
                            })
                            .when(release_url.is_some(), |d| {
                                d.child(
                                    div()
                                        .id("update-open-release")
                                        .px_2()
                                        .py_1()
                                        .rounded_sm()
                                        .text_xs()
                                        .text_color(rgb(MUTED))
                                        .cursor_pointer()
                                        .hover(|h| h.bg(rgb(PANEL2)))
                                        .child(i18n::t(i18n::Key::UpdateOpenRelease))
                                        .on_click(cx.listener(|this, _, _, _cx| {
                                            if let Some(url) = match &this.update.status {
                                                UpdateStatus::Available { release_url, .. }
                                                | UpdateStatus::Downloading {
                                                    release_url, ..
                                                } => Some(release_url.clone()),
                                                _ => None,
                                            } {
                                                this.open_update_release_page(url);
                                            }
                                        })),
                                )
                            }),
                    )
                    .child(
                        div()
                            .id("update-later")
                            .px_2()
                            .py_1()
                            .rounded_sm()
                            .text_xs()
                            .text_color(rgb(MUTED))
                            .cursor_pointer()
                            .hover(|h| h.bg(rgb(PANEL2)))
                            .child(i18n::t(i18n::Key::UpdateLater))
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.dismiss_update_dialog(cx);
                            })),
                    ),
            )
            .child(
                div()
                    .id("update-primary")
                    .px_4()
                    .py_1()
                    .rounded_sm()
                    .text_xs()
                    .when(primary_enabled, |d| {
                        d.bg(rgb(ACCENT))
                            .text_color(rgb(0x0a0a0c))
                            .cursor_pointer()
                            .hover(|h| h.bg(rgb(0x6ad4ff)))
                    })
                    .when(!primary_enabled, |d| {
                        d.bg(rgb(PANEL2)).text_color(rgb(MUTED))
                    })
                    .child(primary_label)
                    .on_click(
                        cx.listener(move |this, _, _, cx| match &this.update.status {
                            UpdateStatus::Available { .. } => this.start_update_download(cx),
                            UpdateStatus::Downloaded { .. } => this.install_update_now(cx),
                            UpdateStatus::Error { .. } => this.retry_update(cx),
                            UpdateStatus::Idle { .. }
                            | UpdateStatus::Checking { .. }
                            | UpdateStatus::NotAvailable { .. } => {
                                this.check_for_updates_manual(cx)
                            }
                            _ => {}
                        }),
                    ),
            );

        div()
            .id("update-dialog-overlay")
            .absolute()
            .top(px(0.))
            .bottom(px(0.))
            .left(px(0.))
            .right(px(0.))
            .bg(rgba(0x000000, 0.55))
            .flex()
            .items_center()
            .justify_center()
            .on_mouse_down(MouseButton::Left, |_, _, cx| cx.stop_propagation())
            .child(card)
    }
}
