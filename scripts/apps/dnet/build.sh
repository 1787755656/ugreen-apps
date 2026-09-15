#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 3.0.1> <arch: amd64|arm64>
#
# D-NET (cxbdasheng/dnet) 是纯 Go 单二进制，WebUI 的 html/js/字体全部
# go:embed 进仓库（web/*.go 的 //go:embed），所以【不需要 Node/npm】，
# 只要一个钉死版本的 Go 工具链交叉编译即可。
#
# 两处有意的适配（都不改上游源码）：
#   1. 端口走 -l 参数改到 19877（上游默认 9877 —— 用户很可能已经在
#      Docker 里跑着 dnet，那边发布的就是 9877），由随仓库的
#      launcher（Go 管理壳）拼参数启动。
#   2. 配置文件走 -c 参数指到 UGAPP_DATA_DIR（上游默认写
#      os.UserHomeDir()，沙箱里落到不可写位置）。
# launcher 还负责 TMPDIR 重定向（沙箱无 /tmp）与守护（SIGTERM 转发、
# 意外退出自动重启、快退保护）。start_cmd 直接是编译好的二进制，
# 不经 shell，无需 SYSTEM.EXEC_SYSTEM_COMMAND 权限。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

# 钉死 Go 版本：上游 go.mod 要求 go 1.26.6，且我们用 GOTOOLCHAIN=local
# 禁止 go 自己去拉工具链（网络抖动时会变成难懂的失败）。
# 升级时改这一处，同时确认 >= 上游 go.mod 的要求。
GO_VERSION="1.26.6"
# 上游 tag 带 v 前缀
UPSTREAM_TAG="v${VERSION}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APPDIR="$REPO_ROOT/$PROJECT_DIR"
ROOTFS="$APPDIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64" ;;
  arm64) ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building D-NET ${VERSION} (linux/${ARCH}) from tag ${UPSTREAM_TAG}"

# ---- 1. 上游源码（tag tarball）----
SRC_URL="https://github.com/cxbdasheng/dnet/archive/refs/tags/${UPSTREAM_TAG}.tar.gz"
echo "==> Downloading source: ${SRC_URL}"
# 本地/CI 都可能碰到 HTTP2 framing 或直连抖动：退 HTTP/1.1 + 显式重试。
# CI 上如需代理，走标准环境变量（HTTPS_PROXY）即可，curl 会自动用。
curl -fL --http1.1 --retry 5 --retry-all-errors --retry-delay 3   --connect-timeout 20 -o "$WORK_DIR/src.tar.gz" "$SRC_URL"
mkdir -p "$WORK_DIR/src"
tar xzf "$WORK_DIR/src.tar.gz" -C "$WORK_DIR/src" --strip-components=1

# 预构建产物断言：前端全部 go:embed 进二进制，但 embed 一个空目录不会报错，
# 上游哪天把 html/js 挪出仓库，这里要当场发现而不是打出一个空壳包。
for f in web/login.html web/home.html web/server.go static/layui.js static/common.js favicon.ico; do
  [ -f "$WORK_DIR/src/$f" ] || {
    echo "::error::上游源码缺少 $f —— 前端可能改为构建时生成，需要在 build.sh 里补 npm 步骤" >&2
    exit 1
  }
done

# ---- 2. Go 工具链 ----
# 按【宿主】的 OS/架构取工具链（目标架构由下面的 GOARCH 决定，两者无关）。
# CI 上宿主永远是 linux-amd64；自动探测是为了在 macOS 上也能直接跑本地验证。
HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac
GO_TARBALL="go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
echo "==> Installing Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
curl -fL --http1.1 --retry 5 --retry-all-errors --retry-delay 3   --connect-timeout 20 -o "$WORK_DIR/${GO_TARBALL}" "https://go.dev/dl/${GO_TARBALL}"
tar xzf "$WORK_DIR/${GO_TARBALL}" -C "$WORK_DIR"
export GOROOT="$WORK_DIR/go"
export PATH="$GOROOT/bin:$PATH"
export GOWORK=off GOTOOLCHAIN=local
go version

# ---- 3. 交叉编译上游 dnet ----
echo "==> go build dnet (CGO off)"
mkdir -p "$ROOTFS/bin"
(
  cd "$WORK_DIR/src"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${UPSTREAM_TAG}" \
    -o "$ROOTFS/bin/dnet" .
)
chmod +x "$ROOTFS/bin/dnet"

# ---- 4. 管理壳 launcher（随仓库源码，先 vet+test 再编译）----
echo "==> 管理壳测试（vet + test）"
( cd "$SCRIPT_DIR/launcher" && go vet ./... && go test ./... )
echo "==> [$ARCH] 编译管理壳 linux/$ARCH"
(
  cd "$SCRIPT_DIR/launcher"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
    go build -trimpath -ldflags="-s -w" \
    -o "$ROOTFS/bin/dnet_launcher" .
)
chmod +x "$ROOTFS/bin/dnet_launcher"

# ---- 5. 架构断言 ----
echo "==> Verifying ELF (expect Linux ${ELF_MACHINE})"
assert_elf() {
  file "$1" | grep -qi 'ELF 64-bit' || { echo "::error::$1 不是 ELF: $(file -b "$1")" >&2; exit 1; }
  file "$1" | grep -qi "$ELF_MACHINE" || {
    echo "::error::$1 架构不是 ${ELF_MACHINE}: $(file -b "$1")" >&2; exit 1;
  }
}
assert_elf "$ROOTFS/bin/dnet"
assert_elf "$ROOTFS/bin/dnet_launcher"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
