#!/bin/bash
set -euo pipefail

# 上游 MariaDB 11.4 LTS。版本源 = archive.mariadb.org 的版本目录列表：
#   https://archive.mariadb.org/mariadb-<ver>/repo/debian/pool/main/m/mariadb/
# 下面的 deb 就是 build.sh/fetch-runtime.py 实际下载的东西，所以"探测到的版本"
# 和"能下载到的 deb"天然一致 —— 探测完还会 HEAD 校验一遍 deb 确实存在才放行。
#
# 只跟 11.4.x（LTS，支持期到 2029）。跨大版本意味着 bootstrap SQL 集合、
# 依赖闭包、数据目录升级路径都要重新真机验证，那是一个人工决策，不自动化。
#
# PINNED_VERSION（meta.env）：钉版开关。数据库应用首发走"先真机、再放行"，
# 钉住已验证的版本；删掉该行即恢复自动跟踪最新 11.4.x。
#
# upsteam_tag 输出保留完整 tag 形式 mariadb-<ver>（MariaDB/server 的 tag 命名）。
# 上游不发 GitHub Release 正文，fetch-upstream-notes 拿不到内容是预期，模板能兜住。

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

MARIADB_SERIES="11.4"
MARIADB_MIRROR="https://archive.mariadb.org"

# 与 fetch-runtime.py 的 fetch_mariadb() 保持同一个 URL 模式。
# 用 amd64 那一路做存在性校验：官方按 release 一起发各架构，amd64 缺 = 整个版本没发好。
deb_url() {
  local v="$1"
  echo "${MARIADB_MIRROR}/mariadb-${v}/repo/debian/pool/main/m/mariadb/mariadb-server_${v}+maria~deb12_amd64.deb"
}

deb_exists() {
  local url="$1"
  [ "$(curl -fsI -o /dev/null -w '%{http_code}' --max-time 30 "$url" 2>/dev/null || true)" = "200" ]
}

validate_version() {
  local v="$1"
  printf '%s' "$v" | grep -qE '^[0-9]+\.[0-9]{1,2}\.[0-9]+$' || {
    echo "版本号 $v 不满足 ugcli 的格式要求（三段数字，中段最多两位）" >&2; return 1; }
  deb_exists "$(deb_url "$v")" || {
    echo "archive.mariadb.org 上找不到 MariaDB ${v} 的 deb12 包：$(deb_url "$v")" >&2; return 1; }
}

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  # 手动指定版本（workflow_dispatch 的 version 输入 / 本地调试）：校验后直接用。
  validate_version "$INPUT_VERSION"
  VERSION="$INPUT_VERSION"
elif [ -n "${PINNED_VERSION:-}" ]; then
  validate_version "$PINNED_VERSION"
  VERSION="$PINNED_VERSION"
else
  # 抓版本目录列表（一个几百 KB 的 HTML 索引），抽出 11.4.x，
  # 从最新往旧走，第一个 deb 真实存在的就是要构建的版本 ——
  # 目录里有而 deb 没发齐（或已撤回）的版本自动跳过。
  INDEX=$(curl -fsSL --max-time 120 "${MARIADB_MIRROR}/") || {
    echo "拉取 ${MARIADB_MIRROR}/ 目录失败" >&2; exit 1; }
  CANDIDATES=$(printf '%s' "$INDEX" \
    | grep -oE "mariadb-${MARIADB_SERIES}\.[0-9]+" \
    | sed 's/^mariadb-//' | sort -u -t. -k1,1n -k2,2n -k3,3n)
  [ -n "$CANDIDATES" ] || { echo "目录里没有任何 ${MARIADB_SERIES}.x 版本" >&2; exit 1; }

  VERSION=""
  while IFS= read -r v; do
    [ -n "$v" ] || continue
    echo "探测 mariadb-${v} …"
    if deb_exists "$(deb_url "$v")"; then
      VERSION="$v"
      break
    fi
  done < <(printf '%s\n' "$CANDIDATES" | tac)

  [ -n "$VERSION" ] || { echo "${MARIADB_SERIES} 系列没有任何一个版本的 deb 可用" >&2; exit 1; }
  echo "==> 最新可用版本：MariaDB ${VERSION}"
fi

PROJECT_VERSION="$VERSION"
UPSTREAM_TAG="mariadb-${VERSION}"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
