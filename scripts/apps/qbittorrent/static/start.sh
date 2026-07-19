#!/bin/sh
# qBittorrent-Enhanced-Edition 启动脚本 (UGOS Pro 原生应用)
# 由系统以应用独立用户身份运行；工作目录 = 应用安装目录下的 data 目录。
set -u

# ---- 目录准备（全部来自系统环境变量，带兜底） ----
INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${INSTALL_DIR}/cache}"

# qBittorrent 使用 profile 目录存放配置与状态；放在可写的 data 下
PROFILE_DIR="${DATA_DIR}/profile"
CONF_DIR="${PROFILE_DIR}/qBittorrent/config"
CONF_FILE="${CONF_DIR}/qBittorrent.conf"
# 兼容旧版扁平布局的配置路径（部分构建把 conf 直接放在 qBittorrent/ 下）
CONF_DIR_FLAT="${PROFILE_DIR}/qBittorrent"
CONF_FILE_FLAT="${CONF_DIR_FLAT}/qBittorrent.conf"

# 配置模板版本标记：升级模板时递增，使已安装实例重启即自愈
CONF_VER=2
MARKER="${PROFILE_DIR}/.ugreen_conf_v${CONF_VER}"

# 默认下载目录：优先用用户已授权的共享目录，否则退回 data/downloads
if [ -n "${UGAPP_SHARED_DIR:-}" ] && [ -d "${UGAPP_SHARED_DIR}" ]; then
    DEFAULT_SAVE="${UGAPP_SHARED_DIR}"
else
    DEFAULT_SAVE="${DATA_DIR}/downloads"
fi

WEBUI_PORT=28080

mkdir -p "${CONF_DIR}" "${CONF_DIR_FLAT}" "${DEFAULT_SAVE}" "${LOG_DIR}" "${CACHE_DIR}" 2>/dev/null

# ---- 预置配置文件 ----
# 首次运行(配置不存在)，或本模板版本尚未应用过(无 MARKER)时写入。
# admin / adminadmin 的 PBKDF2 哈希（本机用 qBittorrent 算法 SHA512/100000轮/64字节 自算并回验）。
if [ ! -f "${CONF_FILE}" ] || [ ! -f "${MARKER}" ]; then
    _gen_conf() {
        cat <<EOF
[LegalNotice]
Accepted=true

[Preferences]
WebUI\\Address=*
WebUI\\Port=${WEBUI_PORT}
WebUI\\Username=admin
WebUI\\Password_PBKDF2="@ByteArray(obLD1OX2BxgpOktcbX6PkA==:iGZx/tBFjLUbJki/HPkqjYhld7x4HxoRoe5ts8X24DQlV4dRHoe9kgcISu/DGVPIO9loM2XEZdsNlZNdaDPzGA==)"
WebUI\\LocalHostAuth=false
WebUI\\CSRFProtection=false
WebUI\\ClickjackingProtection=false
WebUI\\HostHeaderValidation=false
Downloads\\SavePath=${DEFAULT_SAVE}
General\\Locale=zh

[BitTorrent]
Session\\DefaultSavePath=${DEFAULT_SAVE}
Session\\Port=6881
EOF
    }
    # 写到标准(嵌套 config/)与兼容(扁平)两处，规避不同构建的 profile 布局差异
    _gen_conf > "${CONF_FILE}"
    _gen_conf > "${CONF_FILE_FLAT}"
    touch "${MARKER}"
fi

# ---- 启动 ----
# --profile          指定 profile 目录（配置/状态存放处）
# --confirm-legal-notice  跳过首次法律声明交互
# --webui-port       固定 WebUI 端口（与 project.yaml 的 port 一致，供系统探测）
BIN="${INSTALL_DIR}/bin/qbittorrent-nox"
exec "${BIN}" \
    --profile="${PROFILE_DIR}" \
    --confirm-legal-notice \
    --webui-port="${WEBUI_PORT}"
