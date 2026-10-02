#![cfg(target_os = "windows")]

#[path = "../src/model_distribution.rs"]
mod model_distribution;

use gpui::{
    canvas, div, prelude::*, px, size, App, Application, Bounds, Context, Render, StyledText,
    TextLayout, Window, WindowBounds, WindowOptions,
};
use std::{cell::RefCell, rc::Rc};

struct Measurement {
    width: f32,
    original: String,
    layout: TextLayout,
}

struct Fixture {
    results: Rc<RefCell<Vec<Measurement>>>,
}

impl Render for Fixture {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        let mut columns = Vec::new();
        let mut probes = Vec::new();
        for width in [160., 216., 320., 700.] {
            let mut entries = Vec::new();
            for label in [
                "swe-2-medium(adaptive) 19.4M ($2.39)",
                "glm-5-3-flash-high 207.4K ($0.0092)",
                "gpt-5-6-luna-xhigh-thinking-fast(adaptive) 12.3M ($12.34)",
                "compactor 82.1K",
            ] {
                let text = StyledText::new(label.to_string());
                probes.push(Measurement {
                    width,
                    original: label.to_string(),
                    layout: text.layout().clone(),
                });
                entries.push(model_distribution::model_usage_entry(text, 0xffa500));
            }
            columns.push(
                div()
                    .flex()
                    .w(px(width))
                    .child(model_distribution::model_distribution(entries)),
            );
        }
        let results = self.results.clone();
        div()
            .flex()
            .flex_col()
            .text_size(px(12.))
            .children(columns)
            .child(
                canvas(
                    move |_, _, cx| {
                        *results.borrow_mut() = probes;
                        cx.quit();
                    },
                    |_, _, _, _| {},
                )
                .h(px(1.)),
            )
    }
}

#[test]
#[ignore = "Requires an interactive Windows desktop; run with --ignored --test-threads=1"]
fn long_model_labels_wrap_inside_the_column() {
    let results = Rc::new(RefCell::new(Vec::new()));
    let observed = results.clone();
    Application::new().run(move |cx: &mut App| {
        cx.open_window(
            WindowOptions {
                window_bounds: Some(WindowBounds::Windowed(Bounds::new(
                    gpui::point(px(0.), px(0.)),
                    size(px(1180.), px(760.)),
                ))),
                focus: false,
                ..Default::default()
            },
            |_, cx| cx.new(|_| Fixture { results }),
        )
        .unwrap();
    });
    let observed = observed.borrow();
    assert_eq!(observed.len(), 16, "native frame must have been measured");
    for Measurement {
        width,
        original,
        layout,
    } in observed.iter()
    {
        let wrapped = layout.wrapped_text();
        let right: f32 = layout.bounds().right().into();
        let height: f32 = layout.bounds().size.height.into();
        assert!(
            right <= *width + 1.,
            "text exceeds {width}px column: {original}; right={right}, wrapped={wrapped:?}"
        );
        assert_eq!(
            wrapped.replace('\n', ""),
            *original,
            "all label characters must remain visible"
        );
        if *width <= 216. && original.contains("($") {
            assert!(
                wrapped.contains('\n') && height > 20.,
                "long label must grow vertically at {width}px: {original}"
            );
        }
    }
}
