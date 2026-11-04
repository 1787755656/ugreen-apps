#!/bin/sh
# AdGuard Home UGOS 定制入口：首次启动播种默认配置，之后原样放行官方二进制。
# 默认账号 admin / AdGuard@2026（写在应用介绍里，用户登录后应立即修改）；
# conf 里已有 AdGuardHome.yaml（升级重装/用户自选数据目录）时绝不覆盖。
set -u

CONF="/opt/adguardhome/conf/AdGuardHome.yaml"
SEED="/seed/AdGuardHome.yaml"

if [ ! -f "${CONF}" ] && [ -f "${SEED}" ]; then
  mkdir -p "$(dirname "${CONF}")" 2>/dev/null
  if cp "${SEED}" "${CONF}" 2>/dev/null; then
    echo "[ugos-seed] 已写入默认配置（默认账号 admin / AdGuard@2026，请登录后立即改密）: ${CONF}"
  else
    echo "[ugos-seed] 警告: 默认配置写入失败（${CONF} 不可写?），将进入初始化向导" >&2
  fi
fi

exec /opt/adguardhome/AdGuardHome "$@"
