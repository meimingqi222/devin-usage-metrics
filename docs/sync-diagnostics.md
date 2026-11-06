# 同步与账号诊断

桌面加载与同步使用同一套 JSONL 日志。macOS 默认位置为
`~/Library/Caches/devin-usage-metrics/sync.log`；设置 `XDG_CACHE_HOME` 时，
位置为 `$XDG_CACHE_HOME/devin-usage-metrics/sync.log`。
单文件上限 4 MiB，另保留一份 `.1` 轮转文件。
Windows 默认位置为 `%LOCALAPPDATA%\devin-usage-metrics\sync.log`。

## 账号数量、身份与切换异常

运行包含新日志的新构建后，刷新配额会自动记录发现、身份匹配、列表生成、查询和界面应用过程。
账号管理右上角的 **诊断日志** 按钮会记录当前界面快照，然后打开日志目录；遇到异常时先点一次，保留问题现场。
账号日志带 `account_diagnostics_version: 1`，相同 `run` 属于同一次操作，`pid` 与构建信息用于区分同时运行的新旧程序。

| 事件 | 排查内容 |
| --- | --- |
| `quota_refresh_start` / `quota_refresh_deferred` / `quota_refresh_end` | 刷新是否执行、等待查询还是等待同步、耗时与待处理请求 |
| `quota_discovered` | 发现的本机登录来源、文件/钥匙串/状态库类型、名称指纹 |
| `account_identity_lookup` | Google 身份查询是否缺 Token、网络失败、HTTP 状态与是否返回邮箱；不记录返回的邮箱 |
| `account_reconcile_start` / `account_reconcile_end` | 身份修复前后的记录数、可疑凭据副本数量 |
| `account_reconcile_decision` | 身份来自凭据、已有副本还是 Google；匹配依靠稳定身份、相同文档、相同 Token、旧邮箱兼容或未经确认的前次账号；记录是否捕获以及活动账号是否改变 |
| `account_reconcile_skipped` / `account_capture_failed` / `account_manifest_save_failed` | 无法读取凭据、不支持的登录形式、同步删除记录或保存失败 |
| `account_profile` / `quota_target` | 保存记录与实际标签逐项对应；`binding=unmatched_live` 表示当前登录尚未绑定到保存记录 |
| `quota_roster` / `quota_roster_anomaly` | 保存数、可见数、标签数、未匹配的当前登录、重复名称组与未显示的保存记录数量 |
| `quota_query_start` / `quota_query_result` | 每个标签是否已开始查询、耗时、脱敏错误码、是否使用缓存、缓存年龄与限流等待时间；`stale_error` 保留缓存回退时的错误类别 |
| `quota_snapshot_applied` / `quota_snapshot_discarded` | 后台结果是否显示，或因账号列表版本变化被丢弃；比较 `revision` 与 `current_revision` |
| `desktop_account_switch_*` / `account_switch_*` | 目标账号指纹、切换模式、镜像写入、结束错误与界面应用；可区分登录回退、读回不一致、凭据冲突等 |
| `account_snapshot` / `account_roster_updated` | 手动现场记录或账号变更后立即生成的列表 |

例如 `saved=3, targets=4, unmatched_live=1, duplicate_labels=1` 表示三个保存记录之外，另有一个未绑定的当前登录与某个标签同名。
对照 `quota_target.label_ref` 与 `account_profile.email_ref`，再看该次 `account_reconcile_decision` 和 `account_identity_lookup`，可以判断为什么没有绑定；不能只按同名自动删除记录。
同一邮箱可能对应不同组织，名称相同只是诊断线索，不是合并依据。

日志不记录邮箱明文、自定义名称、完整账号 ID、Token、Token 指纹、凭据文档、HTTP 正文或任意错误文本。
账号、稳定身份与名称仅保留哈希关联值；HTTP 错误只保留状态码。
每份列表快照最多记录 64 个保存记录和 64 个标签明细，汇总仍计算全部记录，并给出截断数量。
日志只在操作和刷新时写入，不随每一帧渲染写入；耗时阶段每 5 秒记录一次进度。

## 同步耗时

新增记录带 `diagnostics_version: 2`。一次同步从 `desktop_sync_start` 开始，
本地采集与网络同步共用 `run`；后台界面加载另有独立 `run`。
`pid`、`build_revision`、`build_modified`、`go_version` 可用于区分旧实例和构建。
所有耗时字段的单位由后缀标识；阶段会嵌套或并行，不能直接相加。

分析一次慢同步时，按相同 `run` 查看：

| 记录或阶段 | 用途 |
| --- | --- |
| `local_collection` / `collect_agents` | 本地准备数据的整体耗时，和后续网络阶段分开 |
| `agent_queue_result` / `agent_lock_wait` | 等待全局采集并发槽或同 Agent 加载锁的时间 |
| `agent_load_result` | 比较各 Agent 的总耗时、缓存或源读取模式、记录数和错误数 |
| `snapshot_result` | 快照读取、JSON 解码耗时、大小、年龄、范围覆盖，以及命中或失效原因 |
| `database_cache_result` / `database_result` | 数据库签名匹配、DB/WAL 大小，以及完整、增量或未变缓存模式 |
| `devin_scan_plan` | 总会话数、实际扫描会话数、批次数与查询时间范围 |
| `query_result` | 固定查询类别、行数、物化字节数、消费回调耗时和底层读取计数 |
| `file_sources_plan` / `decode_cache_result` | 发现、变化、删除文件数，以及解码缓存命中、解码次数和输入大小 |
| `file_read_result` | 大文件、慢文件或出错文件的解码原因、耗时和大小 |
| `sort_records` / `hash_records` / `write_snapshot` | 解析后的排序、内容哈希和快照写入耗时 |
| `collection_runtime` / `local_collection_result` | 采集前后的进程内存、GC 和 goroutine 数 |
| `desktop_sync_result` / `desktop_sync_applied` | 同步总耗时、结果，以及后台完成到主线程应用之间的等待 |

耗时较长的被监测阶段每 5 秒产生 `stage_progress`，结束后停止。
文件读取只对至少 1 MiB 的解码、至少 100ms 的读取或错误写分文件结果；
其余使用 Agent 汇总，避免逐行或逐小文件写日志。

`query_result.consumer_ms` 包含 Go 行映射及消费回调（例如 JSON 解析），
`read_and_materialize_ms` 是其余查询时间，包含 SQLite 执行、读取和列物化，
不能直接当作纯磁盘 I/O 时间。`main_read_requests` 是主数据库 VFS 请求数，
`main_cache_hits` 是本程序语句内预读缓存命中数，`main_file_reads` 和
`main_file_bytes` 是实际交给主数据库底层文件的读取次数与返回字节数。
它们不含 WAL 读取，也不能说明操作系统页缓存是否命中。
GC/内存数据属于整个进程，不能归为某个 Agent 的独占成本。

日志只写固定事件、计数和脱敏错误码，源路径与设备标识使用哈希关联，
不记录 SQL 文本、SQL 参数、会话标题、正文、原始路径或账号凭证。
日志写入失败不会阻止同步。增加诊断本身并不代表耗时问题已解决；
需要运行包含这些改动的新构建，采集一次慢同步后才能按阶段归因。
