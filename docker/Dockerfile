# syntax=docker/dockerfile:1
# ArkPix Relay Server 镜像（PLAN M7，设计文档 §10）。
# multi-stage：golang 构建 CGO_ENABLED=0 静态二进制 → distroless 非 root 运行。
# 多架构：buildx --platform linux/amd64,linux/arm64[,linux/arm/v7] 时自动注入
# TARGETOS/TARGETARCH/TARGETVARIANT，CGO_ENABLED=0 + 纯 Go 驱动（modernc.org/sqlite）
# 无 cgo 依赖，跨架构静态编译零障碍。
# 运行基底镜像与模块代理可经 --build-arg 替换（gcr.io / proxy.golang.org 被墙网络
# 分别换 gcr.m.daocloud.io 与 goproxy.cn；goproxy.cn + sum.golang.google.cn 全球可达）。

ARG BASE_IMAGE=gcr.io/distroless/static-debian12:nonroot
ARG GOPROXY="https://goproxy.cn,direct"
ARG GOSUMDB="sum.golang.google.cn"

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG GOPROXY
ARG GOSUMDB
WORKDIR /src

# 先拷 go.mod/go.sum 利用层缓存；modernc.org/sqlite 为纯 Go 驱动，静态编译无 cgo。
COPY go.mod go.sum ./
RUN GOPROXY="${GOPROXY}" GOSUMDB="${GOSUMDB}" go mod download
COPY . .
# 注意：internal/web/dist/ 需先由独立前端工程构建并拷入（仓库内为占位页）。
# GOARM 取 TARGETVARIANT 去 v 前缀（linux/arm/v7 → 7）；空值（amd64/arm64）go 忽略。
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w" -o /server ./cmd/server \
    && mkdir -p /data

FROM ${BASE_IMAGE}
COPY --from=build /server /server
# 数据目录预建并赋给 nonroot（distroless nonroot = uid/gid 65532），
# 保证 named volume 初始化时 nonroot 可写。
COPY --from=build --chown=65532:65532 /data /data

ENV DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]