#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 0.4.6-Beta> <arch: amd64|arm64>
#
# LitePan 是纯 Go 单二进制，前端已由上游预构建并 go:embed 进仓库
# （internal/api/web，`//go:embed web` 见 internal/api/router.go），
# 所以这里【不需要 Node/npm】，只要一个 Go 工具链交叉编译即可。
#
# 上游没有可用的 tag（见 get-latest-version.sh），一律从 main 分支源码构建。
#
# 两处和别的 app 不一样，都是有意的：
#   1. 不带 `fuse` build tag —— 原生沙箱打不开 /dev/fuse（设备节点可见但 open 被拒），
#      带上只会在运行时失败。上游 internal/share/fuse 有 compiled_stub.go 兜底，
#      功能整体停用而不是崩。
#   2. 编译前把 static/ugos_env.go 拷进 cmd/litepan/ —— 一个只设环境变量的 init()，
#      把数据目录/STRM 目录/TMPDIR 对到平台注入的 UGAPP_*。上游仓库不打 patch。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

# 钉死 Go 版本：上游 go.mod 要求 go 1.26.4，且我们用 GOTOOLCHAIN=local
# 禁止 go 自己去拉工具链（网络抖动时会变成难懂的失败）。
# 升级时改这一处，同时确认 >= 上游 go.mod 的要求。
GO_VERSION="1.26.5"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64" ;;
  arm64) ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building LitePan ${VERSION} (linux/${ARCH}) from main branch"

# ---- 1. 上游源码（main 分支 tarball）----
SRC_URL="https://github.com/Ponphil/LitePan/archive/refs/heads/main.tar.gz"
echo "==> Downloading source: ${SRC_URL}"
curl -fL -o "$WORK_DIR/src.tar.gz" "$SRC_URL"
mkdir -p "$WORK_DIR/src"
tar xzf "$WORK_DIR/src.tar.gz" -C "$WORK_DIR/src" --strip-components=1

# 版本探测和这次下载之间 main 可能已经移动过 —— 原地复核一遍。
SRC_VERSION=$(sed -nE 's/.*AppVersion[[:space:]]*=[[:space:]]*"v?([^"]+)".*/\1/p' \
  "$WORK_DIR/src/internal/httpx/user_agent.go" | head -1)
if [ "$SRC_VERSION" != "$VERSION" ]; then
  echo "::warning::main branch AppVersion ($SRC_VERSION) != resolved version ($VERSION); packaging $SRC_VERSION"
fi

# 前端必须是预构建好的 —— 上游哪天改成"构建时才生成"，这里要当场发现，
# 否则会打出一个前端为空的包（go:embed 一个空目录不报错）。
[ -f "$WORK_DIR/src/internal/api/web/index.html" ] || {
  echo "::error::internal/api/web/index.html 不存在 —— 上游可能不再预构建前端，需要在这里补 npm 构建步骤" >&2
  exit 1
}

# ---- 2. Go 工具链 ----
# 按【宿主】的 OS/架构取工具链（目标架构由下面的 GOARCH 决定，两者无关）。
# CI 上宿主永远是 linux-amd64；这里做自动探测纯粹是为了在 macOS 上也能
# 直接跑这个脚本做本地验证 —— 本仓库其它 app 的 build.sh 都能本地跑通，
# 保持一致。
HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac
GO_TARBALL="go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
echo "==> Installing Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
curl -fL -o "$WORK_DIR/${GO_TARBALL}" "https://go.dev/dl/${GO_TARBALL}"
tar xzf "$WORK_DIR/${GO_TARBALL}" -C "$WORK_DIR"
export GOROOT="$WORK_DIR/go"
export PATH="$GOROOT/bin:$PATH"
go version

# ---- 3. UGOS 适配层 ----
# 上游仓库保持原样，只往 package main 里加一个带 init() 的文件。
cp "$SCRIPT_DIR/static/ugos_env.go" "$WORK_DIR/src/cmd/litepan/ugos_env.go"

# ---- 4. 交叉编译 ----
echo "==> go build (CGO off, no fuse tag)"
mkdir -p "$ROOTFS/bin"
(
  cd "$WORK_DIR/src"
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 \
    GOOS=linux GOARCH="$ARCH" \
    go build -trimpath -ldflags="-s -w" -o "$ROOTFS/bin/litepan" ./cmd/litepan
)
chmod +x "$ROOTFS/bin/litepan"

echo "==> Verifying ELF (expect Linux ${ELF_MACHINE})"
file "$ROOTFS/bin/litepan"
file "$ROOTFS/bin/litepan" | grep -q "$ELF_MACHINE" || {
  echo "::error::产物架构不是 ${ELF_MACHINE} —— 交叉编译可能静默退回了宿主架构" >&2
  exit 1
}

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
