#!/bin/sh
# ANI-RSS 启动脚本 (UGOS Pro 原生应用)
# 捆绑 Temurin JRE + Debian C.utf8 locale，强制 UTF-8 路径编码。
set -u

INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${INSTALL_DIR}/cache}"

WEBUI_PORT=7789
CONFIG_DIR="${DATA_DIR}/config"
mkdir -p "${CONFIG_DIR}" "${LOG_DIR}" "${CACHE_DIR}" 2>/dev/null

if [ -n "${UGAPP_SHARED_DIR:-}" ] && [ -d "${UGAPP_SHARED_DIR}" ]; then
    MEDIA_HINT="${UGAPP_SHARED_DIR}"
else
    MEDIA_HINT="${DATA_DIR}/Media"
    mkdir -p "${MEDIA_HINT}/番剧" "${MEDIA_HINT}/剧场版" "${MEDIA_HINT}/已完结番剧" 2>/dev/null
fi

# 安装时选的媒体目录（project.yaml 的 MEDIA_PATH 参数）。ani-rss 自己不读这个变量，
# 打出来是给用户看的：WebUI 里的下载路径要填绝对路径，而"到底该填哪个"是最容易卡住的一步。
#
# 多值参数塞进一个环境变量的编码形状没有文档（可能是 [a b] 这样的切片字面量、
# JSON 数组、或逗号分隔），所以这里不去解析，原样打印 + 逐个探测常见拆法里
# 确实存在的目录。反正只是提示信息，猜错了也不影响启动。
if [ -n "${MEDIA_PATH:-}" ]; then
    echo "MEDIA_PATH(原始值)=${MEDIA_PATH}"
    # printf 末尾那个 \n 不能省：没有它最后一项后面没有换行，while read 会把它整个丢掉
    printf '%s\n' "${MEDIA_PATH}" | tr -d '[]"' | tr ',;' '  ' | tr ' ' '\n' | while IFS= read -r d; do
        [ -n "$d" ] || continue
        [ -d "$d" ] && echo "  已授权媒体目录: $d"
    done
fi

# ---- 强制 UTF-8（locale + JVM 双保险）----
if [ -d "${INSTALL_DIR}/locale" ]; then
    export LOCPATH="${INSTALL_DIR}/locale"
fi
export LANG=C.UTF-8
export LC_ALL=C.UTF-8
export LC_CTYPE=C.UTF-8
export LANGUAGE=C.UTF-8
export TZ="${TZ:-Asia/Shanghai}"

ENC_OPTS="-Dsun.jnu.encoding=UTF-8 -Dfile.encoding=UTF-8 -Dnative.encoding=UTF-8 -Dstdout.encoding=UTF-8 -Dstderr.encoding=UTF-8"
export JDK_JAVA_OPTIONS="${ENC_OPTS} ${JDK_JAVA_OPTIONS:-}"
export JAVA_TOOL_OPTIONS="${ENC_OPTS} ${JAVA_TOOL_OPTIONS:-}"

export CONFIG="${CONFIG_DIR}"
export SERVER_PORT="${WEBUI_PORT}"
export SERVER_ADDRESS="0.0.0.0"
export SWAGGER_ENABLED="${SWAGGER_ENABLED:-false}"
export MCP_ENABLED="${MCP_ENABLED:-false}"

if [ -z "${JAVA_OPTS:-}" ]; then
    JAVA_OPTS="-Xms64m -Xmx512m -Xss256k -XX:+UseG1GC"
fi

JAVA_BIN="${INSTALL_DIR}/jre/bin/java"
JAR_FILE="${INSTALL_DIR}/app/ani-rss.jar"

if [ ! -x "${JAVA_BIN}" ]; then
    echo "ERROR: bundled JRE not found: ${JAVA_BIN}" >&2
    exit 1
fi
if [ ! -f "${JAR_FILE}" ]; then
    echo "ERROR: ani-rss.jar not found: ${JAR_FILE}" >&2
    exit 1
fi

echo "ANI-RSS starting: port=${WEBUI_PORT} config=${CONFIG_DIR} media_hint=${MEDIA_HINT}"
echo "LANG=${LANG} LC_ALL=${LC_ALL} LOCPATH=${LOCPATH:-} JAVA_OPTS=${JAVA_OPTS}"


# shellcheck disable=SC2086
exec "${JAVA_BIN}" ${JAVA_OPTS} \
    -XX:+UseStringDeduplication \
    -XX:+UseCompactObjectHeaders \
    -XX:TieredStopAtLevel=1 \
    -XX:+IgnoreUnrecognizedVMOptions \
    --enable-native-access=ALL-UNNAMED \
    --add-opens=java.base/java.net=ALL-UNNAMED \
    --add-opens=java.base/sun.net.www.protocol.https=ALL-UNNAMED \
    -Dsun.jnu.encoding=UTF-8 \
    -Dfile.encoding=UTF-8 \
    -Dnative.encoding=UTF-8 \
    -Dstdout.encoding=UTF-8 \
    -Dstderr.encoding=UTF-8 \
    -Duser.language=zh \
    -Duser.country=CN \
    -Djava.io.tmpdir="${CACHE_DIR}" \
    -Djava.awt.headless=true \
    -jar "${JAR_FILE}" \
    --config="${CONFIG_DIR}" \
    --server.port="${WEBUI_PORT}" \
    --server.address=0.0.0.0
