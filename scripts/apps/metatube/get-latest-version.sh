#!/bin/bash
set -euo pipefail

# Binaries are cross-posted to metatube-server-releases via the upstream
# project's own CI; the SDK repo (metatube-community/metatube-sdk-go) holds
# the source/tags but the release artifacts we actually download live in
# metatube-community/metatube-server-releases. Track versions from the
# releases repo since that's what determines whether a new downloadable
# build actually exists.

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  TAG=$(curl -sL "https://api.github.com/repos/metatube-community/metatube-server-releases/releases/latest" | \
    jq -r '.tag_name')
  VERSION=$(echo "$TAG" | sed 's/^v//')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for metatube" >&2
  exit 1
fi

# project_version is what gets written into project.yaml's `version:` field
# (must be plain x.y.z). For metatube this is identical to VERSION.
PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
fi
