#!/bin/bash
set -euo pipefail

# Upstream: wushuo894/ani-rss — tags like v3.1.78, asset ani-rss.jar

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="v${VERSION}"
else
  CI_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../ci" && pwd)"
  UPSTREAM_TAG=$(bash "$CI_DIR/github-latest-release-tag.sh" "wushuo894/ani-rss")
  VERSION=$(echo "$UPSTREAM_TAG" | sed -E 's/^v//')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for ani-rss" >&2
  exit 1
fi

# ugcli wants x.y.z — upstream is already 3-component (3.1.78)
PROJECT_VERSION=$(echo "$VERSION" | cut -d. -f1-3)

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
