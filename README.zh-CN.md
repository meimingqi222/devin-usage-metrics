# Agent Usage Metrics

[English](README.md) | **简体中文**

一个原生桌面应用，用于读取 Devin、Claude Code、Codex、Antigravity、Grok Build、ZCode、OpenCode 与 pi-agent 在本地留下的会话数据，并展示 token 用量、模型分布与会话详情。从左侧边栏切换 Agent，界面支持中英双语；本地数据只会在用户开启 GitHub 登录同步后发送到所配置的自建 API。

## 功能

- **用量视图**
  - 左侧边栏常驻全部受支持的 Agent（Devin、Claude Code、Codex、Antigravity、Grok Build、ZCode、OpenCode、pi-agent）及其会话数
  - 中英双语界面，默认跟随系统语言；顶栏点「中 / EN」即时切换并记住选择
  - 按日（15 天）/ 周（12 周）/ 月（12 个月）聚合 token 用量
  - 总 Tokens、输入（新）、输出、缓存读取、轮次/会话统计卡片
  - 堆叠柱状图展示 token 用量趋势（输入 / 输出 / 缓存分层）
  - 周期明细表，含会话数、轮次、各类型 token、按模型分组的用量分布
- **会话视图**
  - 最近 500 条会话列表，按最后活跃时间倒序
  - 显示标题、工作目录、agent 模式、所选模型、消息数、总 tokens
  - 自动识别 `adaptive` 路由模型并显示真实模型
  - 点击任一会话查看详情：TTFT 中位数、轮次数、agent 消息数、时间区间等
- **多设备同步**
  - 只需填入自建同步 API 地址并登录 GitHub；同一 GitHub 账号的设备自动同步到同一份数据
  - 同步默认关闭，顶栏点「同步已关 / 同步已开」一键开关，开启后完全自动
  - 开启后后台每小时自动导出本机数据 + 导入其他设备数据；启动和手动重新加载会立即同步一次
  - 每台设备自动生成稳定设备 ID；同步数据采用压缩的内容寻址分片和原子更新的设备 head，大历史记录不再形成持续膨胀的单一数据包
  - 迁移时会自动导入已有 v1 数据包；旧版本无法读取 v2 格式，因此请先升级所有仍在同步的客户端
  - 侧边栏「设备」区可切换查看单设备或汇总所有设备
  - GitHub 仅用于登录；同步对象只保存在用户配置的自建服务中
- **数据源**
  - Devin：`~/.local/share/devin/{cli,cli-next}/sessions.db`（Windows 使用平台数据目录）
  - Claude Code：`~/.claude/projects/**/*.jsonl`（子代理用量归并到主会话）
  - Codex：`~/.codex/{sessions,archived_sessions}/**/*.jsonl` 或 `$CODEX_HOME`
  - Antigravity：`~/.gemini/antigravity/conversations/*.db`（protobuf 编码的 SQLite）
  - Grok Build：`~/.grok/sessions/*/summary.json`（轮次明细来自同目录的 `updates.jsonl`）
  - ZCode：`~/.zcode/cli/db/db.sqlite`
  - OpenCode：`~/.local/share/opencode/opencode.db`（同时覆盖 `opencode` 与 `opencode2`；旧版 `storage/` 下的 JSON 会话已迁移进该库）
  - pi-agent：`~/.pi/agent/sessions/**/*.jsonl`
  - 各 Agent 按需懒加载，数据源并行解析
  - 使用平台缓存目录中的 5 分钟磁盘缓存，加速二次启动
  - 顶部「重新加载」按钮可强制绕过缓存；通过 mtime 检测未变化的文件，仅重新解析有改动的文件

## 界面语言

界面支持简体中文与英文，语言在启动时解析一次：

1. 若配置目录下的 `config.json` 含有效 `lang`，以它为准
2. 否则跟随系统 locale（`zh` 前缀选中文，其余一律英文）

| 平台 | 配置文件 |
| --- | --- |
| macOS | `~/Library/Application Support/devin-usage-metrics/config.json` |
| Linux | `~/.config/devin-usage-metrics/config.json` |
| Windows | `%APPDATA%\devin-usage-metrics\config.json` |

顶栏点击「中 / EN」会立即写入该文件，重启后保持。同一设置也作用于 `--cli` 的人类可读输出；CSV 列名为方便脚本处理，固定使用稳定的英文标识符。

已知限制：数据源错误消息在加载阶段生成，并随缓存一起落盘，因此会保留加载它那一次运行的语言，直到下次重新加载。

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

# CLI：按日输出 Claude Code 用量（表格口径对齐 ccusage）
cargo run -- --cli --agent claude --since 2026-08-20 --until 2026-08-30 --refresh

# 机器可读 CSV
cargo run -- --cli --agent claude --days 30 --format csv

# 全部 Agent，并按 Agent 展开
cargo run -- --cli --agent all --by-agent --days 30

# 发布构建
cargo build --release

# 运行测试（依赖本机会话数据的集成测试在无数据环境会自动跳过）
cargo test
```

CLI 默认统计 Claude Code 最近 30 天的数据；支持 `all`、`devin`、`claude`、`codex`、`antigravity`、`grok`、`zcode`、`opencode` 和 `pi`。`--by-agent` 可在 `all` 汇总下增加逐 Agent 分项。运行 `cargo run -- --cli --help` 查看全部选项。

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
├── main.rs    # GPUI 应用入口、UI 渲染、用量/会话/配额视图、多设备同步交互
├── lib.rs     # 模块导出
├── data.rs    # 公共记录、Devin SQLite 读取、磁盘缓存、设备标识管理
├── local_sources.rs # Claude Code、Codex、Antigravity、Grok Build、ZCode、OpenCode 与 pi-agent 导入器
├── sync.rs    # 多设备同步编排、存储传输与 v1 迁移
├── sync/v2.rs # 内容寻址、压缩并带完整性校验的同步协议
├── pricing.rs # 模型定价表与费用计算
├── i18n.rs    # 中英双语文案、系统语言探测与偏好读写
├── cli.rs     # --cli 模式：按日用量汇总、表格与 CSV 输出
├── devin-model-pricing.json    # Devin 官方模型价格表（编译期嵌入）
├── models-dev-pricing.json     # 从 models.dev 提取的非 Devin 模型价格子集
└── agg.rs     # 按日/周/月的桶聚合与模型分组（支持按设备过滤）
tests/
└── dump.rs    # 端到端集成测试，校验真实数据加载与聚合
assets/
└── icon*.{png,icns}  # 应用图标资源
```

## 数据与隐私

- 应用以只读方式打开 Devin 数据库，并且只读取其他 Agent 的 JSON/JSONL 文件
- 缓存文件以原子 rename 写入，避免半截文件
- 不发起任何网络请求，所有展示均来自本机及用户配置的共享同步目录
- 多设备同步数据只写入用户配置的自建同步 API；GitHub 仅用于确认账号身份

## 许可证

私有项目，暂未指定开源许可证。
