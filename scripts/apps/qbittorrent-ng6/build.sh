#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 5.2.3_v2.0.14> <arch: amd64|arm64>
#
# 这个 app 的上游是一个 **Docker 镜像**（superng6/qbittorrent），不是 Release 资产。
# 所以构建 = 把镜像扒开取三样东西，再编译我们自己的管理壳：
#
#   usr/local/bin/qbittorrent-nox                → rootfs_<arch>/bin/
#   usr/local/qbittorrent/defaults/qBittorrent.conf → launcher/assets/（go:embed）
#   usr/local/qbittorrent/defaults/Search/*.py      → launcher/assets/Search/
#
# 扒镜像不需要跑它：`docker create` + `docker export` 对**异架构**镜像照样可用
#（只是不能 run），所以 amd64 的 runner 能同时产出两个架构的包。
#
# qbittorrent-nox 是 static-pie 全静态二进制，沙箱里零运行时依赖 ——
# 这是整条打包路线成立的前提，下面有硬断言盯着。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

IMAGE="superng6/qbittorrent:${VERSION}"

# 钉死 Go 版本，并用 GOTOOLCHAIN=local 禁止 go 自己去拉工具链
#（网络抖动时会变成很难懂的失败）。
GO_VERSION="1.26.5"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

PROJECT_ABS="$REPO_ROOT/$PROJECT_DIR"
ROOTFS="$PROJECT_ABS/rootfs_${ARCH}"
COMMON="$PROJECT_ABS/rootfs_common"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64" ;;
  arm64) ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building qBittorrent ${VERSION} (linux/${ARCH}) from ${IMAGE}"

# ---- 1. 扒镜像 ----------------------------------------------------------
echo "==> docker pull --platform linux/${ARCH} ${IMAGE}"
docker pull --platform "linux/${ARCH}" "$IMAGE"

CID=$(docker create --platform "linux/${ARCH}" "$IMAGE")
mkdir -p "$WORK_DIR/root"
# 只解需要的那几个路径，1GB 的镜像没必要整个铺开
docker export "$CID" | tar -x -C "$WORK_DIR/root" \
  usr/local/bin/qbittorrent-nox usr/local/qbittorrent/defaults
docker rm "$CID" >/dev/null

SRC_BIN="$WORK_DIR/root/usr/local/bin/qbittorrent-nox"
SRC_DEFAULTS="$WORK_DIR/root/usr/local/qbittorrent/defaults"
[ -f "$SRC_BIN" ] || { echo "::error::镜像里没有 usr/local/bin/qbittorrent-nox，上游布局可能变了" >&2; exit 1; }
[ -f "$SRC_DEFAULTS/qBittorrent.conf" ] || { echo "::error::镜像里没有 defaults/qBittorrent.conf" >&2; exit 1; }

echo "==> Verifying qbittorrent-nox (expect Linux ${ELF_MACHINE}, statically linked)"
file "$SRC_BIN"
file "$SRC_BIN" | grep -q "$ELF_MACHINE" || {
  echo "::error::qbittorrent-nox 架构不是 ${ELF_MACHINE} —— --platform 可能没生效" >&2
  exit 1
}
# 沙箱里没有 /usr/lib，动态链接的话一个依赖都补不上，必须当场拦下来
file "$SRC_BIN" | grep -q "static" || {
  echo "::error::qbittorrent-nox 不再是静态链接 —— 需要改用依赖闭包方案，见 skill" >&2
  exit 1
}

mkdir -p "$ROOTFS/bin"
install -Dm755 "$SRC_BIN" "$ROOTFS/bin/qbittorrent-nox"

# 镜像里那份"针对国内网络调过的默认配置"和搜索插件要 go:embed 进管理壳，
# 所以必须在编译【之前】铺到 launcher/assets/。
# 仓库里提交了一份（让 go test 能离线跑），这里用镜像里的覆盖掉，
# 上游改了默认配置就自动跟上。
echo "==> Refreshing launcher/assets from image defaults"
install -Dm644 "$SRC_DEFAULTS/qBittorrent.conf" "$SCRIPT_DIR/launcher/assets/qBittorrent.conf"
rm -rf "$SCRIPT_DIR/launcher/assets/Search"
mkdir -p "$SCRIPT_DIR/launcher/assets/Search"
cp "$SRC_DEFAULTS/Search/"*.py "$SCRIPT_DIR/launcher/assets/Search/"
echo "    默认配置 $(wc -c < "$SCRIPT_DIR/launcher/assets/qBittorrent.conf" | tr -d ' ') 字节，搜索插件 $(ls "$SCRIPT_DIR/launcher/assets/Search" | wc -l | tr -d ' ') 个"

# ---- 2. Go 工具链 -------------------------------------------------------
# 按【宿主】的 OS/架构取（目标架构由下面的 GOARCH 决定，两者无关）。
# CI 上宿主永远是 linux-amd64；自动探测是为了在 macOS 上也能直接跑这个脚本
# 做本地验证，和本仓库其它 app 保持一致。
HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac
GO_TARBALL="go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
echo "==> Installing Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
curl -fL -o "$WORK_DIR/${GO_TARBALL}" "https://go.dev/dl/${GO_TARBALL}"
tar xzf "$WORK_DIR/${GO_TARBALL}" -C "$WORK_DIR"
export GOROOT="$WORK_DIR/go"
export PATH="$GOROOT/bin:$PATH"
go version

# ---- 3. 管理壳 ----------------------------------------------------------
# 复刻镜像里那套 s6 脚本（铺配置 / 更新 tracker / 拉起并守护 qbittorrent-nox），
# 外加沙箱适配（无 /tmp、无 /etc/passwd、下载目录跟安装参数走）。
echo "==> go test + cross-compile launcher"
(
  cd "$SCRIPT_DIR/launcher"
  GOWORK=off GOTOOLCHAIN=local go vet ./...
  GOWORK=off GOTOOLCHAIN=local go test ./...
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 \
    GOOS=linux GOARCH="$ARCH" \
    go build -trimpath -ldflags="-s -w" -o "$ROOTFS/bin/launcher" .
)
chmod +x "$ROOTFS/bin/launcher"

echo "==> Verifying launcher ELF (expect Linux ${ELF_MACHINE})"
file "$ROOTFS/bin/launcher"
file "$ROOTFS/bin/launcher" | grep -q "$ELF_MACHINE" || {
  echo "::error::管理壳架构不是 ${ELF_MACHINE} —— 交叉编译可能静默退回了宿主架构" >&2
  exit 1
}

# ---- 4. 自带的 IP 地理数据库 --------------------------------------------
# qBittorrent 自己会去 db-ip.com 下载，但首次启动那一刻库还不存在，执行日志里
# 会先来一条「无法加载 IP 地理数据库。原因：No such file or directory」；
# 而且这条自愈依赖能连上 db-ip.com。自带一份就一上来就是"已加载"。
# 数据许可 CC BY 4.0，署名见 rootfs_common/geoip/ATTRIBUTION.txt。
GEO_DST="$COMMON/geoip/dbip-country-lite.mmdb.gz"
GEO_MIN=$((1024 * 1024))
echo "==> Fetching DB-IP country database"
mkdir -p "$(dirname "$GEO_DST")"
GEO_OK=""
for OFFSET in 0 1; do
  # 当月的还没发布时退回上个月（GNU date；macOS 上走 -v 分支做本地验证）
  YM=$(date -u -d "-${OFFSET} month" +%Y-%m 2>/dev/null || date -u -v-${OFFSET}m +%Y-%m)
  URL="https://download.db-ip.com/free/dbip-country-lite-${YM}.mmdb.gz"
  echo "    trying ${URL}"
  if curl -fsSL -o "$WORK_DIR/geo.gz" "$URL"; then
    SIZE=$(wc -c < "$WORK_DIR/geo.gz" | tr -d ' ')
    # 下到一个错误页也是 200，所以要验 gzip 完整性和大小，别把 HTML 打进包
    if [ "$SIZE" -ge "$GEO_MIN" ] && gzip -t "$WORK_DIR/geo.gz" 2>/dev/null; then
      mv "$WORK_DIR/geo.gz" "$GEO_DST"
      GEO_OK=1
      echo "    ${SIZE} 字节（解压后 $(gzip -dc "$GEO_DST" | wc -c | tr -d ' ')）"
      break
    fi
    echo "    内容不对（${SIZE} 字节或不是合法 gzip），换一个月份"
  fi
done
if [ -z "$GEO_OK" ]; then
  # 不是致命错误：包里没有的话 qBittorrent 会自己联网下载，
  # 只是首次启动会有一条告警。但要留痕，别让它悄悄退化。
  rm -f "$GEO_DST"
  echo "::warning::没能取到 IP 地理数据库，本次打出的包不含它（首次启动会有一条「无法加载」告警）"
fi

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
