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
# 映射规则（2026-10-30 修订）：应用 major = 上游 major + 1，minor/patch 原样
# 保留；patch 到三位时仍把首位（百位）拼进 minor 末尾：
#   0.2.19  → 1.2.19    （major+1，其余原样）
#   0.3.0   → 1.3.0     （跨上游 minor 仍单调）
#   0.2.173 → 1.21.73   （patch 首位 1 拼进 minor → 21）
#
# 为什么 major+1：旧规则（major 原样，patch 前移）在 2026-10 翻车——上游
# 0.1.173~0.1.185 期间 patch 前移把 minor 抬到 11（0.1.176 → 0.11.76），
# 上游随后进 0.2.x（patch 两位内不映射），应用版本从 0.11.x 掉回 0.2.x，
# 数值倒挂后商店更新判定（upk_version 逐段比较）永远认为 0.11.76 更新，
# 8 月初装机的那批用户从此收不到任何更新（真机 0.11.76.0002 实锤）。
# major+1 让 1.2.x 同时大于受灾的 0.11.x 和现役的 0.2.x，两批用户都能收到更新。
# 已知局限：上游在同一 minor 里 patch 冲上 100 且之后才 bump minor 时
# （如 0.2.173 → 1.21.73，再来 0.3.0 → 1.3.0）会再次倒挂，届时再按同样
# 思路抬 major；upstream 版本空间大于 x.y.z 可编码空间，完美映射不存在。
if ! [[ "$VERSION" =~ ^[0-9]+(\.[0-9]+){1,2}$ ]]; then
  echo "VERSION '$VERSION' 不是 x.y 或 x.y.z 形态" >&2
  exit 1
fi

IFS=. read -r MAJOR MINOR PATCH <<<"$VERSION"
PATCH="${PATCH:-0}"
APP_MAJOR=$((MAJOR + 1))
if [ "${#PATCH}" -gt 2 ]; then
  LEAD="${PATCH%??}"         # patch 去除末两位后的首部（如 173 → 1）
  PATCH2="$((10#${PATCH: -2}))"  # 末两位转十进制，顺带剥前导零（100 → 0 而非 "00"）
  NEW_MINOR="${MINOR}${LEAD}"
  if [ "${#NEW_MINOR}" -gt 2 ]; then
    echo "PROJECT_VERSION 无法映射：${MINOR} 拼上 ${LEAD} 后 minor 超两位，请手动调版本" >&2
    exit 1
  fi
  PROJECT_VERSION="${APP_MAJOR}.${NEW_MINOR}.${PATCH2}"
else
  PROJECT_VERSION="${APP_MAJOR}.${MINOR}.${PATCH}"
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