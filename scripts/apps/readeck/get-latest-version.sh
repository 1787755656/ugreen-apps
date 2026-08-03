#!/bin/bash
set -euo pipefail

# Readeck 上游在 Codeberg（https://codeberg.org/readeck/readeck），GitHub
# 上的 readeck/readeck 只是镜像且【没有 Releases】（tags 也只同步到 0.3.x），
# 所以不能用 GitHub API 探测版本。这里走 Codeberg Gitea API 的 releases/latest：
#   https://codeberg.org/api/v1/repos/readeck/readeck/releases/latest
# 返回的 tag_name 是纯数字版本（无 v 前缀，如 "0.22.3"），正好就是 asset 名
# 中间那段（readeck-0.22.3-linux-amd64），也直接是 project.yaml 的 x.y.z。

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  API_URL="https://codeberg.org/api/v1/repos/readeck/readeck/releases/latest"
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
    -H "Accept: application/json"
  )

  if ! API_RESPONSE=$(curl "${CURL_ARGS[@]}" "$API_URL"); then
    echo "Failed to query the Readeck latest-release API: $API_URL" >&2
    exit 1
  fi
  if ! VERSION=$(jq -er '.tag_name // empty' <<<"$API_RESPONSE"); then
    echo "Readeck latest-release API returned no tag_name" >&2
    exit 1
  fi
  # Codeberg tag 可能带 v 前缀（历史上有），去掉只留数字
  VERSION=$(echo "$VERSION" | sed -E 's/^v//')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for readeck" >&2
  exit 1
fi

# Readeck 的版本号恰好是标准 x.y.z（0.22.3），无需截断
PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$VERSION"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$VERSION" >> "$GITHUB_OUTPUT"
fi
