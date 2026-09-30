#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 1.0.0> <arch: amd64|arm64>
#
# PostgreSQL 17 原生应用 = Go 管理壳 + pgdg 官方 deb 解出的运行时。
#
#   bin/launcher           Go 管理壳（探活 HTTP 25432 + 两道鉴权闸 + initdb +
#                          守护 postgres + 管理页后端）
#   pgsql/bin/*            postgres / psql / pg_dump / initdb …（sh 包装脚本 +
#                          .bin 真身，时区路径已改相对 —— 见 fetch-runtime.py 文件头）
#   pgsql/lib/*.so*        【含 glibc 与 ld.so 的完整运行库闭包】（沙箱无 /usr）
#   pgsql/share/*          时区目录 / 扩展控制文件等
#
# 管理页前端（rootfs_common/www）是直接提交的静态文件（index.html + 打好的
# cloudwindow.js），本脚本只对它跑 checkjs 校验 —— JSSDK 的打包在桌面工程的
# scripts/build-jssdk.mjs 里做。
# bundled 的数据库小版本 = 构建当天 pgdg 索引最新 17.x（fetch-runtime 自动挑）。
# project.yaml 的 version 由 workflow 统一 sed，本脚本不碰它。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

# 钉死 Go 版本，配合 GOTOOLCHAIN=local。⚠ 不要随手升到 1.27+：
# 平台注入的 GODEBUG=tlskyber=0 会让 1.27 编译出的包启动即 fatal（it-tools 踩过）。
GO_VERSION="1.26.5"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APP_DIR="$REPO_ROOT/$PROJECT_DIR"
LAUNCHER="$SCRIPT_DIR/launcher"
ROOTFS="$APP_DIR/rootfs_${ARCH}"
CACHE="$SCRIPT_DIR/.build-cache"

case "$ARCH" in
  amd64|arm64) ;;
  *) echo "Unsupported arch: $ARCH (amd64|arm64)" >&2; exit 1 ;;
esac
case "$(printf '%s' "$VERSION" | grep -cE '^[0-9]+\.[0-9]{1,2}\.[0-9]+$')" in
  1) ;;
  *) echo "版本号 $VERSION 不满足 ugcli 的格式要求（三段数字，中段最多两位）" >&2; exit 1 ;;
esac

echo "==> Building PostgreSQL ${VERSION} (${ARCH})"

# ---- 1. 前端静态检查 ----
echo "==> Static check on admin web"
node "$SCRIPT_DIR/checkjs.js" "$APP_DIR/rootfs_common/www/index.html"

# ---- 2. Go 工具链 ----
HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac
GOROOT_DIR="$CACHE/go-${GO_VERSION}-${HOST_OS}-${HOST_ARCH}"
if [ ! -x "$GOROOT_DIR/bin/go" ]; then
  GO_TARBALL="go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
  echo "==> Installing Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
  rm -rf "$GOROOT_DIR.tmp" "$GOROOT_DIR"
  mkdir -p "$CACHE"
  curl -fL -o "$CACHE/${GO_TARBALL}" "https://go.dev/dl/${GO_TARBALL}"
  mkdir -p "$GOROOT_DIR.tmp"
  tar xzf "$CACHE/${GO_TARBALL}" -C "$GOROOT_DIR.tmp"
  mv "$GOROOT_DIR.tmp/go" "$GOROOT_DIR"
  rmdir "$GOROOT_DIR.tmp"
  rm -f "$CACHE/${GO_TARBALL}"
fi
export GOROOT="$GOROOT_DIR"
export PATH="$GOROOT/bin:$PATH"
go version

# ---- 3. 管理壳：测试 + 交叉编译 ----
echo "==> go vet + go test (launcher)"
(
  cd "$LAUNCHER"
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go vet ./...
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go test ./...
)

echo "==> Cross-compiling launcher (linux/$ARCH)"
mkdir -p "$ROOTFS/bin"
(
  cd "$LAUNCHER"
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 \
    GOOS=linux GOARCH="$ARCH" \
    go build -trimpath -ldflags="-s -w" \
    -o "$ROOTFS/bin/launcher" .
)
chmod +x "$ROOTFS/bin/launcher"

# 交叉编译写错环境变量的话会打出宿主架构的包，要装到 NAS 上才暴露 —— 在这里拦。
echo "==> Verifying launcher ELF arch"
EXPECT_ELF=$([ "$ARCH" = "amd64" ] && echo x86_64 || echo aarch64)
python3 "$SCRIPT_DIR/elfdeps.py" arch "$ROOTFS/bin/launcher" \
  | grep -q " $EXPECT_ELF " || {
    echo "::error:: 管理壳不是 $EXPECT_ELF" >&2; exit 1; }

# ---- 4. PostgreSQL 本体 + 运行库闭包 ----
# fetch-runtime.py 自带 SHA256 校验和闭包自检，缺库直接构建失败。
# 不传小版本号：pgdg 索引里 postgresql-17 只有最新一版，取它就是"当前最新"。
echo "==> Fetching PostgreSQL runtime (official pgdg deb)"
python3 "$SCRIPT_DIR/fetch-runtime.py" \
  --arch "$ARCH" \
  --pg-major 17 \
  --out "$ROOTFS/pgsql" \
  --cache "$CACHE/fetch"

echo
echo "==> Done: $ROOTFS"
du -sh "$ROOTFS"/* 2>/dev/null || true
