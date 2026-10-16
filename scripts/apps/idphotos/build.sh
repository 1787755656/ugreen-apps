#!/usr/bin/env bash
# 证件照服务（Docker 型，纯 CPU）build.sh。
#
# 镜像由本地构建并发布在独立仓库
#   1787755656/idphotos-ugreen-app（tag idphotos/v1.0.0-r1）：
#     img-arm64.tar / img-amd64.tar = python:3.10-slim 基础上含项目源码的
#     HivisionIDPhotos Pro CPU 镜像（模型不内置，安装时挂载模型文件夹）。
# 这里只做：下载对应架构镜像 tar（写为 <image>:<version> 对应的文件名，
#   与 compose 的 image tag 严格一致），铺 rootfs_<arch>/images/，收窄
#   support_arch 到当前架构，随后由可复用 workflow 统一执行 ugcli check /
#   pack / 发布。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
VERSION="${1:?usage: build.sh <version> <arch>}"
ARCH="${2:?usage: build.sh <version> <arch>}"
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/meta.env"
APP_DIR="$REPO_ROOT/$PROJECT_DIR"
IMG_DIR="$APP_DIR/rootfs_${ARCH}/images"
mkdir -p "$IMG_DIR"
rm -f "$IMG_DIR"/*.tar

BASE_REPO="1787755656/idphotos-ugreen-app"
BASE_TAG="idphotos/v1.0.0-r1"
ASSET="img-${ARCH}.tar"
URL="https://github.com/${BASE_REPO}/releases/download/${BASE_TAG}/${ASSET}"
TAR_NAME="hivision-idphotos-pro-${VERSION}.tar"

echo "downloading ${URL}"
if [ -n "${GH_TOKEN:-}" ]; then
  curl -sL -H "Authorization: Bearer ${GH_TOKEN}" -o "$IMG_DIR/${TAR_NAME}" "$URL"
else
  curl -sL -o "$IMG_DIR/${TAR_NAME}" "$URL"
fi
SIZE=$(stat -c%s "$IMG_DIR/${TAR_NAME}" 2>/dev/null || stat -f%z "$IMG_DIR/${TAR_NAME}")
if [ "$SIZE" -lt 200000000 ]; then
  echo "ERROR: downloaded image tar too small (${SIZE} bytes)" >&2
  exit 1
fi
echo "image tar ready: ${SIZE} bytes -> $IMG_DIR/${TAR_NAME}"

# 收窄 support_arch 到当前架构（单架构打包；改动只存在于 CI workspace）
python3 - "$ARCH" "$APP_DIR/project.yaml" <<'PY'
import re, sys
arch, path = sys.argv[1], sys.argv[2]
s = open(path, encoding="utf-8").read()
s = re.sub(r"support_arch:\n(  - \w+\n)+", "support_arch:\n  - %s\n" % arch, s)
open(path, "w", encoding="utf-8").write(s)
print("support_arch narrowed to", arch)
PY
