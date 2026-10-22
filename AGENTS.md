# Agent Usage Metrics

主实现使用 Go 1.27.1+ 与固定版本的 `github.com/egoist/mygo` 原生 UI，不使用 WebView 或 cgo。
迁移只替换技术栈：以 `src/main.rs`、`src/tray.rs` 为布局与视觉依据，保留原版尺寸、配色、信息层级及交互。已确认的有意改动不再回退：用量页为四张卡片（费用、Token 总量、缓存命中与节省、模型 I/O）与带坐标轴、悬停明细的 Token 趋势图，可切换不含缓存读取 / 费用；配色无紫色（`internal/desktop/usage.go`）；侧栏 200px、顶栏 46px，代理为字母标识列表，设备筛选在顶栏下拉胶囊中（`internal/desktop/layout.go`）。
`internal/desktop/host.go` 独占窗口、托盘、focus/close 和应用生命周期；界面内容不持有窗口句柄。
业务状态与后台任务在 `internal/desktop/app.go`；后台通过事件队列交付结果，只在主线程更新界面状态。
数据导入、定价、统计、配额、同步、压缩建议分别在 `internal/` 下的对应包；会话数据库只读。
保留既有设备 ID、配置和同步协议，不在日志/界面/测试输出中暴露账号凭证；压缩建议写入 Agent 设置必须显式确认并可撤销。

检查：`gofmt -l main.go internal`、`go vet ./...`、`go test -race ./...`、`CGO_ENABLED=0 go build ./...`。
同步服务是独立 Go module，另在 `sync-server/` 执行 `go test -race ./...`、`go vet ./...`。
打包：`go tool mygo build`。SQLite 通过 `internal/sqlite`（纯 Go 的 `github.com/ncruces/go-sqlite3`）编译进程序，包里不带原生动态库；不要重新引入 MyGo SQLite 插件或任何 SQLite DLL/dylib。Windows 发布包内的可执行文件保持 Rust 的名字 `devin-usage-metrics.exe`，旧版 Rust 更新器才能升级。
原生 UI 回归：`go test ./internal/desktop`；`AUM_SCREENSHOTS=/absolute/path` 可保存 fixture 渲染截图。

`src/`、`Cargo.toml` 与 Rust 测试暂留作迁移行为对照，不参与 Go 构建/发布。
迁移状态和未验证的兼容性见 `docs/go-migration.md`。不要把交叉编译当作原生平台安装/更新验证。
