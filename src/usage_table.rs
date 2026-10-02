use gpui::{div, prelude::*, px, rgb, Div, IntoElement};

pub fn usage_row(
    label: impl IntoElement,
    cells: [String; 8],
    models: impl IntoElement,
    detail: bool,
) -> Div {
    let widths = [60., 52., 70., 70., 76., 76., 76., 70.];
    div()
        .flex()
        .w_full()
        .min_w(px(0.))
        .gap_2()
        .px_2()
        .py_1()
        .text_xs()
        .border_b_1()
        .border_color(rgb(0x2c2c38))
        .when(detail, |row| row.bg(rgb(0x17171d)))
        .child(div().w(px(90.)).flex_none().child(label))
        .children(
            cells
                .into_iter()
                .zip(widths)
                .map(|(text, width)| div().w(px(width)).flex_none().text_right().child(text)),
        )
        .child(models)
}

pub struct ModelBreakdownItem {
    pub name: String,
    pub color: u32,
    pub share: String,
    pub turns: String,
    pub input: String,
    pub output: String,
    pub cache_creation: String,
    pub cached: String,
    pub total: String,
    pub cost: String,
}

pub fn model_breakdown_panel(
    title: impl IntoElement,
    summary: impl IntoElement,
    headers: [String; 9],
    rows: Vec<ModelBreakdownItem>,
) -> Div {
    // 占比 56px, 轮次 52px, 输入 70px, 输出 70px, 缓存写入 76px, 缓存读取 76px, 总计 76px, 费用 70px
    // 与主表对应数值列保持一致规格
    let widths = [56., 52., 70., 70., 76., 76., 76., 70.];
    div()
        .w_full()
        .min_w(px(0.))
        .px_3()
        .py_2()
        .bg(rgb(0x131318))
        .border_b_1()
        .border_color(rgb(0x2c2c38))
        .child(
            div()
                .w_full()
                .min_w(px(0.))
                .pl(px(16.))
                .border_l_2()
                .border_color(rgb(0x4cc2ff))
                .pl_3()
                .flex()
                .flex_col()
                .gap_1()
                .child(
                    div()
                        .flex()
                        .items_center()
                        .gap_3()
                        .pb_1()
                        .border_b_1()
                        .border_color(rgb(0x242430))
                        .text_xs()
                        .child(
                            div()
                                .font_weight(gpui::FontWeight::MEDIUM)
                                .text_color(rgb(0xe6e6ee))
                                .child(title),
                        )
                        .child(div().text_color(rgb(0x8f8fa3)).child(summary)),
                )
                .child(
                    div()
                        .flex()
                        .w_full()
                        .items_center()
                        .gap_2()
                        .py_1()
                        .text_xs()
                        .text_color(rgb(0x8f8fa3))
                        .border_b_1()
                        .border_color(rgb(0x242430))
                        .child(div().w(px(220.)).flex_none().child(headers[0].clone()))
                        .children(headers[1..].iter().zip(widths).map(|(header, width)| {
                            div()
                                .w(px(width))
                                .flex_none()
                                .text_right()
                                .child(header.clone())
                        })),
                )
                .children(rows.into_iter().map(|row| {
                    div()
                        .flex()
                        .w_full()
                        .items_center()
                        .gap_2()
                        .py_1()
                        .text_xs()
                        .hover(|h| h.bg(rgb(0x1a1a24)))
                        .child(
                            div()
                                .w(px(220.))
                                .flex_none()
                                .flex()
                                .items_center()
                                .gap(px(6.))
                                .child(
                                    div()
                                        .w(px(6.))
                                        .h(px(6.))
                                        .flex_none()
                                        .rounded_full()
                                        .bg(rgb(row.color)),
                                )
                                .child(
                                    div()
                                        .min_w(px(0.))
                                        .truncate()
                                        .text_color(rgb(0xe6e6ee))
                                        .child(row.name),
                                ),
                        )
                        .child(
                            div()
                                .w(px(56.))
                                .flex_none()
                                .text_right()
                                .text_color(rgb(0x8f8fa3))
                                .child(row.share),
                        )
                        .child(div().w(px(52.)).flex_none().text_right().child(row.turns))
                        .child(div().w(px(70.)).flex_none().text_right().child(row.input))
                        .child(div().w(px(70.)).flex_none().text_right().child(row.output))
                        .child(
                            div()
                                .w(px(76.))
                                .flex_none()
                                .text_right()
                                .child(row.cache_creation),
                        )
                        .child(div().w(px(76.)).flex_none().text_right().child(row.cached))
                        .child(
                            div()
                                .w(px(76.))
                                .flex_none()
                                .text_right()
                                .font_weight(gpui::FontWeight::MEDIUM)
                                .child(row.total),
                        )
                        .child(
                            div()
                                .w(px(70.))
                                .flex_none()
                                .text_right()
                                .text_color(rgb(0x3ddc97))
                                .child(row.cost),
                        )
                })),
        )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn model_breakdown_item_fields_are_accessible() {
        let item = ModelBreakdownItem {
            name: "swe-2-medium".into(),
            color: 0xffa500,
            share: "99.6%".into(),
            turns: "184".into(),
            input: "1.1M".into(),
            output: "78.5K".into(),
            cache_creation: "0".into(),
            cached: "18.3M".into(),
            total: "19.4M".into(),
            cost: "$2.39".into(),
        };
        assert_eq!(item.name, "swe-2-medium");
        assert_eq!(item.share, "99.6%");
        assert_eq!(item.total, "19.4M");
        assert_eq!(item.cost, "$2.39");
    }
}
