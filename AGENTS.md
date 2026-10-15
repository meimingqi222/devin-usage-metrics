# Agent Usage Metrics UI boundaries

状态栏与桌面窗口机制统一使用 `gpui-app-kit`，Cargo.toml 固定 Git 提交；兄弟目录 `../gpui-app-kit` 用于联合开发和内容守卫。
`src/tray.rs` 只读取 TrayModel、渲染内容并发送 TraySession 事件；改UI时不编辑宿主。
配额加载、成本总览、刷新频率与任务属于 `src/tray_model.rs`；产品动作路由属于 `src/tray_controller.rs`。
禁止在内容中新增窗口句柄、原生托盘、focus/close事件接线、defer/spawn/quit或直接更新模型。
检查：cargo fmt --all --check；cargo clippy --all-targets；cargo test；
python3 ../gpui-app-kit/tools/check_content.py src/tray.rs。
本地路径、Git固定版本与CI配置见 `../gpui-app-kit/README.md`。
