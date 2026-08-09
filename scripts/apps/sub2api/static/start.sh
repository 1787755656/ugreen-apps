#!/bin/sh
# ============================================================
# sub2api — UGOS Pro 原生应用启动脚本
#
# sub2api 自身从这些地方读取配置：
#   - config.yaml 查找路径：CONFIG_FILE env > DATA_DIR env > /app/data > . > ./config > /etc/sub2api
#   - DATA_DIR 同时决定 config.yaml / .installed 锁 / logs 落盘位置
#   - SERVER_HOST / SERVER_PORT 可覆盖 server.host / server.port（viper AutomaticEnv）
#
# 数据落点（照 redis-ugreen-app 的 launcher 语义）：
#   - 用户安装/设置时选了共享文件夹（SUB2API_DATA_DIR）→ 数据放 <目录>/sub2api-data/
#   - 没选 → 放 UGAPP_DATA_DIR
#   首启参数晚 2~3 秒才注入（平台先起服务）；一旦参数就绪且目标目录为空、
#   默认目录已产生数据，就把数据复制过去。之后数据稳定在用户目录。
#
# supervisor 循环：setup wizard 完成后 sub2api 会 os.Exit(0) 请求重启
# （上游假设 systemd Restart=always，UGOS 不保证），退出即自动拉起。
# ============================================================

set -u

APP_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

DEFAULT_DIR="${UGAPP_DATA_DIR:-${APP_ROOT}/data}"
CACHE_DIR="${UGAPP_CACHE_DIR:-${DEFAULT_DIR}/tmp}"
SERVER_PORT="${SUB2API_PORT:-25435}"
SERVER_HOST="${SUB2API_HOST:-0.0.0.0}"

# ---- 数据落点 --------------------------------------------------------
log() { echo "[start.sh] $*" 2>&1; }

# 参数值可能形式（真机）：
#   裸标量 /volume1/共享   或字面量 null   或 JSON ['...'] / ["..."]
# 依次剥掉中括号与首尾引号；不删路径内部的空格。
get_param() {
  if [ -z "${SUB2API_DATA_DIR+x}" ] || [ -z "$SUB2API_DATA_DIR" ]; then
    printf ''; return
  fi
  v="$SUB2API_DATA_DIR"
  v="${v#"${v%%[![:space:]]*}"}"   # 去首部空白
  v="${v%"${v##*[![:space:]]}"}"   # 去尾部空白
  v="${v#[}"                        # 去左中括号
  v="${v%]}"                        # 去右中括号
  v="${v#\"}"; v="${v%\"}"          # 去双引号
  v="${v#\'}"; v="${v%\'}"          # 去单引号
  if [ -z "$v" ] || [ "$v" = "null" ] || [ "$v" = "[]" ]; then
    printf ''
  else
    printf '%s' "$v"
  fi
}

param="$(get_param)"
log "SUB2API_DATA_DIR env passed as: [${SUB2API_DATA_DIR-unset}]"
if [ -n "$param" ]; then
  DATA_DIR="${param}/sub2api-data"
else
  DATA_DIR="${DEFAULT_DIR}"
fi
log "DATA_DIR=${DATA_DIR}  DEFAULT_DIR=${DEFAULT_DIR}"

# 首启迁移：参数刚就绪（目标目录没有数据，而默认目录已有 .installed）。
needs_migrate=false
if [ -n "$param" ] && [ "$DATA_DIR" != "$DEFAULT_DIR" ] \
   && [ -f "$DEFAULT_DIR/.installed" ]; then
  if [ ! -d "$DATA_DIR" ] || [ -z "$(ls -A "$DATA_DIR" 2>/dev/null)" ]; then
    needs_migrate=true
  fi
fi
if [ "$needs_migrate" = "true" ]; then
  log "migrating first-run data from ${DEFAULT_DIR} to ${DATA_DIR}"
  mkdir -p "$DATA_DIR"
  cp -a "$DEFAULT_DIR/." "$DATA_DIR/" 2>/dev/null || true
fi

mkdir -p "$DATA_DIR" "$CACHE_DIR"

# 沙箱没有 /usr/share/zoneinfo；静态二进制未内嵌 tzdata
export ZONEINFO="${APP_ROOT}/zoneinfo.zip"

export DATA_DIR
export TMPDIR="${CACHE_DIR}"
export SERVER_HOST
export SERVER_PORT

log "starting sub2api on ${SERVER_HOST}:${SERVER_PORT}"
cd "${APP_ROOT}"

while true; do
  bin/sub2api
  code=$?
  log "sub2api exited with code ${code}, restarting in 2s..."
  sleep 2
done