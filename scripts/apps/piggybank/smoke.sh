#!/usr/bin/env bash
# 真实打包树冒烟: smoke.sh <rootfs_dir> <port>
# 必须跑在与目标架构一致的机器上（CI 的 ubuntu runner 对 amd64 成立）
set -euo pipefail
ROOTFS="$(cd "$1" && pwd)"
PORT="${2:-23991}"
DATA="$(mktemp -d)"
LOG="$DATA/server.log"
cd "$ROOTFS"
DATA_DIR="$DATA/data" PORT="$PORT" NODE_ENV=production JWT_SECRET=smoke-secret \
  ./bin/node app/server/dist/index.js > "$LOG" 2>&1 &
PID=$!
trap 'kill $PID 2>/dev/null || true' EXIT
sleep 4
code() { curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT$1"; }
[ "$(code /)" = "200" ] || { echo "smoke FAIL: / not 200"; cat "$LOG"; exit 1; }
curl -s "http://127.0.0.1:$PORT/api/auth/children" | grep -q '"children"' || { echo "smoke FAIL: children api"; exit 1; }
RESP=$(curl -s -X POST "http://127.0.0.1:$PORT/api/auth/parent-login" -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}')
echo "$RESP" | grep -q '"token"' || { echo "smoke FAIL: default login"; echo "$RESP"; exit 1; }
TOKEN=$(echo "$RESP" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
[ "$(code /api/point-items)" = "401" ] || { echo "smoke FAIL: unauth api not 401"; exit 1; }
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$PORT/api/dashboard")" = "200" ] \
  || { echo "smoke FAIL: dashboard with token"; exit 1; }
echo "smoke OK: / 200, children api ok, default login ok, unauth 401, dashboard 200"
