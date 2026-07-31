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

# 默认下载目录：优先用安装时选的目录（project.yaml 的 DOWNLOAD_PATH 参数）。
#
# 为什么不再用 UGAPP_SHARED_DIR：那是个【三层的合成视图】
# (shared/volume1/<共享文件夹>/<授权目录>)，只有最里面那层叶子目录是真正的可写挂载，
# 根目录本身写入会 EACCES。把它当 SavePath，qb 一下载就是 permission denied，
# 用户得自己去 WebUI 改路径才能用。DOWNLOAD_PATH 拿到的是真实叶子路径，可以直接写。
#
# 多值参数塞进一个环境变量的编码形状没有文档（可能是 [a b] 切片字面量、JSON 数组、
# 或逗号/分号分隔），所以不去解析整体，而是把几种常见拆法产生的候选逐个探测，
# 取第一个真实存在的目录。猜错也不至于坏事：退回原来的兜底。
DEFAULT_SAVE=""
if [ -n "${DOWNLOAD_PATH:-}" ]; then
    # 末尾那个 \n 不能省：没有它最后一项后面没换行，while read 会整个丢掉
    DEFAULT_SAVE=$(printf '%s\n' "${DOWNLOAD_PATH}" | tr -d '[]"' | tr ',;' '  ' | tr ' ' '\n' \
        | while IFS= read -r d; do
              [ -n "$d" ] && [ -d "$d" ] && { echo "$d"; break; }
          done)
fi

if [ -n "${DEFAULT_SAVE}" ]; then
    echo "下载目录取自安装时选择的 DOWNLOAD_PATH: ${DEFAULT_SAVE}"
elif [ -n "${UGAPP_SHARED_DIR:-}" ] && [ -d "${UGAPP_SHARED_DIR}" ]; then
    # 旧行为保留作兜底（老版本装上来、或用户只走了「访问路径」授权没填参数）。
    # 注意这个路径不可写，用户仍需在 WebUI 里指到具体的子目录。
    DEFAULT_SAVE="${UGAPP_SHARED_DIR}"
    echo "警告：未选择下载目录，暂用共享目录根 ${DEFAULT_SAVE}（该目录不可写，请在 WebUI 里改成具体的子目录）"
else
    DEFAULT_SAVE="${DATA_DIR}/downloads"
    echo "未选择下载目录，退回应用数据目录: ${DEFAULT_SAVE}"
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
