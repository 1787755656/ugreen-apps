#!/bin/sh
# 存钱罐 (Piggy Bank) 启动脚本 — UGOS Pro 原生应用
# 上游: https://github.com/ASH-C776/piggy-bank (MIT)
set -u

INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${INSTALL_DIR}/cache}"

APP_DIR="${INSTALL_DIR}/app"
SERVER_DIR="${APP_DIR}/server"
NODE_BIN="${INSTALL_DIR}/bin/node"

# 与 project.yaml 的 port 一致（monorepo 里 magicpush 占 23000，避开）
WEBUI_PORT="${PORT:-23010}"

mkdir -p "${DATA_DIR}" "${LOG_DIR}" "${CACHE_DIR}" 2>/dev/null

# 沙箱没有 /tmp，重定向到可写缓存目录
export TMPDIR="${CACHE_DIR}/tmp"
mkdir -p "${TMPDIR}" 2>/dev/null

# 数据目录（config.js 读 DATA_DIR）
export DATA_DIR
export NODE_ENV=production
export PORT="${WEBUI_PORT}"
export TZ="${TZ:-Asia/Shanghai}"

# JWT_SECRET 持久化：首次启动生成随机密钥，避免每次重启令牌失效
ENV_FILE="${DATA_DIR}/.env"
if [ ! -f "${ENV_FILE}" ]; then
  SECRET=""
  if [ -x "${NODE_BIN}" ]; then
    SECRET=$("${NODE_BIN}" -e "console.log(require('crypto').randomBytes(32).toString('hex'))" 2>/dev/null || true)
  fi
  if [ -z "${SECRET}" ]; then
    SECRET=$(head -c 32 /dev/urandom 2>/dev/null | od -An -tx1 2>/dev/null | tr -d ' \n')
  fi
  if [ -z "${SECRET}" ]; then
    SECRET="piggy-bank-$(date +%s)-fallback-secret"
  fi
  cat > "${ENV_FILE}" <<ENVEOF
JWT_SECRET=${SECRET}
ENVEOF
fi
# shellcheck disable=SC1090
. "${ENV_FILE}"
export JWT_SECRET

if [ ! -x "${NODE_BIN}" ]; then
  echo "node binary missing: ${NODE_BIN}" >&2
  exit 1
fi

if [ ! -f "${SERVER_DIR}/dist/index.js" ]; then
  echo "app entry missing: ${SERVER_DIR}/dist/index.js" >&2
  exit 1
fi

echo "Piggy Bank starting on port ${PORT}, DATA=${DATA_DIR}"
# exec 使 node 成为主进程，正确接收 SIGTERM（平台停止时 10s 窗口）
# 绝对路径入口：config.js 用 dirname(argv[1])/../.. 推导前端目录
exec "${NODE_BIN}" "${SERVER_DIR}/dist/index.js"
