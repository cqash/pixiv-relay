# AGENTS.md — ArkPix Relay Server

Pixiv 第三方客户端 ArkPix 的服务端：网络中继 + 数据同步 + 已删除作品恢复 + Web 端托管。Go 1.24+ 标准库为主，SQLite（WAL），部署目标 NAS/小 VPS（机械硬盘友好）。

**协议契约**：客户端仓库 `docs/backend-design.md`（v1.1，含 §14 管理端）是唯一权威，端点/错误格式/同步语义改动必须先改文档并升级 `/v1/` 版本号。**开发计划与里程碑进度**：`PLAN.md`（M0–M9，勾选制）。

## 工具链与命令

- 本机无系统级 Go，便携工具链在 `.toolchain/go/`（go1.26.0，已 gitignore，sha256 已校验）。所有命令前：
  `export PATH="$PWD/.toolchain/go/bin:$PATH"`
- 运行：`go run ./cmd/server`（或构建后 `./bin/server.exe`，端口 `PORT` 环境变量，默认 8080）
- 测试：`go test ./...`；压测 `go test -bench ./...`
- 检查：`go vet ./...` + `gofmt -l cmd internal`（**不要**对 `.` 全量跑 gofmt，会扫进 `.toolchain` 的 Go 源码测试数据产生噪音）
- 静态编译验证（部署形态）：`CGO_ENABLED=0 go build -o bin/server.exe ./cmd/server`
- CI：`.github/workflows/ci.yml`（golangci-lint + vet + test + linux/arm64 交叉编译 + docker 多架构镜像发布 GHCR）

## 结构

```
cmd/server/main.go     # 入口：config 加载 → slog JSON 日志 → app.New() → http.Server（优雅退出）
internal/
  app/                 # ServeMux 装配、中间件链、路由注册
  config/              # 环境变量解析（.env.example 是配置全集模板）
  common/              # errors / requestid / pagination / ratelimit / logger（M1）
  auth/ relay/ img/    # 各模块 routes + service（M2–M4）
  cache/disklru.go     # 磁盘 LRU 缓存（M4）
  sync/ recover/       # 同步域与恢复队列（M5–M6）
  crypto/              # 用户数据静态加密（M7，AES-256-GCM，enc:v1: 前缀混存兼容）
  admin/               # 管理端 API（M9，§14）：ADMIN_TOKEN 鉴权、settings 热更、缓存/账号管理
  storage/repository.go # 仓储接口（SQLite 实现，预留 Postgres）
  web/embed.go         # go:embed SPA 静态资源（M7；dist/ 由独立前端工程产出，仅占位 index.html 入库）
  web/spa.go cors.go   # SPA 托管（WEB_DIR 磁盘模式可切）与 CORS_ORIGINS 白名单（M7）
docker/                # Dockerfile（multi-stage → scratch/distroless）+ compose（M7）
```

## 关键约定

- **依赖策略**：尽量标准库（`net/http` ServeMux 路由、`log/slog`、`crypto/*`、`embed`）；外部依赖仅 `modernc.org/sqlite`（纯 Go，保 `CGO_ENABLED=0` 静态编译）与 `golang.org/x/time`（限流），在首个使用它的里程碑引入（提前加会被 `go mod tidy` 清除）。新增其他依赖需先记录到 PLAN.md §0 决策表
- **机械硬盘策略（§6.4，贯穿始终）**：DB 不存图片 blob；图片写 = 同卷临时文件 + `os.Rename` 原子落盘，读 = `io.Copy` 流式；缓存 `CACHE_LAYOUT=sharded` 两级分目录；LRU 元数据存 DB、淘汰水位触发且批量限速；`io.ReadAll` 仅限 ≤1MB 的 API body
- **协议面**：错误统一 `{error:{code,message,requestId}}`；列表端点游标分页 `{items,nextCursor}`；同步 token 按（账号 × domain）单调递增；日志绝不落 `authorization`/body（slog 脱敏 Handler）
- **认证**：服务端账号体系与 Pixiv 解耦，`accountKey` 加入已有账号（只存哈希）；公网部署必须 `INVITE_CODES` 或 `STATIC_TOKENS`
- **静态加密（§9，M7）**：`DATA_ENC_KEY`（base64 32B）开启后 `sync_entries.data` 与 `recover_cache` 的 pages/meta 落库前 AES-256-GCM 加密；密文带 `enc:v1:` 前缀，与存量明文混存兼容；空 = 不加密；密钥格式错误启动直接退出
- **管理端（§14，M9）**：`ADMIN_TOKEN` 空 = `/admin/v1/*` 不注册；六键运行时设置（缓存上限/水位、recover TTL、限流）存 `settings` 表，DB > env > 默认，PATCH 立即热生效；管理 UI 在 Web 前端工程 `/admin` 路由区
- 测试用 `net/http/httptest`，集成测试一律临时目录，不碰真实 `data/`

## 关联工程

- 客户端（HarmonyOS ArkTS）：独立仓库；客户端按设计文档 §11 改造清单实现 relay 模式
- Web 前端：独立工程，构建产物拷入 `internal/web/dist/` 后随二进制 embed 分发
