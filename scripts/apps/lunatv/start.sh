#!/bin/sh
# LunaTV (Next.js 16 standalone) 启动脚本 — UGOS Pro 原生应用
set -u

INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${INSTALL_DIR}/cache}"

APP_DIR="${INSTALL_DIR}/app"
NODE_BIN="${INSTALL_DIR}/bin/node"

# Next standalone 读 PORT 决定监听端口；必须显式 export，
# 否则回退到默认 3000（EADDRINUSE）。bash 的 ${PORT:-28300} 在 PORT
# 未设置时取 28300，已设置（平台注入）时原样沿用。
export PORT="${PORT:-28300}"

mkdir -p "${DATA_DIR}" "${LOG_DIR}" "${CACHE_DIR}/video-cache" "${DATA_DIR}/tmp" 2>/dev/null

# ---- 沙箱适配 ----
# 沙箱没有 /tmp：os.TempDir()/worker 线程都会跟着 TMPDIR 走
export TMPDIR="${CACHE_DIR}/tmp"
# Next standalone 用 HOSTNAME 作绑定地址；必须 0.0.0.0（tab 应用要局域网直连），
# 程序内部自连用的 localhost 由 Next 内部处理，不受此影响
export HOSTNAME="0.0.0.0"
export NODE_ENV=production
export TZ="${TZ:-Asia/Shanghai}"

# ---- 存储与账号 ----
# 数据库模式：观看记录/收藏/用户全进私有 data 目录的 SQLite
export NEXT_PUBLIC_STORAGE_TYPE="sqlite"
export SQLITE_DB_PATH="${DATA_DIR}/lunatv.db"
# 视频转码缓存目录（上游默认 /tmp/video-cache，沙箱里没有 /tmp）
export VIDEO_CACHE_DIR="${CACHE_DIR}/video-cache"

# 站长账号：优先用应用中心「设置」里改的值；首次启动生成随机密码写进 data/
ENV_FILE="${DATA_DIR}/owner.env"
if [ -f "${ENV_FILE}" ]; then
  # shellcheck disable=SC1090
  . "${ENV_FILE}"
  export LUNATV_USERNAME LUNATV_PASSWORD
else
  # 首次启动（参数/配置尚未写盘是正常现象）：生成随机密码并持久化
  LUNATV_USERNAME="${LUNATV_USERNAME:-admin}"
  if [ -z "${LUNATV_PASSWORD:-}" ]; then
    if [ -x "${NODE_BIN}" ]; then
      LUNATV_PASSWORD="$("${NODE_BIN}" -e "console.log(require('crypto').randomBytes(12).toString('base64url'))" 2>/dev/null || true)"
    fi
    [ -n "${LUNATV_PASSWORD:-}" ] || LUNATV_PASSWORD="lunatv$(date +%s)"
  fi
  printf 'LUNATV_USERNAME=%s\nLUNATV_PASSWORD=%s\n' "${LUNATV_USERNAME}" "${LUNATV_PASSWORD}" > "${ENV_FILE}" 2>/dev/null || true
  export LUNATV_USERNAME LUNATV_PASSWORD
fi

export USERNAME="${LUNATV_USERNAME}"
export PASSWORD="${LUNATV_PASSWORD}"

cd "${APP_DIR}" || {
  echo "app dir missing: ${APP_DIR}" >&2
  exit 1
}

exec "${NODE_BIN}" server.js
