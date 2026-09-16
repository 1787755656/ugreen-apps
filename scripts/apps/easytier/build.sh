#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 2.6.4> <arch: amd64|arm64>
#
# EasyTier 是【Docker 型应用】(monorepo 里第一个):不拆镜像取二进制,
# 而是把整个 majosissi/easytier 镜像打进包——平台在安装时把
# rootfs_<arch>/images/ 里的 tar docker load 进本地镜像库,compose 引用
# 改标后的 personal/easytier:<version>(平台禁止 latest tag,上游又只有
# latest/pre/ci 三个浮动 tag,故按内容版本改标;与 aigw/imgocr 的
# personal/<app>:<version> 惯例一致)。
#
# ⚠ ugcli check 对 support_arch 声明的【每个】架构都要求 images/ 里有
#   镜像 tar("missing image tar for service"),而 CI 的 matrix 每个架构
#   一个 job——所以这里不看 ARCH 参数、一次把两个架构的 tar 都备齐;
#   ARCH 参数只用于日志与断言(两个 job 产物等价,pack 各取所需)。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

case "$ARCH" in
  amd64|arm64) ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

IMAGE_SRC="majosissi/easytier:latest"
IMAGE_DST="personal/easytier:${VERSION}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APPDIR="$REPO_ROOT/$PROJECT_DIR"

echo "==> Bundling image ${IMAGE_SRC} as ${IMAGE_DST} (both archs)"

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

mkdir -p "$APPDIR/rootfs_amd64/images" "$APPDIR/rootfs_arm64/images"

# ---- amd64:runner 本机架构,可以直接跑起来做内容版本断言 ----
pull_retry amd64
docker tag "$IMAGE_SRC" "$IMAGE_DST"
GOT=$(docker run --rm --entrypoint easytier-core "$IMAGE_DST" --version 2>/dev/null | head -1)
echo "==> image content: ${GOT}"
# 把 2.6.4-8428a89d 或 2.6.4 都算匹配 2.6.4;防止 :latest 指向的内容与
# check-version 阶段探测到的版本不一致(上游恰好在两次 job 之间重新推送)
printf '%s' "$GOT" | grep -qE "^easytier-core v?${VERSION}([-.-].*)?$" || {
  echo "::error::镜像内容版本(${GOT})与目标版本 ${VERSION} 不符 —— 上游 :latest 可能已更新,请重新触发或核对版本" >&2
  exit 1
}
docker save -o "$APPDIR/rootfs_amd64/images/easytier-${VERSION}-amd64.tar" "$IMAGE_DST"

# ---- arm64:manifest 拉取不需要 qemu;不能跑,断言架构字段 ----
pull_retry arm64
docker tag "$IMAGE_SRC" "$IMAGE_DST"
ARCH_FIELD=$(docker image inspect "$IMAGE_DST" --format '{{.Architecture}}')
[ "$ARCH_FIELD" = "arm64" ] || {
  echo "::error::期望 arm64 镜像,实际 ${ARCH_FIELD}" >&2
  exit 1
}
docker save -o "$APPDIR/rootfs_arm64/images/easytier-${VERSION}-arm64.tar" "$IMAGE_DST"

# ---- compose 里的镜像 tag 跟着版本走(与 project.yaml 的 sed 同机制) ----
sed -i -E "s#image: personal/easytier:.*#image: personal/easytier:${VERSION}#" \
  "$APPDIR/rootfs_common/docker-compose.yaml"
grep -n "image: personal/easytier" "$APPDIR/rootfs_common/docker-compose.yaml"

ls -lh "$APPDIR"/rootfs_*/images/
echo "==> Done: both image tars ready (request arch: $ARCH)"
