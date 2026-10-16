#!/bin/bash
# 证件照服务版本探测：Docker 型应用（纯 CPU，模型不内置、由安装时选择的
# 模型文件夹提供）。无上游可探测的版本号——版本唯一来源是本仓库
# apps/idphotos/com.chuan.idphotos/project.yaml 的 version 字段（人工 bump）。
#
# 镜像由本地构建（python:3.10-slim 基础 + 项目源码，双架构），upk 通过
# 本仓库 Release 发布（tag 形如 idphotos/vX.Y.Z-rN）；版本去重认 tag，
# 已发布的 tag 不会触发重建。
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
  echo "Failed to resolve version for idphotos (check $YAML)" >&2
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
