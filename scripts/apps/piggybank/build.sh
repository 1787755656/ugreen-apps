#!/usr/bin/env bash
# 存钱罐（piggy-bank）构建 —— 上游 TS 源码编译 + 官方 Node 运行时 + better-sqlite3 prebuild。
#
# 用法：build.sh <版本号 x.y.z> <架构 amd64|arm64>
#
# 这个应用的包 = 上游 monorepo 源码（server 端 tsc 编译 + 上游自带的预构建 web/dist）
#              + 官方 Node 20 运行时 + better-sqlite3 官方 v115 prebuild（按架构换装）。
# 上游代码一行不改。本脚本只负责把 rootfs_<arch> 摆好；
# ugcli check / pack / 发 Release 由可复用 workflow（reusable-build-app.yml）统一执行。
#
# ⚠ 三个 workspace 坑都在这里有断言盯着（0001 真机炸包教训）：
#   1) `npm ci --workspace server` 不装根 package.json 的运行时依赖（ws 要显式补装）
#   2) 补装会顺带拉进 web 端生产依赖和 typescript（后者手动删）
#   3) npm 生成的 @org/pkg 自引用软链拷进打包树即悬空（生成运行时树后删 @money-jar）
#   staging 树验证不算数：所有断言和冒烟都跑在真正进 upk 的打包树上。
set -euo pipefail

VERSION="${1:?用法：build.sh <版本号> <架构>}"
ARCH="${2:?用法：build.sh <版本号> <架构>}"
case "$ARCH" in
  amd64) NODE_ARCH="x64";  ELF="x86-64";;
  arm64) NODE_ARCH="arm64"; ELF="aarch64";;
  *) echo "!! 架构只支持 amd64 / arm64，收到：$ARCH" >&2; exit 1 ;;
esac

SLASH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SLASH_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SLASH_DIR/meta.env"
APPDIR="$REPO_ROOT/$PROJECT_DIR"
ROOTFS="$APPDIR/rootfs_$ARCH"
CACHE="${RUNNER_TEMP:-$SLASH_DIR/.build-cache}"
mkdir -p "$CACHE"

NODE_VERSION="20.18.1"   # node 20 = ABI 115，与 better-sqlite3 官方 prebuild 对应
NODE_ABI="115"
BS3_VER="11.10.0"
WS_VER="8.21.3"

echo "==> 存钱罐 ${VERSION}（架构：${ARCH}）"

# ---- 1. 上游源码 @ tag（v1.2 这类两段 tag 兜底尝试，最后退回 main）----
UP="$CACHE/upstream"
rm -rf "$UP"
REF=""
for CAND in "v${VERSION}" "v$(echo "$VERSION" | cut -d. -f1-2)"; do
  if git ls-remote --tags "https://github.com/ASH-C776/piggy-bank.git" "refs/tags/${CAND}" | grep -q .; then
    REF="$CAND"; break
  fi
done
if [ -z "$REF" ]; then
  echo "==> 上游无匹配 tag，取 main"
  git clone --quiet --depth 1 "https://github.com/ASH-C776/piggy-bank.git" "$UP"
else
  echo "==> 取上游源码 @ ${REF}"
  git clone --quiet --depth 1 --branch "$REF" "https://github.com/ASH-C776/piggy-bank.git" "$UP"
fi
# 上游自 v1.3 起打 tag 不再 bump package.json（v1.3.3 的 package.json 仍是 1.0.0），
# 发布身份以上游 tag 为准，这里只警告不拦截——硬断言会把商店永远卡在 v1.2.0。
SRC_VERSION="$(jq -r '.version' "$UP/package.json")"
if [ "$SRC_VERSION" != "$VERSION" ]; then
  echo "⚠ 上游 package.json version（${SRC_VERSION}）与 tag（${REF:-main}）不一致，以 tag 版本 ${VERSION} 为准" >&2
fi

# ---- 2. 编译 server（tsc 需要 devDeps）----
cd "$UP"
npm ci --ignore-scripts --no-audit --no-fund
npm run build --workspace server
[ -f server/dist/index.js ] || { echo "!! server/dist/index.js 缺失" >&2; exit 1; }

# ---- 3. 生产运行时树 ----
BASE="$CACHE/runtime-base"
rm -rf "$BASE"; mkdir -p "$BASE/server" "$BASE/web"
cp package.json package-lock.json "$BASE/"
cp server/package.json "$BASE/server/"
cp web/package.json "$BASE/web/"
( cd "$BASE" \
  && npm ci --workspace server --omit=dev --ignore-scripts --no-audit --no-fund \
  && npm install "ws@${WS_VER}" --omit=dev --ignore-scripts --no-audit --no-fund \
  && rm -rf node_modules/typescript node_modules/.bin/tsc node_modules/.bin/tsserver \
  && rm -rf node_modules/@money-jar )
[ -d "$BASE/node_modules/ws" ]                  || { echo "!! ws 缺失于运行时树" >&2; exit 1; }
[ -d "$BASE/node_modules/better-sqlite3" ]      || { echo "!! better-sqlite3 缺失" >&2; exit 1; }
[ ! -d "$BASE/node_modules/typescript" ]        || { echo "!! typescript 泄漏进运行时树" >&2; exit 1; }

# ---- 4. 下载 Node 运行时与 better-sqlite3 prebuild（缓存复用）----
NODE_TAR="$CACHE/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz"
[ -s "$NODE_TAR" ] || curl -fL --retry 3 -o "$NODE_TAR" "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz"
BS3_TAR="$CACHE/better-sqlite3-v${BS3_VER}-node-v${NODE_ABI}-linux-${NODE_ARCH}.tar.gz"
[ -s "$BS3_TAR" ] || curl -fL --retry 3 -o "$BS3_TAR" "https://github.com/WiseLibs/better-sqlite3/releases/download/v${BS3_VER}/better-sqlite3-v${BS3_VER}-node-v${NODE_ABI}-linux-${NODE_ARCH}.tar.gz"

# ---- 5. 组装 rootfs_$ARCH ----
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS/bin" "$ROOTFS/app/server" "$ROOTFS/app/web"
cp "$SLASH_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"
tar -xJf "$NODE_TAR" -C "$CACHE" "node-v${NODE_VERSION}-linux-${NODE_ARCH}/bin/node"
cp "$CACHE/node-v${NODE_VERSION}-linux-${NODE_ARCH}/bin/node" "$ROOTFS/bin/node"
chmod +x "$ROOTFS/bin/node"
cp -R "$UP/server/dist" "$ROOTFS/app/server/dist"
cp "$UP/server/src/db/schema.sql" "$ROOTFS/app/server/dist/db/schema.sql"
cp "$UP/server/package.json" "$ROOTFS/app/server/package.json"
cp -R "$BASE/node_modules" "$ROOTFS/app/server/node_modules"
rm -rf "$ROOTFS/app/server/node_modules/better-sqlite3/build"
mkdir -p "$ROOTFS/app/server/node_modules/better-sqlite3/build/Release"
tar -xzf "$BS3_TAR" -C "$ROOTFS/app/server/node_modules/better-sqlite3"
cp -R "$UP/web/dist" "$ROOTFS/app/web/dist"

# ---- 6. 打包树断言（staging 验证不算数）----
T="$ROOTFS/app/server"
[ -d "$T/node_modules/ws" ] || { echo "!! ws 缺失于打包树" >&2; exit 1; }
[ -f "$T/node_modules/better-sqlite3/build/Release/better_sqlite3.node" ] || { echo "!! better_sqlite3.node 缺失" >&2; exit 1; }
[ -f "$T/dist/db/schema.sql" ] || { echo "!! schema.sql 缺失" >&2; exit 1; }
file "$ROOTFS/bin/node" | grep -q "$ELF" || { echo "!! node ELF 架构不符" >&2; exit 1; }
file "$T/node_modules/better-sqlite3/build/Release/better_sqlite3.node" | grep -q "$ELF" || { echo "!! better_sqlite3 ELF 架构不符" >&2; exit 1; }
DANGLING="$(find "$T/node_modules" -xtype l 2>/dev/null || true)"
[ -z "$DANGLING" ] || { echo "!! 打包树存在悬空软链：$DANGLING" >&2; exit 1; }

# ---- 7. 冒烟（真实打包树；仅 runner 架构与目标一致时能跑）----
HOST_OS="$(uname -s)"; HOST_ARCH="$(uname -m)"
RUN_SMOKE="no"
if [ "$HOST_OS" = "Linux" ] && [ "$ARCH" = "amd64" ] && [ "$HOST_ARCH" = "x86_64" ]; then RUN_SMOKE="yes"; fi
if [ "$HOST_OS" = "Linux" ] && [ "$ARCH" = "arm64" ] && [ "$HOST_ARCH" = "aarch64" ]; then RUN_SMOKE="yes"; fi
# 注意：shell 的 &&/|| 同级左结合，复合条件必须拆成显式标志位
if [ "$RUN_SMOKE" = "yes" ]; then
  "$SLASH_DIR/smoke.sh" "$ROOTFS" 23991
else
  echo "SKIP smoke: host $HOST_OS-$HOST_ARCH 跑不了 linux-$ARCH 二进制"
fi

echo "OK: rootfs_$ARCH 组装完成（$APPDIR）"
