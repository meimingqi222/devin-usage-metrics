# Agent Usage Metrics

主实现使用 Go 1.27.3+ 与固定版本的 `github.com/egoist/mygo` 原生 UI，不使用 WebView 或 cgo。
迁移只替换技术栈：原版 Rust 源码（`src/`、`Cargo.toml`、`examples/`、`tests/*.rs`）已随 0.3.x 移除，布局与视觉依据以 `internal/desktop` 为准；历史实现保留在 git 历史与 `docs/go-migration.md` 中。已确认的有意改动不再回退：用量页为四张卡片（费用、Token 总量、缓存命中与节省、模型 I/O）与带坐标轴、悬停明细的 Token 趋势图，可切换不含缓存读取 / 费用；配色无紫色（`internal/desktop/usage.go`）；UI 图标规范：禁止使用 Emoji 图标，如需图标一律使用矢量 SVG 图标以保证跨平台清晰度与视觉一致性；侧栏 200px、顶栏 46px，代理为字母标识列表，设备筛选在顶栏下拉胶囊中（`internal/desktop/layout.go`）；侧栏在“实时配额”下有“账号”页入口，账号管理、账号切换开关与账号同步只在该页（`internal/desktop/accounts.go` 的 `accountsView`），设置页只保留 GitHub 登录并链接过去；删除类确认用 `dangerConfirm`（红色按钮、默认焦点在取消）。
`internal/desktop/host.go` 独占窗口、托盘、focus/close 和应用生命周期；界面内容不持有窗口句柄。
业务状态与后台任务在 `internal/desktop/app.go`；后台通过事件队列交付结果，只在主线程更新界面状态。
数据导入、定价、统计、配额、同步、压缩建议分别在 `internal/` 下的对应包；会话数据库只读。
保留既有设备 ID、配置和同步协议，不在日志/界面/测试输出中暴露账号凭证；压缩建议写入 Agent 设置必须显式确认并可撤销。

检查：`gofmt -l main.go internal`、`go vet ./...`、`go test -race ./...`、`CGO_ENABLED=0 go build ./...`。
同步服务是独立 Go module，另在 `sync-server/` 执行 `go test -race ./...`、`go vet ./...`。
打包：`go tool mygo build`。SQLite 通过 `internal/sqlite`（纯 Go 的 `github.com/ncruces/go-sqlite3`）编译进程序，包里不带原生动态库；不要重新引入 MyGo SQLite 插件或任何 SQLite DLL/dylib。Windows 发布包使用 `Agent-Usage-Metrics-windows-x64.zip`（资产名不带空格，发布时空格会被替换成点号导致更新器找不到），包内程序为 `Agent Usage Metrics.exe`；同时发布一个字节相同的旧名别名 `devin-usage-metrics-windows-x64.zip`，让只认旧名的老客户端仍能更新。更新器按分隔符归一化匹配资产名，可读取历史包。旧版 Rust 更新器需要手动升级到新命名版本。
原生 UI 回归：`go test ./internal/desktop`；`AUM_SCREENSHOTS=/absolute/path` 可保存 fixture 渲染截图。

项目已完成 Rust → Go 迁移并移除旧代码；`src/`、`Cargo.toml`、`examples/`、`tests/*.rs` 不再保留在仓库中，历史实现以 git 历史与 `docs/go-migration.md` 为准。不要把交叉编译当作原生平台安装/更新验证。
