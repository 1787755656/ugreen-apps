#!/usr/bin/env bash
# 组装 P2Pee 内网穿透的 rootfs（CI 用，ugcli check/pack 由可复用 workflow 统一执行）。
#
# 用法：build.sh <版本号> <架构>    # 架构：arm64 / amd64
#
# 本应用是一个「管理壳」：Go launcher 包装上游 p2pee-proxy 二进制，提供管理界面
# 与公网访问地址展示。build.sh 做两件事：
#   1) 交叉编译 Go launcher（apps/p2pee-proxy/com.xnkyn.p2peeproxy/tools/launcher）
#      -> rootfs_<arch>/bin/launcher
#   2) 从上游 gitee 下载 p2pee-proxy 二进制
#      -> rootfs_<arch>/bin/p2pee-proxy
#
# 两个有意的点（与 randomvideo 一致）：
#   1. Go 工具链锁 ≤1.23：UGOS 的 systemd 全局环境残留 GODEBUG=tlskyber=0，
#      Go ≥1.24 会在 main 之前 fatal error 启动即崩。Go 1.23 编出的二进制不受影响。
#   2. CGO_ENABLED=0 + GOTOOLCHAIN=local：纯静态、零运行时依赖，沙箱里最稳。
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

APP_DIR="$REPO_ROOT/$PROJECT_DIR"
LAUNCHER_SRC="$APP_DIR/tools/launcher"
BIN_DIR="$APP_DIR/rootfs_${ARCH}/bin"

PROXY_VERSION="1.0.0"
PROXY_URL="https://gitee.com/xnkyn/assets/releases/download/p2pee-proxy/p2pee-proxy-${PROXY_VERSION}-linux-${ARCH}"

GO_VERSION="1.23.12"

# ---- 1. Go 工具链（优先复用本机已装，否则 CI 现下） ----
if [ -n "${RV_GO:-}" ]; then
  GO="$RV_GO"
elif [ -x "$HOME/.local/go123/go/bin/go" ]; then
  GO="$HOME/.local/go123/go/bin/go"
elif command -v go >/dev/null 2>&1; then
  GO="$(command -v go)"
else
  HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in
    x86_64|amd64) HOST_ARCH="amd64" ;;
    arm64|aarch64) HOST_ARCH="arm64" ;;
    *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
  esac
  GO_TARBALL="go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
  WORK=$(mktemp -d)
  trap 'rm -rf "$WORK"' EXIT
  echo "== 下载 Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
  curl -fL -o "$WORK/$GO_TARBALL" "https://go.dev/dl/${GO_TARBALL}"
  tar xzf "$WORK/$GO_TARBALL" -C "$WORK"
  GO="$WORK/go/bin/go"
fi
"$GO" version

# ⚠ Go ≤1.23（见上方设计理由）。RV_ALLOW_NEW_GO=1 可在确认目标机无 GODEBUG 残留时跳过。
if [ "${RV_ALLOW_NEW_GO:-0}" != "1" ]; then
  ver="$("$GO" env GOVERSION 2>/dev/null || "$GO" version | awk '{print $3}')"
  minor="$(printf '%s' "$ver" | sed -E 's/^go1\.([0-9]+).*/\1/')"
  if [ -n "$minor" ] && [ "$minor" != "$(printf '%s' "$minor" | tr -d '0-9')" ] && [ "$minor" -ge 24 ] 2>/dev/null; then
    echo "!! Go >= 1.24 会在 UGOS 上因 GODEBUG=tlskyber=0 启动即崩，请用 Go 1.23.x" >&2
    exit 1
  fi
fi

# ---- 2. 编译 launcher ----
echo "== 编译 launcher (linux/${ARCH})"
[ -d "$LAUNCHER_SRC" ] || { echo "launcher 源码缺失：$LAUNCHER_SRC" >&2; exit 1; }
mkdir -p "$BIN_DIR"
GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  "$GO" build -trimpath -o "$BIN_DIR/launcher" "$LAUNCHER_SRC"
chmod +x "$BIN_DIR/launcher"

# ---- 3. 下载上游 p2pee-proxy ----
echo "== 下载 p2pee-proxy ${PROXY_VERSION} (linux/${ARCH})"
curl -fL -o "$BIN_DIR/p2pee-proxy" "$PROXY_URL"
chmod +x "$BIN_DIR/p2pee-proxy"

echo "== Done: $BIN_DIR"
ls -lh "$BIN_DIR"
