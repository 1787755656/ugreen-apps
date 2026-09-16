#!/usr/bin/env bash
set -euo pipefail

# 组装「100解压」的 rootfs（CI 用）。
#
# 用法：build.sh <版本号，如 0.5.20> <架构>    # 架构：arm64 / amd64
#
# 这个应用的包 = 上游 Go 服务源码交叉编译 + 官方 7-Zip 26.03 静态引擎（双架构）
#              + 自写 Go 管理壳 + 上游前端（拷贝 + 认证适配层 + 文案 patch）。
# 【上游业务代码一行不改】：所有沙箱/认证适配在管理壳与前端桥接层里。
#
# 和其它 app 一样，本脚本只负责把 rootfs_<arch> 与 rootfs_common 摆好；
# ugcli check / pack / 发 Release 由可复用 workflow（reusable-build-app.yml）
# 统一执行。上游源码按 tag v<版本号> 取，manifest 的 version 必须与传入版本一致。
set -euo pipefail

VERSION="${1:?用法：build.sh <版本号> <架构>}"
ARCH="${2:?用法：build.sh <版本号> <架构>}"
case "$ARCH" in
  arm64|amd64) ;;
  *) echo "!! 架构只支持 arm64 / amd64，收到：$ARCH" >&2; exit 1 ;;
esac

SLASH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APPDIR="$SLASH_DIR/../../../apps/100zip/com.personal.100zip"
APPDIR="$(cd "$APPDIR" && pwd)"
CACHE="${RUNNER_TEMP:-$SLASH_DIR/.build-cache}"

UPSTREAM_REPO="github.com/Alicace/100zip"
UPSTREAM_TAG="v${VERSION}"

# 上游 go.mod 要求 go 1.25，钉 1.26.6（与 litepan/dnet 同一份工具链，GOTOOLCHAIN=local
# 禁止 go 自拉工具链——网络抖动会变成难懂的失败）。
GO_VERSION="1.26.6"

# 7-Zip 引擎：官方静态版，双架构 sha256 钉死。x64 那份与上游 vendor.lock.json
# 里的值相同（上游 vendor 只带 linux-x64；arm64 是我们补的官方构建）。
# 上游哪天 bump 引擎版本，下面 vendor.lock 断言会当场拦下提醒换这里。
ENGINE_VER="26.03"
ENGINE_URLVER="2603"   # 7-zip.org 的文件名不带点：7z2603-linux-x64.tar.xz
ENGINE_SHA256_x64="eab4c8d7f193e3d6d3237370bbcaa879a160a3f1dc82202207e27baeab79b6ac"
ENGINE_SHA256_arm64="277907bc627633ec344757fe47699856cbb6e37f75cbc310d37d62cfacdd73b2"

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64"; ENGINE_PLAT="linux-x64" ;;
  arm64) ELF_MACHINE="aarch64"; ENGINE_PLAT="linux-arm64" ;;
esac

echo "==> 100解压 ${VERSION}（架构：${ARCH}）"

mkdir -p "$CACHE"

sha256() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else sha256sum "$1" | awk '{print $1}'; fi
}
fetch() { # fetch <url> <目标文件> <sha256>
  local url="$1" dest="$2" want="$3"
  if [ ! -f "$dest" ]; then
    echo "    下载 $(basename "$dest")"
    curl -fL --retry 3 -o "$dest" "$url"
  fi
  local got; got="$(sha256 "$dest")"
  if [ "$got" != "$want" ]; then
    echo "!! $(basename "$dest") 校验和不符" >&2
    echo "   期望 $want" >&2
    echo "   实际 $got" >&2
    exit 1
  fi
}

# ---- 1. 上游源码 @ tag ----
echo "==> 取上游源码 @ ${UPSTREAM_TAG}"
UP="$CACHE/upstream"
if [ ! -d "$UP" ]; then
  mkdir -p "$UP"
  curl -fL -o "$CACHE/src.tar.gz" "https://${UPSTREAM_REPO}/archive/refs/tags/${UPSTREAM_TAG}.tar.gz"
  tar xzf "$CACHE/src.tar.gz" -C "$UP" --strip-components=1
fi
SRC_VERSION="$(sed -n 's/^version[[:space:]]*=[[:space:]]*//p' "$UP/manifest" | head -1 | tr -d '[:space:]')"
if [ "$SRC_VERSION" != "$VERSION" ]; then
  echo "::error::上游 manifest version ($SRC_VERSION) != 传入版本 ($VERSION)" >&2
  exit 1
fi

# 引擎版本守卫：上游 bump 引擎时，我们钉的 sha 就过时了，当场拦下。
LOCK_VER="$(python3 -c "import json;print(json.load(open('$UP/vendor.lock.json'))['7zip']['version'])" 2>/dev/null || echo '?')"
if [ "$LOCK_VER" != "$ENGINE_VER" ]; then
  echo "::error::上游 vendor.lock.json 的 7-Zip 版本是 $LOCK_VER，本脚本钉的是 $ENGINE_VER" >&2
  echo "        请到 7-zip.org 下载新版的 linux-x64/linux-arm64 tar.xz，更新 ENGINE_VER 与两个 sha256" >&2
  exit 1
fi

# ---- 2. Go 工具链 ----
# 本地调试可用 GO_BIN=/path/to/go 跳过下载（CI 上必须走钉死的工具链）。
HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac
if [ -n "${GO_BIN:-}" ]; then
  echo "==> Using GO_BIN=$GO_BIN（本地调试模式，跳过工具链下载）"
  export PATH="$(dirname "$GO_BIN"):"$PATH
else
  GO_TARBALL="go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
  echo "==> Installing Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
  curl -fsSL -o "$CACHE/${GO_TARBALL}" "https://go.dev/dl/${GO_TARBALL}"
  mkdir -p "$CACHE/go-toolchain" && tar xzf "$CACHE/${GO_TARBALL}" -C "$CACHE/go-toolchain"
  export GOROOT="$CACHE/go-toolchain/go"
  export PATH="$GOROOT/bin:$PATH"
fi
go version

# ---- 3. 压缩引擎（本架构）----
ROOTFS="$APPDIR/rootfs_${ARCH}"
ENGINE_DIR="$ROOTFS/bin/vendor/7zip/${ENGINE_PLAT}"
mkdir -p "$ENGINE_DIR" "$ROOTFS/bin"
# 钉的是【解出的 7zzs 二进制】的 sha256（x64 与上游 vendor.lock.json 同源），
# tar.xz 本身不钉 —— HTTPS 传输 + 二进制校验和已覆盖完整性。
ENGINE_TARBALL="$CACHE/7z${ENGINE_URLVER}-${ENGINE_PLAT}.tar.xz"
if [ ! -f "$ENGINE_TARBALL" ]; then
  curl -fsSL --retry 3 -o "$ENGINE_TARBALL" "https://www.7-zip.org/a/7z${ENGINE_URLVER}-${ENGINE_PLAT}.tar.xz"
fi
tar xJf "$ENGINE_TARBALL" -C "$CACHE" 7zzs License.txt History.txt
GOT="$(sha256 "$CACHE/7zzs")"
WANT="$( [ "$ARCH" = amd64 ] && echo "$ENGINE_SHA256_x64" || echo "$ENGINE_SHA256_arm64" )"
if [ "$GOT" != "$WANT" ]; then
  echo "!! 7zzs ($ENGINE_PLAT) sha256 不符" >&2
  echo "   期望 $WANT" >&2
  echo "   实际 $GOT" >&2
  exit 1
fi
cp "$CACHE/7zzs" "$ENGINE_DIR/7zzs"
cp "$CACHE/License.txt" "$ENGINE_DIR/License.txt"
cp "$CACHE/History.txt" "$ENGINE_DIR/History.txt"
chmod 0755 "$ENGINE_DIR/7zzs"

# ---- 4. 上游服务 + 管理壳交叉编译 ----
echo "==> go build（上游 100zip + 管理壳 zip100-launcher，linux/${ARCH}）"
(
  cd "$UP"
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 \
    GOOS=linux GOARCH="$ARCH" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "$ROOTFS/bin/100zip" ./server
)
(
  cd "$SLASH_DIR/launcher"
  echo "==> launcher vet + test"
  GOWORK=off GOTOOLCHAIN=local go vet ./...
  GOWORK=off GOTOOLCHAIN=local go test ./...
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 \
    GOOS=linux GOARCH="$ARCH" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "$ROOTFS/bin/zip100-launcher" .
)
chmod 0755 "$ROOTFS/bin/100zip" "$ROOTFS/bin/zip100-launcher"
file "$ROOTFS/bin/100zip" | grep -q "$ELF_MACHINE" || {
  echo "::error::100zip 产物架构不是 ${ELF_MACHINE}" >&2; exit 1
}
file "$ROOTFS/bin/zip100-launcher" | grep -q "$ELF_MACHINE" || {
  echo "::error::launcher 产物架构不是 ${ELF_MACHINE}" >&2; exit 1
}
file "$ENGINE_DIR/7zzs" | grep -q "$ELF_MACHINE" || {
  echo "::error::7zzs 引擎架构不是 ${ELF_MACHINE}" >&2; exit 1
}

# ---- 5. 前端（rootfs_common/www，两架构共用，幂等）----
# inner 应用的页面由网关直接从 www/ 提供（只有 /api/ 才反代到管理壳端口）。
# 上游前端是原生 HTML/JS（相对路径引用），目录层级原样搬；桥接层整体替换。
echo "==> 组装前端 www"
WWW="$APPDIR/rootfs_common/www"
rm -rf "$WWW"
mkdir -p "$WWW"
cp -R "$UP/app/www/." "$WWW/"   # /. 结尾：BSD/GNU cp 语义统一为"拷内容"
# 桥接层整体替换：飞牛 SDK 桥在绿联上没有宿主，pickUserFile 会永远挂起。
cp "$SLASH_DIR/overlay/fnos-bridge.js" "$WWW/js/fnos-bridge.js"
cp "$SLASH_DIR/overlay/ugos-auth.js" "$WWW/js/ugos-auth.js"
cp "$SLASH_DIR/overlay/cloudwindow.js" "$WWW/js/cloudwindow.js"
rm -rf "$WWW/js/vendor"
grep -q "ugos-selfdrawn" "$WWW/js/fnos-bridge.js" || {
  echo "::error::桥接层替换未生效" >&2; exit 1
}

# 界面文案本地化：每条断言必须命中，上游改了话术当场失败。
echo "==> 界面文案本地化 + 认证加载接管"
python3 "$SLASH_DIR/patch-ui.py" "$WWW/index.html" "$WWW/js/app.js" "$WWW/probe.html"

# 缓存破解：资源 URL 带版本+内容 hash（网关静态服务无 cache-control，
# 浏览器启发式缓存会在升级后继续发旧 JS —— 真机踩过）。
WWW_VER="${VERSION}-$(cat "$SLASH_DIR/overlay/fnos-bridge.js" "$SLASH_DIR/overlay/ugos-auth.js" \
  "$SLASH_DIR/overlay/cloudwindow.js" "$SLASH_DIR/patch-ui.py" | sha256sum | cut -c1-10)"
perl -pi -e "s/__WWW_VER__/$WWW_VER/g" "$WWW/index.html" "$WWW/js/ugos-auth.js"
if grep -q "__WWW_VER__" "$WWW/index.html" "$WWW/js/ugos-auth.js"; then
  echo "::error::版本号替换未完成" >&2; exit 1
fi

# 桥接层假 DOM 冒烟（离线；上游哪天破坏了我们的加载契约会当场报）
node "$SLASH_DIR/check-bridge.js"

# ---- 6. 图标与描述断言 ----
ICON="$APPDIR/rootfs_common/icon.png"
python3 - "$ICON" <<'PY'
import struct, sys
data = open(sys.argv[1], "rb").read()
assert data[:8] == b"\x89PNG\r\n\x1a\n", "图标不是 PNG"
w, h = struct.unpack(">II", data[16:24])
assert (w, h) == (256, 256), f"图标必须是 256×256，实际 {w}×{h}"
assert len(data) < 100 * 1024, f"图标必须小于 100KB，实际 {len(data)}"
print(f"    图标 {w}×{h}，{len(data)} 字节")
PY

python3 - "$APPDIR/project.yaml" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8").read()
for lang in ("zh-CN", "en-US"):
    m = re.search(rf"^  {lang}:\n(.*?)(?=^  \S|\Z)", text, re.S | re.M)
    if not m:
        sys.exit(f"!! project.yaml 里找不到 {lang} 的 i18n 块")
    d = re.search(r"^    description: >-\n(.*?)(?=^    \S)", m.group(1), re.S | re.M)
    if not d:
        sys.exit(f"!! {lang} 没有 description")
    body = " ".join(line.strip() for line in d.group(1).splitlines() if line.strip())
    print(f"    {lang} description {len(body)} 字符")
    if len(body) > 1000:
        sys.exit(f"!! {lang} 的 description 超过 1000 字符（{len(body)}），pack 会拒")
PY

echo "==> rootfs 组装完成（check/pack 由 workflow 执行）"
