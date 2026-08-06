#!/bin/sh
# OpenList 启动脚本 (UGOS Pro 原生应用)
#
# 由系统以应用独立的非 root 用户身份运行。OpenList 是 Go 单二进制（这里用的是
# musl 静态版），配置/数据库/索引都落在 --data 指定的目录里。
#
# 这个脚本干四件事：
#   1. 解析数据目录（安装时选的 OL_DATA_DIR，没选就退回应用数据目录）
#   2. 检查存储目录的授权是否已经生效，没生效就在日志里说清楚要重启一次
#   3. 管理员密码：安装时填了就应用它，没填就在首次安装时生成一个随机密码
#      并写进数据目录里的文本文件（这样用户在文件管理器里就能看到）
#   4. 用环境变量把监听端口/日志路径钉死，再 exec 起服务
#
# 关于"首次安装拿不到参数"：平台是【先起服务、2~3 秒后才写 .env 和授权目录】的，
# 而进程环境是 exec 那一刻的快照 —— 所以全新安装/升级后的第一次启动，
# OL_* 全是空、授权目录也还没挂进来。这不是错误，重启一次就好，
# 脚本会在日志里明确提示。
set -u

INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${DATA_DIR}/cache}"

BIN="${INSTALL_DIR}/bin/openlist"

# 必须与 project.yaml 的 port 一致 —— 系统靠它探测应用是否起来了
WEBUI_PORT=28244

mkdir -p "${LOG_DIR}" "${CACHE_DIR}" 2>/dev/null

# 沙箱里【没有 /tmp】，Go 的 os.TempDir() 仍会返回 /tmp，任何走临时文件的
# 代码路径都会失败。把 TMPDIR 指到 cache 目录，os.TempDir() 会跟着走。
TMPDIR="${CACHE_DIR}/tmp"
export TMPDIR
mkdir -p "${TMPDIR}" 2>/dev/null

echo "==== OpenList for UGOS Pro ===="

# ---------------------------------------------------------------
# 1. 数据目录
# ---------------------------------------------------------------
# OL_DATA_DIR 由 project.yaml 的 parameters 注入（type: path, multi: false）：
# 选了目录 → /volume1/xxx；一个都没选 → 字面量 "null"（不是空串）。
USER_DIR="${OL_DATA_DIR:-}"
[ "${USER_DIR}" = "null" ] && USER_DIR=""

STORE_DIR="${DATA_DIR}/openlist"
if [ -n "${USER_DIR}" ] && [ -d "${USER_DIR}" ]; then
    STORE_DIR="${USER_DIR}"
    echo "数据目录：${STORE_DIR}（安装时选择的 OL_DATA_DIR）"
elif [ -n "${USER_DIR}" ]; then
    echo "警告：选择的数据目录 ${USER_DIR} 在沙箱里不存在（授权可能还没生效）。"
    echo "      本次先用应用数据目录 ${STORE_DIR}；请在应用中心「停止 → 启动」一次。"
else
    echo "数据目录：${STORE_DIR}（未选择数据目录，或首次启动参数还没写入）"
fi
mkdir -p "${STORE_DIR}" 2>/dev/null || {
    echo "错误：无法创建数据目录 ${STORE_DIR}" >&2
    exit 1
}

# ---------------------------------------------------------------
# 2. 存储目录（OL_STORAGE_PATHS）的授权自检
# ---------------------------------------------------------------
# 这个参数的值本身 OpenList 用不上 —— 它的作用是【顺带授权】：选中的目录会被
# 记进平台的 path_permissions 表，进而生成 systemd unit 的 BindPaths=，
# 沙箱里才读写得到。用户之后在 OpenList 的管理后台添加"本机存储"时，填的就是
# 这里列出的路径。
#
# multi: true 的参数值是 JSON 数组（一个都没选时是字面量 null），沙箱里没有 jq，
# 这里用纯 shell 按引号切出每一项 —— 路径里可能有逗号和空格，不能按分隔符切。
STORAGE_RAW="${OL_STORAGE_PATHS:-}"
if [ -n "${STORAGE_RAW}" ] && [ "${STORAGE_RAW}" != "null" ]; then
    echo "已授权的存储目录（在 OpenList 后台添加「本机存储」时填这些路径）："
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
        # 嵌套检查：平台【只授权你显式选中的目录】，父子都选时父目录反而不会被挂载，
        # 在沙箱里退化成只含已授权子目录的合成视图（表现为"目录里的文件都不见了"）。
        case "${STORE_DIR}/" in
            "${item}/"*)
                echo "  !! 数据目录 ${STORE_DIR} 在这个存储目录【里面】。"
                echo "     平台不允许授权目录嵌套，这会导致 ${item} 里的其它内容看不见。"
                echo "     请把数据目录换到存储目录之外（或干脆留空，用应用数据目录）。"
                ;;
        esac
    done
else
    echo "未选择存储目录：OpenList 暂时只能挂载网盘等远程存储，挂不了 NAS 本机目录。"
    echo "  需要挂本机目录的话，去应用设置里把目录加进「本机存储目录」，然后重启应用。"
fi

# ---------------------------------------------------------------
# 3. 管理员密码
# ---------------------------------------------------------------
# OpenList 原生支持 OPENLIST_ADMIN_PASSWORD：【仅在首次创建 admin 用户时】
# 用它当初始密码，之后再改这个变量是没用的。所以这里还要补一条：值变了就用
# `openlist admin set` 真正改一次。
#
# 记号文件存在应用私有的数据目录里（不是用户目录），只用来判断"这个值应用过没有"，
# 从而做到：用户在 WebUI 里自己改的密码不会被每次重启冲掉，而在应用设置里改了
# 参数就能生效（也是忘记密码时的找回途径）。
ADMIN_PWD="${OL_ADMIN_PASSWORD:-}"
[ "${ADMIN_PWD}" = "null" ] && ADMIN_PWD=""
PW_MARK="${DATA_DIR}/.admin_password_applied"

if [ -n "${ADMIN_PWD}" ]; then
    export OPENLIST_ADMIN_PASSWORD="${ADMIN_PWD}"
    SAVED=""
    [ -f "${PW_MARK}" ] && IFS= read -r SAVED < "${PW_MARK}"
    if [ "${SAVED}" != "${ADMIN_PWD}" ]; then
        echo "应用设置里的管理员密码有变化，正在写入…"
        # 输出丢掉：这条命令会把明文密码打到 stdout，不该进应用日志
        if "${BIN}" admin set "${ADMIN_PWD}" --data "${STORE_DIR}" >/dev/null 2>&1; then
            ( umask 077; printf '%s\n' "${ADMIN_PWD}" > "${PW_MARK}" )
            echo "管理员密码已更新（用户名 admin）。"
        else
            echo "警告：设置管理员密码失败，本次沿用原有密码。" >&2
        fi
    fi
elif [ ! -f "${STORE_DIR}/data.db" ]; then
    # 全新安装 + 没填密码：让 OpenList 自己随机生成一个，把它捞出来写成文件，
    # 用户在文件管理器里打开数据目录就能看到（否则只能去翻系统日志）。
    echo "未设置管理员密码，正在初始化并生成一个随机密码…"
    OUT=$("${BIN}" admin random --data "${STORE_DIR}" 2>/dev/null)
    # 输出末尾形如 "password: xxxxxxxx"，取最后一个 "password: " 之后的内容
    GEN=""
    case "${OUT}" in
        *"password: "*) GEN="${OUT##*password: }" ;;
    esac
    # 只接受"单行、无空白"的结果 —— 解析不出来就别写个垃圾文件误导用户
    case "${GEN}" in
        ""|*[[:space:]]*) GEN="" ;;
    esac
    if [ -n "${GEN}" ]; then
        PW_FILE="${STORE_DIR}/openlist-初始管理员密码.txt"
        ( umask 077
          {
            printf '用户名: admin\n'
            printf '密码:   %s\n' "${GEN}"
            printf '\n登录后请尽快在 OpenList 里修改密码，改完可以删除本文件。\n'
            printf '也可以在绿联「应用中心 → OpenList → 设置」里改「管理员密码」，重启应用后生效。\n'
          } > "${PW_FILE}" )
        echo "初始管理员密码已写入：${PW_FILE}"
    else
        echo "警告：没能取到自动生成的初始密码。请在应用设置里填「管理员密码」后重启应用。" >&2
    fi
fi

# ---------------------------------------------------------------
# 4. 启动
# ---------------------------------------------------------------
# OpenList 读环境变量的前缀是 OPENLIST_，且 Scheme 这一组【没有子前缀】，
# 所以端口/监听地址就是 OPENLIST_HTTP_PORT / OPENLIST_ADDR（不是 SCHEME_*）。
# 环境变量在读完 config.json 之后覆盖、且不回写文件 —— 也就是说 config.json 里
# 的 http_port 会被这里的值盖掉，这是故意的：端口必须和 project.yaml 的 port
# 一致，否则系统会判定应用没起来。
# （例外：config.json 里如果把 "force" 设成 true，OpenList 会跳过整个环境变量
#   覆盖流程，那样端口就回到文件里的值、应用会被判定为未启动。别改那个字段。）
export OPENLIST_ADDR="0.0.0.0"
export OPENLIST_HTTP_PORT="${WEBUI_PORT}"
export OPENLIST_HTTPS_PORT="-1"
# 日志按平台约定放进 log 目录，别塞进用户的数据目录
export OPENLIST_LOG_NAME="${LOG_DIR}/openlist.log"
# 上传/解压的临时文件走 cache 目录，不污染用户选的数据目录
export OPENLIST_TEMP_DIR="${TMPDIR}"

echo "监听 0.0.0.0:${WEBUI_PORT}，数据目录 ${STORE_DIR}"

# 工作目录设成数据目录：OpenList 里少数相对路径是按当前目录解析的
cd "${STORE_DIR}" || exit 1

# exec：让 openlist 直接接管 PID，SIGTERM 能到它手上走优雅关闭
exec "${BIN}" server --data "${STORE_DIR}"
