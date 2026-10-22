#!/bin/sh
# Mook (Go 后端 + 内置前端) 启动脚本 — UGOS Pro 原生应用
set -u

INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${INSTALL_DIR}/cache}"

# 沙箱没有 /tmp：os.TempDir() 跟着 TMPDIR 走
mkdir -p "${DATA_DIR}" "${LOG_DIR}" "${CACHE_DIR}/tmp" 2>/dev/null
export TMPDIR="${CACHE_DIR}/tmp"

# 平台注入 PORT 时沿用，否则用上游默认 5866（与 project.yaml port 一致）
export MOOK_PORT="${PORT:-5866}"
export MOOK_DATA="${DATA_DIR}"
# 前端构建产物随 rootfs_common/app/dist 安装（安装目录只读，静态文件正合适）
export MOOK_DIST="${INSTALL_DIR}/app/dist"

# 沙箱 /etc 下没有 CA bundle：AI 接口(HTTPS) 需要，指向包内捆绑的 Mozilla CA
export SSL_CERT_FILE="${INSTALL_DIR}/certs/cacert.pem"

export TZ="${TZ:-Asia/Shanghai}"

exec "${INSTALL_DIR}/bin/mook"
