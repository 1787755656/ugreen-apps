#!/bin/bash
set -euo pipefail

# Cloudreve 的上游 release tag 形如 4.19.1（**没有 v 前缀**），
# 正好是 project.yaml 需要的 x.y.z，不需要任何映射。
#
# 注意 GitHub API 的 /releases/latest 会跳过 prerelease —— Cloudreve 偶尔会发
# -beta 之类的预发布版本，那种不会被选到，这是想要的（跟稳定版）。
# 真要打预发布版时用 workflow_dispatch 的 version 参数显式指定。

INPUT_VERSION="${1:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="${INPUT_VERSION#v}"
else
  TAG=$(bash "$REPO_ROOT/scripts/ci/github-latest-release-tag.sh" "cloudreve/cloudreve")
  VERSION="${TAG#v}"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for cloudreve" >&2
  exit 1
fi

# VERSION 保留全部精度（含可能的 -beta 后缀），用于 git tag 去重；
# PROJECT_VERSION 截成 ugcli 要求的 x.y.z 写进 project.yaml。
PROJECT_VERSION=$(sed -E 's/-.*$//' <<<"$VERSION" | cut -d. -f1-3)

if ! [[ "$PROJECT_VERSION" =~ ^[0-9]+\.[0-9]{1,2}\.[0-9]+$ ]]; then
  # ugcli 对 version 有个没写进文档的校验：中段(minor)最多两位数。
  echo "PROJECT_VERSION '$PROJECT_VERSION' 不满足 ugcli 的 x.y.z（minor 最多两位）要求" >&2
  exit 1
fi

# 上游 tag 就是版本号本身（无 v 前缀）
UPSTREAM_TAG="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
