#!/bin/bash
set -euo pipefail

# 上游 ASH-C776/piggy-bank：tag 两段三段混用（v1.2、v1.3.3）。版本源 = 最高的 v* tag，
# 归一化成 x.y.z（两段补 .0，同 smartdns 先例）；上游没 tag 时兜底读 main 分支
# package.json 的 .version。
# 注意：上游自 v1.3 起打 tag 不再 bump package.json（v1.3.3 的 package.json 仍是
# 1.0.0），发布身份以上游 tag 为准；build.sh 对两者不一致只警告不拦截（2026-09-25）。

INPUT_VERSION="${1:-}"
UPSTREAM_REPO="https://github.com/ASH-C776/piggy-bank.git"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="v${VERSION}"
else
  RAW=$(git ls-remote --tags "$UPSTREAM_REPO" 'refs/tags/v*' \
    | awk '{print $2}' | sed 's|^refs/tags/||; s|\^{}$||' | sed 's|^v||' | sort -u)
  if [ -n "$RAW" ]; then
    if sort -V </dev/null >/dev/null 2>&1; then
      VERSION=$(printf '%s\n' "$RAW" | sort -V | tail -1)
    else
      VERSION=$(printf '%s\n' "$RAW" | tail -1)
    fi
    UPSTREAM_TAG="v${VERSION}"
  else
    VERSION=$(curl -fsSL "https://raw.githubusercontent.com/ASH-C776/piggy-bank/main/package.json" | jq -r '.version')
    UPSTREAM_TAG="main"
  fi
fi

[ -n "$VERSION" ] && [ "$VERSION" != "null" ] || {
  echo "Failed to resolve version for piggybank" >&2; exit 1;
}

case "$(echo "$VERSION" | awk -F. '{print NF}')" in
  2) VERSION="${VERSION}.0" ;;
  1) VERSION="${VERSION}.0.0" ;;
esac

PROJECT_VERSION=$(echo "$VERSION" | cut -d. -f1-3)

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
