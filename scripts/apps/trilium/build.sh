#!/usr/bin/env bash
# Trilium Notes UGOS Pro 原生应用的组装/打包脚本。
#
# 用法:
#   build.sh <上游版本> <arch>        # 只组装 rootfs（CI 接口，reusable-build-app.yml 调用）
#   build.sh <上游版本> all [构建号]   # 本地一条龙：两架构组装 + ugcli check + pack
#
# 上游: https://github.com/TriliumNext/Trilium 的 Server 独立包
#       TriliumNotes-Server-v<ver>-linux-{arm64,x64}.tar.xz
#       （自带 Node 运行时 + 已构建前端 + better-sqlite3 prebuilds，见 assemble() 注释）
#
# 环境变量:
#   GITHUB_MIRROR  下载加速前缀，默认 https://gh-proxy.com/（CI 上不需要）
#   TRILIUM_SRC    已解压的上游目录（含 TriliumNotes-Server-<ver>-linux-{arm64,x64}），
#                  给了就不再下载，本地重复打包很快
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# 本仓库（ugreen-apps）里脚本在 scripts/apps/trilium/，独立工程里在 scripts/ —— 两种都兼容
if [ -d "$SCRIPT_DIR/../../../apps/trilium" ]; then
  REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
  APP_DIR="$REPO_ROOT/apps/trilium/com.personal.trilium"
  CACHE_DIR="$SCRIPT_DIR/.build-cache"
else
  REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
  APP_DIR="$REPO_ROOT/com.personal.trilium"
  CACHE_DIR="$REPO_ROOT/.cache"
fi
MIRROR="${GITHUB_MIRROR:-https://gh-proxy.com/}"

VER="${1:?usage: build.sh <上游版本> <arch|all> [构建号]}"
MODE="${2:?usage: build.sh <上游版本> <arch|all> [构建号]}"
UPSTREAM_REPO="TriliumNext/Trilium"

# ugcli 的 version 校验：中段最多两位数。上游是 0.x.y，按「去掉前导 0. 再补 .0」
# 确定性映射：0.106.0 → 106.0.0（上游小版本递增时首段单调递增，不会回退）。
map_project_version() {
  python3 - "$1" <<'PY'
import sys
v = sys.argv[1].lstrip("v")
parts = v.split(".")
if parts[0] == "0" and len(parts) >= 2:
    parts = parts[1:] + ["0"]
else:
    parts = parts + ["0"] * (3 - len(parts))
if len(parts[1]) > 2:
    sys.exit(f"上游版本 {v} 的中段超过两位数，ugcli 不接受，需要人工处理")
print(".".join(parts[:3]))
PY
}

plat_of() { case "$1" in arm64) echo arm64 ;; amd64) echo x64 ;; esac; }

need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少依赖: $1" >&2; exit 1; }; }

# 下载（带镜像兜底）并解压上游 Server 包 → 打印解压目录
fetch_src() { # fetch_src <arch: amd64|arm64>
  local arch="$1" tarch name dir url
  tarch=$(plat_of "$arch")
  name="TriliumNotes-Server-v${VER}-linux-${tarch}.tar.xz"
  dir="$CACHE_DIR/TriliumNotes-Server-${VER}-linux-${tarch}"

  if [ -n "${TRILIUM_SRC:-}" ]; then
    src="$TRILIUM_SRC/TriliumNotes-Server-${VER}-linux-${tarch}"
    [ -d "$src" ] || { echo "TRILIUM_SRC 里没有 $src" >&2; exit 1; }
    echo "$src"; return
  fi

  if [ ! -d "$dir" ]; then
    mkdir -p "$CACHE_DIR"
    [ -f "$CACHE_DIR/$name" ] || {
      url="https://github.com/${UPSTREAM_REPO}/releases/download/v${VER}/${name}"
      echo "==> 下载 $name" >&2
      curl -fL --retry 3 --connect-timeout 20 -o "$CACHE_DIR/$name.part" "$url" \
        || curl -fL --retry 3 --connect-timeout 20 -o "$CACHE_DIR/$name.part" "$MIRROR$url"
      mv "$CACHE_DIR/$name.part" "$CACHE_DIR/$name"
    }
    echo "==> 解压 $name" >&2
    tar -xJf "$CACHE_DIR/$name" -C "$CACHE_DIR"
  fi
  echo "$dir"
}

# 组装单个架构：rootfs_common/app（共享）+ rootfs_<arch>（Node 与 sqlite 原生模块）
assemble() { # assemble <arch: amd64|arm64>
  local arch="$1" src tarch
  src=$(fetch_src "$arch")
  tarch=$(plat_of "$arch")

  # 共享部分：上游除 node/ 之外的全部（node/ 里 90% 是 npm 与头文件，用不上）。
  # prebuilds 单独放 rootfs_<arch>（见下），这里排除掉以免 --delete 或覆盖打架。
  rsync -a --delete --exclude='/node/' --exclude='/node_modules/better-sqlite3/prebuilds/' \
    "$src/" "$APP_DIR/rootfs_common/app/"

  # 运行时：上游自带的那份 Node，必须与 better-sqlite3 的 ABI 对齐，不要换版本
  mkdir -p "$APP_DIR/rootfs_${arch}/bin"
  cp "$src/node/bin/node" "$APP_DIR/rootfs_${arch}/bin/node"
  chmod 0755 "$APP_DIR/rootfs_${arch}/bin/node"

  # better-sqlite3 在运行时按 process.platform-arch 挑 prebuilds/<target>.node。
  # 每个架构只放自己那份（common 与 arch 目录树不重叠，不依赖 ugcli 的覆盖顺序）；
  # musl 变体用不上（沙箱是 Debian glibc），不打包。
  local p="$APP_DIR/rootfs_${arch}/app/node_modules/better-sqlite3/prebuilds"
  mkdir -p "$p"
  cp "$src/node_modules/better-sqlite3/prebuilds/linux-${tarch}.node" "$p/"

  echo "==> $arch: node $(ls -lh "$APP_DIR/rootfs_${arch}/bin/node" | awk '{print $5}'), sqlite $(basename "$p"/*.node)"
}

case "$MODE" in
  amd64|arm64)
    need rsync; need curl; need tar; need file
    assemble "$MODE"
    ;;

  all)
    PROJ_VER=$(map_project_version "$VER")
    echo "== 上游 $VER → 包版本 $PROJ_VER"
    for arch in arm64 amd64; do
      assemble "$arch"
    done
    echo "   app $(du -sh "$APP_DIR/rootfs_common/app" | cut -f1)"

    # 版本号写回 project.yaml（CI 上由 reusable workflow 做，这里供本地一条龙）
    python3 - "$APP_DIR/project.yaml" "$PROJ_VER" <<'PY'
import re, sys
p, ver = sys.argv[1], sys.argv[2]
s = open(p, encoding="utf-8").read()
s = re.sub(r"(?m)^version: .*$", f"version: {ver}", s, count=1)
open(p, "w", encoding="utf-8").write(s)
PY

    BUILD="${3:-}"
    if [ -z "$BUILD" ]; then
      last=$(ls "$APP_DIR/build_dir/pkgs/upk" 2>/dev/null \
        | sed -nE 's/.*_[0-9.]+\.([0-9]{4})\.upk$/\1/p' | sort -n | tail -1)
      BUILD=$(( 10#${last:-0} + 1 ))
    fi

    need ugcli
    cd "$APP_DIR"
    ugcli check
    ugcli pack --arch all --build "$BUILD"

    FULL="${PROJ_VER}.$(printf '%04d' "$BUILD")"
    for arch in arm64 amd64; do
      f=$(find "$APP_DIR/build_dir/pkgs/upk" -name "${arch}_nasync_com.personal.trilium_${FULL}.upk")
      [ -n "$f" ] && cp "$f" "$REPO_ROOT/" && echo "→ $REPO_ROOT/$(basename "$f")"
    done
    ;;

  *)
    echo "未知模式: $MODE（应为 amd64 | arm64 | all）" >&2
    exit 1
    ;;
esac
