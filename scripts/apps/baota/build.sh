#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 13.1.0> <arch: amd64|arm64>
#
# 宝塔面板是【Docker 型应用】：把整个 btpanel/baota 镜像打进包——平台在
# 安装时把 rootfs_<arch>/images/ 里的 tar docker load 进本地镜像库，compose
# 引用改标后的 personal/baota:<version>（钉版本 + 改 personal/ 前缀是
# deepseek-harness/1panel 惯例：避免 compose 意外从 Docker Hub 在线拉同名镜像，
# 包自带的 tar 才是唯一事实源）。镜像由堡塔安全官方发布（btpanel 命名空间），
# 本包不改镜像内容。
#
# ⚠ ugcli check 对 support_arch 声明的【每个】架构都要求 images/ 里有
#   镜像 tar("missing image tar for service")，而 CI 的 matrix 每个架构
#   一个 job——所以这里不看 ARCH 参数、一次把两个架构的 tar 都备齐；
#   ARCH 参数只用于日志与断言（两个 job 产物等价，pack 各取所需）。
#
# 内容版本断言：镜像里没有 /www/server/panel（面板本体在根目录 /www.tar.gz，
# 首启才解开，见 get-latest-version.sh 注释），版本从 tarball 里的
# www/server/panel/class/common.py `g.version = 'x.y.z'` 读。取证方式用
# docker create + docker cp——【不运行容器】，所以对 amd64/arm64 完全同构，
# 任何架构的 runner 都能取证任何架构的镜像（qbittorrent-ng6 同款判据）。
# 上游同 tag 重推、或哪天改了打包结构，这里立即拦下。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

case "$ARCH" in
  amd64|arm64) ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

IMAGE_SRC="btpanel/baota:latest"
IMAGE_DST="personal/baota:${VERSION}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APPDIR="$REPO_ROOT/$PROJECT_DIR"
TMPDIR_PROBE="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_PROBE"' EXIT

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

# 内容版本断言（create + cp，不运行容器，双架构同构取证）
probe_version() {
  local container="baota-probe-$ARCH-$$"
  docker create --name "$container" "$IMAGE_DST" >/dev/null
  docker cp "$container:/www.tar.gz" "$TMPDIR_PROBE/www.tar.gz" >/dev/null
  docker rm "$container" >/dev/null
  local got
  got=$(tar -xzOf "$TMPDIR_PROBE/www.tar.gz" www/server/panel/class/common.py 2>/dev/null \
    | grep "g.version" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
  [ "$got" = "$VERSION" ] || {
    echo "::error::镜像内容版本(${got:-空})与目标版本 ${VERSION} 不符 —— 上游可能重推了 tag 或改了打包结构，请核对" >&2
    exit 1
  }
}

mkdir -p "$APPDIR/rootfs_amd64/images" "$APPDIR/rootfs_arm64/images"

# ---- amd64 ----
pull_retry amd64
docker tag "$IMAGE_SRC" "$IMAGE_DST"
probe_version
docker save -o "$APPDIR/rootfs_amd64/images/baota-${VERSION}-amd64.tar" "$IMAGE_DST"

# ---- arm64:manifest 拉取不需要 qemu;create+cp 不运行,断言内容版本 ----
pull_retry arm64
docker tag "$IMAGE_SRC" "$IMAGE_DST"
probe_version
docker save -o "$APPDIR/rootfs_arm64/images/baota-${VERSION}-arm64.tar" "$IMAGE_DST"

# ---- compose 里的镜像 tag 跟着版本走(与 project.yaml 的 sed 同机制) ----
# 写法用 sed -E 重定向 + mv：macOS 的 BSD sed 下 `sed -i -E` 会把 -E 当成
# 备份后缀，留下 compose.yaml-E 垃圾文件害 ugcli check 挂掉
sed -E "s#image: personal/baota:.*#image: personal/baota:${VERSION}#" \
  "$APPDIR/rootfs_common/docker-compose.yaml" > "$APPDIR/rootfs_common/docker-compose.yaml.tmp"
mv "$APPDIR/rootfs_common/docker-compose.yaml.tmp" "$APPDIR/rootfs_common/docker-compose.yaml"
grep -n "image: personal/baota" "$APPDIR/rootfs_common/docker-compose.yaml"

ls -lh "$APPDIR"/rootfs_*/images/
echo "==> Done: both image tars ready (request arch: $ARCH)"
