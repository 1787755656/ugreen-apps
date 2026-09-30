#!/usr/bin/env bash
# 真实打包树冒烟: smoke.sh <rootfs_dir> <port>
# rootfs_dir = rootfs_common 与 rootfs_<arch> 的合并视图（rootfs_common 的父目录
# 加上 rootfs_<arch> 的 bin）；必须跑在与目标架构一致的机器上。
set -euo pipefail
ROOTFS="$(cd "$1" && pwd)"
PORT="${2:-25180}"
DATA="$(mktemp -d)"
LOG="$DATA/server.log"
mkdir -p "$DATA"
cd "$ROOTFS"
UGAPP_INSTALL_DIR="$ROOTFS" ./bin/ittools-server --port="$PORT" > "$LOG" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
sleep 2
code() { curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT$1"; }
[ "$(code /healthz)" = "200" ] || { echo "smoke FAIL: /healthz not 200"; cat "$LOG"; exit 1; }
[ "$(code /)" = "200" ] || { echo "smoke FAIL: / not 200"; cat "$LOG"; exit 1; }
# history 路由的 SPA fallback：任意工具子路径回 index.html
[ "$(code /token-generator)" = "200" ] || { echo "smoke FAIL: SPA fallback"; exit 1; }
curl -s "http://127.0.0.1:$PORT/token-generator" | grep -q "<title>IT Tools" \
  || { echo "smoke FAIL: title not IT Tools"; exit 1; }
# 带扩展名的未知路径必须 404（不许拿 HTML 冒充 JS 掩盖 dist 缺文件）
[ "$(code /assets/nope-1a2b3c.js)" = "404" ] || { echo "smoke FAIL: missing asset not 404"; exit 1; }
# 带 hash 的资源给一年 immutable 缓存
ASSET=$(ls "$ROOTFS"/www/assets | head -1)
CACHE=$(curl -s -o /dev/null -w '%header{cache-control}' "http://127.0.0.1:$PORT/assets/$ASSET")
echo "$CACHE" | grep -q "immutable" || { echo "smoke FAIL: asset cache-control"; exit 1; }
echo "smoke OK: healthz 200, / 200, SPA fallback ok, missing asset 404, asset immutable"
