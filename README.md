# ArkPix Relay Server

Pixiv 第三方客户端 ArkPix 的自托管服务端：网络中继（/relay）、图片中继与磁盘 LRU 缓存（/img）、多设备数据同步（/sync）、已删除作品恢复（/recover）、Web 端静态托管。Go 1.24+ 标准库为主，SQLite（WAL，纯 Go 驱动），单静态二进制交付，NAS / 小 VPS（机械硬盘）友好。

## 快速开始

本机运行（便携工具链，无需系统级 Go）：

```bash
export PATH="$PWD/.toolchain/go/bin:$PATH"
go run ./cmd/server          # 默认监听 :8080，数据落 ./data
```

Docker（单容器，bind 卷 ./data）：

```bash
cp .env.example .env         # 按需修改；公网部署必须配置 INVITE_CODES 或 STATIC_TOKENS
docker compose -f docker/docker-compose.yml up -d --build
```

验证：`curl http://localhost:8080/healthz` → `{"status":"ok"}`。

## 配置

全部环境变量见 [.env.example](.env.example)（注释即文档）：端口/数据目录、缓存容量与分目录策略、注册邀请码与预置 token、上行出口代理、限流配额、恢复源列表与 TTL、CORS 白名单、Web 静态目录、`DATA_ENC_KEY` 用户数据静态加密（AES-256-GCM）。

## 协议与文档

- 协议契约（唯一权威，端点/错误格式/同步语义）：`ArkPix/docs\backend-design.md`
- 端点速查：[docs/api.md](docs/api.md)
- 开发计划与各里程碑验收：[PLAN.md](PLAN.md)

## 开发

```bash
export PATH="$PWD/.toolchain/go/bin:$PATH"   # go1.26.0 便携工具链

go test ./...                                # 测试
go test -bench . -run '^$' ./internal/cache/ ./internal/sync/   # 压测（BENCH_DIR 可指定磁盘）
go vet ./... && gofmt -l cmd internal        # 检查（gofmt 不要对 . 全量跑）
CGO_ENABLED=0 go build -o bin/server.exe ./cmd/server           # 静态编译（部署形态）
```

更多约定（依赖策略、机械硬盘 IO 策略、认证与加密模型）见 [AGENTS.md](AGENTS.md)。
