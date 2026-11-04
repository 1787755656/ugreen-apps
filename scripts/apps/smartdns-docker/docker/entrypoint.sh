#!/bin/sh
# SmartDNS 容器入口脚本
# 数据全部落 /data（compose bind 到应用安装目录 data/）：
#   /data/conf/smartdns.conf  用户配置（首次启动生成默认值，此后不再覆盖）
#   /data/conf/runtime.conf   每次启动重新生成（路径 + 端口 + 引入用户配置）
# 容器内是 root，直接绑 53；管理界面固定 6153。
set -u

DATA_DIR=/data
CONF_DIR="${DATA_DIR}/conf"
USER_CONF="${CONF_DIR}/smartdns.conf"
RUNTIME_CONF="${CONF_DIR}/runtime.conf"

mkdir -p "${CONF_DIR}" "${DATA_DIR}/log" "${DATA_DIR}/cache" \
  "${DATA_DIR}/data" "${DATA_DIR}/run" 2>/dev/null

# ---- 首次运行：生成用户配置（此后不再覆盖） ----
if [ ! -f "${USER_CONF}" ]; then
  cat > "${USER_CONF}" <<'EOF'
# SmartDNS 容器配置 —— 可自由编辑，改完在应用中心重启应用生效。
# 完整指令说明见 /app/smartdns.conf.sample 或
# https://pymumu.github.io/smartdns/config/basic-config/
#
# 注意：
# 1. DNS 监听 53 端口（UDP+TCP），由 runtime.conf 注入；确实要自定义
#    监听时再在这里添加 bind 指令（添加后启动脚本不再注入默认监听）。
# 2. 不要在这里配置 plugin / smartdns-ui.* / data-dir / log-file /
#    cache-file —— 这些每次启动由 runtime.conf 按固定路径生成。
# 3. Web 管理界面默认账号 admin / 密码 password，请登录后尽快修改
#    （也可在此用 smartdns-ui.user / smartdns-ui.password 覆盖）。
# 4. 请勿修改管理界面端口(6153)，否则应用图标将打不开管理页。

server-name smartdns

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

# ---- 用户配置里是否已有 bind 指令（有则不注入默认监听） ----
# smartdns 的 bind 是叠加不是覆盖，不检测会双监听
TAB=$(printf '\t')
USER_HAS_BIND=0
if [ -f "${USER_CONF}" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    while :; do
      case "$line" in
        ' '* | "${TAB}"*) line=${line#?} ;;
        *) break ;;
      esac
    done
    case "$line" in
      '#'* | '') continue ;;
      bind[[:space:]]* | bind-tcp[[:space:]]* | bind-tls[[:space:]]* | bind-https[[:space:]]* | bind-quic[[:space:]]*)
        USER_HAS_BIND=1
        break
        ;;
    esac
  done < "${USER_CONF}"
fi

# ---- 每次启动重新生成 runtime.conf ----
{
  cat <<EOF
# 本文件由容器入口脚本每次启动自动生成，手工修改会丢失；
# 自定义配置请写在 smartdns.conf 里。
plugin /app/smartdns/smartdns_ui.so
smartdns-ui.www-root /app/wwwroot
smartdns-ui.ip http://0.0.0.0:6153
smartdns-ui.max-query-log-age 604800
data-dir /data/data
log-file /data/log/smartdns.log
log-size 512k
log-num 2
audit-enable no
cache-persist yes
cache-file /data/cache/smartdns.cache
EOF
  if [ "${USER_HAS_BIND}" = "0" ]; then
    echo "# DNS 监听（UDP + TCP），53 端口由 compose 映射到宿主"
    echo "bind [::]:53"
    echo "bind-tcp [::]:53"
  else
    echo "# 检测到用户配置自带 bind 指令，不注入默认监听"
  fi
  echo "conf-file ${USER_CONF}"
} > "${RUNTIME_CONF}"

# run-smartdns 会 cd 进 smartdns 目录再 exec 真实二进制（ELF interpreter
# 是相对路径 lib/ld-musl-*.so.1）。exec 后 smartdns 成为 PID 1，信号直达。
exec /app/smartdns/run-smartdns -f -x -c "${RUNTIME_CONF}"
