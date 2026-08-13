#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 1.1.1> <arch: amd64|arm64>
#
# Magicmail = Go(Fiber + GORM + 纯 Go SQLite) 后端 + Vue3/Vite PWA 前端，
# 前端由 //go:embed all:dist 打进二进制，最终产物就是【一个】静态可执行文件。
#
# 和 litepan 那个 Go 应用的关键差别：**上游没有把前端构建产物提交进仓库**，
# server/embedfs/dist 必须在这里现构建出来。而 go:embed 嵌一个空目录【不会报错】，
# 会静默打出一个前端为空的包 —— 所以下面对 dist 有硬断言。
#
# 交叉编译零配置成立的前提是纯 Go：SQLite 用的是 glebarez/sqlite（无 cgo）。
#
# 沙箱适配不打 patch、不 fork：把 static/ugos_*.go 拷进上游 server/ 一起编译，
# 一个只设环境变量的 init()（端口 / 数据目录 / 工作目录 / TMPDIR）。
# 详见 static/ugos_env.go 里的注释。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

# 钉死 Go 版本，配合 GOTOOLCHAIN=local 禁止 go 自己去拉工具链
# （网络抖动会变成很难懂的失败）。升级时确认 >= 上游 go.mod 的要求（当前 1.25.0）。
GO_VERSION="1.26.5"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) ELF_MACHINE="x86-64" ;;
  arm64) ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building Magicmail ${VERSION} (linux/${ARCH})"

# ---- 1. 上游源码（按 tag 拉 tarball，可复现）----
SRC_URL="https://github.com/magiccode1412/magicmail/archive/refs/tags/v${VERSION}.tar.gz"
echo "==> Downloading source: ${SRC_URL}"
curl -fL -o "$WORK_DIR/src.tar.gz" "$SRC_URL"
mkdir -p "$WORK_DIR/src"
tar xzf "$WORK_DIR/src.tar.gz" -C "$WORK_DIR/src" --strip-components=1
SRC="$WORK_DIR/src"

# 复核：tag 里的 version.json 应该和解析出来的版本对得上
SRC_VERSION=$(jq -r '.latest // empty' "$SRC/version.json" 2>/dev/null | sed -E 's/^v//')
if [ -n "$SRC_VERSION" ] && [ "$SRC_VERSION" != "$VERSION" ]; then
  echo "::warning::tag v${VERSION} 里的 version.json 写的是 ${SRC_VERSION}"
fi

# ---- 2. 前端（vite build，跑在 CI 宿主架构上，产物与架构无关）----
echo "==> Building web frontend"
(
  cd "$SRC/web"
  # 上游同时提供 pnpm-lock.yaml 和 package-lock.json。优先 pnpm（上游 Dockerfile
  # 用的就是它），拿不到就退回 npm —— 两条路后面都有同一道产物断言兜底。
  if corepack enable pnpm >/dev/null 2>&1 || command -v pnpm >/dev/null 2>&1; then
    # pnpm ≥10 默认拦截依赖的 install 脚本，esbuild 的原生二进制不会落地，
    # 表现是 vite build 直接报 "You installed esbuild for another platform"。
    # 显式 rebuild 把它补回来。
    pnpm install --frozen-lockfile --ignore-scripts || pnpm install --ignore-scripts
    pnpm rebuild esbuild vue-demi
  else
    npm install --no-audit --no-fund
    npm rebuild esbuild || true
  fi
  npx vite build
)

# go:embed 嵌空目录不报错，会静默打出前端为空的包 —— 必须在这里拦住
[ -f "$SRC/server/dist/index.html" ] || {
  echo "::error::前端构建产物缺失 server/dist/index.html —— vite 构建失败或 outDir 变了" >&2
  exit 1
}
echo "==> Placing frontend at the go:embed path (server/embedfs/dist)"
rm -rf "$SRC/server/embedfs/dist"
cp -R "$SRC/server/dist" "$SRC/server/embedfs/dist"
[ -f "$SRC/server/embedfs/dist/index.html" ] || {
  echo "::error::server/embedfs/dist/index.html 缺失" >&2
  exit 1
}

# ---- 3. Go 工具链 ----
# 按【宿主】的 OS/架构取（目标架构由下面的 GOARCH 决定，两者无关）。
# CI 上宿主永远是 linux-amd64；探测宿主是为了这个脚本在 macOS 上也能直接跑，
# 本地验证不用等 CI —— 本仓库其它 app 的 build.sh 都是这个约定。
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

# ---- 4. UGOS 适配层 ----
echo "==> Applying UGOS overlay"
cp "$SCRIPT_DIR/static/ugos_env.go"       "$SRC/server/ugos_env.go"
cp "$SCRIPT_DIR/static/ugos_port.go"      "$SRC/server/ugos_port.go"
cp "$SCRIPT_DIR/static/ugos_port_test.go" "$SRC/server/ugos_port_test.go"

# 端口解析坏掉的表现是应用起在上游默认的 8080，而 project.yaml 声明的是 23232，
# 应用中心会一直显示"未启动"且日志里看不出原因 —— 值得在这儿钉一道。
# 测试跑在宿主平台上（不设 GOOS），CI 宿主是 linux，正好把 init() 也编进去。
echo "==> go test (overlay)"
(
  cd "$SRC/server"
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go test -run TestPortFromArgs .
)

# ---- 5. 交叉编译 ----
echo "==> go build (CGO off)"
mkdir -p "$ROOTFS/bin"
(
  cd "$SRC/server"
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 \
    GOOS=linux GOARCH="$ARCH" \
    go build -trimpath -ldflags="-s -w -X main.isProduction=true" \
    -o "$ROOTFS/bin/magicmail" .
)
chmod +x "$ROOTFS/bin/magicmail"

echo "==> Verifying ELF (expect Linux ${ELF_MACHINE}, static)"
file "$ROOTFS/bin/magicmail"
file "$ROOTFS/bin/magicmail" | grep -q "$ELF_MACHINE" || {
  echo "::error::产物架构不是 ${ELF_MACHINE} —— 交叉编译可能静默退回了宿主架构" >&2
  exit 1
}
file "$ROOTFS/bin/magicmail" | grep -q "statically linked" || {
  echo "::error::产物不是静态链接 —— CGO 可能被打开了，沙箱里会缺动态库" >&2
  exit 1
}

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
