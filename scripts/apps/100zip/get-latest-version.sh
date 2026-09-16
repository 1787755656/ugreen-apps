#!/bin/bash
set -euo pipefail

# 上游 Alicace/100zip 直接在 GitHub Releases 发版，tag 形如 v0.5.20，
# 去掉 v 前缀就是 x.y.z，可直接作 project.yaml 的 version。
# （同 clouddl：走 scripts/ci/github-latest-release-tag.sh，带 GITHUB_TOKEN
#   防 runner 出口 IP 撞匿名限流。）

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="v${VERSION}"
else
  CI_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../ci" && pwd)"
  TAG=$(bash "$CI_DIR/github-latest-release-tag.sh" "Alicace/100zip")
  VERSION=$(echo "$TAG" | sed 's/^v//')
  UPSTREAM_TAG="$TAG"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for 100zip" >&2
  exit 1
fi

PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=${UPSTREAM_TAG:-v$VERSION}"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=${UPSTREAM_TAG:-v$VERSION}" >> "$GITHUB_OUTPUT"
fi
