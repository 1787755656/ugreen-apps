#!/bin/bash
set -euo pipefail

# Upstream has NO GitHub releases and NO version tags — the only version
# source is version.json on the main branch. Consequence: builds always
# come from main HEAD, and a content change without a version.json bump
# will NOT trigger a rebuild (the git-tag dedup only sees the version
# string). That matches how this app was maintained manually.

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  VERSION=$(curl -sL "https://raw.githubusercontent.com/magiccode1412/magicpush/main/version.json" | \
    jq -r '.version')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for magicpush" >&2
  exit 1
fi

PROJECT_VERSION=$(echo "$VERSION" | cut -d. -f1-3)

# No usable tag upstream — build.sh always downloads the main branch.
UPSTREAM_TAG="main"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
