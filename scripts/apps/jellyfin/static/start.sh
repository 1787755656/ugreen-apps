#!/bin/sh
# Jellyfin Server 启动脚本 (UGOS Pro 原生应用)
# 自包含 .NET 运行时 + 自定义 jellyfin-ffmpeg(含 RK/VAAPI/QSV 等硬件加速支持)。
# 严格对齐 Jellyfin 官方 systemd 启动约定(工作目录=数据目录, 各 dir 显式传参)。
set -u

# ---- 目录准备(来自系统环境变量, 带兜底) ----
INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${INSTALL_DIR}/cache}"
LOG_DIR="${UGAPP_LOG_DIR:-${INSTALL_DIR}/log}"

# Jellyfin 各数据子目录(全部落在可写区)
JF_CONFIG="${DATA_DIR}/config"
JF_DATA="${DATA_DIR}/data"
JF_CACHE="${CACHE_DIR}"
JF_LOG="${LOG_DIR}"

mkdir -p "${JF_CONFIG}" "${JF_DATA}" "${JF_CACHE}" "${JF_LOG}" 2>/dev/null

# ---- 组件路径 ----
JF_DIR="${INSTALL_DIR}/jellyfin"          # 自包含运行时(含 jellyfin 启动器与全部 .so/.dll)
JF_WEB="${JF_DIR}/jellyfin-web"           # 前端静态资源
FFMPEG_BIN="${INSTALL_DIR}/ffmpeg/ffmpeg" # 自定义 jellyfin-ffmpeg(硬解关键)

# 自包含运行时的原生库在 jellyfin 目录内; ffmpeg 目录并入以防其自带库。
export LD_LIBRARY_PATH="${JF_DIR}:${INSTALL_DIR}/ffmpeg:${LD_LIBRARY_PATH:-}"
# 临时文件放可写缓存区(官方可选项; 避免写入只读安装目录)。
export TMPDIR="${JF_CACHE}"

# ---- 配置自愈 ----
# 早期版本曾预置一个字段残缺的 network.xml, 会与 Jellyfin 配置迁移冲突,
# 导致首次向导 /Startup/User 报错。清除它, 让 Jellyfin 自行生成完整配置。
# Jellyfin 默认监听 8096, 与 project.yaml 的 port 天然一致, 无需预置端口。
STALE_NET="${JF_CONFIG}/network.xml"
if [ -f "${STALE_NET}" ] && grep -q "RequireHttps" "${STALE_NET}" 2>/dev/null; then
    if ! grep -q "LocalNetworkSubnets\|PublishedServerUriBySubnet" "${STALE_NET}" 2>/dev/null; then
        rm -f "${STALE_NET}"
    fi
fi

# ---- 修复: 运行用户不在 /etc/passwd 导致首次向导失败 ----
# 绿联沙箱给应用分配的独立 uid 通常不在容器的 /etc/passwd 里。
# Jellyfin 首次创建默认用户时调用 .NET 的 Environment.UserName,
# 其底层是 getpwuid(geteuid()); 查不到用户会返回 ENOENT("No such file or directory"),
# 使 GET /Startup/User 报错、首次向导无法创建管理员账号。
# 解决: 用 nss_wrapper 提供一份包含当前 uid 的伪造 passwd/group, 经 LD_PRELOAD 生效。
# 采用优雅降级: 库缺失或用户已存在于 passwd 时自动跳过, 不影响其余功能。
NSSW_LIB="${INSTALL_DIR}/lib/libnss_wrapper.so"
CUR_UID="$(id -u 2>/dev/null || echo 1000)"
CUR_GID="$(id -g 2>/dev/null || echo 1000)"
if [ -f "${NSSW_LIB}" ] && ! getent passwd "${CUR_UID}" >/dev/null 2>&1; then
    NSSW_PASSWD="${JF_CACHE}/nss_passwd"
    NSSW_GROUP="${JF_CACHE}/nss_group"
    echo "jellyfin:x:${CUR_UID}:${CUR_GID}:Jellyfin:${DATA_DIR}:/bin/sh" > "${NSSW_PASSWD}"
    echo "jellyfin:x:${CUR_GID}:" > "${NSSW_GROUP}"
    export LD_PRELOAD="${NSSW_LIB}${LD_PRELOAD:+:$LD_PRELOAD}"
    export NSS_WRAPPER_PASSWD="${NSSW_PASSWD}"
    export NSS_WRAPPER_GROUP="${NSSW_GROUP}"
    # 同时补 USER/HOME 环境, 供依赖它们的组件兜底。
    export USER="jellyfin"
    export LOGNAME="jellyfin"
    export HOME="${DATA_DIR}"
fi

# ---- 启动 ----
# 关键: 工作目录设为数据目录(对齐官方 WorkingDirectory=/var/lib/jellyfin),
#       而非程序安装目录(只读)。首次向导会以工作目录为基准做文件操作。
# 不加 --service: 官方默认即关闭; 绿联靠 stdout 收集日志, 保留完整启动输出便于排查。
# 用 exec 让 jellyfin 成为主进程, 正确接收系统 SIGTERM。
cd "${JF_DATA}" || cd "${DATA_DIR}" || exit 1
exec "${JF_DIR}/jellyfin" \
    --datadir "${JF_DATA}" \
    --configdir "${JF_CONFIG}" \
    --cachedir "${JF_CACHE}" \
    --logdir "${JF_LOG}" \
    --webdir "${JF_WEB}" \
    --ffmpeg "${FFMPEG_BIN}"
