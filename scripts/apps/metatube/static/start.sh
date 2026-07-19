#!/bin/sh
# MetaTube Server 启动脚本 (UGOS Pro 原生应用)
# 影视媒体元数据刮削 API 服务。由系统以应用独立用户身份运行。
set -u

# ---- 目录准备(来自系统环境变量, 带兜底) ----
INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"

mkdir -p "${DATA_DIR}" "${LOG_DIR}" 2>/dev/null

# ---- 服务配置 ----
# 内部监听端口(须与 project.yaml 的 port 一致, 供系统探活)。
# 选用高位冷门端口, 规避与其它应用/系统组件冲突(8080 常被占用)。
SERVER_PORT=28173
# SQLite 数据库文件(持久化到可写的 data 目录)
DB_FILE="${DATA_DIR}/metatube.db"

# 访问令牌: 由绿联根据用户在安装页填写的 parameters(key=TOKEN) 注入为环境变量。
# 用户未填时为空, 此时服务以无鉴权方式开放(仅局域网/网关隔离)。
TOKEN_VAL="${TOKEN:-}"

export GIN_MODE="release"

# ---- 启动 ----
# 用命令行参数显式传参(ff 优先级: 命令行 > 环境变量 > 默认值, 命令行最可靠):
#   --port            监听端口
#   --dsn             SQLite 数据库文件路径(文件持久化模式)
#   --db-auto-migrate 首次自动建表
#   --token           非空时才追加, 启用 API 鉴权
# 用 exec 让服务成为主进程, 正确接收系统 SIGTERM。
BIN="${INSTALL_DIR}/bin/metatube-server"

set -- --port "${SERVER_PORT}" --dsn "${DB_FILE}" --db-auto-migrate
if [ -n "${TOKEN_VAL}" ]; then
    set -- "$@" --token "${TOKEN_VAL}"
fi

exec "${BIN}" "$@"
