#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 4.2.4> <arch: amd64|arm64>
#
# 上游资产：https://github.com/OpenListTeam/OpenList/releases/download/v<ver>/
#   openlist-linux-musl-<arch>.tar.gz     ← 用这个（静态链接）
#   openlist-linux-<arch>.tar.gz          ← 动态链接（glibc），不用
#   openlist-linux-musl-<arch>-lite.tar.gz ← lite 版不内嵌前端，会去 CDN 拉，不用
#
# 为什么用 musl 版（file 实测）：
#   openlist-linux-arm64       → dynamically linked, interpreter /lib/ld-linux-aarch64.so.1
#   openlist-linux-musl-arm64  → statically linked
# 原生应用沙箱的根目录只有 8 项、库视图很窄，静态二进制零运行时依赖最省心；
# 而且沙箱里【没有 /etc/hosts】，musl 的 resolver 内置了 localhost 特例，
# 比动态 glibc 更不容易在自连场景上翻车。
#
# 为什么不用 -lite：lite 版把 Web 前端剥掉、运行时从公网 CDN 拉，NAS 上没外网
# 或走内网访问时页面直接白屏。完整版体积大（解压后约 118MB）但自带前端。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Building OpenList ${VERSION} (${ARCH})"

ASSET="openlist-linux-musl-${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/OpenListTeam/OpenList/releases/download/v${VERSION}/${ASSET}"
echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL --retry 3 --retry-all-errors -o "$WORK_DIR/openlist.tar.gz" "$DOWNLOAD_URL"

tar xzf "$WORK_DIR/openlist.tar.gz" -C "$WORK_DIR"
[ -f "$WORK_DIR/openlist" ] || { echo "openlist binary not found in $ASSET" >&2; exit 1; }

echo "==> Verifying ELF (expect statically linked Linux ${ARCH})"
file "$WORK_DIR/openlist"
case "$ARCH" in
  amd64) file "$WORK_DIR/openlist" | grep -q 'x86-64' ;;
  arm64) file "$WORK_DIR/openlist" | grep -q 'aarch64' ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac
# 静态链接是选 musl 资产的全部理由 —— 上游哪天改了构建方式就该当场发现，
# 而不是等装到 NAS 上报 "No such file or directory"（找不到 interpreter）。
file "$WORK_DIR/openlist" | grep -q 'statically linked' || {
  echo "::error::${ASSET} is no longer statically linked — check upstream build" >&2
  exit 1
}

mkdir -p "$ROOTFS/bin"
cp "$WORK_DIR/openlist" "$ROOTFS/bin/openlist"
chmod +x "$ROOTFS/bin/openlist"

# 不随上游版本变化的启动脚本，注释见文件本身
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
