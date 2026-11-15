#!/usr/bin/env bash
# LibreSpeed（speedtest-go）UGOS Pro 原生应用 —— CI 组树构建脚本。
#
# 用法：build.sh <版本号> <架构>     # 架构：amd64 / arm64
#
# 产物（由可复用 workflow 随后 ugcli check / pack）：
#   apps/librespeed/com.personal.librespeed/rootfs_<arch>/bin/{speedtest-backend,launcher}
#   apps/librespeed/com.personal.librespeed/rootfs_common/{www,assets-ui,licenses,zoneinfo}
#
# 三步：
#   1. 按 tag v<版本号> 现场克隆上游源码（LGPL-3.0，断言 LICENSE 与 web/assets 在位）
#   2. CGO_ENABLED=0 交叉编译后端 speedtest-backend —— 不用上游 release 里的
#      Linux 二进制：那是 goreleaser 用 upx -9 压壳的，沙箱里自解压行为未验证
#   3. 编译自写 Go 管理壳 launcher（apps/librespeed/server/，随本仓库维护）
#
# 工具链锁 go1.26.x（与本地打包完全一致）：NAS 的 systemd 全局注入
# GODEBUG=tlskyber=0，go1.27+ 编出来的二进制启动即 fatal，go≤1.26 免疫。
# runner 自带的 Go 若不在 1.26 线，脚本会现下一份钉版工具链，稳妥且可复现。
set -euo pipefail

VERSION="${1:?用法：build.sh <版本号> <架构>}"
ARCH="${2:?用法：build.sh <版本号> <架构>}"
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "!! 架构只支持 amd64 / arm64，收到：${ARCH}" >&2; exit 1 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APP_DIR="$REPO_ROOT/$PROJECT_DIR"
UPSTREAM_SLUG="librespeed/speedtest-go"
TAG="v${VERSION}"
GO_PINNED="1.26.5"

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64" ;;
  arm64) ELF_MACHINE="aarch64" ;;
esac

# CI 用 runner 的临时目录；本机跑时落脚本旁（已在 .gitignore 里）
CACHE_ROOT="${RUNNER_TEMP:-$SCRIPT_DIR/.build-cache}"
CACHE="$CACHE_ROOT/librespeed"
mkdir -p "$CACHE"

log() { echo "[build] $*"; }
die() { echo "[build] FATAL: $*" >&2; exit 1; }

# ---------- 1. 上游源码（幂等） ----------
SRC="${LIBRESPEED_SRC_DIR:-$CACHE/src-$VERSION}"
if [ ! -d "$SRC/.git" ] || [ ! -f "$SRC/go.mod" ]; then
  log "克隆上游 ${UPSTREAM_SLUG} @ ${TAG}"
  rm -rf "$SRC"
  git clone --depth 1 --branch "$TAG" "https://github.com/${UPSTREAM_SLUG}.git" "$SRC" \
    || die "克隆 ${UPSTREAM_SLUG} ${TAG} 失败"
fi
CUR_TAG="$(git -C "$SRC" describe --tags --exact-match 2>/dev/null || true)"
[ "$CUR_TAG" = "$TAG" ] || die "上游源码 tag 不符（${CUR_TAG:-unknown} != ${TAG}）"

# 许可与前端资产在位（前端 js 从上游 web/assets 原样随包，LGPL 合规的前提）
grep -qi 'LESSER GENERAL PUBLIC LICENSE' "$SRC/LICENSE" \
  || die "上游 LICENSE 校验失败（预期 LGPL-3.0）"
for f in speedtest.js speedtest_worker.js favicon.ico; do
  [ -f "$SRC/web/assets/$f" ] || die "上游前端资产缺失：web/assets/$f"
done
log "上游源码就绪：$SRC"

# ---------- 2. Go 工具链（锁 1.26.x） ----------
go_minor() {
  # "go version go1.26.5 darwin/arm64" -> "26"
  "$1" version 2>/dev/null | sed -E 's/^go version go[0-9]+\.([0-9]+).*/\1/'
}

GO=""
if command -v go >/dev/null 2>&1; then
  CAND="$(command -v go)"
  if [ "$(go_minor "$CAND")" = "26" ]; then
    GO="$CAND"
    log "复用宿主 Go：$("$GO" version)"
  else
    log "宿主 Go 不在 1.26 线（$("$CAND" version)），改用钉版 go${GO_PINNED}"
  fi
fi

if [ -z "$GO" ]; then
  HOST_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
  case "$(uname -m)" in
    x86_64|amd64) HOST_ARCH="amd64" ;;
    arm64|aarch64) HOST_ARCH="arm64" ;;
    *) die "不支持的主机架构：$(uname -m)" ;;
  esac
  TB="go${GO_PINNED}.${HOST_OS}-${HOST_ARCH}.tar.gz"
  GOROOT_DIR="$CACHE/toolchain/go${GO_PINNED}"
  if [ ! -x "$GOROOT_DIR/bin/go" ]; then
    mkdir -p "$CACHE/toolchain"
    DL="$CACHE/toolchain/$TB"
    if [ ! -s "$DL" ]; then
      log "下载 $TB（go.dev，失败回退阿里云镜像）"
      curl -fL --retry 3 --retry-all-errors --connect-timeout 10 -o "$DL" \
          "https://go.dev/dl/$TB" \
        || curl -fL --retry 3 --retry-all-errors --connect-timeout 10 -o "$DL" \
          "https://mirrors.aliyun.com/golang/$TB" \
        || die "下载 Go 工具链失败：$TB"
    fi
    # 解包到 toolchain/ 后顶层目录名恒为 go/，改名为带版本号的目录便于多版本共存
    tar xzf "$DL" -C "$CACHE/toolchain" || die "解包 Go 工具链失败"
    rm -rf "$GOROOT_DIR"
    mv "$CACHE/toolchain/go" "$GOROOT_DIR" || die "重命名 Go 工具链目录失败"
  fi
  GO="$GOROOT_DIR/bin/go"
  log "使用钉版工具链：$("$GO" version)"
fi

[ "$(go_minor "$GO")" = "26" ] || die "工具链仍不是 1.26 线：$("$GO" version)"

export CGO_ENABLED=0
export GOTOOLCHAIN=local        # 工具链已钉死，禁止 go 自行下载别的版本
export GOTELEMETRY=off
export GOFLAGS="-trimpath"
# 缓存目录可被外部覆盖（本机调试时常指向已有 module cache，省一次全量下载）
export GOPATH="${GOPATH:-$CACHE/gopath}"
export GOMODCACHE="${GOMODCACHE:-$CACHE/gomodcache}"
export GOCACHE="${GOCACHE:-$CACHE/gocache}"

# ---------- 3. 后端（上游源码） ----------
ROOTFS="$APP_DIR/rootfs_${ARCH}"
mkdir -p "$ROOTFS/bin"
BACKEND_OUT="$ROOTFS/bin/speedtest-backend"
log "编译后端 linux/${ARCH} -> $BACKEND_OUT"
(
  cd "$SRC"
  GOOS=linux GOARCH="$ARCH" "$GO" build -mod=mod -trimpath -ldflags "-s -w" -o "$BACKEND_OUT" .
) || die "后端编译失败 linux/${ARCH}"

# ---------- 4. 管理壳 launcher（本仓库源码） ----------
LAUNCHER_OUT="$ROOTFS/bin/launcher"
log "编译 launcher linux/${ARCH}（版本戳 ${VERSION}）"
(
  cd "$REPO_ROOT/apps/librespeed/server"
  GOOS=linux GOARCH="$ARCH" "$GO" build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" -o "$LAUNCHER_OUT" .
) || die "launcher 编译失败 linux/${ARCH}"
chmod 0755 "$BACKEND_OUT" "$LAUNCHER_OUT"

# ---------- 5. 共用树 rootfs_common ----------
COMMON="$APP_DIR/rootfs_common"
mkdir -p "$COMMON"/{www,assets-ui,licenses,zoneinfo}

# www 占位（tab 直连端口，此页不被 serve；ugcli check 强制要求存在）
cat > "$COMMON/www/index.html" <<'EOF'
<!DOCTYPE html><html lang="zh-CN"><head><meta charset="UTF-8"><title>网速测试</title>
<meta http-equiv="refresh" content="0;url=/"></head>
<body><p>本应用为 tab 型，浏览器直连应用端口访问（应用中心打开即可）。</p></body></html>
EOF

# 前端：自写中文页 + 上游 LGPL 原件（speedtest.js / worker / favicon）
cp "$REPO_ROOT/apps/librespeed/assets-ui/index.html" "$COMMON/assets-ui/index.html"
cp "$SRC/web/assets/speedtest.js"        "$COMMON/assets-ui/speedtest.js"
cp "$SRC/web/assets/speedtest_worker.js" "$COMMON/assets-ui/speedtest_worker.js"
cp "$SRC/web/assets/favicon.ico"         "$COMMON/assets-ui/favicon.ico"

# 许可：LGPL-3.0 全文（取自上游 LICENSE）+ 来源说明
cp "$SRC/LICENSE" "$COMMON/licenses/LICENSE-LGPL-3.0.txt"
cat > "$COMMON/licenses/NOTICE.txt" <<EOF
LibreSpeed 绿联 UGOS Pro 打包版 —— 许可与来源说明
=====================================================
本应用包含两份 LGPL-3.0 授权的组件（GitHub License API 与各自 LICENSE 文件双实证）：

1. 后端 speedtest-go ${TAG}（github.com/${UPSTREAM_SLUG}，LGPL-3.0）
   —— LibreSpeed 官方 Go 后端，按上游源码构建（CGO_ENABLED=0）。
2. 前端测速库 speedtest.js / speedtest_worker.js（来自同仓库 web/assets，
   上游自 librespeed/speedtest 项目，Federico Dossena，文件头 "GNU LGPLv3 License"）
   —— 原样随包，未做修改；页面 index.html 为打包方自写的中文界面（同样以
   LGPL-3.0 发布）。

LGPL-3.0 全文见同目录 LICENSE-LGPL-3.0.txt。
源码：https://github.com/${UPSTREAM_SLUG}
EOF

# zoneinfo：沙箱没有 /usr/share/zoneinfo，launcher 用 TZDIR 指包内这份
if [ ! -f "$COMMON/zoneinfo/Asia/Shanghai" ]; then
  log "解包 GOROOT zoneinfo.zip"
  ZDIR="$(mktemp -d)"
  unzip -oq "$("$GO" env GOROOT)/lib/time/zoneinfo.zip" -d "$ZDIR" || die "解包 zoneinfo.zip 失败"
  cp -R "$ZDIR/." "$COMMON/zoneinfo/"
  rm -rf "$ZDIR"
fi

# 图标（入库件；ugcli 要求 256×256、<100KB）
ICON="$COMMON/icon.png"
[ -f "$ICON" ] || die "图标缺失（apps/librespeed/com.personal.librespeed/rootfs_common/icon.png）"

# ---------- 6. 断言 ----------
log "断言 rootfs_${ARCH}"
for f in "$BACKEND_OUT" "$LAUNCHER_OUT"; do
  [ -f "$f" ] || die "二进制缺失：$f"
  file "$f" | grep -q "$ELF_MACHINE" || die "$(basename "$f") 架构不是 ${ELF_MACHINE}"
  file "$f" | grep -q "statically linked" || die "$(basename "$f") 非静态链接"
done
BSZ=$(wc -c < "$BACKEND_OUT" | tr -d ' ')
LSZ=$(wc -c < "$LAUNCHER_OUT" | tr -d ' ')
[ "$BSZ" -gt 15000000 ] || die "后端体积异常（${BSZ} bytes），可能编译失败"
[ "$LSZ" -gt 3000000 ] || die "launcher 体积异常（${LSZ} bytes），可能编译失败"

for f in \
  "$COMMON/www/index.html" \
  "$COMMON/assets-ui/index.html" \
  "$COMMON/assets-ui/speedtest.js" \
  "$COMMON/assets-ui/speedtest_worker.js" \
  "$COMMON/assets-ui/favicon.ico" \
  "$COMMON/licenses/LICENSE-LGPL-3.0.txt" \
  "$COMMON/licenses/NOTICE.txt" \
  "$COMMON/zoneinfo/Asia/Shanghai" ; do
  [ -f "$f" ] || die "共用树缺文件：${f#$APP_DIR/}"
done
grep -q "telemetry_level" "$COMMON/assets-ui/index.html" || die "前端遥测开关缺失"
grep -q "IP_P_D_U" "$COMMON/assets-ui/index.html" || die "前端 test_order 缺 P（默认 IP_D_U 不跑 Ping）"

ICONSZ=$(wc -c < "$ICON" | tr -d ' ')
[ "$ICONSZ" -lt 102400 ] || die "图标超 100KB：${ICONSZ} bytes"

# 打包树内不许有软链（pack 会解引用）
if find "$COMMON" "$ROOTFS" -type l | grep -q .; then
  die "打包树存在软链"
fi

RAWKB=$(du -sk "$APP_DIR" | cut -f1)
[ "$RAWKB" -lt 204800 ] || die "打包树超 200MB（${RAWKB}KB）"

awk -v a="$ARCH" -v b="$BSZ" -v l="$LSZ" 'BEGIN { printf "[build] rootfs_%s 就绪：后端 %.1fMB / launcher %.1fMB\n", a, b/1048576, l/1048576 }'
log "打包树体积：$(du -sh "$APP_DIR" | cut -f1)"
