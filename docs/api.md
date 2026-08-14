# ArkPix Relay Server API 文档

对齐《ArkPix 后端服务设计文档》v1.0 §13 端点清单。所有端点（`/healthz` 除外）需要
`Authorization: Bearer <accessToken>` 鉴权。

## 通用约定（§4）

- 请求/响应体均为 JSON（图片流除外），UTF-8；时间为 Unix 毫秒时间戳。
- 所有响应含 `requestId`（响应体字段 + `X-Request-Id` 响应头）。
- 错误格式统一为：

  ```json
  { "error": { "code": "SYNC_FULL_REQUIRED", "message": "human readable", "requestId": "..." } }
  ```

- 状态码语义：200 成功 / 202 已受理异步处理 / 400 参数错误 / 401 token 缺失或失效 /
  403 无权限 / 404 资源不存在 / 409 需全量重建 / 429 限流（带 `Retry-After` 秒）/ 502 上游不可达。
- 错误码一览：`VALIDATION_FAILED`（400）、`INVALID_TOKEN`（401）、`FORBIDDEN`（403）、
  `NOT_FOUND`（404）、`SENSITIVE_FIELD_REJECTED`（400）、`SYNC_FULL_REQUIRED`（409）、
  `RATE_LIMITED`（429）、`UPSTREAM_UNREACHABLE`（502）、`INTERNAL`（500）。
- 限流（§9）：写端点 60 次/分钟（账号维度，burst 10）、图片端点 300 次/分钟（burst 30）、
  注册端点 10 次/小时（IP 维度，burst 3）；均可经 `RATE_*` 环境变量调整。
- API 响应统一带 `Cache-Control: no-store` 与 `X-Content-Type-Options: nosniff`。

## POST /auth/v1/register

设备注册。**无需鉴权**；独立限流 10 次/小时/IP。

请求：

```json
{ "deviceName": "Mate 60", "inviteCode": "可选", "accountKey": "可选，加入已有账号" }
```

响应 200：

```json
{
  "accessToken": "rl_at_xxx",
  "refreshToken": "rl_rt_xxx",
  "expiresIn": 2592000,
  "accountKey": "rk_xxx",
  "serverVersion": "1.0.0",
  "capabilities": ["relay", "img", "sync", "recover"],
  "requestId": "..."
}
```

- accessToken 30 天、refreshToken 180 天，轮换制；`accountKey` 服务端只存哈希。
- 配置 `INVITE_CODES` 时 inviteCode 不匹配 → 403 `FORBIDDEN`。
- `accountKey` 无效 → 400（不静默新建账号）。

## POST /auth/v1/refresh

刷新服务令牌。**无需 accessToken**，凭 refreshToken 换新对；旧 token 对立即失效。

请求 `{ "refreshToken": "rl_rt_xxx" }` → 响应同上（无 `accountKey`）。
refreshToken 无效/过期 → 401 `INVALID_TOKEN`。

## POST /relay/v1/request

API / OAuth 通用中继（§6.1）。鉴权 + 写限流。

请求：

```json
{
  "method": "GET",
  "url": "https://app-api.pixiv.net/v1/illust/recommended",
  "headers": { "Authorization": "Bearer <pixiv_access_token>", "Accept-Language": "zh-CN" },
  "bodyBase64": "",
  "timeoutMs": 30000
}
```

响应 200：`{ "status": 200, "headers": {"content-type": "..."}, "bodyBase64": "..." }`。

- url 域名白名单：`app-api.pixiv.net`、`oauth.secure.pixiv.net`、`www.pixiv.net`
  （`RELAY_EXTRA_HOSTS` 追加）；白名单外 → 403；非 https → 400。
- 头转发白名单：`Authorization`、`Accept-Language`、`Content-Type`、`User-Agent`、`Referer`。
- body 上限 1 MB（解码后，超限 400）；`timeoutMs` 钳制 ≤ 60 s。
- 上游不可达/超时 → 502 `UPSTREAM_UNREACHABLE`；上游响应体超 1 MB → 502。

## GET /img/v1/fetch?url=&disposition=

图片中继（§6.2）。鉴权 + 图片限流。

- `url` 域名白名单：`i.pximg.net`、`i-f.pximg.net`、`s.pximg.net`（`IMG_EXTRA_HOSTS` 追加）；
  白名单外 → 403；非 https → 400。
- 响应透传图片字节与 `Content-Type`/`Content-Length`/`Cache-Control`；
  额外响应头 `X-Upstream-Status`、`X-Cache: HIT|MISS`；`disposition=inline` 时
  带 `Content-Disposition: inline`。
- 单图上限 50 MB。上游 404 → 404 `NOT_FOUND`；其余失败/超时 → 502 `UPSTREAM_UNREACHABLE`。

## POST /sync/v1/push

同步增量上行（§7.2）。鉴权 + 写限流。

请求：

```json
{
  "domain": "history",
  "baseToken": "st_1718000000000_abcd",
  "items": [
    { "key": "12345678", "data": { "...": "..." }, "updatedAt": 1718000000000, "deleted": false }
  ]
}
```

响应 200：`{ "accepted": 10, "syncToken": "st_...", "conflicts": [] }`。

- domain 六选一：`history` / `search_history` / `bookmark_snapshot` / `mute` /
  `exif_config` / `settings`；未知 → 400。
- 单次单域 ≤ 500 条；LWW 幂等合并（同 key updatedAt 大者胜）；`deleted:true` 为墓碑。
- data 含 `*token*` 字段名 → 400 `SENSITIVE_FIELD_REJECTED`；
  bookmark_snapshot 结构非法（缺 illustId 等）→ 400。
- `baseToken` 与服务端不一致且落后超 90 天保留期 → 409 `SYNC_FULL_REQUIRED`。
- 配置 `DATA_ENC_KEY` 时 data 落库前以 AES-256-GCM 加密（§9）。

## GET /sync/v1/pull?domain=&since=&limit=

同步增量下行（§7.2）。鉴权。

- `since` 为空 = 全量；否则传上次 syncToken 增量拉取；无效/过旧 → 409 `SYNC_FULL_REQUIRED`。
- `limit` 1..100，默认 30。
- 响应 200：`{ "items": [{key,data,updatedAt,deleted}], "syncToken": "st_...", "hasMore": false }`；
  `hasMore:true` 时用返回的 syncToken 作为下一次 `since` 续页。

## GET /recover/v1/illust/{pid}

已删除作品恢复查询（§8.1）。鉴权 + 写限流。`pid` 须纯数字（否则 400）。

- 200 已就绪：`{ "status": "ready", "pages": [{"page":0,"url":"{base}/img/v1/fetch?url=...","width":w,"height":h}], "source": "pixiv_cat", "meta": {...} }`
- 202 抓取中：`{ "status": "fetching", "retryAfterSec": 5 }`（客户端按间隔轮询，最多 30 次）
- 404 全部源均无：`{ "status": "not_found" }`（负缓存 7 天，过期后可重试）

## GET /healthz

存活探针，**无需鉴权**。响应 200 `{ "status": "ok" }`。

## Web 托管与 CORS（§6.5）

- 未被 API 路由命中的 GET 路径由 SPA 静态托管处理：
  命中静态资源按扩展名 Content-Type 返回（`Cache-Control: public, max-age=86400`），
  未命中回退 `index.html`（`Cache-Control: no-cache`，前端路由）。
  静态资源默认来自二进制内嵌产物；`WEB_DIR` 非空时改从磁盘目录服务。
- `CORS_ORIGINS` 逗号分隔白名单；空 = 完全关闭跨域。开启后仅对白名单内 Origin
  在 API 路径上回显 `Access-Control-Allow-Origin` 并应答 preflight
  （允许 `Authorization, Content-Type` 头，`Access-Control-Max-Age: 86400`）。
