#!/bin/bash
set -euo pipefail

# Upstream tags are "vX.Y.Z" (e.g. v0.107.80, v1.0.0). ugcli 实测限制:
# 版本号中段 (minor) 最多两位数,首末段可以三位以上。
#
# 版本映射:
#   - major=0（历史 0.x 时期）: 去掉前导 "0." 再补 ".0",即
#     0.107.80 → 107.80.0。此分支只服务旧版重建，保留原映射不变。
#   - major≥1（2026-11 上游进入 1.0.0 起）: 新版本序列直接与上游对齐，
#     PROJECT_VERSION = VERSION，即 1.0.0 → 1.0.0。
# 两种映射都要保证 ugcli 的中段不超过两位：major=0 时中段是上游 patch，
# major≥1 时中段是上游 minor；超限时脚本直接报错提醒改映射。

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
if ! [[ "$MAJOR" =~ ^[0-9]+$ && "$MINOR" =~ ^[0-9]+$ && "$PATCH" =~ ^[0-9]+$ ]]; then
  echo "Version '$VERSION' is not numeric x.y.z — mapping needs review" >&2
  exit 1
fi
if [ "$MAJOR" = "0" ]; then
  # 历史映射：patch 落到 ugcli 中段，需 <= 99。
  if [ "$PATCH" -gt 99 ]; then
    echo "Upstream patch > 99 ($VERSION) — would violate ugcli minor<=2-digit rule, mapping needs review" >&2
    exit 1
  fi
  PROJECT_VERSION="${MINOR}.${PATCH}.0"
else
  # 新版本序列（1.0.0 起）直接与上游对齐：中段是上游 minor，需 <= 99。
  if [ "$MINOR" -gt 99 ]; then
    echo "Upstream minor > 99 ($VERSION) — would violate ugcli minor<=2-digit rule, mapping needs review" >&2
    exit 1
  fi
  PROJECT_VERSION="$VERSION"
fi

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
