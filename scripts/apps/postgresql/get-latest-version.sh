#!/bin/bash
# PostgreSQL 17 版本探测：与 randomvideo 同款 —— 版本唯一来源是本仓库
# apps/postgresql/com.personal.postgresql17/project.yaml 的 version: 字段，
# 由人工 bump（当前 1.0.x 应用版本线，见 meta.env 顶部说明）。
#
# 设计理由：
#   1. bundled 的数据库小版本由 fetch-runtime.py 在构建时取 pgdg 索引里的
#      最新 17.x，应用版本号不跟它走（桌面工程沿用的方案）；
#   2. 版本字符串不变时，定时任务会按"已发布"跳过，不会反复刷构建号 ——
#      想刷新 bundled 小版本就 bump project.yaml 的 version。
set -euo pipefail

INPUT_VERSION="${1:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"
YAML="$REPO_ROOT/$PROJECT_DIR/project.yaml"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  VERSION=$(grep -E '^version:' "$YAML" | head -1 | sed -E 's/^version:[[:space:]]*//' | tr -d '[:space:]')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for postgresql (check $YAML)" >&2
  exit 1
fi
case "$(printf '%s' "$VERSION" | grep -cE '^[0-9]+\.[0-9]{1,2}\.[0-9]+$')" in
  1) ;;
  *) echo "版本号 $VERSION 不满足 ugcli 的格式要求（三段数字，中段最多两位）" >&2; exit 1 ;;
esac

# 自研版本线：project.yaml 的 version 即发布版本，无独立上游 tag。
PROJECT_VERSION="$VERSION"
UPSTREAM_TAG=""

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
