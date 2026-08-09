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
# 映射规则（用户 2026-08-09 拍板）：把 patch 的超出部分【往前挪一位】，
# 即把 patch 的首位（百位）拼接到 minor 末尾，patch 只保留末两位：
#   0.1.9   → 0.1.9     （patch 一位，不需挪）
#   0.1.40  → 0.1.40    （patch 两位，不需挪）
#   0.1.173 → 0.11.73   （patch 首位 1 拼进 minor → 11）
#   0.2.173 → 0.21.73   （同上）
# 这样 minor / patch 都保持最多两位，且同一 upstream 版本永远映射到同一
# 个 project version（确定性，git tag 去重才不会误判）。
if ! [[ "$VERSION" =~ ^[0-9]+(\.[0-9]+){1,2}$ ]]; then
  echo "VERSION '$VERSION' 不是 x.y 或 x.y.z 形态" >&2
  exit 1
fi

IFS=. read -r MAJOR MINOR PATCH <<<"$VERSION"
PATCH="${PATCH:-0}"
if [ "${#PATCH}" -gt 2 ]; then
  LEAD="${PATCH%??}"         # patch 去除末两位后的首部（如 173 → 1）
  PATCH2="${PATCH: -2}"      # 末两位（如 173 → 73）
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