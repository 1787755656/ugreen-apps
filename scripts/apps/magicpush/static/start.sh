#!/bin/sh
# MagicPush (魔法推送) 启动脚本 — UGOS Pro 原生应用
set -u

INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${INSTALL_DIR}/cache}"

APP_DIR="${INSTALL_DIR}/app"
SERVER_DIR="${APP_DIR}/server"
NODE_BIN="${INSTALL_DIR}/bin/node"

WEBUI_PORT="${PORT:-3000}"

mkdir -p "${DATA_DIR}" "${LOG_DIR}" "${CACHE_DIR}" 2>/dev/null

# 数据库与运行时数据放可写 data 目录
export DB_PATH="${DATA_DIR}/push_service.db"
export UGAPP_LOG_DIR="${LOG_DIR}"
export LOG_DIR="${LOG_DIR}"
export NODE_ENV=production
export PORT="${WEBUI_PORT}"
export TZ="${TZ:-Asia/Shanghai}"
export JWT_ACCESS_EXPIRES_IN="${JWT_ACCESS_EXPIRES_IN:-15m}"
export JWT_REFRESH_EXPIRES_IN="${JWT_REFRESH_EXPIRES_IN:-7d}"
export LOG_LEVEL="${LOG_LEVEL:-info}"

# 可选：首次生成 JWT secret，避免每次重启令牌失效
ENV_FILE="${DATA_DIR}/.env"
if [ ! -f "${ENV_FILE}" ]; then
  # 用 node 生成随机 secret（无 openssl 也行）
  if [ -x "${NODE_BIN}" ]; then
    SECRET=$("${NODE_BIN}" -e "console.log(require('crypto').randomBytes(32).toString('hex'))" 2>/dev/null || true)
  fi
  if [ -z "${SECRET:-}" ]; then
    SECRET="magicpush-$(date +%s)-$$"
  fi
  cat > "${ENV_FILE}" <<ENVEOF
JWT_SECRET=${SECRET}
PORT=${WEBUI_PORT}
NODE_ENV=production
TZ=Asia/Shanghai
ENVEOF
fi

# 加载持久化环境变量
# shellcheck disable=SC1090
. "${ENV_FILE}"
export JWT_SECRET
export PORT
export NODE_ENV
export TZ

# 前端静态资源路径相对 server/src -> ../../web/dist，与 Docker 布局一致
# 工作目录切到 server，匹配相对路径与 dotenv
cd "${SERVER_DIR}" || {
  echo "server dir missing: ${SERVER_DIR}" >&2
  exit 1
}

# 把 data 下的 .env 软链到 server，供 dotenv 读取（server 目录可能只读，失败则 export 已足够）
if [ -f "${ENV_FILE}" ] && [ ! -e "${SERVER_DIR}/.env" ]; then
  ln -sf "${ENV_FILE}" "${SERVER_DIR}/.env" 2>/dev/null || true
fi

if [ ! -x "${NODE_BIN}" ]; then
  echo "node binary missing: ${NODE_BIN}" >&2
  exit 1
fi

if [ ! -f "${SERVER_DIR}/src/app.js" ]; then
  echo "app entry missing: ${SERVER_DIR}/src/app.js" >&2
  exit 1
fi

echo "MagicPush starting on port ${PORT}, DB=${DB_PATH}, LOG=${LOG_DIR}"
# exec 使 node 成为主进程，正确接收 SIGTERM
exec "${NODE_BIN}" src/app.js
