#!/bin/bash
set -euo pipefail

# EasyTier 是 Docker 型应用:包里内置的是 majosissi/easytier 镜像(上游
# EasyTier 官方 release 产物的 core + web 一体打包,入口 /run.sh 同时拉起
# easytier-web-embed 与 easytier-core)。
#
# 版本号【不查】GitHub Release —— 镜像的重构节奏由上游(MajoSissi)手动触发,
# 可能滞后于 EasyTier/EasyTier 的 release。以【镜像内容】为准:
#   docker run --entrypoint easytier-core majosissi/easytier:latest --version
#   → "easytier-core 2.6.4-8428a89d",取 x.y.z 段作版本。
# 也就是说:上游发了 2.7.0 但镜像还没跟 → 不会出新包(正确行为);
# 镜像跟上了 → 当天定时任务自动发现并发布。

INPUT_VERSION="${1:-}"
IMAGE_SRC="majosissi/easytier:latest"

if [ -n "$INPUT_VERSION" ]; then
  # 手动指定版本:直接采用,build.sh 会断言镜像内容与之一致,
  # 防止把 2.6.4 的镜像内容错标成 2.7.0。
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="v${VERSION}"
else
  echo "==> Pulling ${IMAGE_SRC} (version detection)"
  # Docker Hub 匿名拉取在共享出口 IP 上会撞限流,重试几次
  pulled=0
  for i in 1 2 3; do
    if docker pull "$IMAGE_SRC"; then pulled=1; break; fi
    echo "pull attempt $i failed, retrying in 20s..." >&2
    sleep 20
  done
  [ "$pulled" = "1" ] || { echo "::error::docker pull ${IMAGE_SRC} failed after retries" >&2; exit 1; }

  RAW=$(docker run --rm --entrypoint easytier-core "$IMAGE_SRC" --version 2>/dev/null | head -1)
  echo "==> image reports: ${RAW}"

  # "easytier-core 2.6.4-8428a89d" → 2.6.4(可容忍未来出现 v 前缀)
  VERSION=$(printf '%s' "$RAW" | sed -E 's/^easytier-core v?([0-9]+\.[0-9]+\.[0-9]+).*$/\1/')
  if ! printf '%s' "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
    echo "::error::无法从镜像输出解析版本号: '${RAW}'(期望形如 'easytier-core 2.6.4-<commit>')" >&2
    exit 1
  fi
  UPSTREAM_TAG="v${VERSION}"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for easytier" >&2
  exit 1
fi

# project.yaml 的 version 只要 x.y.z
PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
