#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 0.107.80> <arch: amd64|arm64>
#
# AdGuard Home 是【Docker 型应用】：把官方镜像 adguard/adguardhome 的
# 二进制重组进一个轻量派生镜像（alpine 底 + 默认配置播种入口），派生镜像
# 打进 upk 离线加载。派生而不是直接用官方镜像，是为了"开箱即用"：
# 首次启动 entrypoint 播种默认配置（默认账号 admin / AdGuard@2026，
# 见应用介绍），用户免初始化向导；conf 里已有配置（升级/自选数据目录）
# 时绝不覆盖。
#
# ⚠ ugcli check 对 support_arch 声明的每个架构都要求 images/ 里有镜像 tar
#   （"missing image tar for service"），而 CI matrix 每架构一个 job——
#   所以这里不看 ARCH 参数、一次把两个架构的 tar 都备齐（1panel 惯例）。
#   ARCH 参数只用于日志。
#
# 断言（内容不明不出包）：
#   - 派生镜像 Architecture 字段 == 目标架构
#   - 二进制里嵌的版本串（v0.107.80）== 目标上游版本
#   - seed 配置确实进了镜像（schema_version 可 grep）
#
# 跨架构说明：Dockerfile 全程只有 COPY（无 RUN），amd64 runner 上构建
# arm64 镜像不需要 QEMU/binfmt；arm64 镜像只 create+cp 不运行，同上。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"
UPSTREAM_TAG="v${VERSION}"
IMAGE_SRC="adguard/adguardhome:${UPSTREAM_TAG}"
IMAGE_DST="personal/adguardhome:${UPSTREAM_TAG}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APPDIR="$REPO_ROOT/$PROJECT_DIR"
DOCKER_DIR="$SCRIPT_DIR/docker"
mkdir -p "$APPDIR/rootfs_amd64/images" "$APPDIR/rootfs_arm64/images"

echo "==> Building derived image ${IMAGE_DST} from ${IMAGE_SRC} (both archs)"

# Docker Hub 匿名拉取限流重试
pull_retry() {
  local platform="$1"
  for i in 1 2 3; do
    if docker pull --platform "linux/$platform" "$IMAGE_SRC"; then return 0; fi
    echo "pull linux/$platform attempt $i failed, retrying in 20s..." >&2
    sleep 20
  done
  echo "::error::docker pull --platform linux/$platform ${IMAGE_SRC} failed after retries" >&2
  exit 1
}

# 从镜像里取文件（create+cp，不运行容器，跨架构安全）
image_extract() {
  local image="$1" src_path="$2" dst="$3" platform="$4" cid
  cid=$(docker create --platform "linux/$platform" "$image")
  docker cp "$cid:$src_path" "$dst"
  docker rm "$cid" >/dev/null
}

build_derived() {
  local platform="$1"
  echo "==> [linux/$platform] pull + build + assert + save"
  pull_retry "$platform"

  docker buildx build --platform "linux/$platform" \
    --build-arg BASE_IMAGE="${IMAGE_SRC}" \
    -t "${IMAGE_DST}" --load "$DOCKER_DIR"

  # 架构断言（tag 刚构建完即验，防止两架构互相覆盖时看错）
  local got_arch
  got_arch=$(docker image inspect "$IMAGE_DST" --format '{{.Architecture}}')
  [ "$got_arch" = "$platform" ] || {
    echo "::error::派生镜像架构 '${got_arch}' != linux/$platform" >&2
    exit 1
  }

  # 版本断言：二进制内嵌版本串必须等于目标上游版本
  local bin=/tmp/agh-bin-"$platform"
  image_extract "$IMAGE_DST" /opt/adguardhome/AdGuardHome "$bin" "$platform"
  grep -aq "${UPSTREAM_TAG}" "$bin" || {
    echo "::error::二进制里找不到版本串 ${UPSTREAM_TAG} —— 上游可能重推了 tag" >&2
    exit 1
  }
  rm -f "$bin"

  # seed 断言：默认配置必须在镜像里（Dockerfile COPY 目标是 /seed/AdGuardHome.yaml）
  local seed=/tmp/agh-seed-"$platform"
  image_extract "$IMAGE_DST" /seed/AdGuardHome.yaml "$seed" "$platform"
  grep -q "schema_version" "$seed" || {
    echo "::error::镜像里缺 seed 配置（/seed/AdGuardHome.yaml）" >&2
    exit 1
  }
  rm -f "$seed"

  docker save -o "$APPDIR/rootfs_${platform}/images/adguardhome-${UPSTREAM_TAG}.tar" "$IMAGE_DST"
}

build_derived amd64
build_derived arm64

# compose 镜像 tag 跟着版本走。sed -E 重定向 + mv：macOS 的 BSD sed 下
# `sed -i -E` 会把 -E 当备份后缀留下垃圾文件害 check 挂掉（1panel 同款坑）。
COMPOSE="$APPDIR/rootfs_common/docker-compose.yaml"
sed -E "s|^    image: .*|    image: \"${IMAGE_DST}\"|" "$COMPOSE" > "${COMPOSE}.tmp" && mv "${COMPOSE}.tmp" "$COMPOSE"
grep -n "image:" "$COMPOSE"

echo "==> Done"
ls -lh "$APPDIR"/rootfs_*/images/
