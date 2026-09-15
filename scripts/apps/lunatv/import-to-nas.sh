#!/usr/bin/env bash
# 把 lunatv-sources.local.json 导入 NAS 上的 LunaTV（走应用自带 admin API）
# 凭据从 ~/Desktop/"nas ssh.txt" 读（账号 密码 一行），不写死在本脚本
set -euo pipefail
HOST="${LUNATV_HOST:-192.168.2.51}"
PORT="${LUNATV_PORT:-28300}"
LOCAL_JSON="$(cd "$(dirname "$0")" && pwd)/lunatv-sources.local.json"
[[ -f "$LOCAL_JSON" ]] || { echo "missing $LOCAL_JSON"; exit 1; }

CREDS_FILE="$HOME/Desktop/nas ssh.txt"
SSHPASS_BIN="$(command -v sshpass || true)"
[[ -n "$SSHPASS_BIN" ]] || { echo "need sshpass"; exit 1; }
PW=$(awk 'NR==2{print $2}' "$CREDS_FILE")

# 站长密码在 NAS 的 data/owner.env（首启生成）
NAS_PW=$(sshpass -p "$PW" ssh -p 2022 -o StrictHostKeyChecking=no "claude@${HOST}" \
  "echo '$PW' | sudo -S cat /volume1/@appstore/com.personal.lunatv/data/owner.env 2>/dev/null" \
  | grep LUNATV_PASSWORD | cut -d= -f2)

COOKIE=$(curl -s -i -X POST "http://${HOST}:${PORT}/api/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"admin\",\"password\":\"${NAS_PW}\"}" \
  | grep -i '^set-cookie' | sed -n 's/.*user_auth=\([^;]*\).*/\1/p')

python3 - "$LOCAL_JSON" > /tmp/cf-body.$$ <<'PYEOF'
import json, sys
raw = open(sys.argv[1]).read()
print(json.dumps({"configFile": raw}, ensure_ascii=False))
PYEOF

curl -s -X POST "http://${HOST}:${PORT}/api/admin/config_file" \
  -H "Cookie: user_auth=${COOKIE}" -H "Content-Type: application/json" \
  --data-binary @/tmp/cf-body.$$
echo
rm -f /tmp/cf-body.$$
echo "导入完成。回读验证："
curl -s "http://${HOST}:${PORT}/api/server-config" ; echo
