#!/usr/bin/env bash
# 组装 LunaTV 的 rootfs（CI 用，ugcli check/pack 由可复用 workflow 统一执行）。
#
# 用法：build.sh <版本号> <架构>    # 架构：arm64 / amd64
#
# 应用本体是 Next.js 16 standalone（前后端一体、Turbopack 构建、node:sqlite
# 内置存储、零原生模块），每架构捆绑官方 Node 24（glibc）。
# 上游 fork（SzeMeng76/LunaTV）不打 tag —— 永远构建 main 当前提交，
# 且断言 main 的 VERSION.txt 与传入版本一致（fork 无历史版本可回溯）。
#
# 对上游源码只有两处打包补丁（scripts/apps/lunatv/patches/）：
#   001 弃用 next/font/google（构建机无法访问 Google Fonts）
#   002 node:sqlite 改 process.getBuiltinModule（Turbopack 会把
#       require('node:sqlite') 编译成运行时抛错的 stub，上游官方
#       Docker 镜像同样中招）
# 其余 rootfs 级适配全部走环境变量（SQLITE_DB_PATH / VIDEO_CACHE_DIR /
# TMPDIR / HOSTNAME / PORT），见 start.sh。
set -euo pipefail

VERSION="${1:?用法：build.sh <版本号> <架构>}"
ARCH="${2:?用法：build.sh <版本号> <架构>}"
case "$ARCH" in
  arm64|amd64) ;;
  *) echo "!! 架构只支持 arm64 / amd64，收到：$ARCH" >&2; exit 1 ;;
esac

SLASH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APPDIR="$SLASH_DIR/../../../apps/lunatv/com.personal.lunatv"   # 仓库根 = 脚本目录上三级
APPDIR="$(cd "$APPDIR" && pwd)"
CACHE="${RUNNER_TEMP:-$SLASH_DIR/.build-cache}"
mkdir -p "$CACHE"

UPSTREAM_REPO="${UPSTREAM_REPO:-https://github.com/SzeMeng76/LunaTV.git}"
NODE_VER="24.14.0"
NODE_DIR="node-v${NODE_VER}-linux-$( [[ "$ARCH" == "amd64" ]] && echo x64 || echo arm64 )"

# ---- 上游源码（永远 main）----
SRC="$CACHE/src"
rm -rf "$SRC"
git clone --depth 1 "$UPSTREAM_REPO" "$SRC"
cd "$SRC"
BUILD_VERSION=$(tr -d '[:space:]' < VERSION.txt)
if [[ "$BUILD_VERSION" != "$VERSION" ]]; then
  echo "!! 上游 main 的 VERSION.txt ($BUILD_VERSION) 与请求版本 ($VERSION) 不一致。" >&2
  echo "   上游 fork 无 tag，只能构建 main 当前版本；请用 workflow_dispatch 不填 version 重新触发。" >&2
  exit 1
fi

# ---- 打包补丁（已应用过则跳过，支持本地重跑）----
for p in "$SLASH_DIR"/patches/*.patch; do
  if git apply --check "$p" 2>/dev/null; then
    git apply "$p"
  elif git apply --check -R "$p" 2>/dev/null; then
    echo "skip $p (already applied)"
  else
    echo "!! patch failed: $p（上游改动？更新 patches/）" >&2
    exit 1
  fi
done
rm -f public/screenshot*.png    # README 截图 19MB，不进包

# ---- 依赖与构建 ----
if [[ ! -d node_modules ]]; then
  if command -v pnpm >/dev/null 2>&1; then
    pnpm install --frozen-lockfile
  else
    npx -y pnpm@10.14.0 install --frozen-lockfile
  fi
fi
DOCKER_BUILD=true NODE_ENV=production NEXT_TELEMETRY_DISABLED=1 npx next build
[[ -f .next/standalone/server.js ]] || { echo "!! standalone output missing" >&2; exit 1; }
[[ -d .next/static && -n "$(ls .next/static)" ]] || { echo "!! .next/static empty" >&2; exit 1; }

# ---- stage 组装（symlink 实体化 + 补 next 顶层运行时依赖）----
STAGE="$CACHE/stage"
rm -rf "$STAGE"; mkdir -p "$STAGE"
cp -a .next/standalone/. "$STAGE/" 2>/dev/null || true
rm -rf "$STAGE/.next/cache"
cp -a .next/static "$STAGE/.next/static"
cp -a public "$STAGE/public"
NM_TOP="$STAGE/node_modules"
NM_PNPM=$(echo "$STAGE"/node_modules/.pnpm/next@*/node_modules)
rm -rf "$NM_TOP/styled-jsx";           cp -aL "$NM_PNPM/styled-jsx"         "$NM_TOP/styled-jsx" 2>/dev/null || true
mkdir -p "$NM_TOP/@next" "$NM_TOP/@opentelemetry" "$NM_TOP/@swc"
rm -rf "$NM_TOP/@next/env";            cp -aL "$NM_PNPM/@next/env"          "$NM_TOP/@next/env" 2>/dev/null || true
rm -rf "$NM_TOP/@opentelemetry/api";   cp -aL "$NM_PNPM/@opentelemetry/api" "$NM_TOP/@opentelemetry/api" 2>/dev/null || true
rm -rf "$NM_TOP/@swc/helpers";         cp -aL "$NM_PNPM/@swc/helpers"       "$NM_TOP/@swc/helpers" 2>/dev/null || true
for f in styled-jsx/package.json @next/env/package.json @swc/helpers/package.json; do
  [[ -f "$NM_TOP/$f" ]] || { echo "!! missing $f（实体化断链）" >&2; exit 1; }
done
cd "$SLASH_DIR"

# ---- rootfs 布局 ----
NODE_TARBALL="$CACHE/${NODE_DIR}.tar.xz"
if [[ ! -f "$NODE_TARBALL" ]]; then
  curl -fL --retry 3 -o "$NODE_TARBALL" "https://nodejs.org/dist/v${NODE_VER}/${NODE_DIR}.tar.xz"
fi
RFS="$APPDIR/rootfs_${ARCH}"
rm -rf "$RFS"
mkdir -p "$RFS/bin" "$RFS/app"
cp -aL "$STAGE/." "$RFS/app/" 2>/dev/null || true
tar xf "$NODE_TARBALL" -C "$CACHE" "${NODE_DIR}/bin/node"
cp "$CACHE/${NODE_DIR}/bin/node" "$RFS/bin/node"
cp "$SLASH_DIR/start.sh" "$RFS/bin/start.sh"
chmod +x "$RFS/bin/node" "$RFS/bin/start.sh"
# ELF 架构断言
want="x86-64"; [[ "$ARCH" == "arm64" ]] && want="aarch64"
file "$RFS/bin/node" | grep -q "$want" || { echo "!! node arch mismatch for $ARCH" >&2; exit 1; }
# 体积断言：standalone 产物异常为空时早失败
[[ $(du -sm "$RFS/app" | cut -f1) -gt 40 ]] || { echo "!! app tree suspiciously small" >&2; exit 1; }
echo "== rootfs_${ARCH} assembled at $RFS"
