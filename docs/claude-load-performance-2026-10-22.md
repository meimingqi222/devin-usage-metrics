# 2026-10-22 全 agent 导入性能复测

## 方法与边界

比较提交 `e032b36` 与本次工作区；同一台 macOS、Go 工具链、384 MiB Go 软内存上限，同一份只读来源的冻结副本：999 个文件，共 3,664,692,926 字节。SQLite 使用备份副本，原始会话数据库不写入。固定时间窗口 0..4102444800。每种 agent 依次执行强制完整解析、Reload、快照读取，测试不并发。完整解析的应用缓存为空，操作系统文件缓存已预热；这些数据不是系统冷启动、界面渲染或网络同步耗时。单次计时会有波动。

最初基线部分数据库首次打开会生成空 WAL，触发来源变更检测和额外解析；下表使用来源稳定后重跑的基线，不把这部分差异归功于优化。

## 结果（毫秒）

| Agent | 优化前完整加载 | 优化后完整加载 | 提速 | 优化后刷新 | 优化后快照 |
|---|---:|---:|---:|---:|---:|
| devin | 3113 | 2656 | 1.17× | 105 | 49 |
| claude | 4540 | 528 | 8.60× | 15 | 8 |
| codex | 344 | 145 | 2.37× | 16 | 6 |
| antigravity | 429 | 296 | 1.45× | 77 | 27 |
| grok | 1175 | 327 | 3.59× | 13 | 1 |
| zcode | 40 | 32 | 1.25× | 6 | 2 |
| opencode | 1025 | 449 | 2.28× | 46 | 22 |
| pi | 655 | 250 | 2.62× | 62 | 27 |
| mimocode | 122 | 68 | 1.79× | 6 | 2 |
| workbuddy | 658 | 120 | 5.48× | 15 | 6 |

十种 agent、三种加载模式的会话数、轮次数、错误数及排序后的会话/轮次摘要均与基线一致，错误数均为 0。Pi 的 100 字符标题边界差异由比较发现，已修复并加入测试种子。

## 实现

- Claude 跳过 progress 内嵌历史和工具结果的对象构建，只保留统计字段；Codex、Pi、WorkBuddy、Grok、Devin、OpenCode 和 MimoCode 同样按实际读取字段投影。整份 JSON 仍须合法；动态模型用量映射完整保留。
- JSONL 四个解码 worker、最多八个预读位置，结果按原顺序计算；128 MiB 解码缓存按访问顺序淘汰，超大条目不清空其他缓存。
- 文件追加先校验旧内容完整 SHA-256，再从同一文件描述符读取后缀；旧尾行不完整、旧解析错误、历史改写都重新解析。
- SQLite 大 JSON TEXT 在回调内借用读取，避免两次整串复制；所有保留值均拥有独立存储。其他 SQLite 类型保持原行为。
- Antigravity protobuf 只查找目标字段，避免为所有字段构建中间切片。
- SQLite 预读采用 256 KiB × 16，每连接至多 4 MiB。已对比 16 KiB × 64、64 KiB × 64 和 1 MiB × 4；更大单块导致 Devin 退化，未保留。

此前独立 Claude 追加试验约 104–119 ms，最大 128 MiB 源文件的估算解码缓存由约 396 MiB 降至 9 MiB。完整加载十种 agent 会共享缓存预算，超出预算后切回已淘汰历史仍需重新解码。普通刷新在旧版已经有快照复用，本轮主要改善完整加载和追加路径，不宣称所有刷新都同比提速。

## 复现材料

本机冻结输入、基线和结果位于 `/private/tmp/aum-all-agent-perf-20261022/`；`baseline-stable.json` 与 `final.json` 为上述表格数据。临时性能 harness 归档在 `/private/tmp/aum-handoff-bench/`，不作为依赖用户历史的常规单元测试。数据包含本机会话元信息，不纳入仓库。

剩余最慢的是 Devin 大数据库的完整读取；此次测量不能证明所有硬件和历史分布上的理论最优，也不代表 Windows/Linux 原生安装验证。

## 最终验证与交付

`gofmt -l main.go internal`、`git diff --check`、`go vet ./...`、`go test -race ./...`、`CGO_ENABLED=0 go build ./...` 全部通过；sync-server 的 race 测试与 vet 通过。通用 JSON、Claude JSON、protobuf 差异模糊测试分别通过 1,867,474、1,477,916、956,880 次输入。非法 JSON 空白字符回归种子保留在 testdata 中。Agent Notes 验证通过，存在一条既有 Rust 测试锚点警告。

已生成并验证签名的 macOS arm64 0.2.23 应用，CLI help 启动通过：`/private/tmp/aum-optimized-build/darwin-arm64/Agent Usage Metrics.app`。构建期间 Go 尝试写模块缓存被 sandbox 拒绝，但编译、签名与最终产物验证均成功。未替换 Applications 中的旧版本，未进行 Windows/Linux 原生验证，未 commit 或 push。

## Devin 第二轮读取优化（0.2.24）

已在 Devin 导入连接启用 64 KiB × 128 块预读（上限 8 MiB/连接）；其他 agent 保持 256 KiB × 16 块。两份 Devin 数据库相比前版最多增加 8 MiB 缓存。未增加 SQLite 页缓存，也未改变 SQL 查询、顺序、去重、源数据库或同步协议。

使用预编译测试程序交错执行，双方各先预热一次，然后各测三次；前版为 2472/2460/2448 ms，新版为 2588/2151/2169 ms，中位数 **2460 → 2169 ms（下降 11.8%）**。单次有噪声，不能保证每次都更快。较早参数筛选测得 1.85–1.89 秒，最终报告采用实际实现的重复验证结果。消息查询累计文件读取 **14.03 → 3.51 GB（下降约 75%）**，不等于物理磁盘流量。数据与日志保存于上述临时目录的 `verify-base-*.json`、`verify-devin-final-*.json` 和对应缓存日志；`devin-final-all.json` 验证十种 agent 三种模式的所有统计摘要均与基线一致。

64 KiB × 128 的预算低于 64 KiB × 256，性能相当；32 KiB × 256 进一步减少字节却增加读取调用，没有稳定提速。扩大 SQLite 页缓存未显著减少请求，因此不采用。

默认与 Devin 配置均覆盖 WAL/checkpoint、回滚日志写入、语句结束后的缓存失效、短读/边界/错误和溢出页测试。新增散乱读取测试在恢复旧配置后真实失败，恢复新配置后通过。完整 `go test -race ./...`、`go vet ./...`、无 cgo 构建、sync-server race/vet、格式及 diff 检查通过。0.2.24 构建产物位于 `/private/tmp/aum-devin-optimized-build/darwin-arm64/Agent Usage Metrics.app`，未安装、未 commit/push。
