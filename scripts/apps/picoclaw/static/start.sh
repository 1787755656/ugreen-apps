#!/bin/sh
# PicoClaw 启动脚本 (UGOS Pro 原生应用) —— 带崩溃保护的守护循环
# 由系统以应用独立用户身份运行。
#
# 进程模型：start.sh → picoclaw-launcher(WebUI 管理台, 端口 18800)
#           → picoclaw gateway(由 launcher 在 WebUI 里启停的子进程)。
# launcher 不会自我重启（与 lucky 不同），所以不需要"接班进程收养"逻辑；
# 但保留崩溃循环保护：意外崩溃自动拉起，连续快速崩溃才放弃。
set -u

# ---- 目录准备（全部来自系统环境变量，带兜底） ----
INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"

# PicoClaw 的全部数据（config.json / .security.yml / workspace / skills /
# 会话记忆）默认放 ~/.picoclaw；用官方环境变量 PICOCLAW_HOME 钉到应用
# data 目录下，卸载保留数据/迁移安装目录时都能跟着走。
export PICOCLAW_HOME="${DATA_DIR}/picoclaw"

# launcher 在自己旁边找 picoclaw 主程序，这里再用官方变量显式钉死，
# 避免任何查找歧义（launcher 用它拉起 gateway 子进程）。
export PICOCLAW_BINARY="${INSTALL_DIR}/bin/picoclaw"

# 沙箱里没有 /tmp（lucky 真机实测）：Agent 的 shell 工具、Go os.TempDir()
# 都跟着 TMPDIR 走，重定向到可写目录。
TMP_DIR="${UGAPP_CACHE_DIR:-${DATA_DIR}/tmp}"
export TMPDIR="${TMP_DIR}"

# Agent 执行的第三方命令可能依赖 HOME；沙箱若未提供则兜底到 data 目录。
export HOME="${HOME:-${DATA_DIR}}"

mkdir -p "${PICOCLAW_HOME}" "${TMP_DIR}" 2>/dev/null

LAUNCHER="${INSTALL_DIR}/bin/picoclaw-launcher"
# 端口必须与 project.yaml 的 port 一致（18800），改动要两处同步，
# 否则应用图标打不开管理页。
PORT=18800

# ---- 崩溃循环保护（时间限制） ----
FAST_EXIT_SECS=30
MAX_FAST_EXITS=5

STOPPING=0
CHILD_PID=0

# 找本应用的存活进程（launcher + 它拉起的 gateway；按安装路径前缀匹配，
# picoclaw 与 picoclaw-launcher 都会命中，不会误匹配本脚本）
pc_pids() {
  if command -v pgrep >/dev/null 2>&1; then
    pgrep -f "${INSTALL_DIR}/bin/picoclaw" 2>/dev/null
  else
    pidof picoclaw-launcher picoclaw 2>/dev/null | tr ' ' '\n'
  fi
}

on_term() {
  STOPPING=1
  [ "${CHILD_PID}" != "0" ] && kill -TERM "${CHILD_PID}" 2>/dev/null
  # launcher 退出时应自行回收 gateway；这里再兜底 TERM 一遍全家
  for P in $(pc_pids); do
    kill -TERM "${P}" 2>/dev/null
  done
}
trap on_term TERM INT

fast_exits=0
while [ "${STOPPING}" = "0" ]; do
  start_ts=$(date +%s)
  "${LAUNCHER}" -console -no-browser -public -port "${PORT}" -lang zh &
  CHILD_PID=$!
  wait "${CHILD_PID}" 2>/dev/null
  CHILD_PID=0
  [ "${STOPPING}" = "1" ] && break

  ran=$(( $(date +%s) - start_ts ))
  if [ "${ran}" -lt "${FAST_EXIT_SECS}" ]; then
    fast_exits=$((fast_exits + 1))
    if [ "${fast_exits}" -ge "${MAX_FAST_EXITS}" ]; then
      echo "picoclaw-launcher 连续 ${fast_exits} 次在 ${FAST_EXIT_SECS}s 内退出，停止重试" >&2
      exit 1
    fi
    sleep 2
  else
    fast_exits=0
  fi
  echo "picoclaw-launcher exited (ran ${ran}s), restarting..." >&2
done

# ---- 停止：等 launcher/gateway 全部退出，超时补刀 ----
n=0
while [ -n "$(pc_pids)" ] && [ "${n}" -lt 10 ]; do
  sleep 1
  n=$((n + 1))
done
for P in $(pc_pids); do
  kill -9 "${P}" 2>/dev/null
done
exit 0
