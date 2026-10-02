use gpui::{div, prelude::*, px, rgb, Div, IntoElement};

#[allow(dead_code)]
pub fn model_usage_entry(label: impl IntoElement, color: u32) -> Div {
    div()
        .w_full()
        .min_w(px(0.))
        .flex()
        .items_start()
        .gap_1()
        .child(
            div()
                .w(px(7.))
                .h(px(7.))
                .mt(px(4.))
                .flex_none()
                .rounded_full()
                .bg(rgb(color)),
        )
        .child(
            div()
                .flex_1()
                .min_w(px(0.))
                .whitespace_normal()
                .child(label),
        )
}

#[allow(dead_code)]
pub fn model_distribution(entries: impl IntoIterator<Item = Div>) -> Div {
    div()
        .flex_1()
        .min_w(px(0.))
        .overflow_hidden()
        .flex()
        .flex_col()
        .gap_y_1()
        .children(entries)
}

/// 汇总行（单行）模型分布单模型摘要
pub fn model_summary_single(name: impl IntoElement, color: u32) -> Div {
    div()
        .flex_1()
        .min_w(px(0.))
        .overflow_hidden()
        .flex()
        .items_center()
        .gap(px(6.))
        .child(
            div()
                .w(px(6.))
                .h(px(6.))
                .flex_none()
                .rounded_full()
                .bg(rgb(color)),
        )
        .child(div().min_w(px(0.)).truncate().text_xs().child(name))
}

/// 汇总行（单行）多模型比例条与主模型摘要
pub fn model_summary_stacked(
    segments: impl IntoIterator<Item = (u32, f32)>,
    top_model_name: impl IntoElement,
    top_model_color: u32,
    top_pct_text: impl IntoElement,
    extra_count: usize,
) -> Div {
    let bar_width = 48.0f32;
    let seg_divs = segments.into_iter().map(|(color, ratio)| {
        let seg_w = (ratio * bar_width).round().max(2.0);
        div().w(px(seg_w)).h_full().flex_none().bg(rgb(color))
    });

    let bar = div()
        .w(px(bar_width))
        .h(px(5.))
        .flex_none()
        .rounded(px(2.5))
        .bg(rgb(0x272733))
        .overflow_hidden()
        .flex()
        .items_center()
        .children(seg_divs);

    let mut row = div()
        .flex_1()
        .min_w(px(0.))
        .overflow_hidden()
        .flex()
        .items_center()
        .gap(px(4.))
        .child(bar)
        .child(
            div()
                .w(px(5.))
                .h(px(5.))
                .flex_none()
                .rounded_full()
                .bg(rgb(top_model_color)),
        )
        .child(
            div()
                .min_w(px(0.))
                .truncate()
                .text_xs()
                .child(top_model_name),
        )
        .child(
            div()
                .flex_none()
                .text_xs()
                .text_color(rgb(0x8f8fa3))
                .child(top_pct_text),
        );

    if extra_count > 0 {
        row = row.child(
            div()
                .flex_none()
                .px_1()
                .py(px(0.5))
                .rounded(px(2.))
                .bg(rgb(0x272733))
                .text_xs()
                .text_color(rgb(0x8f8fa3))
                .child(format!("+{extra_count}")),
        );
    }

    row
}
