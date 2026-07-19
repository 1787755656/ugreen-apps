#!/bin/sh
# Lucky 启动脚本 (UGOS Pro 原生应用) —— 带守护循环
# 由系统以应用独立用户身份运行。
#
# 为什么需要守护循环：Lucky 在网页里点"重启"（或改设置触发重启）时，是
# "先 fork 一个新进程接班、旧进程再退出"（-ds 也拦不住，真机+本机实测）。
# UGOS 只盯着 start_cmd 这个进程，旧进程一退应用就被判定为"已停止"。
# 所以本脚本自己当监护人：子进程退出后若发现接班进程已在运行就转入监视；
# 接班进程死了才重新拉起；只有"短时间内连续快速崩溃"才放弃，让应用停用。
set -u

# ---- 目录准备（全部来自系统环境变量，带兜底） ----
INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"

# Lucky 全部配置(加密的 .lkcf 文件)集中在一个配置目录里，放进可写的 data 下，
# 卸载保留数据/迁移安装目录时配置都能跟着走。
CONF_DIR="${DATA_DIR}/conf"

# Lucky 用 os.TempDir() 建控制 socket (lucky.control.sock)——应用沙箱里没有
# /tmp，不重定向的话启动 3 秒后 panic 退出（真机实测）。指到可写目录即可。
# 注意 unix socket 路径上限 ~104 字节，这里的路径远短于上限。
TMP_DIR="${UGAPP_CACHE_DIR:-${DATA_DIR}/tmp}"
export TMPDIR="${TMP_DIR}"

mkdir -p "${CONF_DIR}" "${TMP_DIR}" 2>/dev/null

BIN="${INSTALL_DIR}/bin/lucky"

# ---- 崩溃循环保护（时间限制） ----
# 运行不足 FAST_EXIT_SECS 秒就退出算"快退"；连续 MAX_FAST_EXITS 次快退后
# 放弃重启、脚本退出 → 应用进入"已停止"。正常运行(≥30s)后退出总是自动拉起。
FAST_EXIT_SECS=30
MAX_FAST_EXITS=5

STOPPING=0
CHILD_PID=0

# 找存活的 lucky 进程（含它自我重启 fork 出来的接班进程；接班进程沿用同一
# 条命令行，所以按二进制完整路径匹配即可，也不会误匹配到本脚本）
lucky_pid() {
  if command -v pgrep >/dev/null 2>&1; then
    pgrep -f "${BIN}" 2>/dev/null | head -1
  else
    # 兜底：按进程名匹配（沙箱内只有本应用的进程，不会误伤）
    pidof lucky 2>/dev/null | cut -d' ' -f1
  fi
}

on_term() {
  STOPPING=1
  [ "${CHILD_PID}" != "0" ] && kill -TERM "${CHILD_PID}" 2>/dev/null
  P=$(lucky_pid)
  [ -n "${P}" ] && kill -TERM "${P}" 2>/dev/null
}
trap on_term TERM INT

fast_exits=0
while [ "${STOPPING}" = "0" ]; do
  start_ts=$(date +%s)
  P=$(lucky_pid)
  if [ -n "${P}" ]; then
    # 已有实例在跑（lucky 自我重启的接班进程）：只监视，不重复拉起
    while [ -n "$(lucky_pid)" ] && [ "${STOPPING}" = "0" ]; do
      sleep 3
    done
  else
    "${BIN}" -cd "${CONF_DIR}" &
    CHILD_PID=$!
    wait "${CHILD_PID}" 2>/dev/null
    CHILD_PID=0
  fi
  [ "${STOPPING}" = "1" ] && break

  # 给自我重启的接班进程一点出生时间，再判断是否真的死透了
  sleep 2
  if [ -n "$(lucky_pid)" ]; then
    fast_exits=0
    continue
  fi

  ran=$(( $(date +%s) - start_ts ))
  if [ "${ran}" -lt "${FAST_EXIT_SECS}" ]; then
    fast_exits=$((fast_exits + 1))
    if [ "${fast_exits}" -ge "${MAX_FAST_EXITS}" ]; then
      echo "lucky 连续 ${fast_exits} 次在 ${FAST_EXIT_SECS}s 内退出，停止重试" >&2
      exit 1
    fi
    sleep 2
  else
    fast_exits=0
  fi
  echo "lucky exited (ran ${ran}s), restarting..." >&2
done

# ---- 停止：等 lucky 全部退出，超时补刀 ----
n=0
while [ -n "$(lucky_pid)" ] && [ "${n}" -lt 10 ]; do
  sleep 1
  n=$((n + 1))
done
P=$(lucky_pid)
[ -n "${P}" ] && kill -9 "${P}" 2>/dev/null
exit 0
