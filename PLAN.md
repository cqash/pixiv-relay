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

- [ ] 建表 `accounts`（id、account_key 哈希、created_at）与 `devices`：id、account_id、device_name、invite_code、access_token、refresh_token、created_at、expires_at
- [ ] token 方案：accessToken（30d）与 refreshToken（180d），`crypto/rand` 随机不透明串，DB 存储 SHA-256 哈希，轮换制（refresh 后旧 access 立即失效）
- [ ] `POST /auth/v1/register`：可选 inviteCode 校验（`INVITE_CODES` 配置，空则跳过）；可选 `accountKey` 加入已有账号（无效按 400 拒绝，不静默新建），响应含 `accountKey`、`serverVersion`、`capabilities:["relay","img","sync","recover"]`（§12）；注册端点独立限流 10 次/小时/IP（§9）
- [ ] `POST /auth/v1/refresh`：refreshToken 换新对，旧 refreshToken 单次使用
- [ ] 鉴权中间件：`Authorization: Bearer` 解析 → 401（缺失/失效）；预置 token 模式（`STATIC_TOKENS`）供私有部署
- [ ] 验收：注册→访问→refresh→旧 token 401 全链路测试；accountKey 加入已有账号 → 两设备共享同步数据

---

## 6. M3 网络中继 /relay（§6.1）

- [ ] `POST /relay/v1/request`：
  - 域名白名单：`app-api.pixiv.net`、`oauth.secure.pixiv.net`、`www.pixiv.net`（可配置扩展）
  - 头白名单：`authorization`、`accept-language`、`content-type`、`user-agent`、`referer`；其余丢弃
  - bodyBase64 上限 1 MB；`timeoutMs` 钳制 ≤ 60 s
  - 客户端未传 UA 时注入 `PixivIOSApp/5.8.0`
  - `http.Client` + 自定义 `Transport` 出网，`UPSTREAM_PROXY` 经 `Transport.Proxy` 生效
- [ ] 响应透传：status、白名单内响应头、bodyBase64
- [ ] 日志：仅 method + host + 状态码 + 耗时，绝不落 Authorization 与 body（§6.1）
- [ ] 错误映射：上游不可达/超时 → 502
- [ ] 验收：httptest mock 上游，断言白名单过滤、头过滤、UA 注入、超时钳制、日志无敏感字段

---

## 7. M4 图片中继 /img（§6.2）

- [ ] `GET /img/v1/fetch?url=&disposition=`：
  - 域名白名单：`i.pximg.net`、`i-f.pximg.net`、`s.pximg.net` + `IMG_EXTRA_HOSTS` 可配追加（§6.2，恢复模块源域名必须在此声明）
  - 服务端注入 `Referer: https://app-api.pixiv.net/` + UA
  - 响应透传 `Content-Type`、`Content-Length`、`Cache-Control`；加 `X-Upstream-Status`、`X-Cache: HIT/MISS`
- [ ] 磁盘 LRU 缓存 `cache/disklru.go`（§6.4）：
  - 键 = URL SHA-256；默认上限 20 GB 可配；`CACHE_LAYOUT=sharded` 两级分目录；元数据存 DB（size、atime、expire）
  - 写：`CACHE_TMP_DIR`（同卷）临时文件流式下载（`io.Copy` 限速体）→ 校验大小（单图上限 50 MB）→ `os.Rename` 落盘
  - 读：`os.Open` + `io.Copy` 管道直出（或 `http.ServeContent`），禁止整读
  - 淘汰：LRU 按 DB 内 atime，启动 + `CACHE_HIGH_WATERMARK`（默认 90%）触发，`CACHE_EVICTION_BATCH` 批量限速删除
- [ ] 失败映射：上游 404 → 404；超时 → 502；磁盘写满降级直连（不返回 507，§4.2）
- [ ] 验收：HDD 上 `go test -bench` 压测缓存命中率与吞吐，确认无整读入内存、无随机 IO 热点

---

## 8. M5 数据同步 /sync（§7）

- [ ] 建表：`sync_domains`（account_id、domain、sync_token）、`sync_entries`（account_id、domain、key、data、updated_at、deleted、tombstone 时间）、`sync_log`（审计）
- [ ] syncToken 生成：`st_<毫秒时间戳>_<random>`，按（账号 × domain）单调递增
- [ ] `POST /sync/v1/push`：
  - 单域单次 ≤ 500 条，超限 400
  - `baseToken` 与当前不一致且超出保留期（90 天）→ `409 SYNC_FULL_REQUIRED`
  - 幂等：同 key 按 `updatedAt` 取大者（LWW）
- [ ] `GET /sync/v1/pull`：`since` + `limit`，响应 `{items, syncToken, hasMore}`；墓碑带 `deleted:true`（保留 90 天）
- [ ] 域冲突策略 `domains.go`（§7.1）：
  - LWW：history、search_history、bookmark_snapshot、exif_config、settings、mute（按 key 的 LWW，净效果集合并集）
- [ ] bookmark_snapshot 结构校验（§7.3）
- [ ] 敏感字段排除：客户端侧职责（文档 §7.2），服务端校验禁写 `*token*` 字段名（防御）
- [ ] 验收：多设备（同 accountKey 两设备）push/pull 交叉、冲突合并、墓碑传播、90 天全量重建

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
