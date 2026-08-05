#!/bin/bash
set -euo pipefail

# Usage: github-latest-release-tag.sh <owner/repo>
# Prints the upstream repo's latest release tag_name to stdout.
#
# Why this exists: the per-app get-latest-version.sh scripts used to each do
#   curl -sL ".../releases/latest" | jq -r '.tag_name'
# which is unauthenticated and has no --fail. Actions runners share their
# egress IPs, so the anonymous 60 req/h limit gets hit regularly; the rate
# limit body has no .tag_name, jq prints the string "null", and the caller
# dies with a "Failed to resolve version" that looks like upstream vanished.
# GITHUB_TOKEN raises the limit to 1000 req/h, and --fail turns an HTTP error
# into a non-zero exit instead of a JSON error body flowing downstream.

REPO="${1:-}"

if [ -z "$REPO" ]; then
  echo "github-latest-release-tag.sh: missing <owner/repo> argument" >&2
  exit 1
fi

API_URL="https://api.github.com/repos/${REPO}/releases/latest"
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
  echo "Failed to query the latest-release API: $API_URL" >&2
  exit 1
fi

if ! jq -er '.tag_name // empty' <<<"$API_RESPONSE"; then
  echo "Latest-release API returned no tag_name: $API_URL" >&2
  exit 1
fi
