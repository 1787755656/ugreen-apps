#!/bin/bash
set -euo pipefail

# superng6/qbittorrent 的发布渠道是 Docker Hub，不是 GitHub Release
#（SuperNG6/docker-qbittorrent 仓库里只有 Dockerfile，没有 Release、没有版本 tag）。
#
# 好消息是这个镜像的 tag 命名很规整：<qBittorrent版本>_v<libtorrent版本>，
# 例如 5.2.3_v2.0.14。所以版本探测就是"读 tag 列表"：
#
#   latest              sha256:8db863…   ← 和下面这个是同一份 manifest
#   5.2.3_v2.0.14       sha256:8db863…
#   5.2.3_v2.0.13       sha256:1d89e3…
#
# 取法：找出和 `latest` **同一个 manifest 摘要**的那个版本 tag。
# 这比"按时间取最新"准确——上游偶尔会补发旧版本分支的镜像，
# 那种 tag 更新时间更晚，但 latest 并没有指向它。
# 拿不到摘要时（Docker Hub 偶发不返回 digest）退回"按更新时间取最新"。
#
# 顺带一个好处：libtorrent 版本变了（qBittorrent 版本没变）也会得到新的
# VERSION 字符串，于是 tag 去重认得出来、会重新打包。上游只是重新构建、
# 两段版本都没动的话不会触发重建 —— 这和本仓库其它 app 的取舍一致。

INPUT_VERSION="${1:-}"

REPO="superng6/qbittorrent"
API="https://hub.docker.com/v2/repositories/${REPO}/tags/?page_size=100&ordering=last_updated"
# 版本 tag 的形状：5.2.3_v2.0.14
TAG_RE='^[0-9]+\.[0-9]+\.[0-9]+_v[0-9]+(\.[0-9]+)*$'

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  if ! BODY=$(curl --fail --silent --show-error --location \
        --retry 3 --retry-all-errors --retry-delay 2 \
        --connect-timeout 10 --max-time 60 "$API"); then
    echo "Failed to fetch $API" >&2
    exit 1
  fi

  VERSION=$(jq -r --arg re "$TAG_RE" '
    .results as $all
    | ($all[] | select(.name == "latest") | .digest // empty) as $d
    | [ $all[] | select(.name | test($re)) | select(.digest == $d) | .name ][0] // empty
  ' <<<"$BODY")

  if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
    echo "::warning::没能通过摘要把 latest 对应到版本 tag，退回按更新时间取最新" >&2
    VERSION=$(jq -r --arg re "$TAG_RE" '
      [ .results[] | select(.name | test($re)) | .name ][0] // empty
    ' <<<"$BODY")
  fi
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for qbittorrent-ng6" >&2
  exit 1
fi

if ! [[ "$VERSION" =~ $TAG_RE ]]; then
  echo "版本 '$VERSION' 不是预期的 <x.y.z>_v<libtorrent> 形状，上游 tag 命名可能变了" >&2
  exit 1
fi

# VERSION 保留全部精度（含 _v<libtorrent> 后缀）用于 git tag 去重；
# PROJECT_VERSION 截成 ugcli 要求的 x.y.z 写进 project.yaml。
PROJECT_VERSION="${VERSION%%_*}"

if ! [[ "$PROJECT_VERSION" =~ ^[0-9]+\.[0-9]{1,2}\.[0-9]+$ ]]; then
  # ugcli 对 version 有个没写进文档的校验：中段(minor)最多两位数。
  # 撞上了要在这里做确定性映射，别让它到 ugcli check 那步才炸。
  echo "PROJECT_VERSION '$PROJECT_VERSION' 不满足 ugcli 的 x.y.z（minor 最多两位）要求" >&2
  exit 1
fi

# build.sh 直接拉这个 tag，而不是 latest —— 探测和构建之间 latest 可能移动，
# 钉到具体 tag 才能保证"打出来的就是探测到的那个版本"。
UPSTREAM_TAG="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
