#!/usr/bin/env bash
# 证件照服务（Docker 型）build.sh——CI 占位实现。
#
# 该应用的镜像由本地构建并发布（python:3.10-slim 基础 + HivisionIDPhotosPro
# 源码，双架构 docker build 后 docker save），upk 经本仓库 Release 发布。
# 因此 CI 不做镜像构建：should_build=true 到达这里时，断言对应 Release
# 的 upk 资产已存在（即本地已完成发布），否则给出明确指引并失败——
# 避免发布空包或静默过期。
set -euo pipefail

VERSION="${1:?usage: build.sh <version> <arch>}"
ARCH="${2:?usage: build.sh <version> <arch>}"
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/meta.env"
TAG="${FILE_PREFIX}/v${VERSION}"

REPO="${GITHUB_REPOSITORY:-1787755656/ugreen-apps}"
ASSET="${ARCH}_nasync_${APP_ID}_${VERSION}.0001.upk"
URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"

CODE=$(curl -s -o /dev/null -w '%{http_code}' -IL "$URL")
if [ "$CODE" = "200" ] || [ "$CODE" = "302" ]; then
  echo "Release asset already published: $URL (CI 无需构建)"
  exit 0
fi

echo "ERROR: ${TAG} 的 upk 资产不存在（${URL}）。" >&2
echo "该应用由本地构建发布：先在本地执行 docker build/save 与 ugcli pack，" >&2
echo "再以 tag ${TAG} 上传 upk 资产到本仓库 Release，然后重跑。" >&2
exit 1
