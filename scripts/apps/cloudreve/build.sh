#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 4.19.1> <arch: amd64|arm64>
#
# 上游资产：https://github.com/cloudreve/cloudreve/releases/download/<ver>/
#   cloudreve_<ver>_linux_<arch>.tar.gz   ← 单个自包含二进制（前端 go:embed 在里面）
#
# 为什么不用源码编译：上游 release 就是把 assets/build 打进 assets.zip、
# go:embed 进二进制后交叉编译出来的产物，和官方 Docker 镜像同一份；
# 自己编要 Node + yarn 构建前端（assets/ 目录在下游仓库里是空的），纯属重复劳动。
#
# 上游用的是 modernc.org/sqlite（纯 Go SQLite），CGO_ENABLED=0 即可，
# 产物应当是静态链接、零运行时依赖 —— 沙箱根目录视图极窄，这点很关键。
# build.sh 对此有硬断言：一旦上游改成动态链接，这里当场失败，而不是等装到
# NAS 上报 "no such file or directory"（找不到 interpreter）。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
COMMON="$REPO_ROOT/$PROJECT_DIR/rootfs_common"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64" ;;
  arm64) ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building Cloudreve ${VERSION} (${ARCH})"

ASSET="cloudreve_${VERSION}_linux_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/cloudreve/cloudreve/releases/download/${VERSION}/${ASSET}"
echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL --retry 3 --retry-all-errors --connect-timeout 20 -o "$WORK_DIR/cloudreve.tar.gz" "$DOWNLOAD_URL"

tar xzf "$WORK_DIR/cloudreve.tar.gz" -C "$WORK_DIR"

# 上游 tar 里就是根目录一个 cloudreve（历史版本可能套一层目录，这里都兼容）
BIN_SRC=$(find "$WORK_DIR" -maxdepth 3 -type f -name cloudreve | head -1)
[ -n "$BIN_SRC" ] || { echo "cloudreve binary not found in $ASSET" >&2; ls -lR "$WORK_DIR" >&2; exit 1; }

echo "==> Verifying ELF (expect Linux ${ELF_MACHINE})"
file "$BIN_SRC"
file "$BIN_SRC" | grep -q "$ELF_MACHINE" || {
  echo "::error::产物架构不是 ${ELF_MACHINE} —— 上游资产可能换名/换布局了" >&2
  exit 1
}

# 静态链接是能直接塞进原生沙箱的前提（见文件头注释）
file "$BIN_SRC" | grep -q 'statically linked' || {
  echo "::error::cloudreve 不再是静态链接 —— 需要随包带 glibc 闭包，先别发版" >&2
  exit 1
}

# 前端是 go:embed 进二进制的。上游若改成"前端外置/lite 版"，体积会腰斩，
# 这里直接拦住，避免打出一个能启动但页面白屏的包。
BIN_SIZE=$(wc -c < "$BIN_SRC" | tr -d ' ')
if [ "$BIN_SIZE" -lt 20000000 ]; then
  echo "::error::二进制只有 ${BIN_SIZE} 字节（< 20MB），前端可能没有被 embed 进二进制" >&2
  exit 1
fi

mkdir -p "$ROOTFS/bin"
cp "$BIN_SRC" "$ROOTFS/bin/cloudreve"
chmod +x "$ROOTFS/bin/cloudreve"

# 不随上游版本变化的启动脚本，注释见文件本身
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

# 沙箱里没有系统 CA 根证书（skill: native-sandbox），Cloudreve 对接 OneDrive /
# S3 / WebDAV 等远端存储走 HTTPS 时必须自带一份，start.sh 里设 SSL_CERT_FILE。
# 证书包是固定素材，直接随 rootfs_common 提交（与 icon.png 同性质），不在这里生成。
[ -f "$COMMON/ca/ca-certificates.crt" ] || {
  echo "::error::rootfs_common/ca/ca-certificates.crt 缺失 —— HTTPS 存储端会报 x509 错误" >&2
  exit 1
}

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
