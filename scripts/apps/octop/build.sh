#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 1.0.2b9> <arch: arm64|amd64>
#
# Octop 是「全部取上游现成产物」型：CI 里不编译任何东西，也不在 runner 上
# pip 装依赖（原因见下）。四样东西各自钉版本：
#
#   1. site-packages ← 上游发布的【架构专用 fnOS native .fpk】里的 app.tgz
#      （app/site-packages，按目标架构预编译）
#   2. 前端 www       ← 上面那份 site-packages/octop/dashboard（单一来源，
#      与后端同版本；不额外下 wheel，避免前后端版本错位）
#   3. Python 运行时  ← python-build-standalone cpython 3.12.14（钉 tag + sha256）
#   4. tzdata         ← 无条件钉版覆盖（首启崩溃 0001 的根因，见 README）
#
# 为什么不在 CI 里 pip install 那 198 个钉版依赖（本地手工版当初就是这么干的）：
#   - crcmod 等包**根本不发 wheel**，`--only-binary :all:` 解析不出来；
#   - evdev 只有 sdist，x86 runner 上编译不出 aarch64 的 .so（要 qemu + 交叉工具链，
#     单个 C 扩展编译能过，整棵树在 qemu 里装一遍则是几十分钟起步且极易超时）。
# 上游对较新的版本会额外发一份 `Octop-fnos-native-arm64-<ver>.fpk`（b5 那会儿
# 只有通用版），里头就是人家自己在 arm64 上装好的同一棵树 —— 直接用，既快又
# 不引入自建依赖树。该资产缺失时 build.sh 直接报错，不拿通用版凑（那是 x86_64）。
#
# 断言：
#   - fpk 里所有 .so 必须是目标架构（逐文件 file 校验，混进错架构当场失败）
#   - tzdata 必须落进 site-packages，且 zoneinfo 在【屏蔽系统时区库】时必须能
#     解析 Asia/Shanghai（PYTHONTZPATH 指向不存在目录 —— CI runner 自带
#     /usr/share/zoneinfo，不屏蔽就测不出真机沙箱的缺失，0001 就是这么漏的）
#   - 冒烟（可跳过）：真以目标架构跑一遍 import + 时区解析。CI runner 上
#     需要一个能跑 linux/<arch> 的容器（mac 本机 Docker Desktop 原生支持）。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (arm64|amd64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

UPSTREAM_REPO="TencentCloud/Octop"
UPSTREAM_TAG="v${VERSION}"

# python-build-standalone：钉死 release tag + 版本 + sha256（三样一起才可复现）。
PBS_TAG="20260929"
PBS_PY_VERSION="3.12.14"
PBS_SHA256_ARM64="9c797cf657f6dced51d3e74eeabd7c1ca742d5bf10f66080a290e424ced8edaf"
PBS_SHA256_AMD64="06c90b93f419b63371c18f20fed0558a1a901f6518c3c24f755077e048447e7f"

# 纯 Python 包，与架构无关；钉版本是因为「系统没有 /usr/share/zoneinfo 时
# 只能靠它」，上游 fpk 里带不带随版本波动。
TZDATA_PIN="2026.4"

case "$ARCH" in
  arm64)
    FPK_ASSET="Octop-fnos-native-arm64-${VERSION}.fpk"
    PBS_ASSET="cpython-${PBS_PY_VERSION}+${PBS_TAG}-aarch64-unknown-linux-gnu-install_only.tar.gz"
    PBS_SHA256="$PBS_SHA256_ARM64"
    EXPECT_ELF="aarch64"
    ;;
  amd64)
    # 通用版 fpk 的 site-packages 是 x86_64（b5 上核实过）；下面的 ELF 断言会
    # 在架构不符时当场失败，所以这条路即使没被逐版本核实过也不会打出坏包。
    FPK_ASSET="Octop-fnos-native-${VERSION}.fpk"
    PBS_ASSET="cpython-${PBS_PY_VERSION}+${PBS_TAG}-x86_64-unknown-linux-gnu-install_only.tar.gz"
    PBS_SHA256="$PBS_SHA256_AMD64"
    EXPECT_ELF="x86-64"
    ;;
  *)
    echo "Unsupported arch: $ARCH" >&2
    exit 1
    ;;
esac

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
ROOTFS_COMMON="$REPO_ROOT/$PROJECT_DIR/rootfs_common"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Building Octop ${VERSION} (${ARCH})"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

download() {
  local url="$1" dest="$2"
  echo "==> Downloading: ${url}"
  curl -fL --http1.1 --retry 3 --retry-all-errors --retry-delay 2 \
    --connect-timeout 15 --max-time 1800 -o "$dest" "$url"
}

# ---- 1. 解析上游 .fpk 资产（不拼 URL：上游历史上改过资产名）-----------------
API_ARGS=(-fsSL --retry 3 --retry-all-errors -H "Accept: application/vnd.github+json")
if [ -n "${GH_TOKEN:-}" ]; then
  API_ARGS+=(-H "Authorization: Bearer ${GH_TOKEN}")
fi

if ! REL_JSON=$(curl "${API_ARGS[@]}" \
  "https://api.github.com/repos/${UPSTREAM_REPO}/releases/tags/${UPSTREAM_TAG}"); then
  echo "::error::上游 release ${UPSTREAM_TAG} 不存在（TencentCloud/Octop）" >&2
  exit 1
fi

FPK_URL=$(jq -r --arg n "$FPK_ASSET" '.assets[] | select(.name==$n) | .browser_download_url' <<<"$REL_JSON")
FPK_DIGEST=$(jq -r --arg n "$FPK_ASSET" '.assets[] | select(.name==$n) | .digest // ""' <<<"$REL_JSON")

if [ -z "$FPK_URL" ]; then
  echo "::error::上游 ${UPSTREAM_TAG} 没有 ${FPK_ASSET} 资产。该版本实际资产：" >&2
  jq -r '.assets[].name' <<<"$REL_JSON" | LC_ALL=C grep -i 'fpk' >&2 || true
  echo "::error::arm64 包要求上游发 Octop-fnos-native-arm64-*.fpk；上游某版本没发就别出包（不要拿通用版凑，那是 x86_64）" >&2
  exit 1
fi

# ---- 2. 下 .fpk 并解包 -------------------------------------------------------
download "$FPK_URL" "$WORK_DIR/octop.fpk"

ACTUAL_SHA=$(sha256_of "$WORK_DIR/octop.fpk")
if [ -n "$FPK_DIGEST" ] && [ "$FPK_DIGEST" != "null" ]; then
  EXPECT_SHA="${FPK_DIGEST#sha256:}"
  if [ "$ACTUAL_SHA" != "$EXPECT_SHA" ]; then
    echo "::error::fpk sha256 校验失败：期望 ${EXPECT_SHA}，实际 ${ACTUAL_SHA}" >&2
    exit 1
  fi
  echo "==> fpk sha256 OK: ${ACTUAL_SHA}"
else
  echo "==> fpk sha256（API 未提供 digest，跳过校验）: ${ACTUAL_SHA}"
fi

mkdir -p "$WORK_DIR/fpk"
tar -xzf "$WORK_DIR/octop.fpk" -C "$WORK_DIR/fpk"
# fpk 结构：app.tgz（真 payload）+ manifest / cmd / config / wizard / ICON*
if [ ! -f "$WORK_DIR/fpk/app.tgz" ]; then
  echo "::error::fpk 里没有 app.tgz，上游包结构变了" >&2
  ls -la "$WORK_DIR/fpk" >&2
  exit 1
fi

mkdir -p "$WORK_DIR/app"
tar -xzf "$WORK_DIR/fpk/app.tgz" -C "$WORK_DIR/app"

SP="$WORK_DIR/app/site-packages"
if [ ! -d "$SP/octop" ] || [ ! -f "$SP/octop/dashboard/index.html" ]; then
  echo "::error::fpk 的 site-packages 里没有 octop 包 / dashboard 前端，上游包结构变了" >&2
  ls -la "$WORK_DIR/app" >&2
  exit 1
fi

# ---- 3. 架构断言：所有 .so 必须是目标架构 ------------------------------------
SO_LIST="$WORK_DIR/so-list.txt"
# -exec ... + 而不是 xargs：文件数为 0 时不会以空参数跑一遍 file（BSD xargs 也没有 -r）
find "$SP" \( -name '*.so' -o -name '*.so.*' \) -type f -exec file {} + > "$SO_LIST"
SO_COUNT=$(LC_ALL=C grep -c 'ELF' "$SO_LIST" || true)
if [ "${SO_COUNT:-0}" -lt 30 ]; then
  echo "::error::site-packages 里只找到 ${SO_COUNT} 个 ELF .so，明显不对（应该是上百个）" >&2
  head -20 "$SO_LIST" >&2
  exit 1
fi
WRONG_ARCH=$(LC_ALL=C grep 'ELF' "$SO_LIST" | LC_ALL=C grep -v "$EXPECT_ELF" || true)
if [ -n "$WRONG_ARCH" ]; then
  echo "::error::site-packages 里混进了非 ${EXPECT_ELF} 的 .so（上游 fpk 架构不对？）：" >&2
  printf '%s\n' "$WRONG_ARCH" | head -10 >&2
  exit 1
fi
echo "==> 架构断言 OK: ${SO_COUNT} 个 .so 全是 ${EXPECT_ELF}"

# ---- 4. tzdata（无条件钉版覆盖）---------------------------------------------
# 原生沙箱没有 /usr/share/zoneinfo，Python 的 zoneinfo 只能回退到 PyPI tzdata 包；
# 上游 fpk 里带没带随版本波动，所以这里自己保证。
if ! command -v python3 >/dev/null 2>&1; then
  echo "::error::runner 上没有 python3，无法从 PyPI 取 tzdata wheel" >&2
  exit 1
fi

mkdir -p "$WORK_DIR/tzdata"
python3 -m pip download --quiet --no-deps --no-cache-dir --only-binary :all: \
  --dest "$WORK_DIR/tzdata" "tzdata==${TZDATA_PIN}"

TZ_WHEEL=$(ls "$WORK_DIR"/tzdata/tzdata-*.whl 2>/dev/null | head -1 || true)
if [ -z "$TZ_WHEEL" ]; then
  echo "::error::tzdata==${TZDATA_PIN} 没下到 wheel" >&2
  exit 1
fi
rm -rf "$SP"/tzdata "$SP"/tzdata-*.dist-info
if command -v unzip >/dev/null 2>&1; then
  unzip -q "$TZ_WHEEL" -d "$SP"
else
  python3 -m zipfile -e "$TZ_WHEEL" "$SP"
fi
if [ ! -f "$SP/tzdata/zoneinfo/Asia/Shanghai" ]; then
  echo "::error::tzdata 解包后没有 Asia/Shanghai（wheel 布局变了？）" >&2
  exit 1
fi
echo "==> tzdata ${TZDATA_PIN} OK"

# 顺带核对一下搬进来的这棵树确实是本次要打的版本（防止上游资产命名/内容错位）
OCTOP_DIST=$(basename "$(ls -d "$SP"/octop-*.dist-info 2>/dev/null | head -1 || true)")
if [ -n "$OCTOP_DIST" ]; then
  DIST_LC=$(printf '%s' "$OCTOP_DIST" | tr '[:upper:]' '[:lower:]')
  EXPECT_LC="octop-$(printf '%s' "$VERSION" | tr '[:upper:]' '[:lower:]').dist-info"
  if [ "$DIST_LC" != "$EXPECT_LC" ]; then
    echo "::warning::site-packages 里 octop 的 dist-info 是 ${OCTOP_DIST}，与预期 octop-${VERSION}.dist-info 不一致（上游版本号写法变了？）"
  fi
fi

# ---- 5. 组装 rootfs ---------------------------------------------------------
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS/bin"

echo "==> Installing site-packages"
cp -a "$SP" "$ROOTFS/site-packages"

echo "==> Installing Python runtime (${PBS_ASSET})"
download "https://github.com/astral-sh/python-build-standalone/releases/download/${PBS_TAG}/${PBS_ASSET//+/%2B}" \
  "$WORK_DIR/pbs.tar.gz"
ACTUAL_PBS_SHA=$(sha256_of "$WORK_DIR/pbs.tar.gz")
if [ "$ACTUAL_PBS_SHA" != "$PBS_SHA256" ]; then
  echo "::error::PBS python sha256 校验失败：期望 ${PBS_SHA256}，实际 ${ACTUAL_PBS_SHA}" >&2
  exit 1
fi
echo "==> PBS python sha256 OK"
mkdir -p "$WORK_DIR/pbs"
tar -xzf "$WORK_DIR/pbs.tar.gz" -C "$WORK_DIR/pbs"
if [ ! -x "$WORK_DIR/pbs/python/bin/python3.12" ]; then
  echo "::error::PBS 包里没有 python/bin/python3.12" >&2
  ls -la "$WORK_DIR/pbs" >&2
  exit 1
fi
mv "$WORK_DIR/pbs/python" "$ROOTFS/python"

echo "==> Installing web dashboard -> rootfs_common/www"
rm -rf "$ROOTFS_COMMON/www"
cp -a "$SP/octop/dashboard" "$ROOTFS_COMMON/www"

echo "==> Installing start.sh"
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod 0755 "$ROOTFS/bin/start.sh"

# ---- 6. 冒烟：真以目标架构跑一遍（拿不到容器就跳过，不拦发布）---------------
# 这一步测的是「装出来的这棵树在目标架构上 import 得起来 + 时区解析得出来」，
# 比 file 断言更接近真机首启。runner 上要能跑 linux/<arch> 容器，做不到就只告警。
SMOKE_PY=$(cat <<'PYEOF'
import sys
import zoneinfo

# 把系统时区库屏蔽掉，强制走 PyPI tzdata —— 真机沙箱没有 /usr/share/zoneinfo，
# 0001 就是这么崩的（ZoneInfoNotFoundError: Asia/Shanghai）
zoneinfo.reset_tzpath(to=["/nonexistent-tzpath-for-smoke"])
zoneinfo.ZoneInfo("Asia/Shanghai")

import certifi  # noqa: F401  （出站 HTTPS 的 CA 来源）
import tzdata  # noqa: F401  （上面那次解析的回退数据源）
import sqlalchemy  # noqa: F401
import fastapi  # noqa: F401
import uvicorn  # noqa: F401
import octop.cli.main  # noqa: F401  （start.sh 真正跑的入口）

print("smoke ok:", sys.version.split()[0], "tzpath:", zoneinfo.TZPATH)
PYEOF
)

run_smoke() {
  if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    echo "::warning::跳过 import 冒烟：本机/runner 没有可用的 docker"
    return 0
  fi
  local base="debian:bookworm-slim"
  if ! docker run --rm --platform "linux/${ARCH}" "$base" uname -m >/dev/null 2>&1; then
    echo "::warning::跳过 import 冒烟：无法运行 linux/${ARCH} 容器（缺 binfmt/qemu）"
    return 0
  fi
  echo "==> 冒烟：linux/${ARCH} 容器内 import + 时区解析"
  if ! docker run --rm --platform "linux/${ARCH}" \
      -v "$ROOTFS:/r:ro" \
      -e HOME=/tmp/smokehome \
      -e PYTHONPATH=/r/site-packages \
      -e PYTHONTZPATH=/nonexistent-tzpath-for-smoke \
      -e PYTHONDONTWRITEBYTECODE=1 \
      "$base" /r/python/bin/python3.12 -c "$SMOKE_PY"; then
    echo "::error::冒烟失败：目标架构下 import 或时区解析不过（这棵树装到真机上起不来）" >&2
    exit 1
  fi
}

if [ "${SKIP_SMOKE:-0}" != "1" ]; then
  run_smoke
fi

echo "==> Done: $ROOTFS"
du -sh "$ROOTFS" "$ROOTFS_COMMON/www" 2>/dev/null || true
