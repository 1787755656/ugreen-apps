#!/bin/bash
set -euo pipefail

# 上游 chongya369/music-downloader 的 tag 是【不带 v 前缀】的纯版本号
# （0.7.8），仓库根的 version.txt 与 tag 一致，可直接作 project.yaml 的 version。
# 版本号中段是单/两位数，ugcli 对 version 的隐藏校验（中段最多两位）不受影响。

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="$VERSION"
else
  CI_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../ci" && pwd)"
  TAG=$(bash "$CI_DIR/github-latest-release-tag.sh" "chongya369/music-downloader")
  VERSION=$(echo "$TAG" | sed 's/^v//')
  UPSTREAM_TAG="$VERSION"
fi

if [ -z "${VERSION:-}" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for deenmusic" >&2
  exit 1
fi

PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=${UPSTREAM_TAG:-$VERSION}"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=${UPSTREAM_TAG:-$VERSION}" >> "$GITHUB_OUTPUT"
fi
