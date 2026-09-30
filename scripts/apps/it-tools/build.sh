#!/usr/bin/env bash
# IT-Tools 构建 —— 上游 release 自带 dist zip + 自研 Go 静态文件服务器。
#
# 用法：build.sh <版本号 2024.10.22> <架构 amd64|arm64>
#
# 上游是纯前端 SPA（vue-router history 模式），release 附件 it-tools-<ver>.zip
# 就是现成的 dist（github-actions bot 从 CI 产物打包），不用本地构建前端。
# 本脚本把它解到 rootfs_common/www（不进 git，双架构共用），再交叉编译
# scripts/apps/it-tools/server 到 rootfs_<arch>/bin/ittools-server。
# ugcli check / pack / 发 Release 由可复用 workflow（reusable-build-app.yml）统一执行。
#
# ⚠ 上游 tag 形如 v2024.10.22-7ca5933（日期-短哈希），zip 资产名是 tag 去 v 前缀；
#   版本号只取日期段（ugcli 中段最多两位，恰好合法），完整 tag 靠 GitHub API 反查。
set -euo pipefail

VERSION="${1:?用法：build.sh <版本号> <架构>}"
ARCH="${2:?用法：build.sh <版本号> <架构>}"
case "$ARCH" in
  amd64) GOARCH="amd64";  ELF="x86-64";;
  arm64) GOARCH="arm64"; ELF="aarch64";;
  *) echo "!! 架构只支持 amd64 / arm64，收到：$ARCH" >&2; exit 1 ;;
esac

SLASH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SLASH_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SLASH_DIR/meta.env"
APPDIR="$REPO_ROOT/$PROJECT_DIR"
ROOTFS="$APPDIR/rootfs_$ARCH"
WWW="$APPDIR/rootfs_common/www"
CACHE="${RUNNER_TEMP:-$SLASH_DIR/.build-cache}"
mkdir -p "$CACHE"

UPSTREAM_API="https://api.github.com/repos/CorentinTh/it-tools"

echo "==> IT-Tools ${VERSION}（架构：${ARCH}）"

# ---- 1. 版本号 → 完整 tag（v<日期>-<短哈希>）----
AUTH=()
[ -n "${GH_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer ${GH_TOKEN}")
TAG=$(curl -fsSL ${AUTH[@]+"${AUTH[@]}"} "$UPSTREAM_API/releases?per_page=100" \
  | jq -r --arg v "v${VERSION}" '[.[].tag_name
      | select((startswith($v + "-") and test("-[0-9a-f]{6,}$")) or . == $v)]
      | first // empty')
[ -n "$TAG" ] || { echo "!! 上游找不到版本 ${VERSION} 对应的 release tag" >&2; exit 1; }
echo "==> 上游 tag：${TAG}"

# ---- 2. 下载 dist zip（缓存复用）----
ZIP="$CACHE/it-tools-${TAG#v}.zip"
[ -s "$ZIP" ] || curl -fL --retry 3 -o "$ZIP" \
  "https://github.com/CorentinTh/it-tools/releases/download/${TAG}/it-tools-${TAG#v}.zip"

# ---- 3. 铺 rootfs_common/www（幂等；zip 顶层还套一层 dist/）----
UNZIP_DIR="$CACHE/it-tools-www"
rm -rf "$UNZIP_DIR" "$WWW"
mkdir -p "$UNZIP_DIR" "$WWW"
unzip -q "$ZIP" -d "$UNZIP_DIR"
SRC="$UNZIP_DIR/dist"
[ -f "$SRC/index.html" ] || SRC="$UNZIP_DIR"
cp -R "$SRC/." "$WWW/"
# banner.png 只在仓库 README 里用，进包是纯浪费
rm -f "$WWW/banner.png"
[ -f "$WWW/index.html" ] || { echo "!! www/index.html 缺失" >&2; exit 1; }
ls "$WWW"/assets/*.js >/dev/null 2>&1 || { echo "!! www/assets/*.js 缺失，dist 不完整" >&2; exit 1; }
echo "==> www 就绪：$(du -sh "$WWW" | cut -f1)"

# ---- 4. 交叉编译 Go 静态文件服务器 ----
# go.mod 要求 go 1.26.5（GOTOOLCHAIN 自动落到该版本）：平台注入的
# GODEBUG=tlskyber=0 只对 go1.27+ 编译的包是启动即 fatal，锁 1.26 免疫。
(
  cd "$SLASH_DIR/server"
  # GOTOOLCHAIN 钉死 go1.26.5（runner 本地版本再新也会切）：平台注入的
  # GODEBUG=tlskyber=0 只对 go1.27+ 编译的包是启动即 fatal，锁 1.26 免疫。
  GOWORK=off GOTOOLCHAIN=go1.26.5 CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
    go build -trimpath -ldflags "-s -w" \
    -o "$ROOTFS/bin/ittools-server" .
)
chmod 0755 "$ROOTFS/bin/ittools-server"
file "$ROOTFS/bin/ittools-server" | grep -q "$ELF" || {
  echo "::error::ittools-server 产物架构不是 ${ELF}" >&2; exit 1
}
file "$ROOTFS/bin/ittools-server" | grep -q "statically linked" || {
  echo "::error::ittools-server 不是静态链接，沙箱里跑不起来" >&2; exit 1
}
echo "==> ittools-server：$(du -h "$ROOTFS/bin/ittools-server" | cut -f1)（linux/${GOARCH}，静态）"

echo "==> build.sh 完成（rootfs_common/www + rootfs_${ARCH}/bin）"
