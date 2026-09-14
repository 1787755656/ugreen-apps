#!/bin/bash
set -euo pipefail

# 上游 xiaocheng154/nas-cloud-downloader 直接在 GitHub Releases 发版，
# tag 形如 v1.5.9，去掉 v 前缀就是 x.y.z，可直接作 project.yaml 的 version。

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="v${VERSION}"
else
  CI_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../ci" && pwd)"
  TAG=$(bash "$CI_DIR/github-latest-release-tag.sh" "xiaocheng154/nas-cloud-downloader")
  VERSION=$(echo "$TAG" | sed 's/^v//')
  UPSTREAM_TAG="$TAG"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for clouddl" >&2
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
