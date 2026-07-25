#!/bin/bash
set -euo pipefail

# Upstream's git tags are "release-X.Y.Z.W" (4-component version, e.g.
# release-5.2.1.10) — one component more than project.yaml's x.y.z scheme
# allows. VERSION keeps the full 4 parts (needed to correctly detect
# "5.2.1.9 -> 5.2.1.10" as a real update via the git-tag dedup in
# scripts/ci/resolve-release-tag.sh); PROJECT_VERSION truncates to the
# first 3 parts, which is the only thing actually written into
# project.yaml's version field.

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="release-${VERSION}"
else
  API_URL="https://api.github.com/repos/c0re100/qBittorrent-Enhanced-Edition/releases/latest"
  CURL_ARGS=(
    --fail
    --silent
    --show-error
    --location
    --retry 3
    --retry-all-errors
    --retry-delay 2
    --connect-timeout 10
    --max-time 30
    -H "Accept: application/vnd.github+json"
  )
  if [ -n "${GH_TOKEN:-}" ]; then
    CURL_ARGS+=(-H "Authorization: Bearer ${GH_TOKEN}")
  fi

  if ! API_RESPONSE=$(curl "${CURL_ARGS[@]}" "$API_URL"); then
    echo "Failed to query the qBittorrent latest-release API: $API_URL" >&2
    exit 1
  fi
  if ! UPSTREAM_TAG=$(jq -er '.tag_name // empty' <<<"$API_RESPONSE"); then
    echo "qBittorrent latest-release API returned no tag_name" >&2
    exit 1
  fi
  VERSION=$(echo "$UPSTREAM_TAG" | sed -E 's/^release-//')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for qbittorrent" >&2
  exit 1
fi

PROJECT_VERSION=$(echo "$VERSION" | cut -d. -f1-3)

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
