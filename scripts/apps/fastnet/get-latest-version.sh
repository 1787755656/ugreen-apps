#!/bin/bash
set -euo pipefail

# FastNet（KoolCenter 的全平台测速工具）没有 GitHub 仓库、没有 Release API，
# 分发渠道就是官方固件下载服务器上的一个目录：
#
#   https://fw.koolcenter.com/binary/fastnet/
#     version.txt                最新版本号 + 各架构二进制的 sha256
#     FastNet-<版本>.<架构>       各版本各架构的裸二进制（历史版本一直留着）
#
# version.txt 长这样（上游自己的 fastnet-install.sh 也是读它）：
#
#   VERSION=0.7.6
#   FASTNET_AMD64_SHA256=be70f7...
#   FASTNET_ARM64_SHA256=a4a56d...
#   FASTNET_ARMV7_SHA256=7fc6e9...
#
# 所以版本探测 = 读 version.txt 的 VERSION 字段，天然就是 x.y.z 形状，
# 不需要像 qbittorrent/adguardhome 那样做映射。新版本 -> 新版本号 ->
# scripts/ci/resolve-release-tag.sh 的 git tag 去重发现是新 tag -> 触发构建；
# 上游没动 -> 版本号不变 -> tag 已存在 -> 跳过。和其它 app 同一套机制。
#
# 镜像顺序照抄上游 install 脚本：kspeeder 为主、koolcenter 兜底
# （两个站点内容一致，实测 version.txt 逐字节相同）。

INPUT_VERSION="${1:-}"

BASE_URL_PRIMARY="https://fw.kspeeder.com/binary/fastnet"
BASE_URL_FALLBACK="https://fw.koolcenter.com/binary/fastnet"

fetch_version_txt() {
  for base in "$BASE_URL_PRIMARY" "$BASE_URL_FALLBACK"; do
    # 带 no-cache：version.txt 是这条链路上唯一"内容会变、文件名不变"的文件，
    # CDN 边缘节点缓存住旧的会让我们错过新版本。
    if curl -fsSL --max-time 30 \
         -H 'Cache-Control: no-cache' -H 'Pragma: no-cache' "${base}/version.txt"; then
      return 0
    fi
  done
  return 1
}

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  VERSION_TXT=$(fetch_version_txt) || {
    echo "Failed to fetch version.txt for fastnet from all mirrors" >&2
    exit 1
  }
  VERSION=$(printf '%s\n' "$VERSION_TXT" | awk -F= '/^VERSION=/{print $2; exit}' | tr -d '\r')
fi

if [ -z "$VERSION" ]; then
  echo "Failed to resolve version for fastnet (no VERSION= line in version.txt)" >&2
  exit 1
fi

# 形状校验：project.yaml 的 version 必须是 x.y.z，且 ugcli 实测限制中段最多两位数
# （见 ugos-pro-app-dev skill）。上游一路 0.7.x 过来，真超了要在这里当场失败，
# 而不是等 CI 跑到 ugcli check 才报一句看不懂的 "must be a valid version number"。
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]{1,2}\.[0-9]+$ ]]; then
  echo "::error::fastnet version '$VERSION' 不符合 ugcli 的 x.y.z（中段最多两位）要求，需要在本脚本里加映射规则" >&2
  exit 1
fi

PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
fi
