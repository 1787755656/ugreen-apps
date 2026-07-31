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
  local t="$1" out
  # 必须看退出码：tag 不存在时 gh api 退出 1，但那句
  #   {"message":"Not Found","documentation_url":...,"status":"404"}
  # 是打到【stdout】的。原先写成 `gh api ... || true` 把退出码吞了，于是这段 JSON
  # 会被当成上游更新说明原样写进 Release 正文。目前的几个应用 tag 都能解析到，
  # 所以还没真的发出去过，但上游一改 tag 规则就会中招。
  if out=$(gh api "repos/${REPO}/releases/tags/${t}" --jq '.body // empty' 2>/dev/null); then
    printf '%s' "$out"
  fi
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
