# syntax=docker/dockerfile:1
# ArkPix Relay Server 镜像（PLAN M7，设计文档 §10）。
# multi-stage：golang 构建 CGO_ENABLED=0 静态二进制 → distroless 非 root 运行。

FROM golang:1.26-alpine AS build
WORKDIR /src

# 先拷 go.mod/go.sum 利用层缓存；modernc.org/sqlite 为纯 Go 驱动，静态编译无 cgo。
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# 注意：internal/web/dist/ 需先由独立前端工程构建并拷入（仓库内为占位页）。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /server ./cmd/server \
    && mkdir -p /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /server /server
# 数据目录预建并赋给 nonroot（distroless nonroot = uid/gid 65532），
# 保证 named volume 初始化时 nonroot 可写。
COPY --from=build --chown=65532:65532 /data /data

ENV DATA_DIR=/data
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
