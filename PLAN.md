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
| M9 | 管理端 /admin | Admin API + Web 管理页面 | 对照 §14 端点清单全部可用 |

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

- [x] 建表 `recover_cache`（迁移 `0004_recover.sql`）：account_id、pid、pages(JSON)、source、meta(JSON)、status（ready/fetching/not_found）、expire（毫秒），PK(account_id,pid) + 索引 (pid,status,expire)（供共享模式跨账号查询）；正缓存 90 天（`RECOVER_TTL_DAYS`）、负缓存 7 天（`RECOVER_NEGATIVE_TTL_DAYS`）；fetching 仅占位（15 分钟守卫窗口，防 worker 崩溃残留，过期允许重新入队）
- [x] `GET /recover/v1/illust/{pid}`（`internal/recover/routes.go` + `service.go`；auth 鉴权 + 写限流 60/min burst 10 key=accountID）：pid 纯数字校验否则 400；命中 200 `{status:"ready",pages,source,meta}`；未命中入队 202 `{status:"fetching",retryAfterSec:5}`；负缓存/全失败 404 `{status:"not_found"}`（自定义体，非错误信封）；`pages[].url` 按请求推导基址（r.Host + scheme）包装为 `{base}/img/v1/fetch?url=<源URL>`（§8.1）
- [x] 异步队列 `queue.go`：goroutine + 带缓冲 channel 信号量，全局并发 ≤ 8、每源 ≤ 2、单源探测间隔 ≥ 1 s（`x/time/rate` 每源限速器，burst 1）；同 (account,pid) 在途去重只入队一次；并发/间隔/TTL 参数全部可注入
- [x] 数据源插件 `sources/`（`Build` 按 `RECOVER_SOURCES` 顺序组装，条目支持 `name=host` 覆盖镜像域名；未知源 log warn 跳过）：
  - `snapshot.go`：读本账号 bookmark_snapshot 域该 pid 的 `imageUrls`，逐 URL HEAD（405/501 退化 GET Range bytes=0-0）探测存活，存活页拉回落盘；出网带 Pixiv 风格 Referer；meta 透传快照字段（剔除 imageUrls）
  - `mirror.go`：`<pid>.jpg` 首页 + `<pid>_p<N>.jpg` 递增探测，连续 2 个 404 或 50 页上限停止；内容校验 Content-Type 须 image/* 且 > 1 KB；尺寸用 `image.DecodeConfig` 尽力解码（jpeg/png/gif）
  - 源域名启动校验：镜像 host 不在 `IMG_EXTRA_HOSTS` 则 log warn（客户端走 /img/v1/fetch 会 403，§6.2）
- [x] 抓取成功 → 图片先流式落 `RECOVER_TMP_DIR`（默认 `<DATA_DIR>/recover-tmp`，独立于缓存目录，HDD 分卷）校验后经 `cache.Put` 原子入 M4 缓存；出网复用 `relay.NewUpstreamClient`（UPSTREAM_PROXY 生效）；负缓存过期后允许重试
- [x] 恢复产物按账号隔离（account_id），`RECOVER_SHARED=true` 放开共享（查询不限 account_id，ready 优先）；`/img` 磁盘缓存跨用户共享（§8.2 可见性分层）
- [x] 验收：httptest mock 镜像源/pximg（Transport 白名单改写，未登记 host 直接报错，零真实外网）+ t.TempDir()：状态机 202→200（pages 包装 URL 走 img 端点 HIT 字节一致）、全失败 404 负缓存（期内不再抓取，镜像计数断言）、负缓存过期重试、快照源优先（source=snapshot 且镜像零触达）、单源探测间隔 ≥ 注入值与全局并发上限（注入小参数，原子计数器+时间戳断言）、pid 非数字 400 / 无鉴权 401、账号隔离与 RECOVER_SHARED 放开、同 pid 并发 3 请求只入队一次；`go vet`/`gofmt`/`go test ./...`（recover `-count=3`）/`CGO_ENABLED=0` 静态编译全过。**环境限制**：同 M4/M5，本机无 gcc，`-race` 以 `-count=3` 重复并发用例替代

---

## 10. M7 部署形态（§10）

- [x] 环境变量全集（`.env.example`）：端口、DB 路径、缓存目录/上限、`CACHE_LAYOUT`、`CACHE_TMP_DIR`、`CACHE_HIGH_WATERMARK`、`CACHE_EVICTION_BATCH`、INVITE_CODES、STATIC_TOKENS、UPSTREAM_PROXY、限流参数、恢复源列表/优先级、TTL、CORS_ORIGINS、WEB_DIR（Web 前端静态资源目录，默认使用 `embed.FS` 内嵌产物）、DATA_ENC_KEY。**实现说明**：config 新增 `CORSOrigins/WebDir/DataEncKey` + 限流三件套 `RATE_WRITE_PER_MIN`（默认 60，relay/sync/recover 写端点共用）/`RATE_IMG_PER_MIN`（默认 300）/`RATE_REGISTER_PER_HOUR`（默认 10），<=0 或非法回退默认；路由 `RegisterRoutes` 以可选变参接收配额，既有测试调用零改动
- [x] `docker/Dockerfile`（multi-stage：`golang:1.26-alpine` 构建 `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` 静态二进制 → `gcr.io/distroless/static-debian12:nonroot` 运行；EXPOSE 8080、VOLUME /data、ENV DATA_DIR=/data、/data 预建并 chown 65532 保证 nonroot 可写 named volume）+ `docker-compose.yml`（单容器，`./data:/data` bind 卷、8080 端口、`env_file: ../.env`（required:false）、restart unless-stopped）+ 根 `.dockerignore`（.toolchain/bin/data/.git 等；**注意** dist 需保留在构建上下文供 go:embed）
- [x] Web 前端静态托管（§6.5）：`internal/web/embed.go` `go:embed dist` 内嵌 SPA（仓库内为占位 `dist/index.html`，`.gitignore` 加 `!internal/web/dist/index.html` 例外）；`spa.go` 兜底 `GET /`：命中静态资源按扩展名 Content-Type（内置表兜底 Windows mime 注册表缺项）+ `Cache-Control: public, max-age=86400`，未命中 fallback index.html（`no-cache`），`X-Content-Type-Options: nosniff`；`WEB_DIR` 非空改从磁盘服务（前端开发期）；`cors.go` `CORS_ORIGINS` 白名单中间件：空 = 完全关闭；仅白名单 Origin 在 API 路径（/auth//relay//img//sync//recover//healthz）回显 ACAO + preflight 204（允许 `Authorization, Content-Type`，Max-Age 86400）；API 响应统一 `Cache-Control: no-store` + nosniff（common.writeRaw）
- [x] 服务端静态加密同步数据（§9）：`internal/crypto`（AES-256-GCM，nonce crypto/rand，密文 = `enc:v1:` + base64(nonce+ciphertext)）；`DATA_ENC_KEY` 空 = 不加密（nil *Cipher 全链路透传），格式错误（base64 失败/非 32 字节）app.New 报错启动退出；读出按前缀区分密文/存量明文混存兼容，密文无密钥读出报错不静默；接入点：sync.Push 落库前加密 / Pull 读出解密（service.go 各一行），recover_queue loadSnapshot（读 sync_entries 密文快照）/ writeResult（pages/meta 落库加密）+ service.lookup 读出解密
- [x] 文档 `docs/api.md`：端点汇总（对齐 §13，逐端点方法/鉴权/请求体/响应体/错误码 + 通用约定/Web 托管/CORS）
- [x] 验收：web 托管（GET /、/settings fallback、/healthz 不拦截、WEB_DIR 磁盘模式、.js/.html Content-Type、缓存头）、CORS（白名单 preflight 204 回显/非白名单不回显/空配置无头/静态路径不挂 CORS）、加密（sync push→DB 断言 `enc:v1:` 前缀→pull 还原一致、手工明文行混存读出、密文无密钥 500、recover 密文快照读取 + pages/meta 密文落库断言、坏密钥 app.New 报错）、config 默认值/解析单测；`go vet`/`gofmt`/`go test ./... -count=1`/`CGO_ENABLED=0` 静态编译全过；PORT=18087 冒烟（/ 占位页 no-cache、/healthz no-store、/settings fallback、坏密钥启动报错退出）✓。**环境限制**：本机 Docker daemon 未运行，镜像未实际构建，Dockerfile 仅静态审查

---

## 11. M8 集成与验收

- [x] 集成测试：按 §13 端点清单逐端点点对点（httptest）——`internal/app/integration_test.go`，`httptest.NewServer` 起真实 HTTP 服务串通完整用户旅程；出网经新增统一注入点 `app.NewWithClient`（relay/img/recover 共用同一 *http.Client，nil 时按 UPSTREAM_PROXY 建生产客户端）+ blockRewrite Transport 改写到 mock 上游，零真实外网
- [x] 压测：`internal/cache/disklru_bench_test.go`（64KB/1MB/8MB 写读）与 `internal/sync/bench_test.go`（100/500 条批量 push），`ReportAllocs`+`SetBytes`；数据目录默认 t.TempDir()（CI 可跑），`BENCH_DIR` 环境变量可覆盖到指定磁盘；已在 E 盘机械硬盘实跑（数值见下）
- [x] 安全自查：`internal/app/security_test.go`——日志脱敏端到端（relay/img 请求后断言日志无 Authorization/token/query/body）、目录穿越（recover pid `../etc` 400、img 非白名单带 `..` 403）、token/accountKey 不落库明文（直查库断言仅存 SHA-256 哈希）、注册限流 429 + Retry-After、DATA_ENC_KEY 密文落库 app 层确认（逐字段用例沿用 M7 sync/recover 测试）
- [x] 交付：`PLAN.md` 打勾核对 + 验收报告（见下）

### M8 验收报告（2026-08-15，Windows amd64，i5-14600KF）

**端到端用户旅程**（`TestUserJourney`，一次通过，未发现端点串联 bug）：
healthz 200 → 注册设备A（token+accountKey+capabilities）→ 设备B 带 accountKey 注册入同账号 → refresh 轮换（旧 refreshToken 复用 401）→ relay 透传 mock 上游 200（bodyBase64 往返一致）→ img MISS 200 → HIT（字节一致、上游零触达）→ sync push history+bookmark_snapshot → A/B 双设备全量 pull 一致 → B 带 since 增量 pull 只拿新条目 → recover 202 入队 → 轮询 200（快照源，~1s）→ pages[0].url 包装 URL 实取图 200 且缓存 HIT → 5 个端点无 token 均 401 统一错误格式（requestId 头体一致）→ GET / 返回内嵌 SPA index.html。

**压测数值**（`go test -bench . -benchtime 1x`，BENCH_DIR 指向 **E 盘机械硬盘**；单迭代受 OS 页缓存影响，仅供量级参考）：

| Benchmark | 吞吐 | ns/op | allocs/op |
|---|---|---|---|
| CacheWrite/64KB | 97 MB/s | 0.67ms | 80 |
| CacheWrite/1MB | 1165 MB/s | 0.90ms | 70 |
| CacheWrite/8MB | 3364 MB/s | 2.49ms | 75 |
| CacheRead/64KB | 11 MB/s | 5.92ms | 59 |
| CacheRead/1MB | 134 MB/s | 7.83ms | 57 |
| CacheRead/8MB | 1003 MB/s | 8.37ms | 57 |
| SyncPushBatch/100 | 3.95 MB/s（~61k 条目/s） | 1.62ms | 5374 |
| SyncPushBatch/500 | 4.88 MB/s（~76k 条目/s） | 6.56ms | 26424 |

读路径小文件明显慢于写：每次命中刷新 DB atime 引入一次同步写，为 LRU 语义的有意代价（§6.4）；绝对延迟（毫秒级）对图片中继场景无影响。

**安全自查**：全部通过（见 `security_test.go` 五个用例）。

**已知遗留**：`-race` 待 CI（本机无 gcc，以 `-count=N` 重复并发用例替代）；真实 Pixiv 上联调待客户端接入后进行。**Docker 多架构构建已落地并实测**（2026-08-16，本机 Docker Desktop 29.3 + buildx）：`linux/amd64` 与 `linux/arm64` 镜像均构建成功并 healthz 200（arm64 经 QEMU 模拟运行）；Dockerfile 支持 `TARGETOS/TARGETARCH/TARGETVARIANT` 交叉编译（GOARM 取 variant 去 v），gcr.io / proxy.golang.org 被墙网络经 `--build-arg BASE_IMAGE`（gcr.m.daocloud.io）与 `GOPROXY/GOSUMDB`（goproxy.cn）可配，CI 新增 docker 作业发布 GHCR 多架构镜像。

---

## 12. M9 管理端 /admin（§14，设计文档 v1.1）

- [x] 设计文档先行：客户端仓库 `docs/backend-design.md` 新增 §14（管理端鉴权/settings 热更语义/端点清单），版本 v1.0 → v1.1；register 响应 `serverVersion` 同步升 "1.1.0"（`internal/auth/routes.go` + 两处测试断言）
- [x] 鉴权（`internal/admin/middleware.go`）：`ADMIN_TOKEN` 环境变量，空 = 不注册任何 `/admin/` 路由（启动 log info）；Bearer 常量时间比较（`crypto/subtle`），失败 401 INVALID_TOKEN 统一信封；认证路径 per-IP 限流 30/min burst 5 防爆破
- [x] settings 热更（`internal/admin/settings.go` + 迁移 `0005_settings.sql`）：`settings(key,value,updated_at)` 表存运行时覆盖，生效优先级 DB > env > 默认；GET 每项带 `source` 标记；PATCH 全量校验（未知键/非法值 400 报键名）后事务落库 + 立即热生效。白名单六键：`cache_max_bytes`/`cache_high_watermark`/`recover_ttl_days`/`recover_negative_ttl_days`/`rate_write_per_min`/`rate_img_per_min`
- [x] 热更挂钩（最小侵入）：`cache.DiskLRU` MaxBytes/HighWatermark 改原子量 + 新增 `Stats()`/`SetLimits()`（调低触发一次 maybeEvict）；recover `queue.go` ttlMs/negMs 改 `atomic.Int64` + `SetTTLs`；`common.Limiter.SetRate`（持锁更新并同步存量桶）；app 层构造共享 writeLimiter/imgLimiter 注入 relay/img/sync/recover（RegisterRoutes 变参 `...float64` → `...*common.Limiter`，无参调用兼容）
- [x] 端点（`internal/admin/routes.go` + `service.go`）：`GET /overview`（版本/uptime/账号设备计数/缓存用量/recover_cache 按 status 计数/生效 settings）、`GET|PATCH /settings`、`GET /cache/stats`、`POST /cache/evict`（前后 Stats 差值返回 freedBytes/freedEntries）、`GET /accounts`（created_at,id 升序 + common 游标分页，LEFT JOIN 计 deviceCount/syncEntryCount）、`GET /accounts/{id}/devices`、`DELETE /devices/{id}`（吊销即 token 失效，404 语义）、`DELETE /accounts/{id}`（事务级联 devices/sync_entries/sync_domains/recover_cache，FK 无 CASCADE 显式删）
- [x] CORS：`cors.go` API 前缀加 `/admin/`，preflight Allow-Methods 扩 `PATCH, DELETE`
- [x] Web 管理页面（独立 Web 前端工程）：`src/api/admin.ts`（token 存 `arkpix.web.admin`，401 清 token 跳登录）+ `/admin` 路由区（守卫只验 admin token，绕开双登录）+ 五视图（登录/布局导航/概览卡片/缓存进度条+淘汰+上限编辑/账号表格+吊销+级联删除双确认/设置表单）；`vite.config.ts` 代理加 `/admin`；产物经 deploy 拷入 `internal/web/dist/`
- [x] 验收：`internal/admin/admin_test.go` 11 个用例（关闭态 404/405、401 信封、settings 三来源/持久化/非法值、SetLimits 热生效、evict 释放量、分页、吊销后 401、级联删除断言、限流 429）；`go vet`/`gofmt`/`go test ./... -count=1`/`CGO_ENABLED=0` 静态编译全过；前后端联调冒烟（PORT=18080 + ADMIN_TOKEN）：SPA fallback / 401 / overview / PATCH 即时生效 / accounts 分页 ✓。**已知说明**：管理端关闭时 GET /admin/* 会命中 SPA 兜底返回 index.html（语义等同未注册）；成功响应带顶层 `requestId` 字段（WriteJSON 统一行为）

---

## 13. 验收对照（§13 端点清单）

| 方法 | 路径 | 状态 |
|---|---|---|
| POST | /auth/v1/register | ✓ M2（M8 端到端复测通过） |
| POST | /auth/v1/refresh | ✓ M2（M8 端到端复测通过） |
| POST | /relay/v1/request | ✓ M3（M8 端到端复测通过） |
| GET | /img/v1/fetch | ✓ M4（M8 端到端复测通过） |
| POST | /sync/v1/push | ✓ M5（M8 端到端复测通过） |
| GET | /sync/v1/pull | ✓ M5（M8 端到端复测通过） |
| GET | /recover/v1/illust/{pid} | ✓ M6（M8 端到端复测通过） |
| GET | /healthz | ✓ M0（M8 端到端复测通过） |

---

## 14. 风险与对策

| 风险 | 对策 |
|---|---|
| 第三方图床（pixiv.cat 等）可用性不稳定 | 插件化多源 + 优先级可配 + 内容校验 + 负缓存退避 |
| HDD 随机 IO 拖垮整体 | DB 只存元数据、图片流式落盘、分目录存储、LRU 扫描低频触发（§0 策略 / §6.4） |
| 同步数据冲突/乱序 | LWW 幂等合并 + 按（账号 × 域）单调 syncToken + 90 天全量重建兜底 |
| 上游 Pixiv 反爬/限流 | UA 注入、限流友好、超时钳制、重试退避 |
| 机械盘上恢复抓取 IO 竞争 | 抓取临时文件独立目录，抓取与缓存读分离 |
| `modernc.org/sqlite` 性能低于 cgo 版 | 负载为单机小实例，DB 仅元数据小写入；实测不达标再评估 `mattn/go-sqlite3`（需放弃静态编译） |
