#!/bin/bash
set -euo pipefail

# Upstream tags are "vX.Y.Z" (e.g. v0.107.78). ugcli 实测限制:版本号中段
# (minor) 最多两位数,首末段可以三位以上 —— 所以 0.107.78 不能直接用。
# 确定性映射:去掉前导 "0." 再补 ".0",即 0.107.78 → PROJECT_VERSION=107.78.0。
# 该映射随上游单调递增(107.79.0 < 108.0.0);仅当上游 patch 超过 99 时中段
# 会再次超限,届时脚本会直接报错提醒改映射。

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="v${VERSION}"
else
  UPSTREAM_TAG=$(curl -sL "https://api.github.com/repos/AdguardTeam/AdGuardHome/releases/latest" | \
    jq -r '.tag_name')
  VERSION=$(echo "$UPSTREAM_TAG" | sed -E 's/^v//')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for adguardhome" >&2
  exit 1
fi

MAJOR=$(echo "$VERSION" | cut -d. -f1)
MINOR=$(echo "$VERSION" | cut -d. -f2)
PATCH=$(echo "$VERSION" | cut -d. -f3)
if [ "$MAJOR" != "0" ]; then
  echo "Upstream major is no longer 0 ($VERSION) — version mapping needs review" >&2
  exit 1
fi
if [ "$PATCH" -gt 99 ]; then
  echo "Upstream patch > 99 ($VERSION) — would violate ugcli minor<=2-digit rule, mapping needs review" >&2
  exit 1
fi
PROJECT_VERSION="${MINOR}.${PATCH}.0"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
