use gpui::{div, prelude::*, px, rgb, Div, IntoElement};

pub fn model_usage_entry(label: impl IntoElement, color: u32) -> Div {
    div()
        .flex()
        .max_w_full()
        .min_w(px(0.))
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
        .child(div().min_w(px(0.)).whitespace_normal().child(label))
}

pub fn model_distribution(entries: impl IntoIterator<Item = Div>) -> Div {
    div()
        .flex_1()
        .min_w(px(0.))
        .overflow_hidden()
        .flex()
        .flex_wrap()
        .gap_x_2()
        .gap_y_1()
        .children(entries)
}
