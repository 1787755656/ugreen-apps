#!/bin/bash
# 随机视频版本探测：纯自研应用，无上游 Release / tag 可跟踪。
# 版本唯一来源是本仓库 apps/randomvideo/com.personal.randomvideo/project.yaml 的
# version: 字段（ugcli 要求的 x.y.z），由人工在发版前 bump。
#
# 设计理由（和其它"无上游"app 一致，写清楚免得以后困惑）：
#   1. 没有可供钉的 tag，永远构建当前 project.yaml 里写的版本；
#   2. 上游改了内容但没 bump version，不会触发重建——版本去重只认版本字符串，
#      这是预期行为（自研 app 的发版节奏由自己掌控）。
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
  echo "Failed to resolve version for randomvideo (check $YAML)" >&2
  exit 1
fi

# 自研 app：project.yaml 的 version 即是发布版本，无独立的上游版本号。
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
