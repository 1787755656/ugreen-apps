#!/bin/bash
set -euo pipefail

# Trilium（TriliumNext/Trilium）—— 上游每个版本都发 GitHub Release（tag vX.Y.Z），
# 版本号直接取 releases/latest；没有 Release 时退回 tags 列表。
# 上游有更新 → 版本号变化 → resolve-release-tag.sh 看不到同名 tag → 触发构建，
# 这就是「上游更新自动构建」的实现方式（配合 build-apps.yml 的每日 schedule）。
#
# ⚠ 版本号映射：ugcli 对 project.yaml 的 version 有隐藏校验（中段最多两位数），
#   而 Trilium 的版本是 0.106.0 这种形状。按确定性映射「去掉前导 0. 再补 .0」：
#   0.106.0 → 106.0.0（上游小版本递增时首段单调递增，不会回退）。
#   - VERSION / UPSTREAM_TAG 保留上游原样（release tag = trilium/v0.106.0，
#     更新说明里展示的"上游版本"也是它）
#   - PROJECT_VERSION 是映射后的包版本（upk 文件名与商店展示用它）
#   - 若上游哪天发 1.x.y，映射结果会【小于】历史 106.x —— 到时候要人工介入
#     （比如把整数段整体 +1000），脚本只在这里挡住中段超限的情况。

INPUT_VERSION="${1:-}"

error() {
  echo "[ERROR] $1" >&2
  exit 1
}

map_project_version() {
  python3 - "$1" <<'PY'
import sys
v = sys.argv[1]
parts = v.split(".")
if parts[0] == "0" and len(parts) >= 2:
    parts = parts[1:] + ["0"]
else:
    parts = parts + ["0"] * (3 - len(parts))
if len(parts[1]) > 2:
    sys.exit(f"上游版本 {v} 的中段超过两位数，ugcli 不接受，需要人工处理")
print(".".join(parts[:3]))
PY
}

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="v${VERSION}"
else
  UPSTREAM_TAG=$(gh api repos/TriliumNext/Trilium/releases/latest --jq '.tag_name' 2>/dev/null || true)

  if [ -z "$UPSTREAM_TAG" ] || [ "$UPSTREAM_TAG" = "null" ]; then
    # 没有 Release 时按 tag 排（sort -V 保证 v0.9.10 > v0.9.9）
    UPSTREAM_TAG=$(curl -sL https://api.github.com/repos/TriliumNext/Trilium/tags \
      | jq -r '.[].name' | sort -V | tail -1)
  fi

  [ -n "$UPSTREAM_TAG" ] && [ "$UPSTREAM_TAG" != "null" ] \
    || error "无法确定 TriliumNext/Trilium 的最新版本"

  VERSION="${UPSTREAM_TAG#v}"
fi

[ -n "$VERSION" ] || error "版本号为空"

PROJECT_VERSION=$(map_project_version "$VERSION")

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
