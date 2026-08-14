# ArkPix Relay Server — 开发计划

版本：v1.1（技术栈定稿 Go）
基于《ArkPix 后端服务设计文档》v1.0（docs/backend-design.md，含 §6.4 HDD 优化、§6.5 Web 客户端支持）
技术栈：Go 1.24+（标准库 net/http ServeMux）+ SQLite（WAL，纯 Go 驱动）
部署目标：NAS / 小 VPS（低配置自托管，机械硬盘友好）

---

## 0. 决策记录

| 决策点 | 结论 | 理由 |
|---|---|---|
| 语言/框架 | Go 1.24+，标准库 `net/http` + `ServeMux`（方法+路径通配路由），不引 Web 框架 | 中继/代理是标准库主场；单静态二进制交付，镜像 ~20 MB、内存 20–40 MB，适合 NAS/小 VPS |
| Web 托管 | `embed.FS` 将 SPA 静态资源编译进二进制，同源托管（§6.5） | 一个二进制 = API + 网页，免 nginx |
| 数据库 | SQLite（`modernc.org/sqlite`，纯 Go 无 cgo），WAL 模式，`synchronous=NORMAL` | 零外部依赖；纯 Go 驱动保证静态编译与交叉编译不受影响 |
| 机械硬盘约束 | DB 只存结构化小数据，**绝不存图片 blob**；图片缓存落文件系统，流式读写 | 避免大量随机 IO 拖垮 HDD（§6.4） |
| 图片缓存 | 磁盘 LRU（键 = URL SHA-256），分目录存储 + 元数据入 DB，LRU 淘汰按 DB 内 atime | 顺序大文件流式读写，对 HDD 友好 |
| 依赖策略 | 尽量标准库：`log/slog`（日志+脱敏）、`crypto/*`、`embed`；仅引入 `modernc.org/sqlite`、`golang.org/x/time`（限流） | 少依赖 = 少供应链风险，静态编译干净 |
| 上行出口代理 | `UPSTREAM_PROXY` 环境变量，`http.Transport.Proxy` 出网 | 设计文档 §10 |
| 多用户扩展 | 存储层用仓储接口（`internal/storage`），后续可加 PostgreSQL 适配器 | 设计文档 §10 |
| 测试 | `go test` + `net/http/httptest`；集成测试用临时目录；缓存压测用 `go test -bench` 在 HDD 上跑 | 验收需覆盖 HDD 场景 |

### 机械硬盘专项策略（贯穿所有任务，对齐设计文档 §6.4）
- SQLite：WAL + `synchronous=NORMAL`，写事务合并（同步写入分批），`PRAGMA cache_size` 适度；每域 token 游标常驻内存
- 图片缓存：写临时文件（同卷）→ `os.Rename` 原子落盘；读走 `io.Copy` 流式管道；LRU 扫描只在启动/水位阈值时触发，命中路径不 `stat`
- 缓存按哈希前缀两级分目录（`CACHE_LAYOUT=sharded`，默认）；淘汰批量限速（`CACHE_EVICTION_BATCH`）
- 恢复抓取临时文件放独立临时目录，避免与 DB 文件同盘竞争随机 IO
- 所有大文件传输全程流式，禁止整文件读入内存（`io.ReadAll` 仅限 ≤1 MB 的 API body）

---

## 1. 里程碑总览

| 阶段 | 名称 | 产出 | 验收标准 |
|---|---|---|---|
| M0 | 项目初始化 | Go module、CI、lint、测试骨架 | `go run ./cmd/server` 起服务，healthz 端点 200 |
| M1 | 通用协议基础设施 | requestId、错误格式、分页、限流、日志脱敏 | 所有响应符合 §4 协议格式 |
| M2 | 服务认证 /auth | register / refresh / 中间件 | 设备注册→token 鉴权闭环 |
| M3 | 网络中继 /relay | API/OAuth 中继 | 白名单+头过滤+超时钳制生效 |
| M4 | 图片中继 /img | 图片 fetch + 磁盘 LRU 缓存 | 命中/未命中/上游失败映射正确 |
| M5 | 数据同步 /sync | 6 个域 LWW、游标、墓碑 | push/pull/冲突/全量重建闭环 |
| M6 | 删除图恢复 /recover | 异步队列、多源探测、负缓存 | 202→轮询→200/404 流程 |
| M7 | 部署形态 | Docker、UPSTREAM_PROXY、Web 托管 | 一键起容器，机械盘上跑缓存压测 |
| M8 | 集成与验收 | 全链路测试、压测报告 | 对照 §13 端点清单全部可用 |

---

## 2. 目录结构（目标）

```
pix_backend/
├── PLAN.md
├── go.mod  go.sum  .env.example
├── cmd/
│   └── server/
│       └── main.go            # 启动入口
├── internal/
│   ├── app/
│   │   └── app.go             # ServeMux 装配、中间件链、路由注册
│   ├── config/
│   │   └── config.go          # 环境变量解析与校验
│   ├── db/
│   │   ├── db.go              # SQLite 连接（WAL/NORMAL）
│   │   ├── migrate.go         # 迁移执行器
│   │   └── migrations/*.sql
│   ├── common/
│   │   ├── errors.go          # 统一错误格式 {error:{code,message,requestId}}
│   │   ├── requestid.go       # requestId 生成/注入
│   │   ├── pagination.go      # cursor 分页
│   │   ├── ratelimit.go       # x/time/rate 令牌桶
│   │   └── logger.go          # slog + Authorization 头脱敏 handler
│   ├── auth/
│   │   ├── routes.go  service.go  middleware.go  tokens.go
│   ├── relay/
│   │   ├── routes.go  service.go   # 上游请求白名单/过滤/钳制
│   ├── img/
│   │   ├── routes.go  service.go
│   ├── cache/
│   │   └── disklru.go         # 磁盘 LRU 缓存（流式）
│   ├── sync/
│   │   ├── routes.go  service.go  token.go  domains.go
│   ├── recover/
│   │   ├── routes.go  service.go  queue.go
│   │   └── sources/
│   │       ├── sources.go  snapshot.go  mirror.go
│   ├── storage/
│   │   └── repository.go      # 仓储接口（SQLite 实现，预留 Postgres）
│   └── web/
│       ├── embed.go           # go:embed SPA 静态资源（§6.5）
│       └── dist/              # 前端构建产物拷入此处（独立工程产出）
├── docker/
│   ├── Dockerfile  docker-compose.yml
└── docs/
    └── api.md                 # 端点文档
```

---

## 3. M0 项目初始化

- [x] 任务说明：空仓库，初始化 git 已存在
- [x] `go mod init`（module `github.com/arkpix/relay`）；依赖 `modernc.org/sqlite`、`golang.org/x/time` 推迟到首个使用它的里程碑引入（避免 `go mod tidy` 清除未用依赖）
- [x] 工具链：go1.26.0 便携版置于 `.toolchain/`（已 gitignore，校验 sha256）；`go vet` + `golangci-lint` 配置（`.golangci.yml`）
- [x] 目录骨架 + `cmd/server/main.go`（slog JSON 日志 + 优雅退出）+ `GET /healthz`（含 httptest 用例）
- [x] CI（GitHub Actions：golangci-lint + vet + test + `CGO_ENABLED=0` linux 静态编译验证）；`.env.example` 模板（含 §6.4 HDD 项与 §10 全集）
- M0 验收：`go vet` / `go test` / 静态编译全过；`PORT=18080` 起服务，`GET /healthz` 返回 200 `{"status":"ok"}` ✓

---

## 4. M1 通用协议基础设施（§4）

- [x] `requestId`：响应头 `X-Request-Id` + 响应体 `requestId`；`common.RequestID` 中间件生成（可沿用客户端传入值），注入上下文（`internal/common/requestid.go`）
- [x] 错误处理：统一 `{error:{code,message,requestId}}`（`errors.go` + `response.go`）；非 `*APIError` 降级 500 INTERNAL 且细节不外泄；状态码语义对齐 §4.2
- [x] 分页：cursor 不透明（base64url `时间戳:id`），`limit` 1..100 钳制默认 30（`pagination.go`）
- [x] 限流：`common.Limiter`（`x/time/rate` 令牌桶，按 key 独立桶 + 惰性回收），429 + `Retry-After` + `RATE_LIMITED` 错误体；配额参数（写 60/min、图 300/min、注册 10/h/IP）在对应里程碑挂载
- [x] 日志脱敏：`common.RedactHandler`（slog 包装，authorization/token/password/body 等键含嵌套 group 一律 `[REDACTED]`）；`AccessLog` 只记 method+path+status+耗时，不记 query/请求头/body
- [x] 校验：`DecodeJSON`（1MB 上限）+ `Required` 必填字段（`validate.go`），400 返回明确 message
- [x] 验收：`TestWriteError_Unauthorized` 断言 401 + error.code=INVALID_TOKEN + requestId 与响应头一致；vet/test/静态编译全过（x/time v0.15.0 将 go 指令提升到 1.25）

---

## 5. M2 服务认证 /auth（§5）

- [x] 建表 `accounts`（id、account_key 哈希、created_at）与 `devices`：id、account_id、device_name、invite_code、access_token、refresh_token、created_at、expires_at（`internal/db/migrations/0001_init.sql`；`internal/db`：WAL/NORMAL + foreign_keys + busy_timeout=5s 连接，`embed.FS` 迁移执行器按文件名序执行、`schema_migrations` 表防重）
- [x] token 方案：accessToken（30d）与 refreshToken（180d），`crypto/rand` 32B 随机不透明串（前缀 `rl_at_`/`rl_rt_`/`rk_`），DB 存储 SHA-256 十六进制哈希，轮换制（refresh 后旧 access+refresh 立即失效，`auth/tokens.go` + `service.go`）
- [x] `POST /auth/v1/register`：可选 inviteCode 校验（`INVITE_CODES` 配置，空则跳过，不匹配 403）；可选 `accountKey` 加入已有账号（无效按 400 拒绝，不静默新建），响应含 `accountKey`、`serverVersion:"1.0.0"`、`capabilities:["relay","img","sync","recover"]`（§12）；注册端点独立限流 10 次/小时/IP（burst 3，`common.NewLimiter(10.0/60, 3)` + `common.ClientIP`，§9）
- [x] `POST /auth/v1/refresh`：refreshToken 换新对，旧 refreshToken 单次使用（**偏差**：响应不含 accountKey——DB 只存哈希无法还原明文，客户端注册时已持有且账号内恒定，待与设计文档 §5.1 对齐）
- [x] 鉴权中间件 `auth/middleware.go`：`Authorization: Bearer` 解析 → 查库校验未过期 → accountID/deviceID 注入 context，失败 401 INVALID_TOKEN；预置 token 模式（`STATIC_TOKENS`）映射到内置共享账号行（哨兵哈希，无明文可导出）直接通过
- [x] 验收：注册→访问探针→refresh→旧 token 401 全链路测试；accountKey 加入已有账号 → 两设备同 account_id（DB 断言）；inviteCode 403/200、STATIC_TOKENS 直通、缺 Authorization 头 401 统一错误格式全过；config 扩展 DATA_DIR/DB_PATH/INVITE_CODES/STATIC_TOKENS；`CGO_ENABLED=0` 静态编译通过；PORT=18081 冒烟 register→refresh→旧 refresh 401 实测 ✓

---

## 6. M3 网络中继 /relay（§6.1）

- [x] `POST /relay/v1/request`（`internal/relay/routes.go` + `service.go`；挂 auth 鉴权中间件 + 写端点限流 `common.NewLimiter(60, 10)`，key = accountID，§9）：
  - 域名白名单：`app-api.pixiv.net`、`oauth.secure.pixiv.net`、`www.pixiv.net`，`RELAY_EXTRA_HOSTS` 可追加（config 扩展）；不在白名单 → 403 FORBIDDEN
  - 头白名单：`authorization`、`accept-language`、`content-type`、`user-agent`、`referer`（大小写不敏感）；其余丢弃
  - bodyBase64 上限 1 MB（解码后校验，超限 400）；`timeoutMs` 钳制 ≤ 60 s、缺省 30 s（`clampTimeout`）
  - 客户端未传 UA 时注入 `PixivIOSApp/5.8.0`
  - `http.Client` + 自定义 `Transport` 出网（`NewUpstreamClient`），`UPSTREAM_PROXY` 经 `Transport.Proxy` 生效
  - **偏差/收紧**：url scheme 强制 https，否则 400（§4.1 全程 HTTPS；防降级为明文出网）
- [x] 响应透传：status、白名单内响应头（`content-type`/`cache-control`/`retry-after`，小写键）、bodyBase64；响应体流式读但限 1 MB（`io.LimitReader` 多读 1 字节判定），超限 502
- [x] 日志：仅 method + host + 状态码 + 耗时 + requestId，绝不落 Authorization 与 body（§6.1；上游 client.Do 的 err 含 URL query，也不进日志）
- [x] 错误映射：上游不可达/超时 → 502 UPSTREAM_UNREACHABLE；域名 403 / 参数 400
- [x] 验收：httptest mock 上游（`rewriteTransport` 把白名单域名改写到 httptest server，service 注入 `*http.Client`）；覆盖 GET/POST 透传、头过滤、UA 注入/透传、域名 403、method/scheme 400、body 超 1MB 400、超时钳制单测 + 慢上游超时 502、上游不可达 502（统一错误体 + requestId 头体一致）、响应超 1MB 502、未鉴权 401、限流 429（burst 10）、日志无敏感字段；`RELAY_EXTRA_HOSTS` 追加域名生效；vet/test/静态编译全过

---

## 7. M4 图片中继 /img（§6.2）

- [x] `GET /img/v1/fetch?url=&disposition=`（`internal/img/routes.go` + `service.go`；挂 auth 鉴权中间件 + 图片限流 `common.NewLimiter(300, 30)`，key = accountID，§9）：
  - 域名白名单：`i.pximg.net`、`i-f.pximg.net`、`s.pximg.net` + `IMG_EXTRA_HOSTS` 可配追加（§6.2，恢复模块源域名必须在此声明）；非白名单 403；强制 https（沿用 M3 收紧）
  - 服务端注入 `Referer: https://app-api.pixiv.net/` + `User-Agent: PixivIOSApp/5.8.0`；出网 client 复用 `relay.NewUpstreamClient`（UPSTREAM_PROXY 生效）
  - 响应透传 `Content-Type`、`Content-Length`、`Cache-Control`；加 `X-Upstream-Status`、`X-Cache: HIT/MISS`；`disposition=inline` 时设 `Content-Disposition: inline`
- [x] 磁盘 LRU 缓存 `internal/cache/disklru.go`（§6.4）：
  - 键 = URL SHA-256 hex；`CACHE_LAYOUT=sharded`（默认）两级分目录 `<dir>/ab/cd/<hash>`，`flat` 直落；元数据存 DB `cache_meta`（迁移 `0002_cache_meta.sql`：key/size/atime/created_at/content_type），LRU 按 DB 内 atime
  - 写：同卷 `CACHE_TMP_DIR` 临时文件流式下载（`cache.Writer` 配合 `io.TeeReader` 边下边存，单趟流式；限 `MaxFileBytes` 默认 50 MB）→ 校验大小 → `os.Rename` 原子落盘（Windows 瞬时锁退避重试 5 次）；超限/写盘失败降级透传不缓存、不报错码
  - 读：`os.Open` + `io.Copy` 管道直出，命中刷新 DB atime；命中路径不 `stat` 多余元信息
  - 淘汰：启动时 + 写后总量超 `CACHE_HIGH_WATERMARK`（默认 0.9 × `CACHE_MAX_BYTES` 20 GB）触发；按 atime 升序批删 `CACHE_EVICTION_BATCH`（默认 500），批间隔 100ms；`evictMu` 串行化淘汰（后台触发 TryLock、启动/测试阻塞式 `Evict`）；Windows 上文件被读者占用时跳过该条留待下轮
  - 并发同 key 去重（img 层 flight：leader 出网，follower 等待后重查缓存）；导出 `Key/Get/Put` 供 M6 恢复模块复用
- [x] 失败映射：上游 404 → 404 NOT_FOUND（带 X-Upstream-Status）；其余非 200 / 超时 / 连接失败 → 502 UPSTREAM_UNREACHABLE；磁盘写满降级直连（不返回 507，§4.2）
- [x] config 扩展：`CACHE_DIR`/`CACHE_TMP_DIR`/`CACHE_MAX_BYTES`/`CACHE_LAYOUT`/`CACHE_HIGH_WATERMARK`/`CACHE_EVICTION_BATCH`/`IMG_EXTRA_HOSTS`（名称与 `.env.example` 一致，零值走 cache 包默认值）
- [x] 验收：httptest mock 上游 + t.TempDir() 缓存目录覆盖 MISS→落盘→HIT（字节一致、上游计数 1）、404/500 映射不入缓存、超上限（已知长度直连 / chunked 未知长度 Writer 降级）透传不缓存、非白名单 403、http scheme 400、无鉴权 401、X-Cache/X-Upstream-Status 头、并发同 key 仅出网一次、disposition=inline；cache 包覆盖 sharded/flat 路径、tmp 同卷前缀、LRU 淘汰（文件+DB 同清）、启动淘汰、atime 刷新、并发同 key Put；日志仅 host+状态+命中+耗时不落 URL；vet/gofmt/test/静态编译全过；PORT=18082 冒烟 401/403/400 ✓。**环境限制**：本机无 gcc，`-race`（需 cgo）未能执行，以 `-count=5` 重复并发用例替代；HDD 压测（`go test -bench`）并入 M8 验收

---

## 8. M5 数据同步 /sync（§7）

- [x] 建表：`sync_domains`（account_id、domain、sync_token、latest_updated_at，PK(account_id,domain)）、`sync_entries`（account_id、domain、key、data 原始 JSON 文本、updated_at、deleted、seq，PK(account_id,domain,key) + 索引 (account_id,domain,seq)）（迁移 `0003_sync.sql`）。**偏差**：未建 PLAN 中的 `sync_log` 审计表——seq 与 token 数值同源（见下），pull 游标无需额外映射；"tombstone 时间"由 seq 兼任
- [x] syncToken 生成（`token.go`）：`st_<seq>_<6位随机hex>`，seq 按（账号×域）单调递增（`max(上一水位+1, 当前毫秒)` 起分配，条目各占一个 seq）；内存 map 常驻，启动时从 sync_domains 恢复；push 临界区由 `Service.pushMu` 串行化，并发下同域 token 严格单调
- [x] `POST /sync/v1/push`（`routes.go` + `service.go`；auth 鉴权 + 写限流 `common.NewLimiter(60, 10)` key=accountID）：
  - 单域单次 ≤ 500 条，超限 400；domain 必须 6 域之一，否则 400
  - `baseToken` 与当前不一致且其时间戳落后超 90 天保留期 → `409 SYNC_FULL_REQUIRED`；不一致但在保留期内正常接受（服务端权威，LWW 幂等合并容忍旧 baseToken）
  - 幂等 LWW：同 key 已有 updated_at >= 传入 updatedAt 跳过（accepted 不计）；否则 upsert（含墓碑覆盖）
- [x] `GET /sync/v1/pull`：`since` 空串全量，否则解析 token 得 seq 水位增量拉取（`seq > sinceMs`）；无效/过旧超 90 天 → 409 SYNC_FULL_REQUIRED；`limit` 走 `common.ParseLimit`；响应 `{items, syncToken, hasMore}`，含墓碑 `deleted:true`；hasMore 时返回末条 seq 派生的续页 token（格式同源）
- [x] 域冲突策略 `domains.go`（§7.1）：6 域统一按 key LWW（mute 净效果集合并集）
- [x] bookmark_snapshot 结构校验（§7.3）：非墓碑条目 illustId 必填（数字）、imageUrls 可缺省/可空数组但元素须全 string
- [x] 敏感字段排除：客户端侧职责（文档 §7.2），服务端递归拒写含 `token`（大小写不敏感）的字段名 → 400 `SENSITIVE_FIELD_REJECTED`
- [x] 墓碑保留 90 天：**惰性清理**策略——push 时顺手删该域 `deleted=1 AND seq < now-90d` 的过期墓碑（用服务端 seq 而非客户端可控的 updated_at 判定，防伪造绕过）；pull 不过滤
- [x] 验收：push→pull 闭环（同 accountKey 两设备交叉）、LWW（旧写跳过/墓碑覆盖/重复幂等）、多设备增量、baseToken 过旧与 since 无效/过旧 409、墓碑传播与过期清理（直接改 DB seq，不依赖真实时间流逝）、501 条/非法域/敏感字段/快照结构 400、token 单调（含 8 goroutine 并发）、账号隔离、无鉴权 401、翻页续拉不丢不重；`go vet`/`gofmt`/`go test ./...`（sync 包 `-count=3`）/ `CGO_ENABLED=0` 静态编译全过。**环境限制**：同 M4，本机无 gcc，`-race` 以 `-count=3` 重复并发用例替代

---

## 9. M6 删除图恢复 /recover（§8）

- [ ] 建表 `recover_cache`：account_id、pid、pages(JSON)、source、meta、status、expire；负缓存 7 天、正缓存 TTL 90 天
- [ ] `GET /recover/v1/illust/{pid}`：命中 200 `{status:"ready",pages,...}`；未命中入队 202 `{status:"fetching",retryAfterSec}`；负缓存/全失败 404 `{status:"not_found"}`
- [ ] 异步队列 `queue.go`：goroutine + 带缓冲 channel 信号量，全局并发 ≤ 8、每源 ≤ 2、单源探测间隔 ≥ 1 s（`x/time/rate` 限速器）
- [ ] 数据源插件 `sources/`：
  - `snapshot.go`：客户端 push 的快照中 `imageUrls`（可能仍在 CDN 存活期）
  - `mirror.go`：pixiv.cat / pixiv.re 等，`<pid>.jpg`、`<pid>_p<N>.jpg` 探测 + 内容校验（Content-Type/尺寸）
  - 可配置源列表与优先级；源域名须同步加入 `IMG_EXTRA_HOSTS`（§6.2）
- [ ] 抓取成功 → 图片落 `/img` 缓存（复用 M4），`pages[].url` 包装为 `{relay}/img/v1/fetch?url=<第三方源>`（§8.1）
- [ ] 恢复产物按账号隔离（`account_id`），配置项可放开共享；`/img` 磁盘缓存跨用户共享（§8.2 可见性分层）
- [ ] 验收：mock 镜像源，断言探测顺序、并发限制、轮询状态机 202→200/404、负缓存过期重试

---

## 10. M7 部署形态（§10）

- [ ] 环境变量全集（`.env.example`）：端口、DB 路径、缓存目录/上限、`CACHE_LAYOUT`、`CACHE_TMP_DIR`、`CACHE_HIGH_WATERMARK`、`CACHE_EVICTION_BATCH`、INVITE_CODES、STATIC_TOKENS、UPSTREAM_PROXY、限流参数、恢复源列表/优先级、TTL、CORS_ORIGINS、WEB_DIR（Web 前端静态资源目录，默认使用 `embed.FS` 内嵌产物）
- [ ] `docker/Dockerfile`（multi-stage：`golang` 构建 `CGO_ENABLED=0` 静态二进制 → `scratch`/`distroless` 运行镜像 ~20 MB）+ `docker-compose.yml`（单容器，卷挂 DB+缓存）
- [ ] Web 前端静态托管（§6.5）：`internal/web/embed.go` 用 `go:embed` 内嵌 SPA，同源托管；`CORS_ORIGINS` 白名单默认关闭；Web 前端为独立工程，不在本仓库
- [ ] 服务端静态加密同步数据（AES-256-GCM，密钥 `DATA_ENC_KEY` 由部署方管理，§9）——DB 落盘前加密敏感列
- [ ] 文档 `docs/api.md`：端点汇总（对齐 §13）

---

## 11. M8 集成与验收

- [ ] 集成测试：按 §13 端点清单逐端点点对点（httptest）
- [ ] 压测：HDD 环境 `go test -bench` 跑图片中继缓存读写、同步批量 push，输出吞吐/延迟/磁盘 IO 报告
- [ ] 安全自查：日志无敏感字段、token/accountKey 不落库明文、限流生效、目录穿越防护（恢复 pid 与缓存键校验）
- [ ] 交付：`PLAN.md` 打勾核对 + 验收报告

---

## 12. 验收对照（§13 端点清单）

| 方法 | 路径 | 状态 |
|---|---|---|
| POST | /auth/v1/register | M2 |
| POST | /auth/v1/refresh | M2 |
| POST | /relay/v1/request | M3 |
| GET | /img/v1/fetch | M4 |
| POST | /sync/v1/push | M5 |
| GET | /sync/v1/pull | M5 |
| GET | /recover/v1/illust/{pid} | M6 |
| GET | /healthz | M0 |

---

## 13. 风险与对策

| 风险 | 对策 |
|---|---|
| 第三方图床（pixiv.cat 等）可用性不稳定 | 插件化多源 + 优先级可配 + 内容校验 + 负缓存退避 |
| HDD 随机 IO 拖垮整体 | DB 只存元数据、图片流式落盘、分目录存储、LRU 扫描低频触发（§0 策略 / §6.4） |
| 同步数据冲突/乱序 | LWW 幂等合并 + 按（账号 × 域）单调 syncToken + 90 天全量重建兜底 |
| 上游 Pixiv 反爬/限流 | UA 注入、限流友好、超时钳制、重试退避 |
| 机械盘上恢复抓取 IO 竞争 | 抓取临时文件独立目录，抓取与缓存读分离 |
| `modernc.org/sqlite` 性能低于 cgo 版 | 负载为单机小实例，DB 仅元数据小写入；实测不达标再评估 `mattn/go-sqlite3`（需放弃静态编译） |
