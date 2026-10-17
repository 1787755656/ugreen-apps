#!/bin/bash
set -euo pipefail

# 上游 TencentCloud/Octop 的 tag 形如 v1.0.2b9（预发布带 bN 后缀），
# 而 project.yaml 的 version 只接受 x.y.z —— 所以 VERSION 保留完整 tag 名
# （用于 release tag / 上游更新说明定位），PROJECT_VERSION 归一化成 x.y.z。
#
# 注意：bN 不进 project.yaml，等于「同一 x.y.z 下的多个 beta」在 upk 文件名里
# 看不出区别，靠 4 段 build 号区分（resolve-release-tag.sh 递增），商店比对的
# 也是 upk_version(x.y.z.NNNN)，不影响更新判定。

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  CI_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../ci" && pwd)"
  TAG=$(bash "$CI_DIR/github-latest-release-tag.sh" "TencentCloud/Octop")
  VERSION="${TAG#v}"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for octop" >&2
  exit 1
fi

# 1.0.2b9 -> 1.0.2；万一上游改成不带后缀的 1.0.3，原样通过
PROJECT_VERSION=$(echo "$VERSION" | sed -E 's/^([0-9]+\.[0-9]+\.[0-9]+).*/\1/')
if ! echo "$PROJECT_VERSION" | LC_ALL=C grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "Version '${VERSION}' does not normalize to x.y.z (got '${PROJECT_VERSION}')" >&2
  exit 1
fi

UPSTREAM_TAG="v${VERSION}"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
