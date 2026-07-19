#!/bin/sh
# SakuraFrp (樱花FRP) 启动器 natfrp-service 启动脚本 (UGOS Pro 原生应用)
# 内网穿透客户端, 自带本地 WebUI 管理界面。静态 Go 二进制, 无外部依赖。
set -u

# ---- 目录准备(来自系统环境变量, 带兜底) ----
INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"

mkdir -p "${DATA_DIR}" "${LOG_DIR}" 2>/dev/null

# ---- 组件路径 ----
BIN="${INSTALL_DIR}/bin/natfrp-service"
FRPC="${INSTALL_DIR}/bin/frpc"

# WebUI 监听端口(须与 project.yaml 的 port 一致, 供系统探活)
WEBUI_PORT=7102
# WebUI 监听地址: 0.0.0.0 监听所有网卡, 樱花自动生成自签名证书走 HTTPS。
# 说明: 本应用为"直连 IP:7102"访问, 浏览器直接连 https://NAS-IP:7102。
# 必须走 HTTPS —— 直连 HTTP 是"非安全上下文", 樱花 WebUI 会卡在"连接中..."(官方已声明且不修)。
WEBUI_HOST="0.0.0.0"
# WebUI 首次默认密码(樱花要求至少 8 字符)。用户首次登录后请在 WebUI 中修改。
WEBUI_PASS="admin888"

# 工作目录: 核心服务所有文件相对于工作目录创建; 指向可写的 data 目录。
export NATFRP_SERVICE_WD="${DATA_DIR}"
# 指定 frpc 可执行文件路径。
export NATFRP_FRPC_PATH="${FRPC}"

# 配置文件位置(相对工作目录)。
CONF="${DATA_DIR}/config.json"

# ---- 仅首次生成一份最小可用配置 ----
# 重要: 实测本设备上绿联不会把 parameters 注入为环境变量, 故访问密钥/远程管理
#       一律交由用户在 WebUI (https://NAS-IP:7102) 中手动配置。
# 本脚本只在 config.json 不存在时(真正首次安装)生成一份能让 WebUI 起来的最小配置,
# 之后【永不覆盖】—— 保证用户在 WebUI 里填的访问密钥、远程管理等设置始终保留。
if [ ! -f "${CONF}" ]; then
    # update_interval: -1  禁用自动更新(沙箱安装目录只读, 自动更新会失败)。
    # webui_origin_mode: any  放开 Origin 检查, 避免边界情况下 WebSocket 被拒。
    cat > "${CONF}" <<EOF
{
  "log_stdout": true,
  "update_interval": -1,
  "webui_host": "${WEBUI_HOST}",
  "webui_port": ${WEBUI_PORT},
  "webui_pass": "${WEBUI_PASS}",
  "webui_origin_mode": "any"
}
EOF
fi

# ---- 启动 ----
# --daemon: 以守护进程运行(不带此开关会打印提示并退出)。
# -c: 指定配置文件路径。
# 用 exec 让服务成为主进程, 正确接收系统 SIGTERM。
cd "${DATA_DIR}" || exit 1
exec "${BIN}" --daemon -c "${CONF}"
