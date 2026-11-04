#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 48.4> <arch: amd64|arm64>
#
# SmartDNS 是【Docker 型应用】：把 pymumu/smartdns 官方 linux-all 发行包
# （musl 自包含：相对路径 interpreter lib/ld-musl-*.so.1 + lib/ 运行库 +
# smartdns_ui.so WebUI 插件）重组进 alpine 底派生镜像（全程 COPY 无 RUN，
# amd64 runner 上构建 arm64 镜像不需要 QEMU/binfmt），镜像 tar 打进 upk
# 离线加载。entrypoint 首次启动生成默认配置（admin/password，见应用介绍），
# 已有配置（升级/自选数据目录）绝不覆盖。
#
# ⚠ ugcli check 对 support_arch 声明的每个架构都要求 images/ 里有镜像 tar
#   （"missing image tar for service"），而 CI matrix 每架构一个 job——
#   所以这里不看 ARCH 参数、一次把两个架构的 tar 都备齐（1panel 惯例）。
#   ARCH 参数只用于日志。
#
# 上游资产名带日期戳（smartdns.1.YYYY.MM.DD-HHMM.<arch>-linux-all.tar.gz），
# 从 tag 推不出文件名：URL 经 GitHub API release assets 端点做 endswith 匹配
# （x86_64 与 32 位 x86 资产靠 endswith 天然区分）。
#
# 断言（内容不明不出包）：
#   - 派生镜像 Architecture 字段 == 目标架构
#   - 二进制里嵌的版本串（ReleaseX.Y）== 目标上游版本
#   - entrypoint 与 wwwroot 前端确实进了镜像
#
# 运行时网络（host 模式）：容器进程直接绑定宿主 53/6153，无 NAT 层、
# 不占 docker 地址池（池耗尽的机器上也能装，xwyun 实测）。库解析走镜像
# 内 ENV LD_LIBRARY_PATH=/app/smartdns/lib——dlopen 的 $ORIGIN 依赖
# /proc/self/exe，在 UGOS 原生沙箱里不可靠（间歇性 ENOENT，见原生版
# patchelf 教训），容器里显式给 env 是确定性方案，CI 无需 patchelf。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"
UPSTREAM_TAG="Release${VERSION}"
IMAGE_DST="personal/smartdns:${VERSION}"
ALPINE_BASE="alpine:3.20"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APPDIR="$REPO_ROOT/$PROJECT_DIR"
DOCKER_DIR="$SCRIPT_DIR/docker"
mkdir -p "$APPDIR/rootfs_amd64/images" "$APPDIR/rootfs_arm64/images"

WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

GH_AUTH=()
[ -n "${GH_TOKEN:-}" ] && GH_AUTH=(-H "Authorization: Bearer $GH_TOKEN")

asset_url() {
  local sd_arch="$1"
  for i in 1 2 3; do
    URL=$(curl -sL ${GH_AUTH[@]+"${GH_AUTH[@]}"} "https://api.github.com/repos/pymumu/smartdns/releases/tags/${UPSTREAM_TAG}" | \
      jq -r --arg suffix "${sd_arch}-linux-all.tar.gz" \
        '.assets[] | select(.name | endswith($suffix)) | .browser_download_url' | head -1)
    [ -n "$URL" ] && [ "$URL" != "null" ] && { echo "$URL"; return 0; }
    echo "asset resolve attempt $i failed for ${sd_arch}, retrying in 20s..." >&2
    sleep 20
  done
  echo "::error::no ${sd_arch}-linux-all.tar.gz asset found in ${UPSTREAM_TAG}" >&2
  exit 1
}

pull_retry() {
  local platform="$1" image="$2"
  for i in 1 2 3; do
    if docker pull --platform "linux/$platform" "$image"; then return 0; fi
    echo "pull linux/$platform $image attempt $i failed, retrying in 20s..." >&2
    sleep 20
  done
  echo "::error::docker pull --platform linux/$platform $image failed after retries" >&2
  exit 1
}

build_one() {
  local platform="$1"
  case "$platform" in
    amd64) SD_ARCH="x86_64" ;;
    arm64) SD_ARCH="aarch64" ;;
    *) echo "::error::bad platform $platform" >&2; exit 1 ;;
  esac

  echo "==> [linux/$platform] resolve + download upstream tarball"
  URL=$(asset_url "$SD_ARCH")
  echo "    asset: ${URL##*/}"
  curl -fsSL --retry 3 -o "$WORK_DIR/smartdns-$platform.tar.gz" "$URL"

  mkdir -p "$WORK_DIR/extracted-$platform"
  tar xzf "$WORK_DIR/smartdns-$platform.tar.gz" -C "$WORK_DIR/extracted-$platform"
  PAYLOAD="$WORK_DIR/extracted-$platform/smartdns"
  [ -f "$PAYLOAD/usr/local/lib/smartdns/smartdns" ] || { echo "::error::smartdns binary missing in tarball" >&2; exit 1; }
  [ -f "$PAYLOAD/usr/local/lib/smartdns/smartdns_ui.so" ] || { echo "::error::smartdns_ui.so missing in tarball" >&2; exit 1; }
  [ -d "$PAYLOAD/usr/share/smartdns/wwwroot" ] || { echo "::error::wwwroot missing in tarball" >&2; exit 1; }

  echo "==> [linux/$platform] assemble context + buildx build"
  CTX="$WORK_DIR/context-$platform"
  mkdir -p "$CTX"
  cp "$DOCKER_DIR/Dockerfile" "$CTX/Dockerfile"
  cp "$DOCKER_DIR/entrypoint.sh" "$CTX/entrypoint.sh"
  cp -R "$PAYLOAD/usr/local/lib/smartdns" "$CTX/smartdns"
  cp -R "$PAYLOAD/usr/share/smartdns/wwwroot" "$CTX/wwwroot"
  cp "$PAYLOAD/etc/smartdns/smartdns.conf" "$CTX/smartdns.conf.sample"
  chmod +x "$CTX/entrypoint.sh"

  pull_retry "$platform" "$ALPINE_BASE"
  docker buildx build --platform "linux/$platform" -t "$IMAGE_DST" --load "$CTX"

  # 架构断言（tag 刚构建完即验，防止两架构互相覆盖时看错）
  local got_arch
  got_arch=$(docker image inspect "$IMAGE_DST" --format '{{.Architecture}}')
  [ "$got_arch" = "$platform" ] || {
    echo "::error::派生镜像架构 '${got_arch}' != linux/$platform" >&2
    exit 1
  }

  # 版本断言：二进制内嵌版本串必须等于目标上游版本（上游重推 tag 会在此暴露）
  local bin=/tmp/sd-bin-"$platform"
  cid=$(docker create --platform "linux/$platform" "$IMAGE_DST")
  docker cp "$cid:/app/smartdns/smartdns" "$bin"; docker rm "$cid" >/dev/null
  grep -aq "Release${VERSION}" "$bin" || {
    echo "::error::二进制里找不到版本串 Release${VERSION} —— 上游可能重推了 tag" >&2
    exit 1
  }
  rm -f "$bin"

  # entrypoint 断言：默认配置生成逻辑必须在镜像里
  cid=$(docker create --platform "linux/$platform" "$IMAGE_DST")
  docker cp "$cid:/app/entrypoint.sh" "$bin"; docker rm "$cid" >/dev/null
  grep -q "smartdns-ui" "$bin" || {
    echo "::error::镜像里缺 entrypoint（/app/entrypoint.sh）" >&2
    exit 1
  }
  rm -f "$bin"

  docker save -o "$APPDIR/rootfs_${platform}/images/smartdns-${VERSION}.tar" "$IMAGE_DST"
  echo "==> [linux/$platform] saved"
}

build_one amd64
build_one arm64

# compose 镜像 tag 跟着版本走。sed 重定向 + mv：macOS 的 BSD sed 下
# `sed -i -E` 会把 -E 当备份后缀留下垃圾文件害 check 挂掉（1panel 同款坑）。
COMPOSE="$APPDIR/rootfs_common/docker-compose.yaml"
sed -E "s|^    image: .*|    image: \"${IMAGE_DST}\"|" "$COMPOSE" > "${COMPOSE}.tmp" && mv "${COMPOSE}.tmp" "$COMPOSE"
grep -n "image:" "$COMPOSE"

echo "==> Done"
ls -lh "$APPDIR"/rootfs_*/images/
