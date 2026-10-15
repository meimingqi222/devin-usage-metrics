//! 产品动作路由；原生托盘和窗口生命周期属于 gpui-app-host。
use crate::tray::TrayPopover;
use crate::tray_model::{OverviewPeriod, TrayModel};
use devin_usage_metrics::i18n;
use gpui::{App, AppContext, WeakEntity};
use gpui_app_host::{AppCallback, TrayConfig, TrayHost, TrayMenuItem};
use std::sync::{Arc, Mutex};

type CurrentModel = Arc<Mutex<Option<WeakEntity<TrayModel>>>>;
pub enum TrayAction {
    Refresh,
    Period(OverviewPeriod),
    ToggleAccounts,
    OpenDashboard,
}
impl gpui_app_host::TrayAction for TrayAction {
    fn dismisses_popover(&self) -> bool {
        matches!(self, Self::OpenDashboard)
    }
}
pub struct TrayController {
    host: TrayHost,
    current: CurrentModel,
}
impl gpui::Global for TrayController {}
impl TrayController {
    pub fn close_active(cx: &mut App, active: gpui::AnyWindowHandle) -> bool {
        cx.try_global::<Self>()
            .map(|controller| controller.host.clone())
            .is_some_and(|host| host.close_active(active, cx))
    }
    pub fn activate_dashboard(cx: &mut App) {
        gpui_app_host::desktop::show_main_window();
        cx.activate(true);
    }
    pub fn dashboard_closed(cx: &mut App, dashboard_is_open: impl Fn(&App) -> bool + 'static) {
        cx.spawn(async move |cx| {
            let _ = cx.update(move |cx| {
                if !dashboard_is_open(cx) {
                    gpui_app_host::desktop::main_window_closed(cx, cx.has_global::<Self>());
                }
            });
        })
        .detach();
    }
    pub fn open_dashboard(cx: &mut App, action: AppCallback) {
        if let Some(controller) = cx.try_global::<Self>() {
            controller.host.clone().dismiss_then(action, cx);
        } else {
            action(cx);
        }
    }
    pub fn refresh_language(cx: &mut App) {
        gpui_app_host::set_menu_labels(
            cx,
            &[
                i18n::t(i18n::Key::TrayOpenDashboard),
                i18n::t(i18n::Key::TrayRefresh),
                i18n::t(i18n::Key::TrayQuit),
            ],
        );
        cx.refresh_windows();
    }
    pub fn init(cx: &mut App, open_main: AppCallback) -> Result<(), String> {
        let icon = image::load_from_memory(include_bytes!("../assets/tray_icon.png"))
            .map_err(|e| e.to_string())?
            .into_rgba8();
        let (icon_width, icon_height) = icon.dimensions();
        let host = TrayHost::new(350., 520.);
        let current: CurrentModel = Arc::new(Mutex::new(None));
        let active = current.clone();
        let open = open_main.clone();
        host.install(
            cx,
            TrayConfig {
                tooltip: "Agent Usage Metrics".into(),
                icon_rgba: icon.into_raw(),
                icon_width,
                icon_height,
                template: cfg!(target_os = "macos"),
                menu: vec![
                    TrayMenuItem {
                        label: i18n::t(i18n::Key::TrayOpenDashboard).into(),
                        action: open_main,
                        dismiss_popover: true,
                    },
                    TrayMenuItem {
                        label: i18n::t(i18n::Key::TrayRefresh).into(),
                        action: Arc::new(Self::refresh_all),
                        dismiss_popover: false,
                    },
                    TrayMenuItem {
                        label: i18n::t(i18n::Key::TrayQuit).into(),
                        action: Arc::new(|cx| cx.quit()),
                        dismiss_popover: true,
                    },
                ],
            },
            Arc::new(move |action, cx| {
                if matches!(action, TrayAction::OpenDashboard) {
                    open(cx);
                    return;
                }
                let model = active
                    .lock()
                    .unwrap()
                    .as_ref()
                    .and_then(|model| model.upgrade());
                if let Some(model) = model {
                    model.update(cx, |model, cx| match action {
                        TrayAction::Refresh => model.start_load(true, cx),
                        TrayAction::Period(period) => model.switch_period(period, cx),
                        TrayAction::ToggleAccounts => model.toggle_accounts(cx),
                        TrayAction::OpenDashboard => unreachable!(),
                    });
                }
            }),
            {
                let current = current.clone();
                move |session, _, cx| {
                    let model = cx.new(|cx| TrayModel::new(session.lifetime(), cx));
                    *current.lock().unwrap() = Some(model.downgrade());
                    cx.new(|cx| TrayPopover::new(model, session, cx))
                }
            },
        )?;
        cx.set_global(Self { host, current });
        Ok(())
    }
    pub fn refresh_all(cx: &mut App) {
        let model = cx.try_global::<Self>().and_then(|controller| {
            controller
                .current
                .lock()
                .unwrap()
                .as_ref()
                .and_then(|model| model.upgrade())
        });
        if let Some(model) = model {
            model.update(cx, |model, cx| model.start_load(true, cx));
        }
    }
}
