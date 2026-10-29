#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 0.2.1-rc.4> <arch: amd64|arm64>
#
# DeepSeek Harness 是【Docker 型应用】：把整个 smanx/deepseek-harness
# 镜像打进包——平台在安装时把 rootfs_<arch>/images/ 里的 tar docker load
# 进本地镜像库，compose 引用改标后的
# personal/deepseek-harness:<version>（平台禁止 latest tag；devtools-min-latest
# 是浮动 tag，故钉到版本 tag 并按 aigw/imgocr 惯例改标 personal/ 前缀）。
#
# 为什么用 devtools-min 变体：内置 pnpm（dsh plugin / dsh-market 插件市场）
# 与 uv（uvx/uv 启动的 MCP server），slim 变体两者皆无；devtools/admin/test
# 变体更大且对跑 DSH 无增益。
#
# ⚠ ugcli check 对 support_arch 声明的【每个】架构都要求 images/ 里有
#   镜像 tar("missing image tar for service")，而 CI 的 matrix 每个架构
#   一个 job——所以这里不看 ARCH 参数、一次把两个架构的 tar 都备齐；
#   ARCH 参数只用于日志与断言（两个 job 产物等价，pack 各取所需）。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

case "$ARCH" in
  amd64|arm64) ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

IMAGE_SRC="smanx/deepseek-harness:devtools-min-${VERSION}"
IMAGE_DST="personal/deepseek-harness:${VERSION}"

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

# ---- amd64:runner 本机架构,可以跑起来做内容版本断言 ----
pull_retry amd64
docker tag "$IMAGE_SRC" "$IMAGE_DST"
GOT=$(docker run --rm --entrypoint node "$IMAGE_DST" \
  -e "console.log(require('/usr/local/lib/node_modules/@deepseek-ai/dsh/package.json').version)" \
  2>/dev/null | tail -1)
echo "==> image content: DSH ${GOT}"
# 镜像钉的是版本 tag；若上游把同一 tag 重推成别的内容，这里立即拦下
[ "$GOT" = "$VERSION" ] || {
  echo "::error::镜像内容版本(${GOT})与目标版本 ${VERSION} 不符 —— 上游可能重推了 tag，请核对" >&2
  exit 1
}
docker save -o "$APPDIR/rootfs_amd64/images/deepseek-harness-${VERSION}-amd64.tar" "$IMAGE_DST"

# ---- arm64:manifest 拉取不需要 qemu;不能跑,断言架构字段 ----
pull_retry arm64
docker tag "$IMAGE_SRC" "$IMAGE_DST"
ARCH_FIELD=$(docker image inspect "$IMAGE_DST" --format '{{.Architecture}}')
[ "$ARCH_FIELD" = "arm64" ] || {
  echo "::error::期望 arm64 镜像,实际 '${ARCH_FIELD}'" >&2
  exit 1
}
docker save -o "$APPDIR/rootfs_arm64/images/deepseek-harness-${VERSION}-arm64.tar" "$IMAGE_DST"

# ---- compose 里的镜像 tag 跟着版本走(与 project.yaml 的 sed 同机制) ----
# 写法用 sed -E 重定向 + mv：macOS 的 BSD sed 下 `sed -i -E` 会把 -E 当成
# 备份后缀，留下 compose.yaml-E 垃圾文件害 ugcli check 挂掉
sed -E "s#image: personal/deepseek-harness:.*#image: personal/deepseek-harness:${VERSION}#" \
  "$APPDIR/rootfs_common/docker-compose.yaml" > "$APPDIR/rootfs_common/docker-compose.yaml.tmp"
mv "$APPDIR/rootfs_common/docker-compose.yaml.tmp" "$APPDIR/rootfs_common/docker-compose.yaml"
grep -n "image: personal/deepseek-harness" "$APPDIR/rootfs_common/docker-compose.yaml"

ls -lh "$APPDIR"/rootfs_*/images/
echo "==> Done: both image tars ready (request arch: $ARCH)"
