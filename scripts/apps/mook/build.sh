#!/usr/bin/env bash
# 组装 Mook 的 rootfs（CI 用；ugcli check/pack 由可复用 workflow 统一跑）。
#
# 用法：build.sh <版本号> <架构>    # 架构：arm64 / amd64
#
# 上游 NikoYomi/mook（Go 后端 + React/Vite 前端，MIT）。
# v0.4.6 起上游原生支持「直连端口 / 飞牛网关」两种形态，【不需要】 v0.2.8 时代的
# 3 处本地补丁（router 双路径 / TerminalTab / HashRouter）：
#   - 前端经后端注入的 <base href> 推导访问前缀，WebSocket 走 /ws/terminal（与后端
#     唯一注册的 WS 路由一致），BrowserRouter + SPA 回退在直连端口下正常工作。
#
# UGOS 沙箱适配（仍必须）：
#   - overlay/ugos_linux.go：嵌入 time/tzdata（沙箱无 /usr/share/zoneinfo）
#   - rootfs_common/certs/cacert.pem：Mozilla CA bundle（AI 接口 HTTPS 用）
#   - bin/start.sh：设 MOOK_PORT/MOOK_DATA/MOOK_DIST/SSL_CERT_FILE/TZ，建私有目录
#
# 前端构建产物架构无关，落到 rootfs_common/app/dist（两架构共用）；
# Go 后端二进制按目标架构交叉编译，落到 rootfs_<arch>/bin/mook。

set -euo pipefail

VERSION="${1:?用法：build.sh <版本号> <架构>}"
ARCH="${2:?用法：build.sh <版本号> <架构>}"
case "$ARCH" in
  arm64|amd64) ;;
  *) echo "!! 架构只支持 arm64 / amd64，收到：$ARCH" >&2; exit 1 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

PROJECT_DIR_ABS="$REPO_ROOT/$PROJECT_DIR"
ROOTFS="$PROJECT_DIR_ABS/rootfs_${ARCH}"
COMMON="$PROJECT_DIR_ABS/rootfs_common"
CACHE="${RUNNER_TEMP:-$SCRIPT_DIR/.build-cache}"
mkdir -p "$CACHE"

UPSTREAM_REPO="${UPSTREAM_REPO:-https://github.com/NikoYomi/mook.git}"
UPSTREAM_TAG="v${VERSION}"

# ---- 工具链（宿主 OS/arch 自动探测；Go 交叉编译目标架构，Node 产出架构无关）----
HOST_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"   # linux / darwin
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac
GO_VERSION="1.23.4"
NODE_VERSION="22.14.0"
NODE_ARCH="$([ "$HOST_ARCH" = "amd64" ] && echo x64 || echo arm64)"
NODE_DIR="node-v${NODE_VERSION}-${HOST_OS}-${NODE_ARCH}"

install_go() {
  if [ -x "$CACHE/go/bin/go" ]; then return; fi
  echo "==> Installing Go ${GO_VERSION} (${HOST_OS}/${HOST_ARCH})"
  curl -fL --retry 5 --retry-all-errors --retry-delay 3 -o "$CACHE/go.tar.gz" \
    "https://go.dev/dl/go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
  tar xzf "$CACHE/go.tar.gz" -C "$CACHE"
}
install_node() {
  if [ -x "$CACHE/$NODE_DIR/bin/node" ]; then return; fi
  echo "==> Installing Node ${NODE_VERSION} (${NODE_DIR})"
  curl -fL --retry 5 --retry-all-errors --retry-delay 3 -o "$CACHE/node.tar.xz" \
    "https://nodejs.org/dist/v${NODE_VERSION}/${NODE_DIR}.tar.xz"
  tar xf "$CACHE/node.tar.xz" -C "$CACHE"
}

install_go
install_node
export PATH="$CACHE/go/bin:$CACHE/$NODE_DIR/bin:$PATH"
export GOTOOLCHAIN=local
node --version
go version

# ---- 上游源码（按 tag 浅克隆；命中缓存且 tag 一致则复用）----
SRC="$CACHE/src"
if [ -d "$SRC/.git" ] && git -C "$SRC" describe --tags 2>/dev/null | grep -qx "$UPSTREAM_TAG"; then
  echo "==> reuse cached upstream at $UPSTREAM_TAG"
else
  rm -rf "$SRC"
  git clone --depth 1 --branch "$UPSTREAM_TAG" "$UPSTREAM_REPO" "$SRC"
fi
cd "$SRC"
BUILD_VER=$(git describe --tags 2>/dev/null || echo "$UPSTREAM_TAG")
echo "==> upstream source at ${BUILD_VER} (want ${UPSTREAM_TAG})"

# ---- 前端（架构无关，构建一次落到 rootfs_common/app/dist）----
if [ ! -f "$COMMON/app/dist/index.html" ]; then
  echo "==> Building frontend (npm ci + vite build)"
  ( cd "$SRC/frontend" && npm ci && npm run build )
  [ -f "$SRC/frontend/dist/index.html" ] || { echo "!! frontend dist 缺失" >&2; exit 1; }
  rm -rf "$COMMON/app/dist"
  mkdir -p "$COMMON/app/dist"
  cp -a "$SRC/frontend/dist/." "$COMMON/app/dist/"
  echo "    dist -> $COMMON/app/dist ($(du -sh "$COMMON/app/dist" | cut -f1))"
else
  echo "==> frontend dist 已存在，跳过前端构建"
fi

# ---- 后端 Go（交叉编译到目标架构；叠加 tzdata overlay）----
echo "==> Building backend (linux/${ARCH})"
cd "$SRC/backend"
cp "$SCRIPT_DIR/overlay/ugos_linux.go" ./ugos_linux.go
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags="-s -w" -o "$CACHE/mook-$ARCH" .
rm -f ./ugos_linux.go

# ---- 组装 rootfs_<arch> ----
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS/bin"
install -m 0755 "$CACHE/mook-$ARCH" "$ROOTFS/bin/mook"
install -m 0755 "$SCRIPT_DIR/start.sh" "$ROOTFS/bin/start.sh"

# ---- 断言 ----
echo "==> Verifying"
python3 - "$ROOTFS" "$ARCH" "$VERSION" <<'PY'
import os, struct, sys
rootfs, arch, version = sys.argv[1], sys.argv[2], sys.argv[3]
want = {'amd64': 0x3e, 'arm64': 0xb7}[arch]
def elf_machine(p):
    with open(p, 'rb') as f:
        h = f.read(20)
    if h[:4] != b'\x7fELF':
        return None
    return struct.unpack('<H', h[18:20])[0]
p = os.path.join(rootfs, 'bin', 'mook')
m = elf_machine(p)
if m is None:
    print("::error::mook 不是 ELF（构建失败？）"); sys.exit(1)
if m != want:
    print(f"::error::架构不对：mook 是 0x{m:x}，需要 0x{want:x}"); sys.exit(1)
blob = open(p, 'rb').read()
if version.encode() not in blob:
    print(f"::warning::主程序里找不到版本串 {version}（上游可能改了版本写法，请核对）")
print(f"    ELF 架构 OK（0x{m:x}）；二进制 {len(blob)//1024} KiB")
for rel in ('bin/start.sh',):
    if not os.path.exists(os.path.join(rootfs, rel)):
        print(f"::error::缺 {rel}"); sys.exit(1)
PY

echo "==> Done: $ROOTFS"
du -sh "$ROOTFS"
