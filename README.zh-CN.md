# Agent Usage Metrics

[English](README.md) | **简体中文**

一个原生桌面应用，用于读取 Devin、Amp、Claude Code、Codex、Antigravity、Grok Build、ZCode 与 OpenCode 在本地留下的会话数据，并展示 token 用量、模型分布与会话详情。可从顶部切换不同 Agent；所有数据均来自本机，不会上传到任何服务器。

## 功能

- **用量视图**
  - 支持在 Devin、Amp、Claude Code、Codex、Antigravity、Grok Build、ZCode 与 OpenCode 之间切换
  - 按日（15 天）/ 周（12 周）/ 月（12 个月）聚合 token 用量
  - 总 Tokens、输入（新）、输出、缓存读取、轮次/会话统计卡片
  - 堆叠柱状图展示 token 用量趋势（输入 / 输出 / 缓存分层）
  - 周期明细表，含会话数、轮次、各类型 token、按模型分组的用量分布
- **会话视图**
  - 最近 500 条会话列表，按最后活跃时间倒序
  - 显示标题、工作目录、agent 模式、所选模型、消息数、总 tokens
  - 自动识别 `adaptive` 路由模型并显示真实模型
  - 点击任一会话查看详情：TTFT 中位数、轮次数、agent 消息数、时间区间等
- **数据源**
  - Devin：`~/.local/share/devin/{cli,cli-next}/sessions.db`（Windows 使用平台数据目录）
  - Amp：`~/.local/share/amp/threads/*.json`
  - Claude Code：`~/.claude/projects/**/*.jsonl`（子代理用量归并到主会话）
  - Codex：`~/.codex/{sessions,archived_sessions}/**/*.jsonl` 或 `$CODEX_HOME`
  - Antigravity：`~/.gemini/antigravity/conversations/*.db`（protobuf 编码的 SQLite）
  - Grok Build：`~/.grok/sessions/*/summary.json`（轮次明细来自同目录的 `updates.jsonl`）
  - ZCode：`~/.zcode/cli/db/db.sqlite`
  - OpenCode：`~/.local/share/opencode/opencode.db`（同时覆盖 `opencode` 与 `opencode2`；旧版 `storage/` 下的 JSON 会话已迁移进该库）
  - 各 Agent 按需懒加载，数据源并行解析
  - 使用平台缓存目录中的 5 分钟磁盘缓存，加速二次启动
  - 顶部「重新加载」按钮可强制绕过缓存；通过 mtime 检测未变化的文件，仅重新解析有改动的文件

## 技术栈

| 组件 | 说明 |
| --- | --- |
| [GPUI](https://github.com/zed-industries/zed) | Zed 的高性能 Rust UI 框架，用于构建原生窗口与控件 |
| [rusqlite](https://crates.io/crates/rusqlite) | 以 `bundled` 方式编译 SQLite，只读模式打开会话数据库 |
| [serde](https://crates.io/crates/serde) / [serde_json](https://crates.io/crates/serde_json) | 解析会话 metadata 与缓存序列化 |
| [chrono](https://crates.io/crates/chrono) | 日期/周期聚合与时区处理 |

## 前置条件

- Windows 或 macOS（当前实现已在 Windows 验证）
- Rust 工具链（推荐 `rustup` 安装的 stable 版本）
- 本机使用过至少一种受支持的编码 Agent

## 构建与运行

```bash
# 调试运行
cargo run

# 发布构建
cargo build --release

# 运行测试（依赖本机会话数据的集成测试在无数据环境会自动跳过）
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
├── data.rs    # 公共记录、Devin SQLite 读取、磁盘缓存
├── local_sources.rs # Amp、Claude Code、Codex、Antigravity、Grok Build、ZCode 与 OpenCode 导入器
├── pricing.rs # 模型定价表与费用计算
├── devin-model-pricing.json    # Devin 官方模型价格表（编译期嵌入）
├── models-dev-pricing.json     # 从 models.dev 提取的非 Devin 模型价格子集
└── agg.rs     # 按日/周/月的桶聚合与模型分组
tests/
└── dump.rs    # 端到端集成测试，校验真实数据加载与聚合
assets/
└── icon*.{png,icns}  # 应用图标资源
```

## 数据与隐私

- 应用以只读方式打开 Devin 数据库，并且只读取其他 Agent 的 JSON/JSONL 文件
- 缓存文件以原子 rename 写入，避免半截文件
- 不发起任何网络请求，所有展示均来自本机

## 许可证

私有项目，暂未指定开源许可证。
