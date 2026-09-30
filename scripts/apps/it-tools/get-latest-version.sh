#!/bin/bash
set -euo pipefail

# 上游 CorentinTh/it-tools：tag 形如 v2024.10.22-7ca5933（日期-短哈希），
# release 由 CI bot 自动发。版本源 = latest release 的日期段（去 v、去哈希）。
# 日期式版本 2024.10.22 恰好满足 ugcli"中段最多两位"的隐藏限制（月份 ≤12）。
# upsteam_tag 输出保留完整 tag（release notes 拉取用）。

INPUT_VERSION="${1:-}"
UPSTREAM_REPO="CorentinTh/it-tools"
API="https://api.github.com/repos/${UPSTREAM_REPO}"

AUTH=()
[ -n "${GH_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer ${GH_TOKEN}")

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  # 完整 tag 反查：v<日期>-<哈希> 或恰好 v<日期>
  UPSTREAM_TAG=$(curl -fsSL ${AUTH[@]+"${AUTH[@]}"} "$API/releases?per_page=100" \
    | jq -r --arg v "v${VERSION}" '[.[].tag_name
        | select((startswith($v + "-") and test("-[0-9a-f]{6,}$")) or . == $v)]
        | first // empty')
  [ -n "$UPSTREAM_TAG" ] || UPSTREAM_TAG="v${VERSION}"
else
  UPSTREAM_TAG=$(curl -fsSL ${AUTH[@]+"${AUTH[@]}"} "$API/releases/latest" | jq -r '.tag_name')
  [ -n "$UPSTREAM_TAG" ] && [ "$UPSTREAM_TAG" != "null" ] || {
    echo "Failed to resolve latest release for it-tools" >&2; exit 1;
  }
  VERSION=$(echo "$UPSTREAM_TAG" | sed 's/^v//; s/-[0-9a-f]*$//')
fi

# 两段/一段日期兜底补 .0（照 piggybank 先例；上游目前固定三段）
case "$(echo "$VERSION" | awk -F. '{print NF}')" in
  2) VERSION="${VERSION}.0" ;;
  1) VERSION="${VERSION}.0.0" ;;
esac

PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
