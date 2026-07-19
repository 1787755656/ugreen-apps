#!/bin/sh
# AdGuard Home 启动脚本 (UGOS Pro 原生应用) —— 带守护循环
# 由系统以应用独立用户身份运行(非 root, 无法监听 1024 以下端口)。
#
# 说明:
# - 管理界面用 --web-addr 钉死在 3000 端口(与 project.yaml 的 port 一致),
#   即使用户在初始化向导/后台里改了 web 端口也不受影响,应用图标永远打得开。
# - 配置文件与数据(查询日志/统计/过滤规则)全部放进 UGAPP_DATA_DIR,
#   卸载保留数据/迁移安装目录时都能跟着走。
# - --no-check-update 禁用自升级(安装目录由 UGOS 管理,不能让它自己替换二进制;
#   升级走应用中心的新版 upk)。
# - 应用沙箱里没有 /tmp(真机实测),重定向 TMPDIR 到可写目录。
# - 守护循环: 意外退出自动拉起;若发现已有接班进程(自我重启)则只监视;
#   连续快速崩溃才放弃,让应用进入"已停止"。
set -u

# ---- 目录准备(全部来自系统环境变量,带兜底) ----
INSTALL_DIR="${UGAPP_INSTALL_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
DATA_DIR="${UGAPP_DATA_DIR:-${INSTALL_DIR}/data}"

CONF_FILE="${DATA_DIR}/AdGuardHome.yaml"
WORK_DIR="${DATA_DIR}/work"

TMP_DIR="${UGAPP_CACHE_DIR:-${DATA_DIR}/tmp}"
export TMPDIR="${TMP_DIR}"

mkdir -p "${WORK_DIR}" "${TMP_DIR}" 2>/dev/null

BIN="${INSTALL_DIR}/bin/AdGuardHome"
WEB_ADDR="0.0.0.0:3000"

# ---- 崩溃循环保护(时间限制) ----
FAST_EXIT_SECS=30
MAX_FAST_EXITS=5

STOPPING=0
CHILD_PID=0

agh_pid() {
  if command -v pgrep >/dev/null 2>&1; then
    pgrep -f "${BIN}" 2>/dev/null | head -1
  else
    pidof AdGuardHome 2>/dev/null | cut -d' ' -f1
  fi
}

on_term() {
  STOPPING=1
  [ "${CHILD_PID}" != "0" ] && kill -TERM "${CHILD_PID}" 2>/dev/null
  P=$(agh_pid)
  [ -n "${P}" ] && kill -TERM "${P}" 2>/dev/null
}
trap on_term TERM INT

fast_exits=0
while [ "${STOPPING}" = "0" ]; do
  start_ts=$(date +%s)
  P=$(agh_pid)
  if [ -n "${P}" ]; then
    while [ -n "$(agh_pid)" ] && [ "${STOPPING}" = "0" ]; do
      sleep 3
    done
  else
    "${BIN}" -c "${CONF_FILE}" -w "${WORK_DIR}" \
      --web-addr "${WEB_ADDR}" --no-check-update &
    CHILD_PID=$!
    wait "${CHILD_PID}" 2>/dev/null
    CHILD_PID=0
  fi
  [ "${STOPPING}" = "1" ] && break

  sleep 2
  if [ -n "$(agh_pid)" ]; then
    fast_exits=0
    continue
  fi

  ran=$(( $(date +%s) - start_ts ))
  if [ "${ran}" -lt "${FAST_EXIT_SECS}" ]; then
    fast_exits=$((fast_exits + 1))
    if [ "${fast_exits}" -ge "${MAX_FAST_EXITS}" ]; then
      echo "AdGuardHome 连续 ${fast_exits} 次在 ${FAST_EXIT_SECS}s 内退出，停止重试" >&2
      exit 1
    fi
    sleep 2
  else
    fast_exits=0
  fi
  echo "AdGuardHome exited (ran ${ran}s), restarting..." >&2
done

# ---- 停止: 等进程全部退出,超时补刀 ----
n=0
while [ -n "$(agh_pid)" ] && [ "${n}" -lt 10 ]; do
  sleep 1
  n=$((n + 1))
done
P=$(agh_pid)
[ -n "${P}" ] && kill -9 "${P}" 2>/dev/null
exit 0
