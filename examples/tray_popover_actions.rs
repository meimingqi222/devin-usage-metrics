//! 原生窗口 smoke：cargo run --example tray_popover_actions
//! 内容触发动作时必须先完成关窗；完整定位/旧会话/Esc 回归在公共库中。
use gpui::{
    div, prelude::*, App, AppContext, Application, Context, IntoElement, Render, Window,
    WindowOptions,
};
use gpui_app_host::{TrayHost, TraySession};
use std::sync::{Arc, Mutex};

#[derive(Clone, Copy)]
enum Action {
    Stay,
    Open,
}
impl gpui_app_host::TrayAction for Action {
    fn dismisses_popover(&self) -> bool {
        matches!(self, Self::Open)
    }
}
struct Content;
impl Render for Content {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        div().size_full().child("Shared tray host native smoke")
    }
}
fn exercise(cx: &mut App, stage: usize) {
    if stage == 4 {
        println!("PASS: native keep-open, close-before-action, dismiss and Escape");
        cx.quit();
        return;
    }
    let host = TrayHost::new(350., 520.);
    let session: Arc<Mutex<Option<TraySession<Action>>>> = Arc::new(Mutex::new(None));
    let saved = session.clone();
    let window = cx
        .open_window(WindowOptions::default(), |window, cx| {
            cx.new(|cx| {
                host.mount(
                    window,
                    cx,
                    Arc::new(move |action, cx| match action {
                        Action::Stay => assert_eq!(cx.windows().len(), 1),
                        Action::Open => assert!(
                            cx.windows().is_empty(),
                            "business action ran before native window removal"
                        ),
                    }),
                    move |current, _, cx| {
                        *saved.lock().unwrap() = Some(current);
                        cx.new(|_| Content)
                    },
                )
            })
        })
        .unwrap();
    window
        .update(cx, |_, window, _| {
            window.on_next_frame(move |window, cx| {
                let current = session.lock().unwrap().as_ref().unwrap().clone();
                match stage {
                    0 => {
                        current.emit(Action::Stay, cx);
                        current.dismiss(cx);
                    }
                    1 => current.emit(Action::Open, cx),
                    2 => current.dismiss(cx),
                    _ => {
                        window.dispatch_keystroke(gpui::Keystroke::parse("escape").unwrap(), cx);
                    }
                }
                cx.defer(move |cx| {
                    assert!(
                        cx.windows().is_empty(),
                        "native popover remained open at stage {stage}"
                    );
                    exercise(cx, stage + 1);
                });
            });
        })
        .unwrap();
}
fn main() {
    Application::new().run(|cx| exercise(cx, 0));
}
