# Devin Usage Metrics Sync API

一个可自托管、无数据库依赖的 v3 同步对象服务。桌面端只需填写本服务的 API 地址并登录 GitHub；服务端按 GitHub 用户 ID 自动隔离数据，因此同一个 GitHub 账号在多台设备登录后会同步到同一份数据。GitHub access token 不会写入磁盘，也不会发送给桌面端。

## 启动

先创建一个 GitHub OAuth App 并记录 **Client ID**。本服务使用 GitHub Device Flow，不需要回调地址或 Client Secret。再生成一个至少 32 字符的随机 `SYNC_AUTH_SECRET`，它用于签发服务自身的同步会话。

```sh
export GITHUB_CLIENT_ID='your-github-oauth-client-id'
export SYNC_AUTH_SECRET='replace-with-a-random-secret-at-least-32-chars'
export SYNC_DATA_DIR=/var/lib/devin-usage-metrics-sync
go run .
```

生产环境必须由反向代理提供 HTTPS，例如 Caddy 或 Nginx。`SYNC_AUTH_SECRET` 等同服务端签名密钥，不能提交到仓库或输出到日志。桌面端把短期同步会话保存在系统钥匙串；会话默认有效 90 天，到期后重新点一次 GitHub 登录即可。

容器部署：

```sh
docker build -t devin-usage-metrics-sync .
docker run -d --restart unless-stopped -p 127.0.0.1:8080:8080 \
  -e GITHUB_CLIENT_ID='your-github-oauth-client-id' \
  -e SYNC_AUTH_SECRET='replace-with-a-random-secret-at-least-32-chars' \
  -v /srv/devin-usage-metrics-sync:/data \
  -e SYNC_DATA_DIR=/data \
  -e SYNC_QUOTA_BYTES=$((2 * 1024 * 1024 * 1024)) \
  devin-usage-metrics-sync
```

## API

登录相关端点无需认证；同步对象端点要求 `Authorization: Bearer <sync-session>`。这个会话由服务端在 GitHub 登录成功后签发，客户端不需要也不能手填令牌。

- `POST /v1/auth/github/device`：启动 GitHub Device Flow，返回浏览器地址和验证码。
- `POST /v1/auth/github/token`：轮询 GitHub 授权结果，成功时返回同步会话和 GitHub 登录名。

- `GET /v1/objects`：列出当前用户的对象名。
- `GET /v1/usage`：返回当前租户的对象数、已用字节、配额和 80% 预警状态。
- `GET` / `HEAD /v1/objects/{name}`：读取对象并返回 SHA-256 `ETag`。
- `PUT /v1/objects/{name}`：原子写入对象；支持 `If-None-Match: *` 与 `If-Match: "etag"`。
- `POST /v1/gc`：立即对当前租户执行一次安全的垃圾回收（日常也会自动执行）。

对象名不能包含路径分隔符，单对象上限 8MiB；v3 客户端实际生成 256–512KiB 的内容寻址 block。默认每个租户的硬配额为 2GiB，80% 起在 usage 中标记预警，写入超过配额会返回 `507`，但 GET/list/GC 保持可用。

GC 每日运行一次：保留当前 head、上一 manifest、最近 7 天的 manifest，以及它们引用的 block；v3 只保留最近 366 天的 block 和 session snapshot，未引用对象还需经过 7 天宽限期才会删除。v1/v2 仅会在**每一个**发现的旧设备都已存在 v3 head，且各自的首次 v3 发布已满 14 天后，才允许按同一宽限期回收；任一设备未迁移、迁移时间未知或不足 14 天都会保留全部旧协议对象。可用 `SYNC_QUOTA_BYTES`（`0` 表示不限额）和 `SYNC_GC_GRACE`（如 `168h`）调整。首期使用目录扫描统计用量和标记对象，避免为小型个人部署引入数据库。

## 桌面端配置

打开「同步设置」，填入 `https://sync.example.com` 这类 API 基础地址，然后点击「登录 GitHub」。浏览器完成授权后，开启同步即可；不要在地址后加 `/v1`，也不需要填写用户名、密码、WebDAV 地址或访问令牌。
