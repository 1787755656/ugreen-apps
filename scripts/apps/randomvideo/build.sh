#!/usr/bin/env bash
# 组装「随机视频」的 rootfs（CI 用，ugcli check/pack 由可复用 workflow 统一执行）。
#
# 用法：build.sh <版本号> <架构>    # 架构：arm64 / amd64
#
# 后端 randomvideo_serv 是纯标准库 Go 单二进制（零外部依赖，go.mod 无 require），
# 前端 web/index.html 在编译期通过 //go:embed 打进二进制；同时网关也从
# rootfs_common/www 提供同一份前端——build.sh 把 src/web/index.html 同步进
# rootfs_common/www，确保两份同源。
#
# 两个有意的点：
#   1. Go 工具链锁 ≤1.23（UGCLI_VERSION 无关，是运行时约束）：NAS 的 systemd 全局环境
#      里残留 GODEBUG=tlskyber=0，Go ≥1.24 会在 main 之前 fatal error 启动即崩。
#      用 Go 1.23 编出来的二进制完全不受影响（未知 GODEBUG 键在 1.23 只是告警）。
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
SRC_DIR="$REPO_ROOT/apps/randomvideo/src"

GO_VERSION="1.23.12"
case "$ARCH" in
  amd64) ELF_MACHINE="x86-64" ;;
  arm64) ELF_MACHINE="aarch64" ;;
esac

# ---- 1. 前端：src/web 是唯一源，同步到安装包的 www/ ----
echo "== 同步前端"
mkdir -p "$APP_DIR/rootfs_common/www"
cp "$SRC_DIR/web/index.html" "$APP_DIR/rootfs_common/www/index.html"

# ---- 2. 图标（缺了才生成，正常已在版本库里） ----
if [ ! -f "$APP_DIR/rootfs_common/icon.png" ]; then
  echo "== 生成图标"
  python3 "$REPO_ROOT/apps/randomvideo/scripts/make-icon.py" "$APP_DIR/rootfs_common/icon.png"
fi

# ---- 3. Go 工具链 ----
# 优先复用本机已装的工具链（本地验证用 RV_GO 或 ~/.local/go123/go），
# 否则 CI 上现下 Go（宿主固定 linux-amd64）。
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
    echo "✗ 当前 Go 是 $ver，≥1.24 —— 编出来的二进制在残留 GODEBUG 的 NAS 上会启动即崩。" >&2
    echo "  请改用 Go 1.23 工具链，或确认目标机无 GODEBUG=tlskyber=0 残留后设 RV_ALLOW_NEW_GO=1。" >&2
    exit 1
  fi
  echo "== Go 版本检查通过：$ver"
fi

# ---- 4. 交叉编译 ----
export CGO_ENABLED=0
export GOTELEMETRY=off
export GOTOOLCHAIN=local
export GOFLAGS="-trimpath"
ROOTFS="$APP_DIR/rootfs_${ARCH}"
mkdir -p "$ROOTFS/bin"
echo "== 编译 linux/$ARCH -> $ROOTFS/bin/randomvideo_serv"
(
  cd "$SRC_DIR"
  GOOS=linux GOARCH="$ARCH" "$GO" build -ldflags="-s -w" -o "$ROOTFS/bin/randomvideo_serv" .
)
chmod +x "$ROOTFS/bin/randomvideo_serv"

# ---- 5. 架构断言 + 体积兜底 ----
file "$ROOTFS/bin/randomvideo_serv" | grep -q "$ELF_MACHINE" || {
  echo "!! 产物架构不是 ${ELF_MACHINE} —— 交叉编译可能静默退回了宿主架构" >&2; exit 1; }
[ "$(stat -c%s "$ROOTFS/bin/randomvideo_serv" 2>/dev/null || stat -f%z "$ROOTFS/bin/randomvideo_serv")" -gt 1000000 ] || {
  echo "!! 二进制小得可疑（<1MB），可能编译失败" >&2; exit 1; }

echo "== rootfs_${ARCH} assembled at $ROOTFS"
ls -lh "$ROOTFS/bin/randomvideo_serv" "$APP_DIR/rootfs_common/www/index.html" "$APP_DIR/rootfs_common/icon.png"
