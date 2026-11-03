#!/bin/bash
set -euo pipefail

# 1Panel 是 Docker 型应用：包里内置 moelin/1panel 镜像（1Panel 官方只发安装
# 脚本、不发自身 Docker 镜像，moelin 是社区事实标准），版本探测以【镜像】为准，
# 不查 1Panel-dev/1Panel 的 GitHub release——镜像重打可能滞后于官方发版
# （deepseek-harness 同款判据）。
#
# 判据（qbittorrent-ng6 / deepseek-harness 同款）：Docker Hub tags API 里
# `latest` 与它的版本号 tag 共享同一个 manifest digest，按 digest 反查版本
# ——比"按 last_updated 取最新"准，上游补发旧分支镜像不会选错。打包钉到
# 具体版本 tag，不用 latest。
#
# moelin 的 tag 族（2026-11 实测，digest 反查按下面的过滤走）：
#   v2.4.2            版本 tag（跟这个）
#   v2 / v1           浮动 major tag，被 x.y.z 格式过滤排除
#   latest            浮动 tag，只当反查锚点
#   global-v2.4.2     国际版变体，digest 与标准版不同，排除
#   v1.10.34-lts      v1 LTS 线，带 -lts 后缀过不了 x.y.z 格式过滤，排除
#
# 1Panel 稳定版是纯 x.y.z（无 bN/-rc.N 之类预发布后缀），所以本应用用不上
# base-version-only（scripts/ci/base-version-only.conf）那套——每个新 tag
# 都是基础版本变化，schedule 自然每次都会构建。
#
# VERSION = 2.4.2           （release tag 与镜像 tag 用，去 v）
# PROJECT_VERSION = 2.4.2   （1Panel 稳定版本来就是 x.y.z，无需归一化）
# UPSTREAM_TAG = v2.4.2     （1Panel-dev/1Panel 的 release tag，抓更新说明用）

INPUT_VERSION="${1:-}"
HUB_API="https://hub.docker.com/v2/repositories/moelin/1panel/tags"

if [ -n "$INPUT_VERSION" ]; then
  # 手动指定版本：接受 2.4.2 或 v2.4.2
  VERSION="${INPUT_VERSION#v}"
else
  echo "==> Querying Docker Hub tags API (version detection)"
  # latest 与每个发布批次同时推送，恒在第一页（tag 总数远小于 100）
  TAGS_JSON=$(curl -fsS "${HUB_API}?page_size=100")
  LATEST_DIGEST=$(printf '%s' "$TAGS_JSON" \
    | jq -r '.results[] | select(.name == "latest") | .digest' | head -1)
  if [ -z "$LATEST_DIGEST" ]; then
    echo "::error::Docker Hub 上找不到 moelin/1panel 的 latest tag" >&2
    exit 1
  fi
  # 共享 latest digest 的 tag 里取纯 vX.Y.Z：浮动 major（v2）、global-* 变体、
  # -lts 后缀的 LTS 线都被格式过滤掉；万一同 digest 多个版本 tag，sort -V 取最大兜底
  VERSION=$(printf '%s' "$TAGS_JSON" \
    | jq -r --arg d "$LATEST_DIGEST" \
      '.results[] | select(.digest == $d) | select(.name | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")) | .name' \
    | sed 's/^v//' | sort -V | tail -1)
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for 1panel" >&2
  exit 1
fi

if ! printf '%s' "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "::error::镜像版本号不符合预期格式: '${VERSION}'（期望纯 x.y.z，如 2.4.2）" >&2
  exit 1
fi

PROJECT_VERSION="$VERSION"
UPSTREAM_TAG="v${VERSION}"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
