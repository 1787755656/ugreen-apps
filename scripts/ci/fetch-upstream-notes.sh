#!/bin/bash
set -euo pipefail

# Usage: fetch-upstream-notes.sh <owner/repo> <tag>
# Prints upstream GitHub release body to stdout (may be empty).
# Tries exact tag, then tag with/without leading v.

REPO="${1:-}"
TAG="${2:-}"

if [ -z "$REPO" ] || [ -z "$TAG" ]; then
  exit 0
fi

if ! command -v gh >/dev/null 2>&1; then
  exit 0
fi

try_tag() {
  local t="$1"
  gh api "repos/${REPO}/releases/tags/${t}" --jq '.body // empty' 2>/dev/null || true
}

BODY=$(try_tag "$TAG")
if [ -z "$BODY" ] && [[ "$TAG" == v* ]]; then
  BODY=$(try_tag "${TAG#v}")
fi
if [ -z "$BODY" ] && [[ "$TAG" != v* ]]; then
  BODY=$(try_tag "v${TAG}")
fi

# qbittorrent-style: release-X.Y.Z.W
if [ -z "$BODY" ] && [[ "$TAG" != release-* ]]; then
  BODY=$(try_tag "release-${TAG}")
fi

printf '%s' "$BODY"
