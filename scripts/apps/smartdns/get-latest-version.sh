#!/bin/bash
set -euo pipefail

# Upstream tags are "ReleaseX.Y" (e.g. Release48.2) — usually only 2
# components, while project.yaml requires x.y.z, so PROJECT_VERSION pads
# with ".0" (and truncates should upstream ever grow a 4th component).

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="Release${VERSION}"
else
  UPSTREAM_TAG=$(curl -sL "https://api.github.com/repos/pymumu/smartdns/releases/latest" | \
    jq -r '.tag_name')
  VERSION=$(echo "$UPSTREAM_TAG" | sed -E 's/^Release//')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for smartdns" >&2
  exit 1
fi

PROJECT_VERSION=$(echo "$VERSION" | cut -d. -f1-3)
while [ "$(echo "$PROJECT_VERSION" | awk -F. '{print NF}')" -lt 3 ]; do
  PROJECT_VERSION="${PROJECT_VERSION}.0"
done

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
