# Go / MyGo 迁移验收记录（2026-10-22）

当前保持 draft PR：[PR #1](https://github.com/meimingqi222/devin-usage-metrics/pull/1)。此记录区分自动化验证、原生冒烟和安装更新验收；不据此发布正式版本。

## 真实历史对比

在 macOS arm64 上，从当前保留的 Rust 源码重新构建参考程序。Go 使用包含本次 Grok / WorkBuddy 修复的源码。原始记录来自 983 个文件，共 3,546,648,856 字节；SQLite 使用只读连接和 backup API 冻结，JSONL 与 Grok summary 复制到权限为 0700 的临时目录。没有复制登录凭证，没有将原始记录、会话 ID、账号、模型清单或费用金额提交到仓库。

固定窗口为 2020-01-01 至 2026-10-21（含结束日），时区 Asia/Taipei，关闭两版在线价格刷新，使用各自当前内嵌价格表。每个 Agent 运行强制全量、缓存命中、缓存过期后新进程读取三种路径。

| Agent | 会话数 | turn 数 | 三种路径 |
| --- | ---: | ---: | --- |
| Claude | 16 | 2,496 | 一致 |
| Codex | 155 | 1,986 | 一致 |
| Devin | 133 | 22,156 | 一致 |
| Antigravity | 57 | 15,072 | 一致 |
| Grok | 111 | 333 | 一致 |
| ZCode | 67 | 1,122 | 一致 |
| OpenCode | 260 | 11,398 | 一致 |
| Pi | 117 | 13,371 | 一致 |
| MimoCode | 30 | 1,075 | 一致 |
| WorkBuddy | 139 | 2,341 | 一致 |

30/30 组通过。比较逐日日期、模型集合、五类 token 汇总及费用，token 精确相等，费用容差为 CSV 最末位的 2 倍（0.00000002 USD）；另比较解析缓存中的会话、turn 和错误数。每个来源的采集错误数均为 0；两版相同的缺失价格警告仍存在，不能把相等的估算当作实际账单。报告没有验证每条会话元数据字段。新增数据后的增量行为另由现有数据库/JSONL 回归测试覆盖，本次真实快照不变更历史内容。

首轮只比较汇总会漏过空 turn 和元数据会话。加入记录数检查后，发现并修复：Grok 缺失可选更新日志、没有 usage 的完成事件；WorkBuddy 每行的缺省 session ID。解析缓存版本由 1 升至 2。回归依据见对应 Agent Note。

安全摘要：`docs/agent-notes-evidence/go-real-history-summary.json`。原始 CSV、日志和快照只在本机临时目录中保留。重现命令（Python 3.11+，当前脚本支持 macOS 路径）：

```sh
cargo build --bin devin-usage-metrics
go build -o /private/tmp/aum-migration-go .
python3 scripts/compare-migration.py \
  --snapshot --root /private/tmp/aum-history-new \
  --rust target/debug/devin-usage-metrics \
  --go /private/tmp/aum-migration-go --until 2026-10-21
```

同一快照重跑时省略 `--snapshot`；不要把 `--root` 指向真实 HOME。单个进程限时 30 分钟，进度逐项输出，摘要每项落盘。

## Rust / Go 同步互通

`examples/migration_sync.rs` 编译原 Rust 同步实现；`internal/syncclient/interop_test.go` 启动实际 Go sync-server 进程。使用 loopback HTTP、随机测试签名密钥、独立设备 ID、独立 HOME 和临时存储，不接入线上账号。

已通过 Rust→Go、Go→Rust、双方新增 turn 后再同步、清空 Go 内存缓存后重读。逐字段比较 fixture 会话和 turn，覆盖十个当前 Agent 及旧 Amp 数据、中文、Unicode 分隔符、浮点用量和记录费用。该项是可选验收测试，普通 CI 会明确跳过，不应把普通测试绿灯当作已执行互通。

```sh
cargo build --example migration_sync
(cd sync-server && go build -o /private/tmp/aum-migration-sync-server .)
AUM_RUST_SYNC_HELPER="$PWD/target/debug/examples/migration_sync" \
AUM_SYNC_SERVER_BINARY=/private/tmp/aum-migration-sync-server \
go test -race ./internal/syncclient -run '^TestRustGoLiveInterop$' -count=1 -v
```

v1/v2 回退、损坏对象、部分设备失败和 CAS 冲突仍由独立协议 fixture 测试覆盖；本项不声称已在部署中的服务上完成所有故障注入。同设备同时启动 Rust 和 Go 的重编码开销仍存在，应避免长期并行运行。

## 原生与发布门槛

macOS arm64 已通过本地原生打包、codesign 校验、启动和配额页冒烟，确认额外的 Devin 添加入口消失。验收时使用已有授权状态，没有覆盖首次钥匙串授权或拒绝。候选进程已退出，没有替换 `/Applications` 的既有 Rust 安装。

CI 与 Release 共用 `scripts/package-release.py`。Windows ZIP 内仅有 `devin-usage-metrics.exe`；macOS ZIP 保留完整 `.app`、权限和签名资源。CI 上传 ZIP 和 SHA-256，14 天保留，提交和运行可追溯；Release 流程仍只应在正式验收后触发。macOS 当前为 ad-hoc 签名，codesign 通过不等于 Developer ID 公证或 Gatekeeper 首装通过。

以下各项仍需真实打包版本完成，不能由交叉编译或单元测试替代：

| 平台 | 待验收 |
| --- | --- |
| Windows x64 | 全新安装/解压启动，Antigravity 真实账号发现，旧版 Rust 0.2.11 更新到 Go，Go 更新到下一候选版本，退出替换与重启，失败恢复，凭证文件 ACL |
| macOS arm64 | 从下载包安装并启动，首次钥匙串允许/拒绝，Rust 0.2.11→Go、Go→下一候选版本的下载/校验/替换/重启，失败恢复，设备 ID/配置保留 |
| 两平台 | 托盘驻留/重新激活、第二实例、跳过版本及自动检查偏好；每项记录 OS、旧/新版本、包 SHA-256 和结果 |

PR 验收 ZIP 沿用仓库版本 0.2.11，不能因此声称测试过 0.2.11 的“发现新版本”路径。CI 支持手动 `workflow_dispatch`，通过 `package_version` 构建具有更高版本号的候选包（例如 0.2.12 / 0.2.13），只上传 Actions artifact，不创建 Release。跨版本测试仍需要隔离的更新元数据/测试环境。不要为了让旧更新器发现候选包而把未经验收的版本发布到稳定 `/releases/latest`。

MyGo 固定在 `v0.5.6-0.20261020022821-633b369dc388`，不浮动跟踪分支。尚未正式发布的提交需要作为迁移维护成本保留：依赖升级单独提交并重跑三平台 UI/打包检查；回退时使用已验证的完整旧应用包，不能假定降级依赖仍能编译现有 UI API。

在安装更新链路验收前，PR 维持 draft，不合并、不打正式标签。
