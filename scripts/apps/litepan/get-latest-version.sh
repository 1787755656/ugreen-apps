#!/bin/bash
set -euo pipefail

# LitePan（Go 版）上游【没有 GitHub Release】，tag 也停在 v0.3.0-beta，而实际
# 版本已经是 v0.4.6-Beta —— 发布渠道是 Docker Hub（ponphil/litepan:beta）。
# 所以唯一可靠的版本源是 main 分支源码里的品牌版本常量：
#
#   internal/httpx/user_agent.go:  AppVersion = "v0.4.6-Beta"
#
# （前端 web/src/version.ts 里有同一个值，上游注释里明说两处要保持一致；
#   取 Go 那份是因为它就在我们要编译的那棵树里，build.sh 能原地复核。）
#
# 后果和 magicpush 一样，写清楚免得以后困惑：
#   1. 构建永远来自 main HEAD（没有可用的 tag 可钉）；
#   2. 上游改了内容但没 bump 这个常量，**不会**触发重建 —— tag 去重只认版本字符串。
#
# 交叉验证（非自动，排查时手动跑）：Docker Hub 的最新 beta tag 应该和这个值对得上
#   curl -s "https://hub.docker.com/v2/repositories/ponphil/litepan/tags/?page_size=25&ordering=last_updated" \
#     | jq -r '.results[].name'

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  RAW_URL="https://raw.githubusercontent.com/Ponphil/LitePan/main/internal/httpx/user_agent.go"
  if ! SRC=$(curl --fail --silent --show-error --location \
        --retry 3 --retry-all-errors --retry-delay 2 \
        --connect-timeout 10 --max-time 30 "$RAW_URL"); then
    echo "Failed to fetch $RAW_URL" >&2
    exit 1
  fi
  # AppVersion = "v0.4.6-Beta"  →  0.4.6-Beta
  VERSION=$(sed -nE 's/.*AppVersion[[:space:]]*=[[:space:]]*"v?([^"]+)".*/\1/p' <<<"$SRC" | head -1)
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for litepan" >&2
  exit 1
fi

# VERSION 保留全部精度（含 -Beta 后缀），用于 git tag 去重；
# PROJECT_VERSION 截成 ugcli 要求的 x.y.z 写进 project.yaml。
PROJECT_VERSION=$(sed -E 's/-.*$//' <<<"$VERSION" | cut -d. -f1-3)

if ! [[ "$PROJECT_VERSION" =~ ^[0-9]+\.[0-9]{1,2}\.[0-9]+$ ]]; then
  # ugcli 对 version 有个没写进文档的校验：中段(minor)最多两位数。
  # 撞上了要在这里做确定性映射，别让它到 ugcli check 那步才炸。
  echo "PROJECT_VERSION '$PROJECT_VERSION' 不满足 ugcli 的 x.y.z（minor 最多两位）要求" >&2
  exit 1
fi

# 上游没有可用的 tag，build.sh 一律拉 main。
UPSTREAM_TAG="main"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
