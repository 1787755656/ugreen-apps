#!/usr/bin/env bash
# 证件照服务（Docker 型，纯 CPU）build.sh。
#
# 镜像由本地构建并发布在独立仓库
#   1787755656/idphotos-ugreen-app（tag idphotos/v1.0.0-r1）：
#     img-arm64.tar / img-amd64.tar = python:3.10-slim 基础上含项目源码的
#     HivisionIDPhotos Pro CPU 镜像（模型不内置，安装时挂载模型文件夹）。
# 这里只做：下载对应架构镜像 tar，铺 rootfs_<arch>/images/，随后由可复用
# workflow 统一执行 ugcli check / pack / 发布。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
VERSION="${1:?usage: build.sh <version> <arch>}"
ARCH="${2:?usage: build.sh <version> <arch>}"
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/meta.env"
APP_DIR="$REPO_ROOT/$PROJECT_DIR"
IMG_DIR="$APP_DIR/rootfs_${ARCH}/images"
mkdir -p "$IMG_DIR"

BASE_REPO="1787755656/idphotos-ugreen-app"
BASE_TAG="idphotos/v1.0.0-r1"
ASSET="img-${ARCH}.tar"
URL="https://github.com/${BASE_REPO}/releases/download/${BASE_TAG}/${ASSET}"

echo "downloading ${URL}"
if [ -n "${GH_TOKEN:-}" ]; then
  curl -sL -H "Authorization: Bearer ${GH_TOKEN}" -o "$IMG_DIR/hivision-idphotos-pro-1.0.0.tar" "$URL"
else
  curl -sL -o "$IMG_DIR/hivision-idphotos-pro-1.0.0.tar" "$URL"
fi
SIZE=$(stat -c%s "$IMG_DIR/hivision-idphotos-pro-1.0.0.tar" 2>/dev/null || stat -f%z "$IMG_DIR/hivision-idphotos-pro-1.0.0.tar")
if [ "$SIZE" -lt 200000000 ]; then
  echo "ERROR: downloaded image tar too small (${SIZE} bytes)" >&2
  exit 1
fi
echo "image tar ready: ${SIZE} bytes -> $IMG_DIR/hivision-idphotos-pro-1.0.0.tar"
