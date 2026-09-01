# Devin Usage Metrics Sync API

一个可自托管、无数据库依赖的 v3 同步对象服务。它不保存账户资料；`SYNC_AUTH_SECRET` 中的每个令牌映射到一个隔离的数据目录。服务端代码与桌面端一起开源，用户可以自行部署和审计。

## 启动

生成高熵令牌，并为每个用户指定一个稳定的目录名：

```sh
export SYNC_AUTH_SECRET='replace-with-a-random-secret-at-least-32-chars'
export SYNC_DATA_DIR=/var/lib/devin-usage-metrics-sync
go run .
```

生产环境必须由反向代理提供 HTTPS，例如 Caddy 或 Nginx。令牌等同密码，不能放在 URL、日志或客户端配置文件；桌面端应存入系统钥匙串。

容器部署：

```sh
docker build -t devin-usage-metrics-sync .
docker run -d --restart unless-stopped -p 127.0.0.1:8080:8080 \
  -e SYNC_AUTH_SECRET='replace-with-a-random-secret-at-least-32-chars' \
  -v /srv/devin-usage-metrics-sync:/data \
  -e SYNC_DATA_DIR=/data \
  -e SYNC_QUOTA_BYTES=$((2 * 1024 * 1024 * 1024)) \
  devin-usage-metrics-sync
```

## API

所有 `/v1/*` 请求都需要 `Authorization: Bearer <token>`。

- `GET /v1/objects`：列出当前用户的对象名。
- `GET /v1/usage`：返回当前租户的对象数、已用字节、配额和 80% 预警状态。
- `GET` / `HEAD /v1/objects/{name}`：读取对象并返回 SHA-256 `ETag`。
- `PUT /v1/objects/{name}`：原子写入对象；支持 `If-None-Match: *` 与 `If-Match: "etag"`。
- `POST /v1/gc`：立即对当前租户执行一次安全的垃圾回收（日常也会自动执行）。

对象名不能包含路径分隔符，单对象上限 8MiB；v3 客户端实际生成 256–512KiB 的内容寻址 block。默认每个租户的硬配额为 2GiB，80% 起在 usage 中标记预警，写入超过配额会返回 `507`，但 GET/list/GC 保持可用。

GC 每日运行一次：保留当前 head、上一 manifest、最近 7 天的 manifest，以及它们引用的 block；v3 只保留最近 366 天的 block 和 session snapshot，未引用对象还需经过 7 天宽限期才会删除。v1/v2 仅会在**每一个**发现的旧设备都已存在 v3 head，且各自的首次 v3 发布已满 14 天后，才允许按同一宽限期回收；任一设备未迁移、迁移时间未知或不足 14 天都会保留全部旧协议对象。可用 `SYNC_QUOTA_BYTES`（`0` 表示不限额）和 `SYNC_GC_GRACE`（如 `168h`）调整。首期使用目录扫描统计用量和标记对象，避免为小型个人部署引入数据库。

## GitHub 登录

首版使用可撤销令牌，适合个人自托管且不需要回调 URL 或 GitHub App 密钥。GitHub OAuth 可以后续作为“换取同一类短期同步令牌”的可选登录层加入；它不会、更不应该把同步对象写入 GitHub 仓库。
