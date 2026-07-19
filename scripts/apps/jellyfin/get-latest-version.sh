#!/bin/bash
set -euo pipefail

# Resolves the Jellyfin SERVER version only. jellyfin-ffmpeg is tracked
# independently in build.sh via repo.jellyfin.org's "latest-7.x" pointer
# directory (not GitHub releases) — it has its own release cadence and
# isn't meaningfully "a version of the app", so it isn't part of the
# version string used for the release tag / project.yaml.

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  VERSION=$(curl -sL "https://api.github.com/repos/jellyfin/jellyfin/releases/latest" | \
    jq -r '.tag_name' | sed 's/^v//')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for jellyfin" >&2
  exit 1
fi

# project_version is what gets written into project.yaml's `version:` field
# (must be plain x.y.z). For jellyfin this is identical to VERSION — see
# scripts/apps/qbittorrent/get-latest-version.sh for a case where it isn't.
PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
fi
