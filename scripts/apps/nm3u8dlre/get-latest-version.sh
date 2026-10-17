#!/bin/bash
# 解析上游引擎（nilaoda/N_m3u8DL-RE）最新版本。
#
# ⚠ 上游 tag 带预发布后缀（v0.6.0-beta），而 ugcli 只接受 x.y.z 三段版本号，
#   所以 project_version 归一化成 0.6.0，完整 tag 放 upstream_tag。
#   同一上游版本的重发靠四段版本号最后一位（构建号）区分，不会互相顶掉。
set -euo pipefail

INPUT_VERSION="${1:-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="${INPUT_VERSION#v}"
else
  TAG=$(bash "$REPO_ROOT/scripts/ci/github-latest-release-tag.sh" "nilaoda/N_m3u8DL-RE")
  VERSION="${TAG#v}"
fi

[ -n "$VERSION" ] && [ "$VERSION" != "null" ] || {
  echo "Failed to resolve version for nm3u8dlre" >&2
  exit 1
}

# 0.6.0-beta → 0.6.0
PROJECT_VERSION=$(printf '%s' "$VERSION" | sed -E 's/^([0-9]+\.[0-9]+\.[0-9]+).*/\1/')
printf '%s\n' "$PROJECT_VERSION" | LC_ALL=C grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || {
  echo "无法把上游版本 '$VERSION' 归一化成 x.y.z" >&2
  exit 1
}

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=v$VERSION"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "version=$VERSION"
    echo "project_version=$PROJECT_VERSION"
    echo "upstream_tag=v$VERSION"
  } >> "$GITHUB_OUTPUT"
fi
