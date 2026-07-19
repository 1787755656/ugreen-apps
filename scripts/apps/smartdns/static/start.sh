#!/bin/sh
# SmartDNS 启动脚本 (UGOS Pro 原生应用) —— 带守护循环
# 由系统以应用独立用户身份运行。
#
# 配置分两层：
#   conf/smartdns.conf  用户配置，首次运行生成默认值，此后永不覆盖，随便改；
#   conf/runtime.conf   每次启动重新生成，只放依赖绝对路径的条目
#                       （UI 插件/wwwroot/日志/缓存/数据目录）。安装目录迁移
#                       (support_migration) 或数据目录变化后路径自动跟上。
#   runtime.conf 末尾 conf-file 引入用户配置，所以用户配置里的同名项
#   （如 smartdns-ui.user / smartdns-ui.password）会覆盖默认值。
#
# 注意：应用以普通用户运行，监听不了 1024 以下端口（53 不可用），
# 默认 DNS 端口为 8653 (UDP+TCP)。路由器侧转发示例（OpenWrt/dnsmasq）：
#   server=NAS的IP#8653
set -u

# ---- 目录准备（全部来自系统环境变量，带兜底） ----
INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"

CONF_DIR="${DATA_DIR}/conf"
LOG_DIR="${DATA_DIR}/log"
RUN_DIR="${DATA_DIR}/run"
SD_DATA_DIR="${DATA_DIR}/data"     # smartdns data-dir（查询日志数据库等）
CACHE_FILE_DIR="${DATA_DIR}/cache" # DNS 缓存持久化

# 应用沙箱里没有 /tmp——重定向到可写目录，避免依赖 tmpfile 的代码崩溃
TMP_DIR="${UGAPP_CACHE_DIR:-${DATA_DIR}/tmp}"
export TMPDIR="${TMP_DIR}"

mkdir -p "${CONF_DIR}" "${LOG_DIR}" "${RUN_DIR}" "${SD_DATA_DIR}" \
  "${CACHE_FILE_DIR}" "${TMP_DIR}" 2>/dev/null

SD_HOME="${INSTALL_DIR}/smartdns"
BIN="${SD_HOME}/smartdns"
USER_CONF="${CONF_DIR}/smartdns.conf"
RUNTIME_CONF="${CONF_DIR}/runtime.conf"

# ---- 首次运行：生成用户配置（此后不再覆盖） ----
if [ ! -f "${USER_CONF}" ]; then
  cat > "${USER_CONF}" <<'EOF'
# SmartDNS 用户配置 —— 可自由编辑，改完在应用中心重启应用生效。
# 全部指令说明见安装目录下的 smartdns.conf.sample 或
# https://pymumu.github.io/smartdns/config/basic-config/
#
# 注意：
# 1. 应用以普通用户运行，无法监听 1024 以下端口，所以 DNS 端口是 8653。
#    路由器/客户端把 DNS 指向 本机IP:8653（dnsmasq: server=IP#8653）。
# 2. 不要在这里配置 plugin / smartdns-ui.www-root / smartdns-ui.ip /
#    data-dir / log-file / cache-file —— 这些由启动脚本在 runtime.conf
#    里按实际安装路径生成，重复配置会冲突。
# 3. Web 管理界面默认账号 admin / 密码 password，请登录后尽快修改
#    （也可在此用 smartdns-ui.user / smartdns-ui.password 覆盖）。
# 4. 请勿修改管理界面端口(6080)，否则应用图标将打不开管理页。

server-name smartdns

# DNS 服务监听端口（UDP + TCP）
bind [::]:8653
bind-tcp [::]:8653

# 缓存与预取
cache-size 32768
prefetch-domain yes
serve-expired yes

# 测速与双栈优选
speed-check-mode ping,tcp:80,tcp:443
dualstack-ip-selection yes

# 上游 DNS（默认国内公共 DNS，可自行增删；
# 支持 server / server-tcp / server-tls / server-https / server-quic）
server 223.5.5.5
server 119.29.29.29
server-tls 223.5.5.5
server-tls 1.12.12.12
EOF
fi

# ---- 每次启动重新生成 runtime.conf（绝对路径都集中在这里） ----
cat > "${RUNTIME_CONF}" <<EOF
# 本文件由 bin/start.sh 每次启动自动生成，手工修改会丢失；
# 自定义配置请写在 smartdns.conf 里。
plugin ${SD_HOME}/smartdns_ui.so
smartdns-ui.www-root ${INSTALL_DIR}/wwwroot
smartdns-ui.ip http://0.0.0.0:6080
smartdns-ui.max-query-log-age 604800
data-dir ${SD_DATA_DIR}
log-file ${LOG_DIR}/smartdns.log
log-size 512k
log-num 2
audit-enable no
cache-persist yes
cache-file ${CACHE_FILE_DIR}/smartdns.cache
conf-file ${USER_CONF}
EOF

# ---- 崩溃循环保护（时间限制） ----
# 运行不足 FAST_EXIT_SECS 秒就退出算"快退"；连续 MAX_FAST_EXITS 次快退后
# 放弃重启、脚本退出 → 应用进入"已停止"。正常运行(≥30s)后退出总是自动拉起。
FAST_EXIT_SECS=30
MAX_FAST_EXITS=5

STOPPING=0
CHILD_PID=0

# 找存活的 smartdns 进程（含它自我重启 fork 出来的接班进程；按二进制完整
# 路径匹配，不会误匹配到本脚本或 run-smartdns 包装脚本）
sd_pid() {
  if command -v pgrep >/dev/null 2>&1; then
    pgrep -f "${BIN}" 2>/dev/null | head -1
  else
    pidof smartdns 2>/dev/null | cut -d' ' -f1
  fi
}

on_term() {
  STOPPING=1
  [ "${CHILD_PID}" != "0" ] && kill -TERM "${CHILD_PID}" 2>/dev/null
  P=$(sd_pid)
  [ -n "${P}" ] && kill -TERM "${P}" 2>/dev/null
}
trap on_term TERM INT

fast_exits=0
while [ "${STOPPING}" = "0" ]; do
  start_ts=$(date +%s)
  P=$(sd_pid)
  if [ -n "${P}" ]; then
    # 已有实例在跑（自我重启的接班进程）：只监视，不重复拉起
    while [ -n "$(sd_pid)" ] && [ "${STOPPING}" = "0" ]; do
      sleep 3
    done
  else
    # run-smartdns 会 cd 进 smartdns 目录再 exec 真实二进制——它的 ELF
    # interpreter 是相对路径(lib/ld-musl-*.so.1)，必须经由这个包装脚本启动。
    # -f 前台运行, -x 日志同时输出到 stdout（进 {appid}.log 便于排查）
    "${SD_HOME}/run-smartdns" -f -x -c "${RUNTIME_CONF}" -p "${RUN_DIR}/smartdns.pid" &
    CHILD_PID=$!
    wait "${CHILD_PID}" 2>/dev/null
    CHILD_PID=0
  fi
  [ "${STOPPING}" = "1" ] && break

  # 给自我重启的接班进程一点出生时间，再判断是否真的死透了
  sleep 2
  if [ -n "$(sd_pid)" ]; then
    fast_exits=0
    continue
  fi

  ran=$(( $(date +%s) - start_ts ))
  if [ "${ran}" -lt "${FAST_EXIT_SECS}" ]; then
    fast_exits=$((fast_exits + 1))
    if [ "${fast_exits}" -ge "${MAX_FAST_EXITS}" ]; then
      echo "smartdns 连续 ${fast_exits} 次在 ${FAST_EXIT_SECS}s 内退出，停止重试" >&2
      exit 1
    fi
    sleep 2
  else
    fast_exits=0
  fi
  echo "smartdns exited (ran ${ran}s), restarting..." >&2
done

# ---- 停止：等 smartdns 全部退出，超时补刀 ----
n=0
while [ -n "$(sd_pid)" ] && [ "${n}" -lt 10 ]; do
  sleep 1
  n=$((n + 1))
done
P=$(sd_pid)
[ -n "${P}" ] && kill -9 "${P}" 2>/dev/null
exit 0
