# Agent Usage Metrics

[English](README.md) | **简体中文**

本地编码 Agent 用量统计工具，主实现已迁移到 **Go + [MyGo 原生 UI](https://github.com/egoist/mygo)**。桌面构建不再需要编译 Rust、WebView、JavaScript 运行时或 cgo。

> 当前是 Go 迁移候选实现，不代表已经证明与 Rust 完全等价。旧 Rust 源码暂留作行为对照，不参与 Go 构建和发布。已知差异和验证边界见 [迁移状态](docs/go-migration.md)。

## 功能

- 读取 Devin、Claude Code、Codex、Antigravity、Grok Build、ZCode、OpenCode、pi-agent、MimoCode、WorkBuddy / CodeBuddy 的本地历史。
- 按日（15 天）/ 周（12 周）/ 月（12 个月）聚合，堆叠 Token 图、模型分布、自报/API 等价费用、设备筛选。
- 最近 500 条会话、搜索、模型识别、Token 总数、时间范围、TTFT 中位数及续读命令。
- Claude、Codex、Grok、Devin、Antigravity 实时配额，缓存过期标记与限流冷却；钥匙串访问需明确开启。
- Claude / Codex 历史压缩回放、确认应用本机配置、外部修改保护及跨重启撤销。估算不预测质量、订阅配额或实际账单。
- 默认关闭的 GitHub 身份验证 + 自建 API 同步；导入 v1/v2 历史，以内容寻址 v3 协议发布。
- 原生桌面窗口与托盘配额面板，中英双语 UI 并记住选择。CLI 人类可读输出目前仍为英文，CSV 列名固定英文。

## 环境要求

- **Go 1.27.1+**。`go.mod` 固定 MyGo 精确版本，`go tool mygo` 与库版本一致。
- Windows 10/11、macOS 12+，或安装 GTK 3 的 Linux。Linux 托盘额外需要 `libayatana-appindicator3` 及支持 indicator 的桌面，使用托盘菜单打开面板，不支持与 Windows/macOS 相同的直接点击交互。
- 本机使用过至少一种受支持的 Agent，才有真实统计数据。

SQLite 通过纯 Go 的 [`github.com/ncruces/go-sqlite3`](https://github.com/ncruces/go-sqlite3) 编译进可执行文件，无需 C 编译器，也不需要任何原生动态库：单个 exe（或 `.app`）就是完整应用。

## 运行、验证与打包

```sh
go run .
go run . --cli --agent claude --days 30
go run . --cli --agent all --by-agent --since 2026-08-20 --until 2026-08-30 --format csv
go run . --cli --help

gofmt -l main.go internal
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build ./...

# 原生打包，不需要 Bun/npm
go tool mygo build
go tool mygo build -platform darwin/arm64,windows/amd64
```

Windows PowerShell 应先执行 `$env:CGO_ENABLED = '0'`，再执行 `go build ./...`，不要直接使用上面的 shell 环境变量赋值语法。Race 检测需 Go 的 race 工具链/C 编译器；正式构建不需要。

输出位于 `build/<os>-<arch>/`。macOS 签名/DMG 必须在 macOS 执行；Windows 安装器需要 NSIS（MyGo 可获取），发布工作流分发只含 `devin-usage-metrics.exe` 的便携 ZIP。自动更新的安装/重启流程仍需原生平台实测后才能发布。

同步服务本来就是独立的 Go module，不改服务端协议：

```sh
(cd sync-server && go test -race ./... && go vet ./...)
```

## 数据与兼容

数据源位置基本与旧版一致：Claude 在 `~/.claude/projects`，Codex 在 `$CODEX_HOME`（默认 `~/.codex`），Antigravity 同时读取桌面端 `~/.gemini/antigravity/conversations` 与 CLI 的 `~/.gemini/antigravity-cli/conversations`，其他默认路径见 [英文数据源表](README.md#data-sources)。支持 WorkBuddy/CodeBuddy 路径别名、Claude 子 Agent 归并和跨文件去重、Codex 累积用量去重，以及只读 SQLite 导入。

Go 磁盘缓存与 Rust 快照分开，TTL 为 5 分钟；强制重新加载绕过缓存，进程内按文件/数据库签名复用解析结果。配置目录继续使用平台目录下的 `devin-usage-metrics/`：Linux `~/.config`、macOS `~/Library/Application Support`、Windows `%APPDATA%`。保留设备 ID、语言偏好、同步设置、加密登录及旧版压缩撤销记录。

## 隐私和网络

- 仅在开启同步后向用户配置的自建 API 发送本地历史，GitHub 仅用于身份验证。
- 实时配额访问对应供应商 API，OAuth 轮换可能写回 Agent 凭证；钥匙串发现需要明确授权。
- 定价从 `models.opencode.ai` 与 Devin 文档刷新，缓存 24h，失败退避 1h。`DEVIN_USAGE_DISABLE_MODELS_FETCH=1` 关闭定价拉取；`DEVIN_USAGE_MODELS_URL` / `DEVIN_USAGE_MODELS_PATH` 可覆盖来源和缓存。
- 打包版本启动 10 秒后及每 4 小时检查一次 GitHub 更新并自动下载（`config.json` 中 `auto_check_updates: false` 可关闭），安装仍需用户确认；ZIP SHA-256 校验用于检测损坏，不代替发布者签名。
- 压缩建议只有明确应用/撤销才修改 Claude/Codex 设置，保留无关内容、拒绝覆盖检测到的外部修改；撤销记录不参与同步。应用后请开启新的 Agent 会话。

## 代码边界

`main.go` 选择 GUI/CLI；`internal/desktop/host.go` 独占窗口/托盘/生命周期，`app.go` 管理状态和后台事件。`internal/` 下按数据、定价、统计、CLI、配额、同步、压缩建议、更新器分包。`mygo.json` 管理打包，`sync-server/` 独立。`src/`、Cargo 文件和 Rust 测试目前仅用于迁移对照。

私有项目，暂未指定开源许可证。
