#!/bin/bash
set -euo pipefail

# Sub2API 上游以 GitHub Release 发版，tag 形如 v0.1.173，
# 资产 sub2api_0.1.173_linux_<arch>.tar.gz（版本号无 v 前缀）。
# 版本源直接用 GitHub 最新 release，不需要编译源码里的常量。

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  VERSION=$(curl -sS --fail https://api.github.com/repos/Wei-Shaw/sub2api/releases/latest | jq -r '.tag_name')
  VERSION="${VERSION#v}"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for sub2api" >&2
  exit 1
fi

# ---- 版本映射 ------------------------------------------------------------
# ugcli 对 version 有个没写进文档的校验：minor 与 patch 都【最多两位】。
# 上游 patch 会到三位（0.1.173 之类），直接写进 project.yaml 会撞校验。
#
# 映射规则（2026-10-31 定版，用户拍板）：major/minor 原样保留，仅 patch 到
# 三位时把首位（百位）拼进 minor 末尾，前导零归一：
#   0.2.19  → 0.2.19    （patch 两位内，原样）
#   0.2.100 → 0.21.0    （patch 首位 1 拼进 minor → 21）
#   0.2.173 → 0.21.73   （同上）
#
# 决策史（别删，防止再绕一圈）：
#   · 旧规则在本 2026-10 翻过车——上游 0.1.173~0.1.185 期间 patch 前移把
#     minor 抬到 11（0.1.176 → 0.11.76），上游随后进 0.2.x（patch 两位内不
#     映射），应用版本从 0.11.x 掉回 0.2.x，商店更新判定（upk_version 四段
#     逐段比较，见 ugos-store internal/core/version.go——不是只看构建号）
#     永远认为 0.11.76 更新，8 月初装机用户收不到任何更新（真机实锤）。
#   · 2026-10-30 曾改 major+1（0.2.19 → 1.2.19，发过 1.2.19.0013/0014）
#     保更新永续；但商店展示版本跟着变成 1.2.19，与上游对不上，用户
#     2026-10-31 拍板改回 0.2.x——代价是已装 1.2.19.x 的机器会显示
#     "已是最新"，需要【卸载重装】重置到 0.2.19.0015 才能恢复更新链，
#     且上游 0.2.x 冲上 100 后再进 0.3.0（0.3.0 < 0.21.99）时还会倒挂一次。
if ! [[ "$VERSION" =~ ^[0-9]+(\.[0-9]+){1,2}$ ]]; then
  echo "VERSION '$VERSION' 不是 x.y 或 x.y.z 形态" >&2
  exit 1
fi

IFS=. read -r MAJOR MINOR PATCH <<<"$VERSION"
PATCH="${PATCH:-0}"
if [ "${#PATCH}" -gt 2 ]; then
  LEAD="${PATCH%??}"         # patch 去除末两位后的首部（如 173 → 1）
  PATCH2="$((10#${PATCH: -2}))"  # 末两位转十进制，顺带剥前导零（100 → 0 而非 "00"）
  NEW_MINOR="${MINOR}${LEAD}"
  if [ "${#NEW_MINOR}" -gt 2 ]; then
    echo "PROJECT_VERSION 无法映射：${MINOR} 拼上 ${LEAD} 后 minor 超两位，请手动调版本" >&2
    exit 1
  fi
  PROJECT_VERSION="${MAJOR}.${NEW_MINOR}.${PATCH2}"
else
  PROJECT_VERSION="${MAJOR}.${MINOR}.${PATCH}"
fi

# 双重保险：最终值再验一遍 x.y.z 且 y/z 都不超过两位。
if ! [[ "$PROJECT_VERSION" =~ ^[0-9]+\.[0-9]{1,2}\.[0-9]{1,2}$ ]]; then
  echo "PROJECT_VERSION '$PROJECT_VERSION' 不满足 ugcli 的 x.y.z（minor/patch 最多两位）要求" >&2
  exit 1
fi

# VERSION 保留完整精度（用于 git tag 去重 + release 资产名），
# PROJECT_VERSION 为映射后的 project.yaml 版本，UPSTREAM_TAG 指到上游 release。
echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=v${VERSION}"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=v${VERSION}" >> "$GITHUB_OUTPUT"
fi