#!/bin/bash
# LunaTV 版本探测：上游 fork（SzeMeng76/LunaTV）不打 tag/release，
# 版本唯一来源是 main 分支根目录的 VERSION.txt（与 package.json 同步）。
set -euo pipefail

INPUT_VERSION="${1:-}"
REPO="SzeMeng76/LunaTV"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  VERSION=$(curl -fsSL --retry 3 --connect-timeout 20 \
    "https://raw.githubusercontent.com/${REPO}/main/VERSION.txt" | tr -d '[:space:]')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for lunatv" >&2
  exit 1
fi

# project.yaml 的 version 字段（纯 x.y.z，与 VERSION.txt 一致）
PROJECT_VERSION="$VERSION"
# 上游无 tag：留空，release notes 会优雅降级为"到上游仓库查看"
UPSTREAM_TAG=""

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
echo "lunatv version=$VERSION project_version=$PROJECT_VERSION upstream_tag=(none)"
