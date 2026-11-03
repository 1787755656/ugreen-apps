#!/bin/bash
set -euo pipefail

# MyIP 上游 release tag 形如 v8.0.0（带 v 前缀），去掉前缀即 x.y.z。
# /releases/latest 会跳过 prerelease —— 真要打预发布版用 workflow_dispatch
# 的 version 参数显式指定。
#
# 上游不发任何 release 资产（dist 也自己构建），版本即内容源：build.sh
# 从同一 tag 拉源码现场构建前端 + 后端，不存在本地版本映射。

INPUT_VERSION="${1:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="${INPUT_VERSION#v}"
else
  TAG=$(bash "$REPO_ROOT/scripts/ci/github-latest-release-tag.sh" "jason5ng32/MyIP")
  VERSION="${TAG#v}"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for myip" >&2
  exit 1
fi

PROJECT_VERSION=$(sed -E 's/-.*$//' <<<"$VERSION" | cut -d. -f1-3)

if ! [[ "$PROJECT_VERSION" =~ ^[0-9]+\.[0-9]{1,2}\.[0-9]+$ ]]; then
  # ugcli 对 version 有个没写进文档的校验：中段(minor)最多两位数。
  echo "PROJECT_VERSION '$PROJECT_VERSION' 不满足 ugcli 的 x.y.z（minor 最多两位）要求" >&2
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
