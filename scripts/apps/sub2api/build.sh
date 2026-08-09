#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 0.1.173> <arch: amd64|arm64>
#
# Sub2API 官方发布的是预编译静态单二进制：
#   sub2api_<version>_linux_<arch>.tar.gz  （内含根级 sub2api）
# 无需 Go 工具链 / 交叉编译 —— 直接下载 + sha256 校验（checksums.txt）即可。
#
# 两个注意点：
#   1. VERSION 传的是【完整上游版本】（0.1.173），不是 project.yaml 的映射
#      版本（0.11.73）—— 资产名用的是上游原版号码。
#   2. 沙箱没有 /usr/share/zoneinfo，start.sh 里 export ZONEINFO=$APP_ROOT/zoneinfo.zip，
#      所以 rootfs_common/zoneinfo.zip 必须在（git 里已提交）。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
PROJECT_ROOT="$REPO_ROOT/$PROJECT_DIR"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64" ;;
  arm64) ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

# ---- 必需静态资产检查（rootfs_common 由 git 提交，若缺说明 clone 有问题）----
for f in rootfs_common/zoneinfo.zip rootfs_common/icon.png; do
  [ -f "$PROJECT_ROOT/$f" ] || { echo "::error::$f 缺失" >&2; exit 1; }
done

# ---- 1. 下载官方 release 资产 -------------------------------------------
DOWNLOAD_DIR="releases/download/v${VERSION}"
TARBALL="sub2api_${VERSION}_linux_${ARCH}.tar.gz"
URL="https://github.com/Wei-Shaw/sub2api/${DOWNLOAD_DIR}/${TARBALL}"
echo "==> Downloading ${URL}"
curl -fL -o "$WORK_DIR/$TARBALL" "$URL"
curl -fL -o "$WORK_DIR/checksums.txt" \
  "https://github.com/Wei-Shaw/sub2api/${DOWNLOAD_DIR}/checksums.txt"

# ---- 2. sha256 校验（防下载损坏 / 上游发布被篡改）------------------------
EXPECTED=$(awk -v f="$TARBALL" '$2==f {print $1}' "$WORK_DIR/checksums.txt")
if [ -z "$EXPECTED" ]; then
  echo "::error::checksums.txt 里找不到 $TARBALL —— 上游资产可能改名了" >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then SHA_CMD="sha256sum"; else SHA_CMD="shasum -a 256"; fi
ACTUAL=$($SHA_CMD "$WORK_DIR/$TARBALL" | awk '{print $1}')
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "::error::sha256 校验失败 ${TARBALL}（期望 $EXPECTED，实际 $ACTUAL）" >&2
  exit 1
fi
echo "==> sha256 OK: $EXPECTED"

# ---- 3. 解压 + 装入 rootfs ----------------------------------------------
mkdir -p "$ROOTFS/bin"
tar xzf "$WORK_DIR/$TARBALL" -C "$WORK_DIR" sub2api
chmod +x "$WORK_DIR/sub2api"
cp "$WORK_DIR/sub2api" "$ROOTFS/bin/sub2api"

# start.sh 是固定包装脚本（supervisor 循环 + env 注入），随 git 走。
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

# 沙箱 /usr/share/zoneinfo 不存在，zoneinfo.zip 放 rootfs_common 顶层，
# start.sh 里 ZONEINFO=$APP_ROOT/zoneinfo.zip（APP_ROOT=rootfs 合并根）。
[ -f "$PROJECT_ROOT/rootfs_common/zoneinfo.zip" ] || {
  echo "::error::rootfs_common/zoneinfo.zip 缺失" >&2; exit 1;
}

# ---- 4. ELF 架构校验 -------------------------------------------------------
echo "==> Verifying ELF (expect Linux ${ELF_MACHINE})"
file "$ROOTFS/bin/sub2api"
file "$ROOTFS/bin/sub2api" | grep -q "$ELF_MACHINE" || {
  echo "::error::产物架构不是 ${ELF_MACHINE} —— 可能下错了资产" >&2
  exit 1
}

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"