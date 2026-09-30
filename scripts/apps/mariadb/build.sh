#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 11.4.12> <arch: amd64|arm64>
#
# MariaDB 原生应用 = Go 管理壳 + MariaDB 官方 deb 解出的运行时。
#
#   bin/mariadb-launcher   Go 管理壳（纯标准库静态二进制；探活 HTTP 端口 + 两道
#                          鉴权闸 + 守护 mariadbd + 管理页后端，web 由 go:embed）
#   mariadb/sbin/mariadbd  数据库本体（官方 deb12 构建，按显式 loader 启动）
#   mariadb/bin/*          mariadb / mariadb-dump / mariadb-check / mariadb-upgrade
#   mariadb/lib/*.so*      【含 glibc 与 ld.so 的完整运行库闭包】—— 沙箱里没有
#                          /usr，/lib 不可依赖，闭包由 elfdeps 解析 DT_NEEDED
#                          精确得出，缺一个 .so 的表现是启动即退
#   mariadb/lib/plugin/*   服务端插件（auth_pam 系剔除）
#   mariadb/share/*        charsets / errmsg.sys / bootstrap 用的 .sql
#
# rootfs_<arch>/ 由本脚本在 CI 现场生成，不进 git。
# project.yaml 的 version 由 workflow 统一 sed，本脚本不碰它。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

# 钉死 Go 版本，配合 GOTOOLCHAIN=local 禁止 go 自己去拉工具链。
# ⚠ 不要随手升到 1.27+：平台注入的 GODEBUG=tlskyber=0 会让 1.27 编译出的
#   包启动即 fatal（it-tools 接入时真机踩过）。升级前先在真机验证。
GO_VERSION="1.26.5"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APP_DIR="$REPO_ROOT/$PROJECT_DIR"
LAUNCHER="$SCRIPT_DIR/launcher"
ROOTFS="$APP_DIR/rootfs_${ARCH}"
CACHE="$SCRIPT_DIR/.build-cache"

case "$ARCH" in
  amd64|arm64) ;;
  *) echo "Unsupported arch: $ARCH (amd64|arm64)" >&2; exit 1 ;;
esac
case "$(printf '%s' "$VERSION" | grep -cE '^[0-9]+\.[0-9]{1,2}\.[0-9]+$')" in
  1) ;;
  *) echo "版本号 $VERSION 不满足 ugcli 的格式要求（三段数字，中段最多两位）" >&2; exit 1 ;;
esac

echo "==> Building MariaDB ${VERSION} (${ARCH})"

# ---- 1. 管理页前端 ----
# 单一来源是 launcher/web/index.html：既被 go:embed 进管理壳（本地开发用），
# 又要拷进 rootfs_common/www 供 UGOS 网关提供（open_type: inner 的页面由网关
# 从 www/ serve）。rootfs_common/www 整体 gitignore，两边都由这里现场同步。
echo "==> Sync admin web into rootfs_common/www"
mkdir -p "$APP_DIR/rootfs_common/www"
cp "$LAUNCHER/web/index.html" "$APP_DIR/rootfs_common/www/index.html"
# cloudwindow.js 是 UGOS JSSDK 打成的 IIFE，页面靠它取登录认证 token；
# 漏拷的话页面能打开但所有 /api 都 401。
cp "$LAUNCHER/web/cloudwindow.js" "$APP_DIR/rootfs_common/www/cloudwindow.js"

# ---- 2. 前端静态检查 ----
# 手写 HTML 没有构建流程，这两个检查器抓"调用了未定义的函数"和"引用了页面上
# 不存在的 id"两类最常见的 bug（症状都是"点了没反应"）。都做过反向验证：
# 故意改坏一个会报错并非 0 退出。
echo "==> Static checks on admin web"
python3 "$SCRIPT_DIR/checkids.py" "$LAUNCHER/web/index.html"
node "$SCRIPT_DIR/checkjs.js" "$LAUNCHER/web/index.html"

# ---- 3. Go 工具链 ----
# 按【宿主】的 OS/架构取（目标架构由 GOARCH 决定，两者无关）。
# CI 宿主永远是 linux-amd64；探测宿主是为了脚本在 macOS 上也能直接跑。
HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac
GOROOT_DIR="$CACHE/go-${GO_VERSION}-${HOST_OS}-${HOST_ARCH}"
if [ ! -x "$GOROOT_DIR/bin/go" ]; then
  GO_TARBALL="go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
  echo "==> Installing Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
  rm -rf "$GOROOT_DIR.tmp" "$GOROOT_DIR"
  mkdir -p "$CACHE"
  curl -fL -o "$CACHE/${GO_TARBALL}" "https://go.dev/dl/${GO_TARBALL}"
  mkdir -p "$GOROOT_DIR.tmp"
  tar xzf "$CACHE/${GO_TARBALL}" -C "$GOROOT_DIR.tmp"
  # 解完原子改名：半截的工具链目录不能当成解好的（同 fetch-runtime 的规矩）
  mv "$GOROOT_DIR.tmp/go" "$GOROOT_DIR"
  rmdir "$GOROOT_DIR.tmp"
  rm -f "$CACHE/${GO_TARBALL}"
fi
export GOROOT="$GOROOT_DIR"
export PATH="$GOROOT/bin:$PATH"
go version

# ---- 4. 管理壳：测试 + 交叉编译 ----
# 测试重点是 SQL 转义那组：密码是用户输入、又要拼进 SQL 语句，转义错了会在
# 用户毫不知情的情况下留下一个密码不对的 root 账号。测试跑在宿主平台上
# （不设 GOOS），CI 宿主是 linux，正好覆盖路径处理里的平台差异。
echo "==> go vet + go test (launcher)"
(
  cd "$LAUNCHER"
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go vet ./...
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 go test ./...
)

echo "==> Cross-compiling launcher (linux/$ARCH)"
mkdir -p "$ROOTFS/bin"
(
  cd "$LAUNCHER"
  GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 \
    GOOS=linux GOARCH="$ARCH" \
    go build -trimpath -ldflags="-s -w" \
    -o "$ROOTFS/bin/mariadb-launcher" .
)
chmod +x "$ROOTFS/bin/mariadb-launcher"

# 交叉编译写错环境变量的话会打出宿主架构的包，而这种错误要装到 NAS 上才会
# 暴露（Exec format error）—— 在这里就用 ELF 头拦住。
echo "==> Verifying launcher ELF arch"
EXPECT_ELF=$([ "$ARCH" = "amd64" ] && echo x86_64 || echo aarch64)
python3 "$SCRIPT_DIR/elfdeps.py" arch "$ROOTFS/bin/mariadb-launcher" \
  | grep -q " $EXPECT_ELF " || {
    echo "::error:: 管理壳不是 $EXPECT_ELF" >&2; exit 1; }

# ---- 5. MariaDB 本体 + 运行库闭包 ----
# fetch-runtime.py 自带完整性校验（Content-Length + SHA256）和闭包自检：
# 解析不出完整闭包会直接构建失败，不会打出一个启动即退的包。
echo "==> Fetching MariaDB runtime (official deb12 packages)"
python3 "$SCRIPT_DIR/fetch-runtime.py" \
  --arch "$ARCH" \
  --mariadb-version "$VERSION" \
  --out "$ROOTFS/mariadb" \
  --cache "$CACHE/fetch"

echo
echo "==> Done: $ROOTFS"
du -sh "$ROOTFS"/* 2>/dev/null || true
