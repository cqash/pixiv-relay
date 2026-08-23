#!/usr/bin/env bash
# ArkPix Relay Server 镜像构建脚本（含 ARM 多架构）。
# 纯 Go + CGO_ENABLED=0，Dockerfile 经 buildx 的 TARGETOS/TARGETARCH/TARGETVARIANT 交叉编译。
#
# 用法（在仓库根目录执行）：
#   bash docker/build.sh                        # 本机当前架构 → docker images（本地调试）
#   bash docker/build.sh --push <镜像:tag>       # linux/amd64,linux/arm64 多架构 → 推送 registry
#   bash docker/build.sh --push <镜像:tag> arm/v7  # 额外加 linux/arm/v7（老款 32 位 NAS）
#
# 前置：Docker ≥ 23 + buildx（docker buildx version）；跨架构构建需 QEMU：
#   docker run --privileged --rm tonistiigi/binfmt --install all
# 推送到 GHCR 时先登录： echo $CR_PAT | docker login ghcr.io -u <name> --password-stdin
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

IMAGE="${2:-pixiv-relay:latest}"
EXTRA_ARCH="${3:-}"

# gcr.io / proxy.golang.org 被墙网络经环境变量换镜像源与模块代理（默认已指向可达源）。
BASE_IMAGE="${BASE_IMAGE:-gcr.m.daocloud.io/distroless/static-debian12:nonroot}"
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
GOSUMDB="${GOSUMDB:-sum.golang.google.cn}"

PLATFORMS="linux/amd64,linux/arm64"
if [[ -n "$EXTRA_ARCH" ]]; then
  PLATFORMS="$PLATFORMS,linux/$EXTRA_ARCH"
fi

docker buildx version >/dev/null 2>&1 || { echo "需要 docker buildx" >&2; exit 1; }

if [[ "${1:-}" == "--push" ]]; then
  echo "==> 多架构构建并推送：$PLATFORMS → $IMAGE"
  docker buildx build \
    --platform "$PLATFORMS" \
    --build-arg BASE_IMAGE="$BASE_IMAGE" \
    --build-arg GOPROXY="$GOPROXY" \
    --build-arg GOSUMDB="$GOSUMDB" \
    -f docker/Dockerfile \
    -t "$IMAGE" \
    --push \
    .
else
  echo "==> 本机架构构建：$IMAGE"
  docker build \
    --build-arg BASE_IMAGE="$BASE_IMAGE" \
    --build-arg GOPROXY="$GOPROXY" \
    --build-arg GOSUMDB="$GOSUMDB" \
    -f docker/Dockerfile -t "$IMAGE" .
fi