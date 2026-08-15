#!/bin/bash
set -euo pipefail

# 知音音乐上游【只发 Docker 镜像】—— GitHub 仓库里只有 README、配置示例和
# docker-compose.yml，没有 Rust 源码、没有 Release、没有 tag。
# 所以唯一的版本源是 Docker Hub 上的镜像 tag：
#
#   https://hub.docker.com/r/qwex333/zhiyin-music/tags
#
# tag 长这样（2026-08 实际列表）：
#   latest
#   0.8.1  0.8.0  0.7.1  0.7.0  0.6.14 …          ← 我们要的
#   build-amd64-57d81a57…  build-arm64-57d81a57…  ← CI 中间产物，要滤掉
#
# 取"版本号最大"的那个，不取 last_updated 最新的：上游偶尔会重推旧版本的
# 修补镜像，按时间取会让版本号往回走，而 build_num 是全局递增的，
# 回退版本会打出一个"版本更低但构建号更高"的包，装到 NAS 上是降级。

REPO="qwex333/zhiyin-music"
INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  API="https://hub.docker.com/v2/repositories/${REPO}/tags?page_size=100&ordering=-last_updated"
  if ! BODY=$(curl --fail --silent --show-error --location \
        --retry 3 --retry-all-errors --retry-delay 2 \
        --connect-timeout 10 --max-time 30 "$API"); then
    echo "Failed to fetch $API" >&2
    exit 1
  fi
  # 只认纯 x.y.z 的 tag，然后按版本号排序取最大
  VERSION=$(jq -r '.results[].name' <<<"$BODY" \
    | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' \
    | sort -t. -k1,1n -k2,2n -k3,3n \
    | tail -1)
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for zhiyinmusic" >&2
  exit 1
fi

# 上游本来就是 x.y.z，两个值一样；仍然分开输出，保持和其它 app 一致的契约
# （VERSION 用于 git tag 去重，PROJECT_VERSION 写进 project.yaml）。
PROJECT_VERSION="$VERSION"

if ! [[ "$PROJECT_VERSION" =~ ^[0-9]+\.[0-9]{1,2}\.[0-9]+$ ]]; then
  # ugcli 对 version 有个没写进文档的校验：中段(minor)最多两位数。
  # 撞上了要在这里做确定性映射，别让它到 ugcli check 那步才炸。
  echo "PROJECT_VERSION '$PROJECT_VERSION' 不满足 ugcli 的 x.y.z（minor 最多两位）要求" >&2
  exit 1
fi

# build.sh 拿这个去 registry 查 manifest；上游的镜像 tag 就是版本号本身。
UPSTREAM_TAG="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
