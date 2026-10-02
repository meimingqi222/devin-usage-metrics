#![cfg(target_os = "windows")]

#[path = "../src/model_distribution.rs"]
mod model_distribution;

#[path = "../src/usage_table.rs"]
mod usage_table;

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
    entry_widths: Rc<RefCell<Vec<(f32, f32)>>>,
}

impl Render for Fixture {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        let mut columns = Vec::new();
        let mut probes = Vec::new();
        for width in [160., 216., 320., 700.] {
            let mut entries = Vec::new();
            for label in [
                "swe-2-medium(adaptive) 19.4M",
                "glm-5-3-flash-high 207.4K",
                "gpt-5-6-luna-xhigh-thinking-fast(adaptive) 12.3M",
                "compactor 82.1K",
            ] {
                let text = StyledText::new(label.to_string());
                probes.push(Measurement {
                    width,
                    original: label.to_string(),
                    layout: text.layout().clone(),
                });
                entries.push(model_distribution::model_usage_entry(text, 0xffa500));
                let entry_widths = self.entry_widths.clone();
                entries.push(
                    model_distribution::model_usage_entry(label.to_string(), 0xffa500)
                        .relative()
                        .child(
                            canvas(
                                move |bounds, _, _| {
                                    entry_widths
                                        .borrow_mut()
                                        .push((width, bounds.size.width.into()));
                                },
                                |_, _, _, _| {},
                            )
                            .absolute()
                            .size_full(),
                        ),
                );
            }
            columns.push(
                div()
                    .flex()
                    .w(px(width))
                    .child(model_distribution::model_distribution(entries)),
            );
        }
        for width in [960., 1080.] {
            for detail in [false, true] {
                let label = if detail {
                    "gpt-5-6-luna-xhigh-thinking-fast(adaptive)"
                } else {
                    "gpt-5-6-luna-xhigh-thinking-fast(adaptive) 12.3M"
                };
                let text = StyledText::new(label.to_string());
                probes.push(Measurement {
                    width,
                    original: label.to_string(),
                    layout: text.layout().clone(),
                });
                columns.push(
                    div().w(px(width)).child(usage_table::usage_row(
                        if detail { "↳" } else { "▾ 09-23 Wed" },
                        ["1", "184", "1.1M", "78.5K", "0", "18.3M", "19.5M", "$0.00"]
                            .map(str::to_string),
                        model_distribution::model_distribution([
                            model_distribution::model_usage_entry(text, 0xffa500),
                        ]),
                        detail,
                    )),
                );
            }
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
    let entry_widths = Rc::new(RefCell::new(Vec::new()));
    let observed_widths = entry_widths.clone();
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
            |_, cx| {
                cx.new(|_| Fixture {
                    results,
                    entry_widths,
                })
            },
        )
        .unwrap();
    });
    assert_eq!(observed_widths.borrow().len(), 16);
    for (available, actual) in observed_widths.borrow().iter() {
        assert!(
            (actual - available).abs() < 1.,
            "plain model entry must occupy its full column: {actual}px of {available}px"
        );
    }
    let observed = observed.borrow();
    assert_eq!(observed.len(), 20, "native frame must have been measured");
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
        for (index, character) in wrapped.char_indices() {
            if character == '\n' {
                continue;
            }
            let position = layout
                .position_for_index(index)
                .expect("glyph must be laid out");
            let bounds = layout.bounds();
            assert!(
                position.x >= bounds.left() && position.x <= bounds.right(),
                "glyph outside text bounds: {original}, index={index}"
            );
        }
        if *width <= 216. && original.starts_with("gpt-") {
            assert!(
                wrapped.contains('\n') && height > 20.,
                "long label must grow vertically at {width}px: {original}"
            );
        }
    }
}

struct SummaryMeasurement {
    #[allow(dead_code)]
    name: String,
    bounds: Bounds<gpui::Pixels>,
}

struct SummaryFixture {
    measurements: Rc<RefCell<Vec<SummaryMeasurement>>>,
}

impl Render for SummaryFixture {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        let mut rows = Vec::new();
        let m = self.measurements.clone();

        // 1. 单模型行
        let single_el =
            model_distribution::model_summary_single("glm-5-3-flash-high 150.5K", 0xf472b6);
        let m_clone = m.clone();
        rows.push(
            div().w(px(236.)).flex().child(single_el).child(
                canvas(
                    move |b, _, _| {
                        m_clone.borrow_mut().push(SummaryMeasurement {
                            name: "single_row".to_string(),
                            bounds: b,
                        });
                    },
                    |_, _, _, _| {},
                )
                .size_full()
                .absolute(),
            ),
        );

        // 2. 多模型行
        let stacked_el = model_distribution::model_summary_stacked(
            vec![(0xffa500, 0.99), (0x4cc2ff, 0.01)],
            "swe-2-medium(adaptive)",
            0xffa500,
            "99%",
            1,
        );
        let m_clone2 = m.clone();
        rows.push(
            div().w(px(236.)).flex().child(stacked_el).child(
                canvas(
                    move |b, _, _| {
                        m_clone2.borrow_mut().push(SummaryMeasurement {
                            name: "stacked_row".to_string(),
                            bounds: b,
                        });
                    },
                    |_, _, _, _| {},
                )
                .size_full()
                .absolute(),
            ),
        );

        // 3. 全屏模式下的模型明细子面板 (1920px)
        let sub_headers = [
            "模型".to_string(),
            "占比".to_string(),
            "轮次".to_string(),
            "输入".to_string(),
            "输出".to_string(),
            "缓存写入".to_string(),
            "缓存读取".to_string(),
            "总计".to_string(),
            "费用".to_string(),
        ];
        let sub_rows = vec![
            usage_table::ModelBreakdownItem {
                name: "claude-opus-5-5-medium".into(),
                color: 0xf472b6,
                share: "37.5%".into(),
                turns: "108".into(),
                input: "264".into(),
                output: "111.8K".into(),
                cache_creation: "1.5M".into(),
                cached: "27.3M".into(),
                total: "29.0M".into(),
                cost: "$15.27".into(),
            },
            usage_table::ModelBreakdownItem {
                name: "swe-2-high".into(),
                color: 0xffa500,
                share: "31.4%".into(),
                turns: "221".into(),
                input: "755.0K".into(),
                output: "195.8K".into(),
                cache_creation: "0".into(),
                cached: "23.3M".into(),
                total: "24.3M".into(),
                cost: "$0.00".into(),
            },
        ];
        let panel_el = usage_table::model_breakdown_panel(
            "09-28 模型明细",
            "共 2 个模型",
            sub_headers,
            sub_rows,
        );
        let m_clone3 = m.clone();
        rows.push(
            div().w(px(1920.)).flex().child(panel_el).child(
                canvas(
                    move |b, _, _| {
                        m_clone3.borrow_mut().push(SummaryMeasurement {
                            name: "fullscreen_panel".to_string(),
                            bounds: b,
                        });
                    },
                    |_, _, _, _| {},
                )
                .size_full()
                .absolute(),
            ),
        );

        div().flex().flex_col().children(rows).child(
            canvas(
                move |_, _, cx| {
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
fn summary_layout_renders_correctly_without_abnormal_gaps() {
    let measurements = Rc::new(RefCell::new(Vec::new()));
    let app_m = measurements.clone();
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
            |_, cx| {
                cx.new(|_| SummaryFixture {
                    measurements: app_m,
                })
            },
        )
        .unwrap();
    });

    let records = measurements.borrow();
    assert_eq!(
        records.len(),
        3,
        "Single, stacked, and fullscreen panel rows must be measured"
    );
    assert!((f32::from(records[0].bounds.size.width) - 236.).abs() < 1.);
    assert!((f32::from(records[1].bounds.size.width) - 236.).abs() < 1.);
    assert!((f32::from(records[2].bounds.size.width) - 1920.).abs() < 1.);
}
