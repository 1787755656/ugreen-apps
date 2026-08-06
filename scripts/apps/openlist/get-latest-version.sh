#!/bin/bash
set -euo pipefail

# OpenList（AList 的社区接续分支）上游在 GitHub：OpenListTeam/OpenList。
# Release tag 形如 v4.2.4 —— 去掉 v 前缀正好是 project.yaml 要的 x.y.z，
# 不需要任何映射（ugcli 对中段最多两位数的限制这里也满足：minor=2）。

INPUT_VERSION="${1:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="${INPUT_VERSION#v}"
else
  TAG=$(bash "$REPO_ROOT/scripts/ci/github-latest-release-tag.sh" "OpenListTeam/OpenList")
  VERSION="${TAG#v}"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for openlist" >&2
  exit 1
fi

# 上游是标准三段 x.y.z，project.yaml 直接用，无需截断
PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=v$VERSION"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=v$VERSION" >> "$GITHUB_OUTPUT"
fi
