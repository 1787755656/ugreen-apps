#!/bin/sh
# Readeck 启动脚本 (UGOS Pro 原生应用)
# 由系统以应用独立用户身份运行；工作目录 = 数据目录。
# Readeck 是 Go 单二进制：数据目录由 READECK_DATA_DIRECTORY 决定，
# 数据库 sqlite 路径、首次生成的 config.toml 都落在里面。
# 注意：数据库路径相对工作目录，config.toml 也生成在工作目录，
#       所以这里要 cd 到数据目录再 exec。
set -u

INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"

WEBUI_PORT=28180

mkdir -p "${LOG_DIR}" 2>/dev/null

# ---- 数据目录解析 ----
# READEK_DATA_DIR 由 project.yaml 的 parameters 注入：
#   multi: false，选了一个目录 → /volume1/xxx；没选 → 字面量 null。
# 全新安装首启时参数和授权目录是空的（平台先起服务、约 3 秒后才写 .env，
# 进程环境是 exec 那一刻的快照），所以首启拿不到、走兜底，下次重启生效，
# 不是错误。这里只取一个真实存在的目录即可。
USER_DIR="${READEK_DATA_DIR:-}"
if [ "${USER_DIR}" = "null" ]; then USER_DIR=""; fi

STORE_DIR="${DATA_DIR}/data"
if [ -n "${USER_DIR}" ] && [ -d "${USER_DIR}" ]; then
    STORE_DIR="${USER_DIR}"
    echo "数据目录取自安装时选择的 READEK_DATA_DIR: ${STORE_DIR}"
else
    echo "未选择数据目录（或首启参数未生效），数据存到应用数据目录: ${STORE_DIR}"
fi

mkdir -p "${STORE_DIR}" "${LOG_DIR}" 2>/dev/null

# ---- 启动 ----
# 工作目录放到数据目录下：Readeck 首次启动会把 config.toml 生成在当前目录。
cd "${STORE_DIR}" || exit 1

export READECK_DATA_DIRECTORY="${STORE_DIR}"
export READECK_DATABASE_SOURCE="sqlite3:${STORE_DIR}/db.sqlite3"
export READECK_SERVER_HOST="0.0.0.0"
export READECK_SERVER_PORT="${WEBUI_PORT}"

exec "${INSTALL_DIR}/bin/readeck" serve
