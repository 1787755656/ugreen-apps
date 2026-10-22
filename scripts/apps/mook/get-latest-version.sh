#!/bin/bash
# Mook 版本探测：上游 NikoYomi/mook 正常发 GitHub Release（tag 形如 v0.4.6）。
# 最新 release 的 tag 即上游版本；project.yaml 的 version 取去掉 v 前缀的 x.y.z。
set -euo pipefail

INPUT_VERSION="${1:-}"
UPSTREAM_REPO="NikoYomi/mook"
API="https://api.github.com/repos/${UPSTREAM_REPO}"

AUTH=()
[ -n "${GH_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer ${GH_TOKEN}")

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="v${VERSION}"
else
  UPSTREAM_TAG=$(curl -fsSL ${AUTH[@]+"${AUTH[@]}"} "$API/releases/latest" | jq -r '.tag_name')
  [ -n "$UPSTREAM_TAG" ] && [ "$UPSTREAM_TAG" != "null" ] || {
    echo "Failed to resolve latest release for mook" >&2; exit 1;
  }
  VERSION=$(echo "$UPSTREAM_TAG" | sed 's/^v//')
fi

PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
