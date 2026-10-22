# 同步耗时诊断

桌面加载与同步使用同一套 JSONL 日志。macOS 默认位置为
`~/Library/Caches/devin-usage-metrics/sync.log`；设置 `XDG_CACHE_HOME` 时，
位置为 `$XDG_CACHE_HOME/devin-usage-metrics/sync.log`。
单文件上限 4 MiB，另保留一份 `.1` 轮转文件。

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
