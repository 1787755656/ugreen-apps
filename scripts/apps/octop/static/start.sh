#!/bin/bash
# ============================================================
# Octop 绿联 UGOS Pro 原生版启动脚本（arm64）
# 逻辑移植自官方 fnOS 包 app/bin/octop（适配 UGAPP_* 环境变量）
#
# 目录约定：
#   <安装目录>/python/          python-build-standalone cpython 3.12（只读）
#   <安装目录>/site-packages/   钉版依赖（只读，import 兜底）
#   <数据目录>/home/            HOME（playwright 浏览器等用户级缓存落这里）
#   <数据目录>/.octop/          OCTOP_HOME（数据库/配置/工作区，核心数据）
#   <数据目录>/site-packages/   应用内「在线升级」的安装目标（可写、升级保留）
#   <缓存目录>/pycache          PYTHONPYCACHEPREFIX（安装目录只读，字节码外置）
# ============================================================
set -uo pipefail

APP_ID="com.tencent.octop"
# UGAPP_* 由平台注入（新固件布局 /var/packages/<id>，旧布局 /volume1/@appstore/<id>）。
# fallback 只在绕过平台手动跑时用，按真实存在者探测。
if [ -z "${UGAPP_INSTALL_DIR:-}" ]; then
    if [ -d "/var/packages/${APP_ID}" ]; then
        INSTALL="/var/packages/${APP_ID}"; DATA="${INSTALL}/data"; LOGD="${INSTALL}/log"
    else
        INSTALL="/volume1/@appstore/${APP_ID}"; DATA="/volume1/@appdata/${APP_ID}"; LOGD="/volume1/@applog/${APP_ID}"
    fi
else
    INSTALL="${UGAPP_INSTALL_DIR}"
    DATA="${UGAPP_DATA_DIR:-${INSTALL}/data}"
    LOGD="${UGAPP_LOG_DIR:-${INSTALL}/log}"
fi
CACHED="${UGAPP_CACHE_DIR:-${DATA}/cache}"
SHARED="${UGAPP_SHARED_DIR:-}"

PY="$INSTALL/python/bin/python3.12"

# ---- 沙箱目录准备（shared 可能不挂，mkdir 必须容错）----
mkdir -p "$DATA" "$LOGD" "$CACHED" 2>/dev/null || true
mkdir -p "$DATA/home" "$DATA/tmp" "$DATA/site-packages" 2>/dev/null || true
mkdir -p "$CACHED/pycache" 2>/dev/null || true
[ -n "$SHARED" ] && mkdir -p "$SHARED" 2>/dev/null || true

# ---- 沙箱没有 /tmp，所有临时文件指到数据目录 ----
export TMPDIR="${DATA}/tmp"

# ---- HOME / Octop 数据目录（与 fnOS 版 bin/octop 同名变量，语义一致）----
export HOME="${DATA}/home"
export OCTOP_HOME="${DATA}/.octop"
export OCTOP_DATA_DIR="${DATA}"
mkdir -p "$OCTOP_HOME"

# ---- Python 运行时 ----
# 数据目录的 site-packages 放最前：应用内在线升级装到这里（OCTOP_FPK_SITE_PACKAGES），
# 重启即加载新版；安装目录里的钉版依赖只读兜底（self_update 的 fpk-native 分支约定）
export PYTHONPATH="${DATA}/site-packages:${INSTALL}/site-packages"
export OCTOP_INSTALL_MODE="fpk-native"
export OCTOP_FPK_SITE_PACKAGES="${DATA}/site-packages"

# ---- 面板版本号（当前版本 / 左上角 vX.Y.Z）的坑 -----------------------------
# 上游 self_update.get_local_version() 的规则是：只要 OCTOP_FPK_SITE_PACKAGES
# 非空，就不再查安装目录里的 dist-info，而是【起子进程只在这个目录里 import
# octop】读版本（还要求 octop.__file__ 落在该目录内）。而这个目录是「应用内
# 在线升级」的安装目标，刚装完是空的 → 探测必失败 → 恒返回 0.0.0，面板就一直
# 显示 0.0.0 并提示有新版本（补 dist-info 治不了这个分支）。
# 修法：启动时保证该目录里有一份 octop —— 空就硬链接安装目录里的那份（同盘
# 秒级、不额外占空间，跨盘自动退回拷贝）；如果是应用内升级装进来的独立副本
# （inode 不同）则原样保留，不能覆盖用户升级的成果。
sync_octop_pkg_to_data() {
    local src="${INSTALL}/site-packages" dst="${DATA}/site-packages"
    [ -d "$src/octop" ] || return 0
    local rebuild=0 src_di dst_di
    if [ ! -f "$dst/octop/__init__.py" ]; then
        rebuild=1
    elif [ "$dst/octop/__init__.py" -ef "$src/octop/__init__.py" ]; then
        # 是硬链接副本：只有安装目录换了版本（dist-info 目录名变了）才重建
        src_di=$(basename "$(ls -d "$src"/octop-*.dist-info 2>/dev/null | head -1)" 2>/dev/null)
        dst_di=$(basename "$(ls -d "$dst"/octop-*.dist-info 2>/dev/null | head -1)" 2>/dev/null)
        [ "$src_di" != "$dst_di" ] && rebuild=1
    fi
    [ "$rebuild" = 1 ] || return 0
    rm -rf "$dst/octop" "$dst"/octop-*.dist-info 2>/dev/null || true
    if cp -al "$src/octop" "$dst/" 2>/dev/null; then
        echo "[octop-ugreen] 已硬链接 octop 到数据目录（版本号探测用）"
    elif cp -a "$src/octop" "$dst/" 2>/dev/null; then
        echo "[octop-ugreen] 已拷贝 octop 到数据目录（版本号探测用，跨盘退回）"
    else
        echo "[octop-ugreen] 警告：octop 无法放进数据目录，面板版本号可能仍是 0.0.0" >&2
        return 0
    fi
    cp -a "$src"/octop-*.dist-info "$dst/" 2>/dev/null || true
}
sync_octop_pkg_to_data

export PYTHONPYCACHEPREFIX="${CACHED}/pycache"
export PYTHONUNBUFFERED=1
# 沙箱 /etc 下无 CA bundle 保证，Python 出站 HTTPS（LLM API）用 certifi
SSL_CERT_FILE="$("$PY" -c 'import certifi,sys;sys.stdout.write(certifi.where())' 2>/dev/null || true)"
[ -n "$SSL_CERT_FILE" ] && export SSL_CERT_FILE

# ---- 服务参数 ----
export OCTOP_PORT="${OCTOP_PORT:-28088}"
export OCTOP_BIND_HOST="${OCTOP_BIND_HOST:-0.0.0.0}"
export OCTOP_LOG_LEVEL="${OCTOP_LOG_LEVEL:-info}"

octop_cmd() { "$PY" -m octop.cli.main "$@"; }

# ---- 管理员账号默认凭据 ------------------------------------------------------
# 早期版本是「随机密码 + 写进共享目录的 octop-login.txt」，但原生沙箱里
# UGAPP_SHARED_DIR 不保证可写/可见（真机上文件没落进授权目录，用户拿不到密码
# 就等于进不去），所以改成固定默认密码，并写进应用介绍里，登录后自行修改。
# 必须过上游密码策略：≥8 位、含字母和数字、不在弱口令黑名单（octop123 在黑名单里）。
# 需要别的默认值可用 OCTOP_DEFAULT_PASSWORD 覆盖（例如自建流水线时）。
DEFAULT_ADMIN_USER="admin"
DEFAULT_ADMIN_PASSWORD="octop2026"

# 兜底用：万一默认密码被上游策略改了，退回随机密码并打到应用日志里
gen_password() {
    "$PY" - <<'PYEOF'
import secrets
letters = 'abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ'
digits = '23456789'
pool = letters + digits
pw = secrets.choice(letters)
pw += ''.join(secrets.choice(pool) for _ in range(14))
pw += secrets.choice(digits)
print(pw)
PYEOF
}

write_login_file() {
    # $1=用户名 $2=密码。只是留个记录：数据目录必然可写；共享目录可写就再放一份
    # （放不进去不算失败 —— 默认密码本来就写在应用介绍里，不依赖这个文件）
    local user="$1" pw="$2" f
    umask 077
    for f in "${DATA}/octop-login.txt" "${SHARED:+${SHARED}/octop-login.txt}"; do
        [ -z "$f" ] && continue
        {
            echo "Octop AI 助手平台（绿联 UGOS 版）"
            echo "================================="
            echo "生成时间: $(date '+%Y-%m-%d %H:%M:%S')"
            echo "内嵌入口: 桌面 → Octop 图标"
            echo "直连地址: http://<NAS_IP>:${OCTOP_PORT}"
            echo "管理员账号: ${user}"
            echo "初始密码: ${pw}"
            echo "（登录后请在「控制台 → 设置」里修改密码；本文件仅作记录，可删除）"
        } > "$f" 2>/dev/null || true
    done
    umask 022
}

# ---- 管理员密码：应用中心参数优先，留空用默认值 ------------------------------
# 应用中心「配置」里的 OCTOP_ADMIN_PASSWORD（project.yaml 里 type: password）
# 由平台以【同名环境变量】注入（同 openlist 的 OL_ADMIN_PASSWORD 用法）。
#   - 首次初始化（没有 octop.db）：用参数值；没填就用默认 octop2026
#   - 之后每次启动：参数里有值、且和上次应用过的不一样 → 用 `user passwd` 重置
#     （所以「忘了密码」时到应用中心填个新值、重启应用就能拿回来）
#   - 参数留空：完全不碰密码，由应用内「控制台 → 设置」自己管
# 记号文件只存 sha256，不落明文（数据目录本身是应用私有的）。
ADMIN_USER="${OCTOP_ADMIN_USERNAME:-$DEFAULT_ADMIN_USER}"
PARAM_PW="${OCTOP_ADMIN_PASSWORD:-}"
[ "$PARAM_PW" = "null" ] && PARAM_PW=""
PW_MARK="$OCTOP_HOME/.ugos-admin-password.sha256"

sha256_of_str() {
    printf '%s' "$1" | "$PY" -c 'import sys,hashlib;print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest())'
}

# ---- 首次初始化管理员账号 ----
if [ ! -f "$OCTOP_HOME/octop.db" ]; then
    if [ -n "$PARAM_PW" ]; then
        INIT_PW="$PARAM_PW"
        echo "[octop-ugreen] 首次启动，用应用中心配置的密码初始化管理员账号 ${ADMIN_USER} ..."
    else
        INIT_PW="${OCTOP_DEFAULT_PASSWORD:-$DEFAULT_ADMIN_PASSWORD}"
        echo "[octop-ugreen] 首次启动，初始化管理员账号 ${ADMIN_USER}（默认密码）..."
    fi
    if ! octop_cmd init --yes \
        --admin-username "$ADMIN_USER" \
        --admin-password "$INIT_PW"; then
        # 走到这里说明密码没过上游策略（太长/太短/命中黑名单），退回随机密码，
        # 并把密码打到 stdout —— 它会进应用日志，用户/客服都能查到。
        echo "[octop-ugreen] 配置的密码未通过上游密码策略，改用随机密码。"
        INIT_PW="$(gen_password)"
        octop_cmd init --yes \
            --admin-username "$ADMIN_USER" \
            --admin-password "$INIT_PW" || {
            echo "[octop-ugreen] 错误：管理员账号初始化失败，请查看日志。" >&2
            exit 1
        }
    fi
    if [ -n "$PARAM_PW" ]; then
        ( umask 077; printf '%s\n' "$(sha256_of_str "$PARAM_PW")" > "$PW_MARK" )
    fi
    write_login_file "$ADMIN_USER" "$INIT_PW"
    echo "[octop-ugreen] 管理员账号：${ADMIN_USER}    初始密码：${INIT_PW}"
    echo "[octop-ugreen] 登录后请到「控制台 → 设置」里修改密码。"
else
    # 已有数据库：只有「应用中心里填了密码、且与上次应用过的不同」才重置，
    # 免得用户自己在应用里改的密码每次重启被冲掉。
    if [ -n "$PARAM_PW" ]; then
        WANT_SHA="$(sha256_of_str "$PARAM_PW")"
        APPLIED_SHA=""
        [ -f "$PW_MARK" ] && IFS= read -r APPLIED_SHA < "$PW_MARK"
        if [ "$WANT_SHA" != "$APPLIED_SHA" ]; then
            echo "[octop-ugreen] 应用中心里的管理员密码有变化，正在重置 ${ADMIN_USER} 的密码 ..."
            # 输出丢掉：这条命令会把明文密码打到 stdout，不该进应用日志
            if octop_cmd user passwd "$ADMIN_USER" --password "$PARAM_PW" >/dev/null 2>&1; then
                ( umask 077; printf '%s\n' "$WANT_SHA" > "$PW_MARK" )
                echo "[octop-ugreen] 管理员密码已更新为应用中心里配置的值。"
            else
                echo "[octop-ugreen] 警告：重置管理员密码失败，本次沿用原密码。" >&2
            fi
        fi
    fi
fi

echo "[octop-ugreen] 正在启动 Octop，${OCTOP_BIND_HOST}:${OCTOP_PORT} ..."
exec "$PY" -m octop.cli.main run --host "$OCTOP_BIND_HOST" --port "$OCTOP_PORT" --log-level "$OCTOP_LOG_LEVEL"
