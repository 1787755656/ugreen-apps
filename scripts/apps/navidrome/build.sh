#!/bin/bash
# ============================================================
# Navidrome UGOS Pro 原生应用构建脚本
# 用法: ./scripts/build.sh [版本号] [构建号]
#   默认: 版本取 project.yaml，构建号 1
# 产物: dist/{amd64,arm64}_nasync_com.personal.navidrome_<ver>.<build>.upk
# 依赖: ~/bin/ugcli, rsync, curl, shasum, ~/Desktop/绿联开发/jellyfin-ugreen-app（ffmpeg 源）
# ============================================================
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$(pwd)"
APP_ID="com.personal.navidrome"
APPDIR="$ROOT/$APP_ID"
UP_VER="$(python3 -c "import yaml;print(yaml.safe_load(open('$APPDIR/project.yaml'))['version'])" 2>/dev/null || grep -m1 '^version:' "$APPDIR/project.yaml" | awk '{print $2}')"
VER="${1:-$UP_VER}"
BUILD="${2:-1}"
ND_TAG="v${VER}"
GH_PROXY="https://gh-proxy.com/https://github.com/navidrome/navidrome/releases/download/${ND_TAG}"

echo "== Navidrome ${VER} build ${BUILD} =="

# 1. 下载上游双架构发行包 + 校验和验证（走 gh-proxy，直连 GitHub 极慢）
mkdir -p "$ROOT/downloads"
cd "$ROOT/downloads"
for ARCH in amd64 arm64; do
    F="navidrome_${VER}_linux_${ARCH}.tar.gz"
    [ -f "$F" ] || curl -sL --retry 3 -o "$F" "${GH_PROXY}/$F"
done
curl -sL --retry 3 -o checksums.txt "${GH_PROXY}/navidrome_checksums.txt" || true
if [ -s checksums.txt ]; then
    grep "linux_amd64.tar.gz\|linux_arm64.tar.gz" checksums.txt | sed "s/navidrome_${VER}_linux_amd64.tar.gz/nd_amd64.tar.gz/; s/navidrome_${VER}_linux_arm64.tar.gz/nd_arm64.tar.gz/" > /tmp/nd_verify.txt
    cp navidrome_${VER}_linux_amd64.tar.gz nd_amd64.tar.gz; cp navidrome_${VER}_linux_arm64.tar.gz nd_arm64.tar.gz
    shasum -a 256 -c /tmp/nd_verify.txt
fi

# 2. 解包 + 静态链接断言
rm -rf ex && mkdir -p ex/amd64 ex/arm64
tar xzf nd_amd64.tar.gz -C ex/amd64; tar xzf nd_arm64.tar.gz -C ex/arm64
file ex/amd64/navidrome | grep -q "statically linked" || { echo "amd64 非静态链接"; exit 1; }
file ex/arm64/navidrome | grep -q "statically linked" || { echo "arm64 非静态链接"; exit 1; }

# 3. 组装 rootfs
FULL_TMP="${VER}.$(printf '%04d' "$((10#${BUILD}))"))"
for ARCH in amd64 arm64; do
    D="$APPDIR/rootfs_${ARCH}"
    mkdir -p "$D/bin" "$D/ffmpeg"
    cp "ex/${ARCH}/navidrome" "$D/bin/navidrome"
    chmod +x "$D/bin/navidrome"
    [ -f "$D/bin/start.sh" ] || { echo "缺 bin/start.sh（手工维护件，不放 build 里覆盖）"; exit 1; }
    # launcher（Go 管理壳：反代 + /app/* SPA fallback + 首启自愈 + 信号转发）
    (cd "$APPDIR/launcher" && CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath \
        -ldflags "-s -w -X main.version=${FULL_TMP}" -o "../rootfs_${ARCH}/bin/launcher" .) || exit 1
    cp "$ROOT/../jellyfin-ugreen-app/com.jellyfin.server/rootfs_${ARCH}/ffmpeg/ffmpeg"  "$D/ffmpeg/ffmpeg"
    cp "$ROOT/../jellyfin-ugreen-app/com.jellyfin.server/rootfs_${ARCH}/ffmpeg/ffprobe" "$D/ffmpeg/ffprobe"
    chmod +x "$D/ffmpeg/ffmpeg" "$D/ffmpeg/ffprobe"
done

# 4. check + pack（串行！共享 build_dir 并行会冲突；--build 十进制不要前导零）
cd "$APPDIR"
~/bin/ugcli check
~/bin/ugcli pack --arch amd64 --build "$BUILD"
~/bin/ugcli pack --arch arm64 --build "$BUILD"

# 5. 按完整构建号搬运产物（别按版本号通配，会捞到旧包）
FULL="${VER}.$(printf '%04d' "$((10#${BUILD}))"))"
mkdir -p "$ROOT/dist"
rm -f "$ROOT"/dist/*_"${APP_ID}"_*.upk
find build_dir/pkgs/upk -name "*_${APP_ID}_${FULL}.upk" -exec cp {} "$ROOT/dist/" \;
ls -la "$ROOT/dist/"
echo "== 完成：dist/ 下 ${FULL} =="
