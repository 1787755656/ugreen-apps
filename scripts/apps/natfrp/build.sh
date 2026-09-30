#!/bin/bash
set -euo pipefail

# Usage: build.sh <version, unused> <arch: amd64|arm64>
# version is accepted for interface consistency with the other apps'
# build.sh but unused here — the download URL has no version in it at all
# (see get-latest-version.sh's comment for why).
#
# 包结构 = 本地已真机验证的 inner 变体（natfrp-ugreen-app 独立仓库同步而来）：
#   - start_cmd 是自研 Go 管理壳 bin/launcher（--port=7101），不是 shell 脚本。
#     它接管 natfrp-service 子进程、写 config.json 默认值、并在 7101 提供
#     http 管理页（/api/*，经 UGOS 网关 proxy_path: api 反代）。
#   - 为什么不能回到 open_type: tab 直开 7102：UGOS 桌面对 tab 应用按
#     http:// 直开端口，而 natfrp WebUI 只说 https（http 一律 400），
#     图标落在一个打不开的页面上 —— 2026.9.30 真机事故的根因。launcher 的
#     管理页用 http 是安全的（页面无 WebSocket），"打开樱花 WebUI"按钮
#     才按正确的 https:// 新开标签。
#   - config.json 默认值由 launcher ensureDefaults() 写（首次、不覆盖用户
#     已改项）：webui_origin_mode: any、update_interval: -1（沙箱安装目录
#     只读，自更新必失败）——这两条是从旧 start.sh 时代继承的硬约束，别丢。

ARCH="${2:?ARCH is required (amd64|arm64)}"

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

echo "==> Building natfrp (${ARCH})"

# ---- 1. 上游 natfrp-service + frpc（静态 Go 二进制，官方原样使用）----

LAUNCHER_URL="https://nya.globalslb.net/natfrp/client/launcher-unix/latest/natfrp-service_linux_${ARCH}.tar.zst"
echo "==> Downloading: ${LAUNCHER_URL}"
curl -fL --http1.1 --retry 5 --retry-all-errors --retry-delay 3 \
  --connect-timeout 20 -o "$WORK_DIR/service.tar.zst" "$LAUNCHER_URL"

zstd -d -f -q "$WORK_DIR/service.tar.zst" -o "$WORK_DIR/service.tar"
mkdir -p "$WORK_DIR/extracted"
tar -xf "$WORK_DIR/service.tar" -C "$WORK_DIR/extracted"

[ -f "$WORK_DIR/extracted/natfrp-service" ] || { echo "natfrp-service binary not found in archive" >&2; exit 1; }
[ -f "$WORK_DIR/extracted/frpc" ] || { echo "frpc binary not found in archive" >&2; exit 1; }

mkdir -p "$ROOTFS/bin"
cp "$WORK_DIR/extracted/natfrp-service" "$ROOTFS/bin/natfrp-service"
cp "$WORK_DIR/extracted/frpc" "$ROOTFS/bin/frpc"
chmod +x "$ROOTFS/bin/natfrp-service" "$ROOTFS/bin/frpc"

# ---- 2. Go 工具链（钉死版本，同 dnet；GOTOOLCHAIN=local 禁自动拉链）----
# 管理壳 go.mod 只要求 go 1.21，这里跟 dnet 用同一个钉死版本，全仓统一。
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
export GOWORK=off GOTOOLCHAIN=local
go version

# ---- 3. 管理壳 launcher（随仓库源码，先 vet+test 再交叉编译）----
echo "==> 管理壳测试（vet + test）"
( cd "$SCRIPT_DIR/launcher" && go vet ./... && go test ./... )
echo "==> [$ARCH] 编译管理壳 linux/$ARCH"
(
  cd "$SCRIPT_DIR/launcher"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
    go build -trimpath -ldflags="-s -w" \
    -o "$ROOTFS/bin/launcher" .
)
chmod +x "$ROOTFS/bin/launcher"

# 管理页单源：launcher 用 go:embed 打包 index.html，网关 inner 窗口 serve 的
# www/index.html 在构建时从同一份源码拷出，两边保证一致。
[ -f "$SCRIPT_DIR/launcher/index.html" ] || {
  echo "::error::缺少 $SCRIPT_DIR/launcher/index.html —— 管理页源码丢了" >&2; exit 1;
}
cp "$SCRIPT_DIR/launcher/index.html" "$APPDIR/rootfs_common/www/index.html"

# ---- 4. 架构断言（上游二进制是官方静态 Go，也顺手断言防 URL 变卦）----
assert_elf() {
  file "$1" | grep -qi 'ELF 64-bit' || { echo "::error::$1 不是 ELF: $(file -b "$1")" >&2; exit 1; }
  file "$1" | grep -qi "$ELF_MACHINE" || {
    echo "::error::$1 架构不是 ${ELF_MACHINE}: $(file -b "$1")" >&2; exit 1;
  }
}
assert_elf "$ROOTFS/bin/natfrp-service"
assert_elf "$ROOTFS/bin/frpc"
assert_elf "$ROOTFS/bin/launcher"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
