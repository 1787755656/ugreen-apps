#!/usr/bin/env bash
# 解析 P2Pee 内网穿透的版本号。
#
# 上游 p2pee-proxy 在 gitee 以固定版本发布（当前 1.0.0），下载 URL 直接带版本号，
# 没有可轮询的「最新」接口；本应用版本跟随上游，因此这里固定输出 1.0.0。
# 若后续上游出新版本，可手动改此处，或传入 $1 覆盖。
set -euo pipefail

PROXY_VERSION="${1:-1.0.0}"
VERSION="$PROXY_VERSION"
PROJECT_VERSION="$PROXY_VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
fi
