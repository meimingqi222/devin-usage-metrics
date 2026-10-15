//! 产品输入配色适配；交互与IME由公共组件维护。
use gpui::{Context, SharedString};
pub use gpui_app_widgets::entity_input::{bind_keys, TextInput};

pub fn new(
    cx: &mut Context<TextInput>,
    content: impl Into<SharedString>,
    placeholder: impl Into<SharedString>,
    masked: bool,
) -> TextInput {
    TextInput::new(
        cx,
        content,
        placeholder,
        masked,
        gpui_app_widgets::entity_input::InputStyle {
            placeholder: gpui::rgb(0x8f8fa3).into(),
            caret: gpui::rgb(0x4cc2ff).into(),
            selection: gpui::rgba(0x4cc2ff40).into(),
        },
    )
}
