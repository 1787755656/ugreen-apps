#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 2.4.2> <arch: amd64|arm64>
#
# 1Panel 是【Docker 型应用】：把整个 moelin/1panel 镜像打进包——平台在安装
# 时把 rootfs_<arch>/images/ 里的 tar docker load 进本地镜像库，compose 引用
# 改标后的 personal/1panel:<version>（平台禁止 latest tag；这里钉的本来就是
# 版本 tag，改标 personal/ 前缀是 aigw/imgocr/deepseek-harness 惯例：避免
# compose 意外从 Docker Hub 在线拉同名镜像，包自带的 tar 才是唯一事实源）。
#
# 1Panel 官方只发安装脚本、不发自身镜像；moelin/1panel 是社区事实标准
# （Ubuntu 底包 + Supervisor 托管 1Panel 进程，amd64/arm64 双架构，实际
# 构建源是 okxlin/docker-1panel——镜像 label 里可见）。
#
# ⚠ ugcli check 对 support_arch 声明的【每个】架构都要求 images/ 里有
#   镜像 tar("missing image tar for service")，而 CI 的 matrix 每个架构
#   一个 job——所以这里不看 ARCH 参数、一次把两个架构的 tar 都备齐；
#   ARCH 参数只用于日志与断言（两个 job 产物等价，pack 各取所需）。
#
# 【按 digest 拉取，不按 --platform+tag】ubuntu-26.04 runner（2026-11-11 起，
#   Docker Server 29.4.2）默认启用 containerd image store：同一个 tag 先后
#   pull 两个平台后，tag 指向多平台 index，`docker image inspect` 恒返回
#   宿主架构变体（v2.5.1 两连败实证），`docker save` 也会把已知全部平台
#   导进同一个 tar（baota 本地 containerd 同款假失败）。按【单平台 manifest
#   digest】拉取后 tag 与平台一一对应，classic/containerd 两种存储模式下
#   inspect/save 行为都正确。产物 tar 格式随存储模式变化（classic 出
#   docker-archive、containerd 出 OCI-layout），两者都有真机先例
#   （1panel 2.4.2-r2 / ugtheme 1.0.0.0003）。
#
# 内容版本断言：镜像 label org.opencontainers.image.version（构建时从
# 1Panel 版本参数烘焙，如 "v2.4.2"）。镜像里的 `1pctl version` 在面板未
# 初始化前打印的 version 字段是空的（2026-11-04 实测 v2.4.2），用不了；
# label 是唯一可靠的版本源。label 缺失/不匹配 → 当场失败，不打出内容
# 不明的包。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

case "$ARCH" in
  amd64|arm64) ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

IMAGE_SRC="moelin/1panel:v${VERSION}"
IMAGE_DST="personal/1panel:${VERSION}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APPDIR="$REPO_ROOT/$PROJECT_DIR"

echo "==> Bundling image ${IMAGE_SRC} as ${IMAGE_DST} (both archs)"

# Docker Hub 匿名拉取限流重试（ref 可以是 name:tag 或 name@digest）
pull_retry() {
  local ref="$1"
  for i in 1 2 3; do
    if docker pull "$ref"; then return 0; fi
    echo "pull $ref attempt $i failed, retrying in 20s..." >&2
    sleep 20
  done
  echo "::error::docker pull $ref failed after retries" >&2
  exit 1
}

# 从 IMAGE_SRC 的 manifest 列表里取 linux/<arch> 的单平台 manifest digest。
# tag 本身就是单 manifest（无 manifests 键）时输出空串——调用方回退按原 ref 拉取。
resolve_manifest_digest() {
  local arch="$1" raw
  raw=$(docker buildx imagetools inspect "$IMAGE_SRC" --raw) || {
    echo "::error::docker buildx imagetools inspect 失败，无法解析 ${IMAGE_SRC} 的 manifest 列表" >&2
    exit 1
  }
  printf '%s' "$raw" | python3 -c '
import json, sys
m = json.load(sys.stdin)
arch, src = sys.argv[1], sys.argv[2]
entries = m.get("manifests")
if entries is None:  # 单 manifest，tag 本身即目标平台
    sys.exit(0)
hit = [x["digest"] for x in entries
       if x.get("platform", {}).get("os") == "linux"
       and x.get("platform", {}).get("architecture") == arch]
if len(hit) != 1:
    sys.exit(f"::error::{src} 的 manifest 列表里 linux/{arch} 期望 1 项，实际 {len(hit)} 项")
print(hit[0])
' "$arch" "$IMAGE_SRC"
}

# 内容版本断言（两个架构都做）：镜像 label 里的版本必须与目标版本一致——
# 上游把同一 tag 重推成别的内容、或哪天不烘焙 label 了，这里立即拦下
assert_version_label() {
  local got
  got=$(docker image inspect "$IMAGE_DST" \
    --format '{{index .Config.Labels "org.opencontainers.image.version"}}' | sed 's/^v//')
  [ "$got" = "$VERSION" ] || {
    echo "::error::镜像 label 版本(${got:-空})与目标版本 ${VERSION} 不符 —— 上游可能重推了 tag 或改了构建方式，请核对" >&2
    exit 1
  }
}

assert_arch() {
  local expected="$1" got
  got=$(docker image inspect "$IMAGE_DST" --format '{{.Architecture}}')
  [ "$got" = "$expected" ] || {
    echo "::error::期望 ${expected} 镜像,实际 '${got}'" >&2
    exit 1
  }
}

mkdir -p "$APPDIR/rootfs_amd64/images" "$APPDIR/rootfs_arm64/images"

# 拉取指定平台并改标成 personal/1panel:<version>。
# digest 指向单平台 manifest，tag 语义无歧义；pull 不需要 qemu（不运行容器）。
pull_platform() {
  local arch="$1" digest ref
  digest=$(resolve_manifest_digest "$arch")
  ref="$IMAGE_SRC"
  [ -n "$digest" ] && ref="${IMAGE_SRC%:*}@${digest}"
  echo "==> [$arch] pull $ref"
  pull_retry "$ref"
  docker tag "$ref" "$IMAGE_DST"
}

# ---- amd64 ----
pull_platform amd64
assert_arch amd64
assert_version_label
docker save -o "$APPDIR/rootfs_amd64/images/1panel-${VERSION}-amd64.tar" "$IMAGE_DST"

# ---- arm64 ----
pull_platform arm64
assert_arch arm64
assert_version_label
docker save -o "$APPDIR/rootfs_arm64/images/1panel-${VERSION}-arm64.tar" "$IMAGE_DST"

# ---- compose 里的镜像 tag 跟着版本走(与 project.yaml 的 sed 同机制) ----
# 写法用 sed -E 重定向 + mv：macOS 的 BSD sed 下 `sed -i -E` 会把 -E 当成
# 备份后缀，留下 compose.yaml-E 垃圾文件害 ugcli check 挂掉
sed -E "s#image: personal/1panel:.*#image: personal/1panel:${VERSION}#" \
  "$APPDIR/rootfs_common/docker-compose.yaml" > "$APPDIR/rootfs_common/docker-compose.yaml.tmp"
mv "$APPDIR/rootfs_common/docker-compose.yaml.tmp" "$APPDIR/rootfs_common/docker-compose.yaml"
grep -n "image: personal/1panel" "$APPDIR/rootfs_common/docker-compose.yaml"

ls -lh "$APPDIR"/rootfs_*/images/
echo "==> Done: both image tars ready (request arch: $ARCH)"
