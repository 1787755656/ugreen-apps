#!/bin/bash
# 主题美化中心版本探测：Docker 型应用，纯自研（无上游可探测的版本号）。
# 版本唯一来源是本仓库 apps/ugtheme/com.personal.ugtheme/project.yaml 的
# version 字段（人工 bump）。镜像由本地构建（alpine + python3，
# 双架构），CI 只负责下载镜像 tar 铺 rootfs 后 ugcli pack —— 与 idphotos 同套路。
#
# 去重认 tag：本应用的 Release tag 形如 ughtheme/vX.Y.Z[-rN]，
# 同一版本重复触发只会出 -rN 修订（resolve-release-tag.sh 负责）。
set -euo pipefail

INPUT_VERSION="${1:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"
YAML="$REPO_ROOT/$PROJECT_DIR/project.yaml"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  VERSION=$(grep -E '^version:' "$YAML" | head -1 | sed -E 's/^version:[[:space:]]*//' | tr -d '[:space:]')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for ughtheme (check $YAML)" >&2
  exit 1
fi

PROJECT_VERSION="$VERSION"
UPSTREAM_TAG=""

echo "version=$VERSION"
echo "project_version=$PROJECT_VERSION"
echo "upstream_tag=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
