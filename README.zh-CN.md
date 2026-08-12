# Devin Usage Metrics

[English](README.md) | **简体中文**

一个原生 macOS 桌面应用，用于读取 Devin CLI 在本地留下的会话数据库，并以仪表盘的形式展示 token 用量、模型分布与会话详情。所有数据均来自本机，不会上传到任何服务器。

## 功能

- **用量视图**
  - 按日（14 天）/ 周（12 周）/ 月（6 个月）聚合 token 用量
  - 总 Tokens、输入（新）、输出、缓存读取、轮次/会话统计卡片
  - 堆叠柱状图展示 token 用量趋势（输入 / 输出 / 缓存分层）
  - 周期明细表，含会话数、轮次、各类型 token、按模型分组的用量分布
- **会话视图**
  - 最近 500 条会话列表，按最后活跃时间倒序
  - 显示标题、工作目录、agent 模式、所选模型、消息数、总 tokens
  - 自动识别 `adaptive` 路由模型并显示真实模型
  - 点击任一会话查看详情：TTFT 中位数、轮次数、agent 消息数、时间区间等
- **数据源**
  - 只读打开 `~/.local/share/devin/cli/sessions.db` 与 `~/.local/share/devin/cli-next/sessions.db`
  - 并行加载多个数据源，结果合并展示
  - 5 分钟磁盘缓存（`~/.cache/devin-usage-metrics/cache.json`），加速二次启动
  - 顶部「重新加载」按钮可强制绕过缓存重新读取数据库

## 技术栈

| 组件 | 说明 |
| --- | --- |
| [GPUI](https://github.com/zed-industries/zed) | Zed 的高性能 Rust UI 框架，用于构建原生窗口与控件 |
| [rusqlite](https://crates.io/crates/rusqlite) | 以 `bundled` 方式编译 SQLite，只读模式打开会话数据库 |
| [serde](https://crates.io/crates/serde) / [serde_json](https://crates.io/crates/serde_json) | 解析会话 metadata 与缓存序列化 |
| [chrono](https://crates.io/crates/chrono) | 日期/周期聚合与时区处理 |

## 前置条件

- macOS（本项目当前仅配置和验证了 macOS 构建）
- Rust 工具链（推荐 `rustup` 安装的 stable 版本）
- 本机已使用过 Devin CLI，存在 `~/.local/share/devin/cli/sessions.db`

## 构建与运行

```bash
# 调试运行
cargo run

# 发布构建
cargo build --release

# 运行测试（需要本机存在 Devin 会话数据）
cargo test
```

## 打包为 .app

`Cargo.toml` 中已配置 `[package.metadata.bundle]`，可配合 [`cargo-bundle`](https://crates.io/crates/cargo-bundle) 打包：

```bash
cargo install cargo-bundle
cargo bundle --release
```

生成的 `.app` 会出现在 `target/release/bundle/osx/`，图标使用 `assets/icon.icns`。

## 项目结构

```
src/
├── main.rs    # GPUI 应用入口、UI 渲染、用量/会话视图
├── lib.rs     # 模块导出
├── data.rs    # SQLite 读取、metadata 解析、磁盘缓存
└── agg.rs     # 按日/周/月的桶聚合与模型分组
tests/
└── dump.rs    # 端到端集成测试，校验真实数据加载与聚合
assets/
└── icon*.{png,icns}  # 应用图标资源
```

## 数据与隐私

- 应用仅以 `SQLITE_OPEN_READ_ONLY` 方式打开 Devin CLI 的本地数据库，不会写入或修改 Devin 的任何数据
- 缓存文件以原子 rename 写入，避免半截文件
- 不发起任何网络请求，所有展示均来自本机

## 许可证

私有项目，暂未指定开源许可证。
