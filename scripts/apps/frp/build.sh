#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 0.71.1> <arch: amd64|arm64>
#
# 上游资产:https://github.com/fatedier/frp/releases/download/v<ver>/
#   frp_<ver>_linux_<arch>.tar.gz   ← 官方静态 Go 二进制(frpc/frps),原样使用
#
# 包结构(frp-ugreen-app 独立仓库同步而来,双架构容器冒烟 57 项断言全绿):
#   - start_cmd 是自研 Go 管理壳 bin/frp-mgr(--port=28600),不是 shell 脚本。
#     它托管 frpc/frps 子进程(守护循环+退避重启)、提供网页管理(登录/配置
#     编辑带 frp verify 校验/日志),tab 直连形态。
#   - 子进程 env 由管理壳剥 GODEBUG(固件 DefaultEnvironment 注入的
#     tlskyber=0 对 go1.27+ 二进制 fatal;管理壳自身钉 go1.26 线编译)。
#   - frp 0.71.1 上游是 go1.26.8 编译,静态链接零运行时依赖,沙箱可用。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APPDIR="$REPO_ROOT/$PROJECT_DIR"
ROOTFS="$APPDIR/rootfs_${ARCH}"
COMMON="$APPDIR/rootfs_common"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64" ;;
  arm64) ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building frp ${VERSION} (${ARCH})"

# ---- 1. 上游 frpc/frps(官方静态 Go 二进制) ----
ASSET="frp_${VERSION}_linux_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/fatedier/frp/releases/download/v${VERSION}/${ASSET}"
echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL --http1.1 --retry 5 --retry-all-errors --retry-delay 3 \
  --connect-timeout 20 -o "$WORK_DIR/frp.tar.gz" "$DOWNLOAD_URL"

mkdir -p "$WORK_DIR/extracted"
tar xzf "$WORK_DIR/frp.tar.gz" -C "$WORK_DIR/extracted"

SRC_DIR="$WORK_DIR/extracted/frp_${VERSION}_linux_${ARCH}"
for b in frpc frps; do
  [ -f "$SRC_DIR/$b" ] || { echo "::error::$b not found in $ASSET" >&2; ls -lR "$WORK_DIR/extracted" >&2; exit 1; }
done

mkdir -p "$ROOTFS/bin"
install -m 0755 "$SRC_DIR/frpc" "$ROOTFS/bin/frpc"
install -m 0755 "$SRC_DIR/frps" "$ROOTFS/bin/frps"

# ---- 2. Go 工具链(钉死版本,同 natfrp/dnet;GOTOOLCHAIN=local 禁自动拉链) ----
GO_VERSION="1.26.6"

HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac
GO_TARBALL="go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
echo "==> Installing Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
curl -fL --http1.1 --retry 5 --retry-all-errors --retry-delay 3 \
  --connect-timeout 20 -o "$WORK_DIR/${GO_TARBALL}" "https://go.dev/dl/${GO_TARBALL}"
tar xzf "$WORK_DIR/${GO_TARBALL}" -C "$WORK_DIR"
export GOROOT="$WORK_DIR/go"
export PATH="$GOROOT/bin:$PATH"
export GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0
go version

# ---- 3. 管理壳(随仓库源码,先 vet+test 再交叉编译) ----
echo "==> 管理壳测试(vet + test)"
( cd "$SCRIPT_DIR/launcher" && go vet ./... && go test ./... )
echo "==> [$ARCH] 编译管理壳 linux/$ARCH"
(
  cd "$SCRIPT_DIR/launcher"
  GOOS=linux GOARCH="$ARCH" \
    go build -trimpath \
    -ldflags="-s -w -X main.AppVersion=${VERSION}" \
    -o "$ROOTFS/bin/frp-mgr" .
)
chmod +x "$ROOTFS/bin/frp-mgr"

# 管理页单源:launcher 用 go:embed 打包 www/,rootfs_common/www 只是 tab 形态
# 的占位页(独立提交),无需同步;但要确保两边素材同源的是图标(favicon)。
[ -f "$SCRIPT_DIR/launcher/www/icon.png" ] || {
  echo "::error::缺少 $SCRIPT_DIR/launcher/www/icon.png —— 内嵌 favicon 素材丢了" >&2; exit 1;
}
cmp -s "$SCRIPT_DIR/launcher/www/icon.png" "$COMMON/icon.png" || {
  echo "::error::rootfs_common/icon.png 与 launcher/www/icon.png 不同源,重新 cp 一次" >&2; exit 1;
}

# ---- 4. 架构断言 ----
assert_elf() {
  file "$1" | grep -qi 'ELF 64-bit' || { echo "::error::$1 不是 ELF: $(file -b "$1")" >&2; exit 1; }
  file "$1" | grep -qi "$ELF_MACHINE" || {
    echo "::error::$1 架构不是 ${ELF_MACHINE}: $(file -b "$1")" >&2; exit 1;
  }
  file "$1" | grep -qi 'statically linked' || {
    echo "::error::$1 不是静态链接: $(file -b "$1")" >&2; exit 1;
  }
}
assert_elf "$ROOTFS/bin/frpc"
assert_elf "$ROOTFS/bin/frps"
assert_elf "$ROOTFS/bin/frp-mgr"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
