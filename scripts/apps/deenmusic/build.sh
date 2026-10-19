#!/usr/bin/env bash
# 组装「Deen 音乐下载器」的 rootfs（CI 用）。
#
# 用法：build.sh <版本号> <架构>    # 架构：arm64 / amd64
#
# 这个应用的包 = 上游 Python 源码（Flask）+ 自带的 CPython 运行时
#              + 自己写的 Go 管理壳 + 三个平台 API 服务
#              + 一个只负责"挂到 /api 前缀"的入口层（overlay/ugos_main.py）
# 除了那个入口层，【上游代码一行都没改】。
#
# 三个平台 API 服务的来源（这是本应用唯一按架构分叉的地方）：
#
#   amd64：直接用上游仓库里内置的 linux-x64 二进制（网易云/酷狗是 pkg 打包的
#          Node，QQ 音乐是 PyInstaller）。和上游在 Windows/Linux/fnOS 上跑的
#          是同一份文件。
#
#   arm64：上游这三个都只有 x86-64，没有 arm64 版本（三个上游仓库都不发）。
#          做法是把它们换成"源码 + 包内自带的 Node/CPython 运行时"，外面各套一个
#          同名 Go 替身（api-shim）把环境摆好后 exec：
#          · 网易云 api-enhanced（Node）→ service/apinode/ncm/app.js
#          · 酷狗 KuGouMusicApi（Node）→ service/apinode/kugou/app.js
#          · QQ 音乐 QQMusicApi（Python）→ service/apiqq/web/run.py
#          ⚠ 不走 pkg 单文件：pkg 5 交叉构建 arm64 时对 api-enhanced 会直接失败
#            （`--no-bytecode and no source breaks final executable`，它的
#            unblockmusic 插件有动态 require），而且 pkg 还要在构建时下载对应
#            架构的 Node 基座（慢且脆）。源码 + 自带运行时这条路可控得多。
#          文件名与 amd64 完全一致，所以上游 bridge 的启动契约
#          （exec <install>/api/<固定文件名>，用环境变量指定监听）一行都不用改。
#
# 本脚本只负责把 rootfs_<arch> 与 rootfs_common 摆好；ugcli check / pack /
# 发 Release 由可复用 workflow（reusable-build-app.yml）统一执行。
set -euo pipefail

VERSION="${1:?用法：build.sh <版本号> <架构>}"
ARCH="${2:?用法：build.sh <版本号> <架构>}"
case "$ARCH" in
  arm64|amd64) ;;
  *) echo "!! 架构只支持 arm64 / amd64，收到：$ARCH" >&2; exit 1 ;;
esac

SLASH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APPDIR="$(cd "$SLASH_DIR/../../../apps/deenmusic/com.personal.deenmusic" && pwd)"
# CI 上放 runner 临时目录（每次全新下载）；本地调试落到脚本旁的 .build-cache/。
CACHE="${RUNNER_TEMP:-$SLASH_DIR/.build-cache}"

UPSTREAM_REPO="https://github.com/chongya369/music-downloader.git"
# ⚠ 上游的 tag 是【不带 v 前缀】的纯版本号（0.7.8），别按惯例写 v$VERSION。
UPSTREAM_TAG="${VERSION}"

# CPython 运行时（python-build-standalone，与 clouddl 同一份，校验和也一致）。
PY_RELEASE="20260728"
PY_VER="3.11.15"
PY_SHA256_arm64="a6decd180099e6768269bd8e8968aeaa84bb01511f00f6921dc204fe648729aa"
PY_SHA256_amd64="39dfd79560d9b0c22ce7d16bccc171ed93537331a8c26a8aebe960301621b65e"

# 上游 requirements.txt 只给下限，这里钉死实测可用的版本（都是纯 Python 或
# 有对应架构 manylinux wheel，跨架构 pip --platform 装得下来）。
PY_REQ=(
  "Flask==3.1.3"
  "Flask-SQLAlchemy==3.1.1"
  "APScheduler==3.11.3"
  "requests==2.34.2"
  "mutagen==1.48.1"
  # ⚠ tzdata 是【必需品】，不是可选依赖（2026-10-19 真机 0001 就是漏了它）：
  #   原生沙箱把 /usr 遮掉 → 没有 /usr/share/zoneinfo；APScheduler 构造
  #   BackgroundScheduler 时会 get_localzone() → zoneinfo.ZoneInfo("Asia/Shanghai")
  #   → 系统库找不到 → 再退回 import tzdata → 也没有 →
  #   ZoneInfoNotFoundError，import app 阶段直接崩，应用无限重启。
  #   SSH 里手工直跑不会暴露（宿主有 zoneinfo），只有装成应用才会。
  "tzdata==2026.4"
)

# arm64 才用到的三个上游 API 源码项目（版本对齐上游内置二进制）
NCM_REPO="https://github.com/NeteaseCloudMusicApiEnhanced/api-enhanced.git"
NCM_TAG="v4.41.1"
KUGOU_REPO="https://github.com/MakcRe/KuGouMusicApi.git"
KUGOU_TAG="v1.5.1"
QQ_REPO="https://github.com/chongya369/QQMusicApi.git"
QQ_TAG="v0.7.2-dev.2"

# arm64 自带的 Node 运行时（跑网易云/酷狗两个 Node 版 API 服务）。
# 版本与上游 pkg 构建用的基线一致（上游内置二进制就是 node18 打出来的）。
NODE_VER="18.20.4"
NODE_SHA256_arm64="1cb5053bd4109aec41a8104ea3e9f48b95b1adef4d23bb4fc64f8c2d90c65ae3"
NODE_SHA256_amd64="592eb35c352c7c0c8c4b2ecf9c19d615e78de68c20b660eb74bd85f8c8395063"

# QQ 音乐 API 的运行时依赖（从上游 uv 导出的 requirements 里挑出真正运行需要的，
# 去掉 mkdocs/pytest/ruff 这些文档与测试工具）。
QQ_REQ=(
  "fastapi==0.140.0"
  "uvicorn==0.51.0"
  "loguru==0.7.3"
  "pydantic==2.13.4"
  "pydantic-settings==2.14.2"
  "orjson==3.11.9"
  "niquests==3.20.1"
  "paho-mqtt==2.1.0"
  "jsonpath-ng==1.8.0"
  "python-dotenv==1.2.2"
  "cryptography==49.0.0"
  "qrcode==8.2"
  "terminal-qrcode==1.1.0"
  "tomli==2.4.1"
  "xmltodict==1.0.4"
  "deepmerge==2.1.0"
  "typing-extensions==4.16.0"
  # griffe：web 的 /docs 路由用它在运行时解析 SDK 方法的文档字符串
  # （起初我把它当成"文档工具"漏掉了，真机启动直接 ModuleNotFoundError）。
  "griffe==2.1.0"
)

pbs_triple() { case "$1" in arm64) echo aarch64-unknown-linux-gnu;; amd64) echo x86_64-unknown-linux-gnu;; esac; }
elf_arch()   { case "$1" in arm64) echo aarch64;;                  amd64) echo x86-64;;                  esac; }
# pip 的 --platform 是"接受哪些 wheel 标签"，多给几个不同年代的 manylinux
platforms() {
  case "$1" in
    arm64) echo "--platform manylinux2014_aarch64 --platform manylinux_2_17_aarch64 --platform manylinux_2_28_aarch64" ;;
    amd64) echo "--platform manylinux2014_x86_64  --platform manylinux_2_17_x86_64  --platform manylinux_2_28_x86_64" ;;
  esac
}

echo "==> Deen 音乐下载器 ${VERSION}（架构：${ARCH}）"
mkdir -p "$CACHE"

sha256() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else sha256sum "$1" | awk '{print $1}'; fi
}
fetch() { # fetch <url> <目标文件> <sha256>
  local url="$1" dest="$2" want="$3"
  if [ ! -f "$dest" ]; then
    echo "    下载 $(basename "$dest")"
    curl -fL --retry 3 --retry-delay 2 -o "$dest" "$url"
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
if [ ! -d "$UP/.git" ]; then
  git clone --quiet --depth 1 --branch "$UPSTREAM_TAG" "$UPSTREAM_REPO" "$UP" || {
    echo "!! 上游不存在 tag ${UPSTREAM_TAG} —— 版本号写对了吗？" >&2
    exit 1
  }
fi
git -C "$UP" fetch --quiet --depth 1 origin "refs/tags/${UPSTREAM_TAG}:refs/tags/${UPSTREAM_TAG}" 2>/dev/null || true
git -C "$UP" checkout --quiet "$UPSTREAM_TAG"

# 上游的版本号必须和传入版本对得上，否则界面上显示的版本会和应用中心不一致。
UP_VERSION="$(tr -d ' \r\n' < "$UP/version.txt")"
if [ "$UP_VERSION" != "$VERSION" ]; then
  echo "!! 上游 version.txt=${UP_VERSION}，而传入版本是 $VERSION —— 改一个再来" >&2
  exit 1
fi

# ---- 2. 组装 rootfs_<arch> ----
dir="$APPDIR/rootfs_$ARCH"
elf="$(elf_arch "$ARCH")"
triple="$(pbs_triple "$ARCH")"
eval "py_sha=\$PY_SHA256_$ARCH"

echo "==> [$ARCH] 组装 rootfs_$ARCH"
rm -rf "$dir/service" "$dir/runtime" "$dir/bin"
mkdir -p "$dir/bin" "$dir/service" "$dir/runtime"

# a. 上游 Python 源码。目录结构必须原样保留：上游 app.py 用
#    `_ROOT = webapp 的父目录` 找 version.txt / 数据库 / api 目录。
rsync -a --exclude '__pycache__' --exclude '._*' --exclude '.DS_Store' "$UP/core/"   "$dir/service/core/"
rsync -a --exclude '__pycache__' --exclude '._*' --exclude '.DS_Store' "$UP/webapp/" "$dir/service/webapp/"
cp "$UP/version.txt" "$dir/service/version.txt"
cp "$UP/requirements.txt" "$dir/service/requirements.txt" 2>/dev/null || true

# b. 适配层：唯一一个我们自己加进上游源码目录的文件（挂 /api 前缀 + 下载目录）。
cp "$SLASH_DIR/overlay/ugos_main.py" "$dir/service/webapp/ugos_main.py"

# c. 运行时依赖：按目标架构从 PyPI 取 wheel（纯 Python 的随意，带 .so 的
#    必须是对应架构）。--only-binary 保证不会去编源码 —— 交叉编译必然失败，
#    宁可在这里报错也不要打出个装到 NAS 上才炸的包。
echo "==> [$ARCH] 安装 Python 依赖到 service/vendor"
# shellcheck disable=SC2046
python3 -m pip install --quiet --target "$dir/service/vendor" \
  --only-binary=:all: --python-version 3.11 --implementation cp --no-compile \
  $(platforms "$ARCH") "${PY_REQ[@]}"

# d. CPython 运行时 + 瘦身
py_tarball="cpython-${PY_VER}+${PY_RELEASE}-${triple}-install_only_stripped.tar.gz"
py_url="https://github.com/astral-sh/python-build-standalone/releases/download/${PY_RELEASE}/cpython-${PY_VER}%2B${PY_RELEASE}-${triple}-install_only_stripped.tar.gz"
fetch "$py_url" "$CACHE/$py_tarball" "$py_sha"
tar -xzf "$CACHE/$py_tarball" -C "$dir/runtime"    # 解出来是 runtime/python/

# 瘦身：这些在服务端一行都用不到。
# ⚠ 只删"确定不会被 import 的"。sqlite3 必须留着（上游用 SQLite 存账号与任务）。
pylib="$dir/runtime/python/lib/python3.11"
rm -rf "$pylib/test" "$pylib/idlelib" "$pylib/tkinter" "$pylib/turtledemo" \
       "$pylib/ensurepip" "$pylib/pydoc_data" "$pylib/lib2to3" \
       "$pylib"/config-3.11-* \
       "$dir/runtime/python/include" "$dir/runtime/python/share"
find "$dir/runtime/python/lib" -name 'libpython*.a' -delete
find "$pylib/lib-dynload" -name '_tkinter*' -delete

# e. Go 管理壳。纯 Go 无 cgo，零配置交叉编译；测试只跑一次（与架构无关）。
echo "==> 管理壳测试（vet + test）"
( cd "$SLASH_DIR/launcher" && go vet ./... && go test ./... ) || {
  echo "!! 管理壳测试未通过" >&2; exit 1
}
echo "==> [$ARCH] 编译管理壳 linux/$ARCH"
( cd "$SLASH_DIR/launcher" && \
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o "$dir/bin/deenmusic" . )

# ---- 3. 三个平台 API 服务 ----
mkdir -p "$dir/service/api"

# 自带的 Node 运行时（arm64 跑网易云/酷狗两个 Node 版 API 服务）
install_node_runtime() {
  local tarball="node-v${NODE_VER}-linux-arm64.tar.xz"
  local url="https://nodejs.org/dist/v${NODE_VER}/${tarball}"
  eval "node_sha=\$NODE_SHA256_$ARCH"
  fetch "$url" "$CACHE/$tarball" "$node_sha"
  tar -xJf "$CACHE/$tarball" -C "$dir/runtime"
  mv "$dir/runtime/node-v${NODE_VER}-linux-arm64" "$dir/runtime/node"
  # npm/corepack 运行时用不到（运行的是已装好的 node_modules），删掉省 20MB
  rm -rf "$dir/runtime/node/lib/node_modules/npm" \
         "$dir/runtime/node/lib/node_modules/corepack" \
         "$dir/runtime/node/bin/npm" "$dir/runtime/node/bin/npx" \
         "$dir/runtime/node/bin/corepack" \
         "$dir/runtime/node/include" "$dir/runtime/node/share"
}

build_node_api() { # build_node_api <名字> <仓库> <tag>
  local name="$1" repo="$2" tag="$3"
  local work="$CACHE/node-$name"
  echo "==> [arm64] 装配 ${name} API（源码 ${tag} + 自带 Node）"
  if [ ! -d "$work/.git" ]; then
    git clone --quiet --depth 1 --branch "$tag" "$repo" "$work" || {
      echo "!! 克隆 $repo @ $tag 失败" >&2; exit 1
    }
  fi
  git -C "$work" checkout --quiet "$tag"
  # --ignore-scripts：api-enhanced 的 prepare 是 `husky install`，而 husky 在
  # devDependencies 里（--omit=dev 不装），不忽略就会 127 直接失败。
  # 两个项目都是纯 JS，没有需要 postinstall 编译的原生模块（下面有断言盯着）。
  ( cd "$work" && npm install --omit=dev --ignore-scripts --no-audit --no-fund --loglevel=error )
  # 断言：必须是纯 JS —— 混进 x64 的 .node 就是"装到 NAS 上才炸"的典型
  if find "$work/node_modules" -name '*.node' -type f | head -1 | grep -q .; then
    echo "!! $name 的依赖里有原生模块（.node），arm64 从源码跑不成立" >&2
    find "$work/node_modules" -name '*.node' | head -5 >&2
    exit 1
  fi
  # 整棵源码树（含 node_modules）原样搬进包里；.git/.github/docs/tests 不要
  local dst="$dir/service/apinode/$name"
  rm -rf "$dst"
  mkdir -p "$dst"
  rsync -a --exclude '.git' --exclude '.github' --exclude 'docs' --exclude 'test' \
        --exclude 'tests' --exclude '.build-cache' \
        --exclude '._*' --exclude '.DS_Store' "$work/" "$dst/"
}

build_qq_python() {
  echo "==> [arm64] 装配 QQ 音乐 API（Python 源码 + 自带解释器）"
  local work="$CACHE/qqapi"
  if [ ! -d "$work/.git" ]; then
    git clone --quiet --depth 1 --branch "$QQ_TAG" "$QQ_REPO" "$work" || {
      echo "!! 克隆 $QQ_REPO @ $QQ_TAG 失败" >&2; exit 1
    }
  fi
  git -C "$work" checkout --quiet "$QQ_TAG"

  local dst="$dir/service/apiqq"
  rm -rf "$dst"
  mkdir -p "$dst/web"
  rsync -a --exclude '__pycache__' --exclude '._*' --exclude '.DS_Store' "$work/qqmusic_api/" "$dst/qqmusic_api/"
  rsync -a --exclude '__pycache__' --exclude '._*' --exclude '.DS_Store' "$work/web/" "$dst/web/"

  # shellcheck disable=SC2046
  python3 -m pip install --quiet --target "$dst/vendor" \
    --only-binary=:all: --python-version 3.11 --implementation cp --no-compile \
    $(platforms "$ARCH") "${QQ_REQ[@]}"
}

if [ "$ARCH" = "amd64" ]; then
  # 上游内置的三份 linux-x64 二进制 + QQ 的 config.toml（限流/监听配置）
  echo "==> [amd64] 使用上游内置的 API 二进制"
  for f in ncm-api-linux-x64 kugou_api_linux qqmusic-api-linux-x64; do
    if [ ! -f "$UP/api/$f" ]; then
      echo "!! 上游 api/$f 不存在 —— 上游改了内置产物的名字？" >&2
      exit 1
    fi
    cp "$UP/api/$f" "$dir/service/api/$f"
    chmod +x "$dir/service/api/$f"
  done
else
  install_node_runtime
  build_node_api "ncm"   "$NCM_REPO"   "$NCM_TAG"
  build_node_api "kugou" "$KUGOU_REPO" "$KUGOU_TAG"
  build_qq_python

  # 三个同名替身：一次编译，运行时按自己的文件名分发（见 api-shim/main.go）。
  # 上游 bridge 只会 exec 这三个写死的文件名，所以名字必须一模一样。
  echo "==> [arm64] 编译 API 替身（api-shim）"
  ( cd "$SLASH_DIR/api-shim" && \
    CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags="-s -w" \
      -o "$dir/service/api/.shim" . )
  for n in ncm-api-linux-x64 kugou_api_linux qqmusic-api-linux-x64; do
    cp "$dir/service/api/.shim" "$dir/service/api/$n"
    chmod +x "$dir/service/api/$n"
  done
  rm -f "$dir/service/api/.shim"
fi
cp "$UP/api/config.toml" "$dir/service/api/config.toml"

# ---- 4. 图标 ----
# 上游 fnOS 里那张 256 图标就是它的正式图标，直接用（换个图标用户反而认不出来）。
# ⚠ 但它是 263×256（横向多出 7px，不是正方形），而 ugcli 要求 256×256，
#   所以这里等比缩放成正圆内接的正方形，肉眼无差别。
echo "==> 图标"
ICON_SRC="$UP/fnos/app/ui/images/icon-256.png"
python3 -c "import PIL" 2>/dev/null || python3 -m pip install --quiet pillow
python3 - "$ICON_SRC" "$APPDIR/rootfs_common/icon.png" <<'PY'
import struct, sys
from PIL import Image

src, dst = sys.argv[1], sys.argv[2]
im = Image.open(src).convert("RGBA")
if im.size != (256, 256):
    # 居中裁成正方形再缩放：263×256 → 256×256 只损失 7px 边缘
    side = min(im.size)
    left = (im.width - side) // 2
    im = im.crop((left, 0, left + side, side)).resize((256, 256), Image.LANCZOS)
im.save(dst, "PNG", optimize=True)

data = open(dst, "rb").read()
assert data[:8] == b"\x89PNG\r\n\x1a\n", "图标不是 PNG"
w, h = struct.unpack(">II", data[16:24])
assert (w, h) == (256, 256), f"图标必须是 256×256，实际 {w}×{h}"
assert len(data) < 100 * 1024, f"图标必须小于 100KB，实际 {len(data)}"
print(f"    图标 {w}×{h}，{len(data)} 字节")
PY

# ---- 5. 静态跳转页（网关在 / 上 serve 它，页面本体在 /api/ 下）----
if [ ! -f "$APPDIR/rootfs_common/www/index.html" ]; then
  echo "!! 缺少 rootfs_common/www/index.html（inner 应用必须有它）" >&2
  exit 1
fi

# ---- 6. 断言：时区数据、架构、文件齐全、描述长度 ----
# 沙箱里【没有 /usr/share/zoneinfo】，APScheduler 启动就要用本地时区，
# 所以 tzdata 必须在包里并且真的能解析（0001 就是漏了它，真机无限重启）。
echo "==> 校验时区数据（tzdata）"
TZDATA_DIR="$dir/service/vendor/tzdata/zoneinfo"
if [ ! -s "$TZDATA_DIR/Asia/Shanghai" ]; then
  echo "!! vendor/tzdata 缺失或没有 Asia/Shanghai —— 装到 NAS 上会 ZoneInfoNotFoundError" >&2
  exit 1
fi
# 用构建机的 python 读【包内那份】数据：PYTHONTZPATH 置空 = 屏蔽系统 zoneinfo，
# 等价于沙箱环境（宿主 python 只负责跑，数据完全来自 vendor）。
if PYTHONTZPATH= PYTHONPATH="$dir/service/vendor" python3 - <<'PY'
import sys
try:
    from zoneinfo import ZoneInfo
except ImportError:          # python < 3.9 的构建机：跳过（CI 是 3.12）
    print("    构建机 python 无 zoneinfo，跳过解析测试")
    sys.exit(0)
try:
    tz = ZoneInfo("Asia/Shanghai")
except Exception as exc:     # noqa: BLE001
    sys.exit(f"!! 屏蔽系统时区库后仍解析不到 Asia/Shanghai（{exc}）：tzdata 没装进 vendor？")
print(f"    Asia/Shanghai 解析通过：{tz}")
PY
then :; else
  echo "!! 时区数据自检失败" >&2
  exit 1
fi

echo "==> [$ARCH] 校验 ELF 架构（应为 ${elf}）"
assert_elf() {
  file "$1" | grep -qi 'ELF 64-bit' || { echo "!! $1 不是 ELF：$(file -b "$1")" >&2; exit 1; }
  file "$1" | grep -qi "$elf"       || { echo "!! $1 架构不对：$(file -b "$1")" >&2; exit 1; }
}
assert_elf "$dir/bin/deenmusic"
assert_elf "$dir/runtime/python/bin/python3.11"
# Node 运行时只有 arm64 那份才有
if [ -x "$dir/runtime/node/bin/node" ]; then assert_elf "$dir/runtime/node/bin/node"; fi
for f in ncm-api-linux-x64 kugou_api_linux qqmusic-api-linux-x64; do
  assert_elf "$dir/service/api/$f"
done
# 运行时与依赖里【所有】.so 都过一遍，防止 tarball 或 wheel 混了别的架构进来
while IFS= read -r so; do assert_elf "$so"; done \
  < <(find "$dir/runtime/python" "$dir/service/vendor" "$dir/service/apiqq" -name '*.so*' -type f 2>/dev/null)

echo "==> 校验入口与上游文件"
# macOS 的 AppleDouble 残渣（._xxx）混进包里会直接毁掉 Node 版 API 服务：
# 上游 main.js 会把 module/ 下【所有】 *.js 逐个 require，一个二进制 ._foo.js
# 就让网易云 API 起不来（SyntaxError: Invalid or unexpected token）。
# CI 跑在 Linux 上不会出现，本机构建（rsync 前未清）会 —— 这里兜一道。
if find "$dir" -name '._*' -print -quit | grep -q .; then
  echo "!! 包里混进了 macOS 的 AppleDouble 文件（._*）：" >&2
  find "$dir" -name '._*' | head -5 >&2
  exit 1
fi
for f in "$dir/service/webapp/ugos_main.py" \
         "$dir/service/webapp/app.py" \
         "$dir/service/core" \
         "$dir/service/api/config.toml" \
         "$dir/service/version.txt"; do
  [ -e "$f" ] || { echo "!! 缺少 $f" >&2; exit 1; }
done

# i18n description 有 1000【字符】上限：`ugcli check` 不查，`pack` 才拒。
echo "==> 校验描述长度"
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

echo "==> rootfs 组装完成（pack 由 workflow 执行）"
du -sh "$dir" 2>/dev/null || true
