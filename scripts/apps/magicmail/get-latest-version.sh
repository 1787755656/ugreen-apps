#!/bin/bash
set -euo pipefail

# Magicmail 上游有正经的 GitHub Release 和 tag（v1.0.0 … v1.1.1），tag 名就是
# 版本号加 v 前缀，且和仓库根的 version.json 里的 latest 一致。
#
# 版本源选 Release 而不是 version.json，理由：version.json 是 main 分支上的一个
# 清单文件，作者可能先 bump 它再发版（也可能反过来），拿它会构建到一个还没打
# tag 的中间状态；而 releases/latest 拿到的一定是作者真正发布过的东西，
# build.sh 也就能钉着那个 tag 的源码 tarball 构建，可复现。

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="${INPUT_VERSION#v}"
else
  API_URL="https://api.github.com/repos/magiccode1412/magicmail/releases/latest"
  CURL_ARGS=(
    --fail --silent --show-error --location
    --retry 3 --retry-all-errors --retry-delay 2
    --connect-timeout 10 --max-time 30
    -H "Accept: application/vnd.github+json"
  )
  # CI 上带 token 免匿名限流；本地直接跑也能通（匿名有配额）
  if [ -n "${GH_TOKEN:-}" ]; then
    CURL_ARGS+=(-H "Authorization: Bearer ${GH_TOKEN}")
  fi

  if ! API_RESPONSE=$(curl "${CURL_ARGS[@]}" "$API_URL"); then
    echo "Failed to query the Magicmail latest-release API: $API_URL" >&2
    exit 1
  fi
  if ! TAG=$(jq -er '.tag_name // empty' <<<"$API_RESPONSE"); then
    echo "Magicmail latest-release API returned no tag_name" >&2
    exit 1
  fi
  VERSION="${TAG#v}"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for magicmail" >&2
  exit 1
fi

# 上游版本号就是标准 x.y.z，无需截断；仍然校验一遍，别让不合规的值
# 拖到 ugcli check 那步才炸（ugcli 有个没写进文档的校验：minor 最多两位数）。
PROJECT_VERSION=$(sed -E 's/-.*$//' <<<"$VERSION" | cut -d. -f1-3)
if ! [[ "$PROJECT_VERSION" =~ ^[0-9]+\.[0-9]{1,2}\.[0-9]+$ ]]; then
  echo "PROJECT_VERSION '$PROJECT_VERSION' 不满足 ugcli 的 x.y.z（minor 最多两位）要求" >&2
  exit 1
fi

# build.sh 按这个 tag 拉源码 tarball；Release 正文也从这个 tag 取。
UPSTREAM_TAG="v${VERSION}"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
