#!/bin/bash
# 组装 rootfs_<arch>：引擎 + 静态 ffmpeg + Go 管理壳。
#
# 用法：build.sh <上游版本，如 0.6.0-beta> <arch: amd64|arm64>
#
# 约定（与 monorepo 其它应用一致）：只准备 rootfs，不调 ugcli、不算构建号、
# 不写 build_dir —— 这些由 reusable-build-app.yml 统一做。
#
# 前端 www 是直接入库的（apps/<slug>/<app_id>/rootfs_common/www），
# 不在这里重新生成：它是从 Youngxj/N_m3u8DL-RE-FN 的上游 www 经
# scripts/patch-ui.py 打过补丁、并换掉 fnOS 宿主 SDK 的成品，入库才能保证
# 每次构建出来的前端完全一致。改前端就改那份文件 + 手动改 patch-ui.py。
set -euo pipefail

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

PROJECT_DIR_ABS="$REPO_ROOT/$PROJECT_DIR"
ROOTFS="$PROJECT_DIR_ABS/rootfs_${ARCH}"
COMMON="$PROJECT_DIR_ABS/rootfs_common"
WWW="$COMMON/www"
CACHE="${NRE_BUILD_CACHE:-$SCRIPT_DIR/.build-cache}"
mkdir -p "$CACHE" "$ROOTFS/bin"

# 上游 tag 带预发布后缀（v0.6.0-beta），资产名里还带日期，无法拼 URL，
# 所以一律走 API 查一次 release 列表再取资产直链。
UPSTREAM_TAG="v${VERSION}"

# 静态 ffmpeg：绿联沙箱里 /usr/lib 不存在，发行版那种动态链的 ffmpeg 依赖闭包
# 凑不齐；用 johnvansickle 的全静态构建（与 zhiyinmusic 同一个来源与校验和）。
FFMPEG_VER="7.0.2"
FFMPEG_SHA256_arm64="f4149bb2b0784e30e99bdda85471c9b5930d3402014e934a5098b41d0f7201b1"
FFMPEG_SHA256_amd64="abda8d77ce8309141f83ab8edf0596834087c52467f6badf376a6a2a4c87cf67"

case "$ARCH" in
  amd64) NRE_ARCH="x64";   FF_ARCH="amd64"; ELF_MACHINE="x86-64" ;;
  arm64) NRE_ARCH="arm64"; FF_ARCH="arm64"; ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac
eval "FF_SHA=\$FFMPEG_SHA256_${ARCH}"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

fetch() { # fetch <url> <out> <sha256|""> [extra curl args...]
  local url="$1" out="$2" want="$3"
  shift 3
  if [ -f "$out" ]; then
    if [ -z "$want" ] || [ "$(sha256_of "$out")" = "$want" ]; then
      echo "    复用缓存 $out"
      return 0
    fi
    rm -f "$out"
  fi
  echo "    GET $url"
  curl -fL --retry 3 --retry-all-errors "$@" -o "$out" "$url"
  if [ -n "$want" ]; then
    local got
    got="$(sha256_of "$out")"
    [ "$got" = "$want" ] || { echo "::error::sha256 mismatch for $url (want $want, got $got)" >&2; exit 1; }
  fi
}

echo "==> nm3u8dlre ${VERSION} (linux/${ARCH})"

# ---- 1. 前端成品断言 -------------------------------------------------------
for f in index.html app.js style.css vendor/trim-app.js vendor/bootstrap.min.css; do
  [ -f "$WWW/$f" ] || { echo "::error::前端缺少 $WWW/$f（www 应随仓库提交）" >&2; exit 1; }
done
# app.js 必须已经改成 /api 前缀，否则 ipk 里会是一份调不通的前端
grep -q "const API = (params) => '/api/?" "$WWW/app.js" || {
  echo "::error::www/app.js 未被 patch-ui.py 改造（API 前缀不是 /api/?）" >&2; exit 1; }
# 前端资源指纹：网关对静态资源带长缓存，改了 www 里的 js/css 一定要同步改
# index.html 的 ?v=（当前 0.6.0.1）。改 UI 时三处一起改，否则用户会"看不到更新"。
grep -q 'app.js?v=0.6.0.1' "$WWW/index.html" || echo "::warning::index.html 的 app.js?v= 不是 0.6.0.1，确认是否忘了改指纹"
# 网关的 gzip_static 会把陈旧 .gz 钉死，www 里绝不能留
if ls "$WWW"/*.gz >/dev/null 2>&1; then
  echo "::error::www/ 里存在 .gz 文件（会盖住新前端，见踩坑记录）" >&2; exit 1
fi

# ---- 2. 引擎（N_m3u8DL-RE 官方单文件二进制）--------------------------------
ENGINE_TAR="$CACHE/nre-${VERSION}-${NRE_ARCH}.tar.gz"
if [ ! -f "$ENGINE_TAR" ]; then
  echo "==> 解析引擎资产 ${UPSTREAM_TAG} ${NRE_ARCH}"
  ASSET_JSON="$(curl -fsSL --retry 3 \
    -H "Accept: application/vnd.github+json" \
    -H "User-Agent: ugreen-apps-build" \
    ${GH_TOKEN:+-H "Authorization: Bearer $GH_TOKEN"} \
    "https://api.github.com/repos/nilaoda/N_m3u8DL-RE/releases?per_page=30")"
  read -r ASSET_ID ASSET_NAME < <(printf '%s' "$ASSET_JSON" | python3 -c '
import json,sys
tag, arch = sys.argv[1], sys.argv[2]
rels = json.load(sys.stdin)
rel = next((r for r in rels if r.get("tag_name") == tag), None)
if rel is None:
    sys.exit("找不到上游 release: " + tag)
for a in rel.get("assets", []):
    n = a["name"]
    if n.startswith("N_m3u8DL-RE_") and ("_linux-%s_" % arch) in n and n.endswith(".tar.gz"):
        print(a["id"], n)
        break
else:
    sys.exit("release %s 里没有 linux-%s 资产" % (tag, arch))
' "$UPSTREAM_TAG" "$NRE_ARCH")
  [ -n "${ASSET_ID:-}" ] || { echo "::error::未解析到引擎资产" >&2; exit 1; }
  echo "    $ASSET_NAME"
  # 走 API 资产端点（比 browser_download_url 更稳，不受 302 到 objects. 的影响）
  fetch "https://api.github.com/repos/nilaoda/N_m3u8DL-RE/releases/assets/${ASSET_ID}" \
        "$ENGINE_TAR" "" -H "Accept: application/octet-stream"
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

tar xzf "$ENGINE_TAR" -C "$WORK_DIR"
ENGINE_SRC="$(find "$WORK_DIR" -type f -name 'N_m3u8DL-RE' | head -1)"
[ -n "$ENGINE_SRC" ] || { echo "::error::$ENGINE_TAR 里没有 N_m3u8DL-RE" >&2; exit 1; }
install -m 0755 "$ENGINE_SRC" "$ROOTFS/bin/N_m3u8DL-RE"
# 架构断言：arm64 包在 amd64 runner 上跑不了，只能看 ELF 头
file "$ROOTFS/bin/N_m3u8DL-RE" | grep -q "$ELF_MACHINE" || {
  echo "::error::引擎 ELF 架构不符（期望 $ELF_MACHINE）: $(file "$ROOTFS/bin/N_m3u8DL-RE")" >&2; exit 1; }

# ---- 3. ffmpeg（混流用，静态）---------------------------------------------
# CI 走 johnvansickle 全静态构建（沙箱里 /lib 只读挂了 libc，动态版也能跑，
# 但静态版零依赖最稳）。本地离线/慢网时可以用 NRE_FFMPEG_BIN 指定已有的
# ffmpeg 可执行文件跳过下载 —— 只影响本地产物，CI 始终走下面这条。
if [ -n "${NRE_FFMPEG_BIN:-}" ]; then
  echo "==> 使用本地 ffmpeg: $NRE_FFMPEG_BIN"
  install -m 0755 "$NRE_FFMPEG_BIN" "$ROOTFS/bin/ffmpeg"
else
  FF_TAR="$CACHE/ffmpeg-${FFMPEG_VER}-${FF_ARCH}-static.tar.xz"
  fetch "https://johnvansickle.com/ffmpeg/releases/ffmpeg-${FFMPEG_VER}-${FF_ARCH}-static.tar.xz" \
        "$FF_TAR" "$FF_SHA"
  mkdir -p "$WORK_DIR/ff"
  tar xJf "$FF_TAR" -C "$WORK_DIR/ff" --strip-components=1 \
    "ffmpeg-${FFMPEG_VER}-${FF_ARCH}-static/ffmpeg"
  install -m 0755 "$WORK_DIR/ff/ffmpeg" "$ROOTFS/bin/ffmpeg"
fi
file "$ROOTFS/bin/ffmpeg" | grep -q "$ELF_MACHINE" || {
  echo "::error::ffmpeg ELF 架构不符" >&2; exit 1; }

# ---- 4. Go 管理壳 ---------------------------------------------------------
# 工具链：本机有够新的 go 就直接用；CI（ubuntu-latest）没有就用钉死版本下
# 一份 —— 与 zhiyinmusic/mariadb 的做法一致，避免"runner 上没 go"这种偶发失败。
GO_VERSION="1.26.5"
if command -v go >/dev/null 2>&1 && go version 2>/dev/null | grep -qE 'go1\.(2[2-9]|[3-9][0-9])(\.|$)'; then
  echo "==> 使用本机 Go: $(go version)"
else
  HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in
    x86_64|amd64) HOST_ARCH="amd64" ;;
    arm64|aarch64) HOST_ARCH="arm64" ;;
    *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
  esac
  echo "==> 下载 Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
  fetch "https://go.dev/dl/go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz" \
        "$CACHE/go${GO_VERSION}-${HOST_OS}-${HOST_ARCH}.tar.gz" ""
  mkdir -p "$WORK_DIR/goroot"
  tar xzf "$CACHE/go${GO_VERSION}-${HOST_OS}-${HOST_ARCH}.tar.gz" -C "$WORK_DIR/goroot" --strip-components=1
  export GOROOT="$WORK_DIR/goroot"
  export PATH="$GOROOT/bin:$PATH"
  go version
fi

export GOTOOLCHAIN=local
export GOWORK=off
export GOFLAGS="-trimpath"

# workflow 不会单独跑 go test，在这里跑（progress_test.go 的用例取自真机日志）。
# ⚠ 必须在设 GOOS/GOARCH 之前跑：测试二进制要在【宿主】上是可执行的，
#   否则 macOS/amd64 机器上会 "exec format error"。
echo "==> go test (launcher, host)"
( cd "$SCRIPT_DIR/launcher" && go test ./... )

export CGO_ENABLED=0
export GOOS=linux
export GOARCH="$ARCH"

echo "==> go build (launcher, linux/${ARCH})"
( cd "$SCRIPT_DIR/launcher" && go build \
    -ldflags "-s -w -X main.engineVersion=${VERSION#v}" \
    -o "$ROOTFS/bin/nm3u8dlre" . )
file "$ROOTFS/bin/nm3u8dlre" | grep -q "$ELF_MACHINE" || {
  echo "::error::管理壳 ELF 架构不符" >&2; exit 1; }

echo "==> 完成 $ROOTFS"
ls -la "$ROOTFS/bin"
