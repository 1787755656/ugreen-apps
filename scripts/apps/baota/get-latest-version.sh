#!/bin/bash
set -euo pipefail

# 宝塔面板 Docker 型应用：包里内置 btpanel/baota 镜像（堡塔安全官方发布，
# amd64/arm64 双架构）。版本探测以【镜像内容】为准——上游的 tag 全是变体名
# （latest / nas / slim / lnmp / debian13 / 10.0_lts…），不带版本号，且官方
# README 明说"镜像会随着宝塔面板更新"（同 tag 重推）。这与 easytier 同款：
# 拉镜像读内容版本。
#
# 版本藏在哪里（2026-11-04 实测 latest）：
#   镜像里没有 /www/server/panel（面板本体打包成根目录 /www.tar.gz，约 131MB，
#   首次启动时 bt.sh 的 restore_panel_data 解到 /）。面板版本在 tarball 内
#   www/server/panel/class/common.py 的 `g.version = '13.1.0'` 一行。
#   config blob 无 label、无版本 env，registry 侧没有更便宜的读法，只能拉镜像。
#
# ⚠ 这里拉的是 amd64（runner 本机架构，快）；build.sh 会对两个架构分别做
#   同样的内容断言，所以单架构探测不会打出双架构版本不一致的包。
#
# 版本号是纯 x.y.z（如 13.1.0），无预发布后缀；"镜像重打但面板版本没变"时
# VERSION 不变，schedule 走 resolve-release-tag 的"已发布跳过"。用户要求
# 本应用只追基础版本（scripts/ci/base-version-only.conf 里有 baota），当前
# 上游形态下该策略与"版本没变不重发"天然重合， conf 条目是政策声明 +
# 对未来 tag 形态变化的保险。
#
# VERSION = 13.1.0   （release tag 用）
# UPSTREAM_TAG 为空  （无 GitHub release 可引用）

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="${INPUT_VERSION#v}"
else
  echo "==> Pulling btpanel/baota:latest (amd64) for content version detection"
  for i in 1 2 3; do
    if docker pull --platform linux/amd64 "btpanel/baota:latest"; then break; fi
    echo "pull attempt $i failed, retrying in 20s..." >&2
    sleep 20
    [ "$i" = "3" ] && { echo "::error::docker pull btpanel/baota:latest failed after retries" >&2; exit 1; }
  done
  # 面板版本从 /www.tar.gz 里的 common.py 读（见文件头注释）
  VERSION=$(docker run --rm --entrypoint /bin/sh "btpanel/baota:latest" -c \
    "tar -xzf /www.tar.gz -O www/server/panel/class/common.py" 2>/dev/null \
    | grep "g.version" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "::error::无法从 btpanel/baota:latest 镜像内容解析面板版本（common.py 的 g.version 行缺失？上游改了打包结构？）" >&2
  exit 1
fi

if ! printf '%s' "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "::error::面板版本号不符合预期格式: '${VERSION}'（期望纯 x.y.z，如 13.1.0）" >&2
  exit 1
fi

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
