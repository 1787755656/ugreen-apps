#!/bin/sh
# Cloudreve 启动脚本 (UGOS Pro 原生应用)
#
# 由系统以应用独立的非 root 用户身份运行。Cloudreve 4 是 Go 单二进制
# （前端 go:embed 在里面），配置/数据库/上传文件都按「可执行文件目录」解析，
# 而安装目录是【只读】的 —— 所以这里做四件事：
#
#   0. GODEBUG 消毒 —— 剥掉 UGOS 全局注入的已移除项(tlskyber=0 等)，
#      Go 1.27 编译的二进制见到它会直接 fatal（见下面第 0 节）
#   1. 用 -w 把「可执行文件目录」换成工作目录，并 cd 到应用数据目录
#      ⇒ data/conf.ini、data/cloudreve.db、data/uploads 全部落在可写位置
#   2. 首次启动生成 conf.ini（端口/监听地址/SQLite 路径/SessionSecret），
#      之后不再覆盖，用户在 conf.ini 里的修改会保留
#   3. TMPDIR / HOME / CA 根证书重定向（沙箱没有 /tmp，也没有系统 CA）
#
# 关于"首次安装拿不到参数"：平台是【先起服务、2~3 秒后才写 .env 和授权目录】的，
# 所以全新安装/升级后的第一次启动 CR_STORAGE_PATHS 是空的、授权目录也还没挂进来。
# 脚本只把值打进日志，不阻塞启动；重启一次即可。
set -u

INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${DATA_DIR}/cache}"
BIN="${INSTALL_DIR}/bin/cloudreve"

# Cloudreve 的全部持久数据都放这里；-w + cd 之后相对路径自动指向它
CR_HOME="${DATA_DIR}/cloudreve"
CR_DATA="${CR_HOME}/data"
CONF="${CR_DATA}/conf.ini"

# 必须与 project.yaml 的 port 一致 —— 系统靠它探测应用是否起来了
WEBUI_PORT=25212

mkdir -p "${CR_DATA}" "${LOG_DIR}" "${CACHE_DIR}" 2>/dev/null

# 沙箱里【没有 /tmp】，Go 的 os.TempDir() 仍会返回 /tmp，任何走临时文件的
# 代码路径都会失败（分片上传、压缩包预览等）。把 TMPDIR 指到可写目录。
TMPDIR="${CACHE_DIR}/tmp"
export TMPDIR
mkdir -p "${TMPDIR}" 2>/dev/null

# 少数库会读 HOME（沙箱里它可能是 /root 之类不可写的位置），一并指到数据目录
HOME="${CR_HOME}"
export HOME

# ---------------------------------------------------------------
# 0. GODEBUG 消毒（Go 1.27 fatal 防护）
# ---------------------------------------------------------------
# UGOS 的 systemd 全局配置(/etc/systemd/system.conf)给【所有】服务注入了
# DefaultEnvironment="GODEBUG=tlskyber=0"。Go 1.24 起陆续移除旧 GODEBUG 项，
# 到 Go 1.27 已移除 9 项(x509sha1/tlskyber/gotypesalias/tlsunsafeekm/tlsrsakex/
# tls3des/tls10server/x509keypairleaf/asynctimerchan)，规则是:环境里出现已移除
# 项【且值等于旧默认值】时进程直接 fatal(https://go.dev/doc/godebug#go-124)。
# Cloudreve 用上游预编译二进制，编译期用的 Go 版本不受我们控制，所以无条件消毒。
SANITIZE_GODEBUG() {
    (   # 子 shell 里改,不影响外层
        SAN_RAW="${GODEBUG:-}"
        [ -n "${SAN_RAW}" ] || return 0
        SAN_OUT=""
        SAN_IFS="${IFS}"
        IFS=','
        for SAN_ITEM in ${SAN_RAW}; do
            case "${SAN_ITEM}" in
                x509sha1=*|tlskyber=*|gotypesalias=*|tlsunsafeekm=*|tlsrsakex=*|tls3des=*|tls10server=*|x509keypairleaf=*|asynctimerchan=*)
                    echo "已剥离已移除的 GODEBUG 项: ${SAN_ITEM}（新版 Go 运行时不再接受）" >&2
                    continue
                    ;;
            esac
            [ -n "${SAN_ITEM}" ] || continue
            if [ -z "${SAN_OUT}" ]; then SAN_OUT="${SAN_ITEM}"; else SAN_OUT="${SAN_OUT},${SAN_ITEM}"; fi
        done
        IFS="${SAN_IFS}"
        if [ -n "${SAN_OUT}" ]; then
            printf '%s' "${SAN_OUT}"
        fi
    )
}
SANITIZED="$(SANITIZE_GODEBUG)"
if [ -n "${SANITIZED}" ]; then
    export GODEBUG="${SANITIZED}"
else
    unset GODEBUG
fi
unset SANITIZED

# ---------------------------------------------------------------
# 1. CA 根证书
# ---------------------------------------------------------------
# 原生沙箱根目录里没有 /etc/ssl/certs。Cloudreve 对接 OneDrive / S3 /
# WebDAV 等远端存储时要校验 TLS，缺根证书会一律 x509: certificate signed
# by unknown authority。build.sh 把 Mozilla 的 CA 包放进 ca/ 目录。
CR_CA="${INSTALL_DIR}/ca/ca-certificates.crt"
if [ -f "${CR_CA}" ]; then
    SSL_CERT_FILE="${CR_CA}"
    export SSL_CERT_FILE
else
    echo "警告：包内没有 ca/ca-certificates.crt，访问 HTTPS 存储端可能报证书错误。" >&2
fi

echo "==== Cloudreve for UGOS Pro ===="
echo "安装目录：${INSTALL_DIR}"
echo "数据目录：${CR_HOME}"

# ---------------------------------------------------------------
# 2. 首次启动生成 conf.ini
# ---------------------------------------------------------------
# 配置文件的默认路径是 data/conf.ini（相对可执行文件目录）；我们用绝对路径
# -c 传进去，配合 -w + cd 让 Cloudreve 内部所有 DataPath 都落到 CR_HOME。
#
# SessionSecret 是会话签名密钥，必须【持久且每个安装不同】：
#   - 不写：Cloudreve 用空密钥签名，任何人可伪造会话 Cookie；
#   - 每次启动随机：重启后全体登出，且多进程不一致。
# 所以只在首次生成时随机 64 位并写进 conf.ini，之后一直复用。
if [ ! -f "${CONF}" ]; then
    echo "首次启动：生成配置文件 ${CONF}"
    SESSION_SECRET=$(head -c 96 /dev/urandom 2>/dev/null | base64 2>/dev/null | tr -dc 'A-Za-z0-9' | head -c 64)
    if [ -z "${SESSION_SECRET}" ]; then
        # base64 不可用的极端情况：退化成 hex（仍然来自内核 CSPRNG）
        SESSION_SECRET=$(od -An -tx1 -N32 /dev/urandom 2>/dev/null | tr -d ' \n')
    fi
    if [ -z "${SESSION_SECRET}" ]; then
        echo "错误：无法生成 SessionSecret（/dev/urandom 不可用），拒绝以空密钥启动。" >&2
        exit 1
    fi

    cat > "${CONF}" <<EOF
; Cloudreve 配置文件（由 UGOS 启动脚本在首次启动时生成）
; 改完需要重启应用。完整字段见 https://docs.cloudreve.org/zh/overview/configure
[System]
Mode = master
; 监听所有网卡，端口必须与 project.yaml 的 port 一致
Listen = 0.0.0.0:${WEBUI_PORT}
Debug = false
LogLevel = info
; 应用停止时最多等 5 秒让数据库优雅收尾（unit 只给 10 秒）
GracePeriod = 5
SessionSecret = ${SESSION_SECRET}

[Database]
; 默认 SQLite（modernc.org/sqlite，纯 Go，无需外部数据库）
Type = sqlite
DBFile = ${CR_DATA}/cloudreve.db
EOF
    chmod 600 "${CONF}" 2>/dev/null
else
    echo "沿用已有配置：${CONF}"
    if ! grep -q '^[[:space:]]*SessionSecret[[:space:]]*=[[:space:]]*[^[:space:]]' "${CONF}"; then
        echo "警告：conf.ini 里 SessionSecret 为空，会话可被伪造。建议停止应用、" >&2
        echo "      删掉该行后重启（会自动生成新的），代价是全体用户重新登录。" >&2
    fi
fi

# ---------------------------------------------------------------
# 3. 本机存储目录（只做可访问性自检，值本身不传给 Cloudreve）
# ---------------------------------------------------------------
# CR_STORAGE_PATHS 这个 path 参数的作用是【顺带授权】：选中的目录进平台的
# path_permissions 表，生成 systemd unit 的 BindPaths=，沙箱里才读写得到。
# 用户随后在 Cloudreve 管理面板里建「本机存储」策略，填的就是这些路径。
# multi: true 的值是 JSON 数组（没选时是字面量 null），沙箱里没有 jq，
# 这里用纯 shell 按引号切分（路径可能含空格/逗号，不能按分隔符切）。
STORAGE_RAW="${CR_STORAGE_PATHS:-}"
if [ -n "${STORAGE_RAW}" ] && [ "${STORAGE_RAW}" != "null" ]; then
    echo "已授权的本机存储目录（在 Cloudreve「存储策略 → 本机」里填这些路径）："
    rest="${STORAGE_RAW}"
    while :; do
        case "${rest}" in
            *'"'*) ;;
            *) break ;;
        esac
        rest="${rest#*\"}"
        item="${rest%%\"*}"
        rest="${rest#*\"}"
        [ -z "${item}" ] && continue
        if [ -d "${item}" ]; then
            echo "  [可访问] ${item}"
        else
            echo "  [不可访问] ${item} —— 授权还没生效，请在应用中心「停止 → 启动」一次"
        fi
    done
else
    echo "未选择本机存储目录：只能使用网盘/对象存储（需要 NAS 本机文件时去应用设置里授权）。"
fi

# ---------------------------------------------------------------
# 4. 启动
# ---------------------------------------------------------------
# -w / --use-working-dir：把 Cloudreve 的「可执行文件目录」基准换成当前工作
#   目录。安装目录只读，不加这个参数时默认存储策略会指向
#   /var/packages/<appid>/data/uploads（写不进去），SQLite/缓存的默认路径同理。
# 然后 cd 到 CR_HOME，让所有相对路径落到可写的数据目录。
echo "监听 0.0.0.0:${WEBUI_PORT}，配置 ${CONF}"
cd "${CR_HOME}" || exit 1

# exec：让 cloudreve 直接接管 PID，SIGTERM 能到它手上走优雅关闭
exec "${BIN}" server -w -c "${CONF}"
