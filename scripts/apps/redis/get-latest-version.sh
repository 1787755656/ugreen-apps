#!/bin/bash
set -euo pipefail

# 上游 Redis 8.x。版本源 = packages.redis.io 的 Packages 索引（bookworm main）：
# 索引里 redis-server 的历史版本条目全部保留（6.x 一路到当前 8.10.x），
# build.sh 里的 fetch-runtime.py 用的就是同一份索引 —— "探测到的版本"和
# "能下载到的 deb"天然一致。
#
# 只跟 8.x 系列（新协议/模块行为与 6.x/7.x 有差异，跨大版本要真机重新验证）。
#
# PINNED_VERSION（meta.env）：钉版开关，机制同 MariaDB。删掉该行即恢复自动跟踪。
#
# 注意 deb 版本带 epoch 和打包尾缀（6:8.10.0-1rl1~bookworm1），对外一律剥成
# 8.10.0 —— 和 fetch-runtime.py main() 里的 upstream 归一化完全一致。

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

MIRROR="https://packages.redis.io/deb"
SUITE="bookworm"

# 用 amd64 索引做探测/校验：官方两个架构的版本一起发，amd64 有 = arm64 有。
# fetch-runtime 在构建时按各自架构的索引取包，那边的 sha256 校验是最终闸门。
index_stanzas() {
  curl -fsSL --max-time 180 "${MIRROR}/dists/${SUITE}/main/binary-amd64/Packages" 2>/dev/null \
    | awk -v pkg="redis-server" '
        /^Package: /{ inpkg = ($0 == "Package: " pkg) }
        inpkg && /^Version: /  { v = substr($0, 10) }
        inpkg && /^Filename: / { f = substr($0, 11) }
        /^$/ { if (v != "" && f != "") print v "\t" f; v = f = "" }
        END  { if (v != "" && f != "") print v "\t" f }'
}

# deb 版本 → 对外版本：剥 epoch（6:）、剥打包尾缀（-1rl1~bookworm1）。
upstream_of() { sed -E 's/^[0-9]+://; s/[-+~].*$//' <<<"$1"; }

latest_upstream() {
  index_stanzas | cut -f1 | while read -r v; do upstream_of "$v"; done \
    | grep -E '^8\.' | sort -u -t. -k1,1n -k2,2n -k3,3n | tail -1
}

# 校验某个版本在索引里真实存在，返回 0/1。
version_in_index() {
  local want="$1" v f
  while IFS=$'\t' read -r v f; do
    if [ "$(upstream_of "$v")" = "$want" ]; then
      [ "$(curl -fsI -o /dev/null -w '%{http_code}' --max-time 30 "${MIRROR}/${f}" 2>/dev/null || true)" = "200" ]
      return
    fi
  done < <(index_stanzas)
  return 1
}

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  # 手动指定版本（workflow_dispatch / 本地调试）：校验后直接用。
  version_in_index "$INPUT_VERSION" || {
    echo "packages.redis.io 索引里找不到 Redis ${INPUT_VERSION}" >&2; exit 1; }
  VERSION="$INPUT_VERSION"
elif [ -n "${PINNED_VERSION:-}" ]; then
  version_in_index "$PINNED_VERSION" || {
    echo "钉住的 Redis ${PINNED_VERSION} 已从 packages.redis.io 索引消失，请解除钉版或换版本" >&2; exit 1; }
  VERSION="$PINNED_VERSION"
else
  VERSION="$(latest_upstream)"
  [ -n "$VERSION" ] || { echo "索引里没有任何 8.x 版本" >&2; exit 1; }
  version_in_index "$VERSION" || {
    echo "探测到的 ${VERSION} 的 deb 不可下载，请人工检查索引" >&2; exit 1; }
  echo "==> 最新可用版本：Redis ${VERSION}"
fi

PROJECT_VERSION="$VERSION"
# redis/redis 无 GitHub Release；tag 形式仅用于 Release 说明里的链接兜底。
UPSTREAM_TAG=""

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
