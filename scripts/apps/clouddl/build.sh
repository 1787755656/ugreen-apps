#!/usr/bin/env bash
# 组装「多网盘下载器」的 rootfs（CI 用）。
#
# 用法：build.sh <版本号> <架构>    # 架构：arm64 / amd64
#
# 这个应用的包 = 上游 Python 源码 + 上游预装依赖（换掉一个二进制轮子）
#              + 自带的 CPython 运行时 + 自己写的 Go 管理壳 + 上游前端。
# 除了 ugos_main.py 那个入口层，【上游代码一行都没改】。
#
# 和其它 app 一样，本脚本只负责把 rootfs_<arch> 与 rootfs_common 摆好；
# ugcli check / pack / 发 Release 由可复用 workflow（reusable-build-app.yml）
# 统一执行。上游源码按 tag v<版本号> 取，上游 APP_VERSION 必须与传入版本一致。
set -euo pipefail

VERSION="${1:?用法：build.sh <版本号> <架构>}"
ARCH="${2:?用法：build.sh <版本号> <架构>}"
case "$ARCH" in
  arm64|amd64) ;;
  *) echo "!! 架构只支持 arm64 / amd64，收到：$ARCH" >&2; exit 1 ;;
esac

SLASH_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APPDIR="$SLASH_DIR/../../../apps/clouddl/com.personal.clouddl"   # 仓库根 = 脚本目录上三级
APPDIR="$(cd "$APPDIR" && pwd)"
# CI 上放 runner 临时目录（每次全新下载）；本地调试落到脚本旁的 .build-cache/。
CACHE="${RUNNER_TEMP:-$SLASH_DIR/.build-cache}"

UPSTREAM_REPO="https://github.com/xiaocheng154/nas-cloud-downloader.git"
UPSTREAM_TAG="v${VERSION}"   # 上游 tag 带 v 前缀

# CPython 运行时（python-build-standalone，同一个 release 的两个架构）。
# arm64 那份和上游 build_arm64_fpk.py 里钉的是同一个文件，校验和也一致。
PY_RELEASE="20260728"
PY_VER="3.11.15"
PY_SHA256_arm64="a6decd180099e6768269bd8e8968aeaa84bb01511f00f6921dc204fe648729aa"
PY_SHA256_amd64="39dfd79560d9b0c22ce7d16bccc171ed93537331a8c26a8aebe960301621b65e"

# pydantic_core 是上游依赖里【唯一】的二进制扩展，所以只有它需要按架构换轮子。
# 这个假设由下面 assemble() 里那条“vendor 里不许残留别的架构产物”的断言守着；
# 版本是否还和上游 vendor 一致，由 assemble() 里的 PC 版本断言守着 ——
# 上游哪天升级了 pydantic，这里会当场报错提醒换 PC_VER。
PC_VER="2.46.4"
PC_WHEEL_arm64="pydantic_core-${PC_VER}-cp311-cp311-manylinux_2_17_aarch64.manylinux2014_aarch64.whl"
PC_URL_arm64="https://files.pythonhosted.org/packages/43/3a/41114a9f7569b84b4d84e7a018c57c56347dac30c0d4a872946ec4e36c46/${PC_WHEEL_arm64}"
PC_SHA256_arm64="7bfb192b3f4b9e8a89b6277b6ce787564f62cfd272055f6e685726b111dc7826"
PC_WHEEL_amd64="pydantic_core-${PC_VER}-cp311-cp311-manylinux_2_17_x86_64.manylinux2014_x86_64.whl"
PC_URL_amd64="https://files.pythonhosted.org/packages/80/50/540cd3aeefc041beb111125c4bff779831a2111fc6b15a9138cda277d32c/${PC_WHEEL_amd64}"
PC_SHA256_amd64="f9fa868638bf362d3d138ea55829cefb3d5f4b0d7f142234382a15e2485dbec4"

# 绿联的架构名 → python-build-standalone 的三元组名 / file(1) 输出里的架构串
py_triple() { case "$1" in arm64) echo aarch64-unknown-linux-gnu;; amd64) echo x86_64-unknown-linux-gnu;; esac; }
elf_arch()  { case "$1" in arm64) echo aarch64;;                  amd64) echo x86-64;;                  esac; }

echo "==> 多网盘下载器 ${VERSION}（架构：${ARCH}）"

mkdir -p "$CACHE"

# ---- 0. 校验和工具 ----
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
if [ ! -d "$UP/.git" ]; then
  git clone --quiet --depth 1 --branch "$UPSTREAM_TAG" "$UPSTREAM_REPO" "$UP" || {
    echo "!! 上游不存在 tag ${UPSTREAM_TAG} —— 版本号写对了吗？" >&2
    exit 1
  }
fi
git -C "$UP" checkout --quiet "$UPSTREAM_TAG"

# 上游的版本号必须和传入版本对得上，否则界面上显示的版本会和应用中心不一致。
UP_VERSION="$(grep -E '^APP_VERSION' "$UP/app/service/src/app.py" | head -1 | cut -d'"' -f2)"
if [ "$UP_VERSION" != "$VERSION" ]; then
  echo "!! 上游 APP_VERSION=${UP_VERSION}，而传入版本是 $VERSION —— 改一个再来" >&2
  exit 1
fi

# ---- 2. 按架构组装 rootfs_<arch> ----
# 管理壳 + CPython 运行时 + 上游源码 + 依赖。上游源码是纯 Python、两个架构
# 一模一样，本可以放进 rootfs_common，但 common 和 arch 目录同名子目录的
# 合并行为没验证过 —— 各放一份最省心，代价只是每个 upk 多十几 MB。
dir="$APPDIR/rootfs_$ARCH"
elf="$(elf_arch "$ARCH")"
triple="$(py_triple "$ARCH")"
py_tarball="cpython-${PY_VER}+${PY_RELEASE}-${triple}-install_only_stripped.tar.gz"
py_url="https://github.com/astral-sh/python-build-standalone/releases/download/${PY_RELEASE}/cpython-${PY_VER}%2B${PY_RELEASE}-${triple}-install_only_stripped.tar.gz"
eval "py_sha=\$PY_SHA256_$ARCH"
eval "pc_wheel=\$PC_WHEEL_$ARCH"
eval "pc_url=\$PC_URL_$ARCH"
eval "pc_sha=\$PC_SHA256_$ARCH"

echo "==> [$ARCH] 取 CPython 运行时和 pydantic_core 轮子"
fetch "$py_url" "$CACHE/$py_tarball" "$py_sha"
fetch "$pc_url" "$CACHE/$pc_wheel" "$pc_sha"

echo "==> [$ARCH] 组装 rootfs_$ARCH"
rm -rf "$dir/service" "$dir/runtime"
mkdir -p "$dir/bin" "$dir/service/src" "$dir/service/vendor"

# a. 上游 Python 源码。start.sh 是飞牛那边的启动脚本（`exec python3 /app/app.py`），
#    在绿联这边由管理壳负责启动，带进来只会误导人。
for f in "$UP"/app/service/src/*.py "$UP"/app/service/src/requirements.txt; do
  cp "$f" "$dir/service/src/"
done
cp -R "$UP/app/service/src/static" "$dir/service/src/static"

# b. 适配层：唯一一个我们自己加进上游源码目录的文件。
cp "$SLASH_DIR/overlay/ugos_main.py" "$dir/service/src/ugos_main.py"

# c. 上游预装依赖，然后把 pydantic_core 换成本架构的轮子。
#    ⚠ 上游 vendor/ 里只有 pydantic_core 一个二进制扩展，其余全是纯 Python；
#       这条假设由下面的断言守着，将来上游加了新的原生依赖会当场失败。
cp -R "$UP"/app/service/vendor/. "$dir/service/vendor/"
rm -rf "$dir/service/vendor/bin"   # 只是几个 console script，运行时用不到
UP_PC_VER="$(ls -d "$dir/service/vendor"/pydantic_core-*.dist-info 2>/dev/null | sed -E 's/.*pydantic_core-([0-9.]+)\.dist-info/\1/' | head -1 || true)"
if [ -n "$UP_PC_VER" ] && [ "$UP_PC_VER" != "$PC_VER" ]; then
  echo "!! 上游 vendor 里的 pydantic_core 是 ${UP_PC_VER}，脚本钉的是 ${PC_VER}。" >&2
  echo "   先把脚本顶部的 PC_VER 与轮子 URL/校验和对齐再构建。" >&2
  exit 1
fi
rm -rf "$dir/service/vendor/pydantic_core" "$dir"/service/vendor/pydantic_core-*.dist-info
( cd "$dir/service/vendor" && unzip -q "$CACHE/$pc_wheel" )

# d. CPython 运行时。
mkdir -p "$dir/runtime"
tar -xzf "$CACHE/$py_tarball" -C "$dir/runtime"    # 解出来是 runtime/python/

# 瘦身：这些在服务端一行都用不到，留着白白让 upk 大几十 MB。
#      ⚠ 只删“确定不会被 import 的”，别顺手删 distutils/sqlite3 之类
#        —— 上游哪天间接用上了，表现是运行时 ImportError，本地根本测不出来。
pylib="$dir/runtime/python/lib/python3.11"
rm -rf "$pylib/test" "$pylib/idlelib" "$pylib/tkinter" "$pylib/turtledemo" \
       "$pylib/ensurepip" "$pylib/pydoc_data" "$pylib/lib2to3" \
       "$pylib"/config-3.11-* \
       "$dir/runtime/python/include" "$dir/runtime/python/share"
find "$dir/runtime/python/lib" -name 'libpython*.a' -delete
find "$pylib/lib-dynload" -name '_tkinter*' -delete

# e. Go 管理壳。纯 Go 无 cgo，零配置交叉编译；测试一起跑。
#    vet/test 只跑一次（跟架构无关），放编译前 —— 测试红了就别浪费编译时间。
echo "==> 管理壳测试（vet + test）"
( cd "$SLASH_DIR/launcher" && go vet ./... && go test ./... )
echo "==> [$ARCH] 编译管理壳 linux/$ARCH"
( cd "$SLASH_DIR/launcher" && \
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o "$dir/bin/clouddl" . )

# f. 认证适配层 + JSSDK 也要放一份到上游自己 serve 的 static 里
#    （生产环境走不到，因为管理壳只反代 /api/；放齐是免得留下坏引用）。
cp "$SLASH_DIR/overlay/ugos-auth.js" "$SLASH_DIR/overlay/cloudwindow.js" "$dir/service/src/static/"

# g. 文案本地化（上游是给飞牛写的）+ 把 app.js 的加载改由认证适配层接管。
python3 "$SLASH_DIR/patch-ui.py" \
  "$dir/service/src/static/index.html" \
  "$dir/service/src/onboarding.py" \
  "$dir/service/src/alipan.py" >/dev/null

# h. 架构断言。打错架构的表现是“装到 NAS 上才炸”，
#    而 upk 里是什么架构从外面完全看不出来 —— 必须在这里拦住。
echo "==> [$ARCH] 校验 ELF 架构（应为 ${elf}）"
assert_elf() {
  file "$1" | grep -qi 'ELF 64-bit' || { echo "!! $1 不是 ELF" >&2; exit 1; }
  file "$1" | grep -qi "$elf"       || { echo "!! $1 架构不对：$(file -b "$1")" >&2; exit 1; }
}
assert_elf "$dir/bin/clouddl"
assert_elf "$dir/runtime/python/bin/python3.11"
assert_elf "$(find "$dir/service/vendor/pydantic_core" -name '*.so' | head -1)"
# 运行时和依赖里【所有】.so 都过一遍，防止 tarball 或轮子混了别的架构进来
while IFS= read -r so; do assert_elf "$so"; done \
  < <(find "$dir/runtime/python" "$dir/service/vendor" -name '*.so*' -type f)

# ---- 3. 前端（rootfs_common，两架构 runner 各自完整生成一遍） ----
# inner 应用的页面由网关直接从 www/ 提供（只有 /api/ 才反代到应用端口），
# 所以上游那套静态文件要原样搬到 www/ 下，且【路径必须保持 /static/xxx】——
# index.html 里引用的就是绝对路径 /static/app.js。
echo "==> 同步前端到 rootfs_common/www"
rm -rf "$APPDIR/rootfs_common/www"
mkdir -p "$APPDIR/rootfs_common/www"
cp -R "$UP/app/service/src/static" "$APPDIR/rootfs_common/www/static"
cp "$UP/app/service/src/static/index.html" "$APPDIR/rootfs_common/www/index.html"

# UGOS 登录认证适配层 + 绿联 JSSDK（都是本移植版新增的，上游没有）。
# 没有这两个文件，网关不会给后端注入用户身份 —— 界面会一直弹“未通过登录认证”。
cp "$SLASH_DIR/overlay/ugos-auth.js" "$SLASH_DIR/overlay/cloudwindow.js" \
   "$APPDIR/rootfs_common/www/static/"

# ---- 3b. 界面文案本地化 ----
# 上游是给飞牛 fnOS 写的，界面上有“下载到你的 fnOS”“在 fnOS 文件管理器中打开”
# 和 /vol1/... 的路径示例 —— 在绿联上原样显示会误导用户（尤其是路径示例）。
# 只替换文案，逻辑一行不动；每条都断言必须命中，上游改了话术就当场失败。
echo "==> 界面文案本地化"
python3 "$SLASH_DIR/patch-ui.py" \
  "$APPDIR/rootfs_common/www/index.html" \
  "$APPDIR/rootfs_common/www/static/index.html"

# ---- 4. 图标 ----
# 直接用上游的 256×256 图标（署名许可证允许，且换个图标反而让用户认不出来）。
cp "$UP/ICON_256.PNG" "$APPDIR/rootfs_common/icon.png"
python3 - "$APPDIR/rootfs_common/icon.png" <<'PY'
import struct, sys, os
path = sys.argv[1]
data = open(path, "rb").read()
assert data[:8] == b"\x89PNG\r\n\x1a\n", "图标不是 PNG"
w, h = struct.unpack(">II", data[16:24])
assert (w, h) == (256, 256), f"图标必须是 256×256，实际 {w}×{h}"
assert len(data) < 100 * 1024, f"图标必须小于 100KB，实际 {len(data)}"
print(f"    图标 {w}×{h}，{len(data)} 字节")
PY

# ---- 5. i18n 描述长度 ----
# ⚠ i18n 的 description 有 1000【字符】上限（按字符不按字节，所以中文能写得更长）。
#   `ugcli check` 不查这个、`ugcli pack` 才拒 —— 在这里先拦一次，
#   等到 workflow 里 pack 才发现就要白跑一遍全流程。
echo "==> 校验描述长度"
python3 - "$APPDIR/project.yaml" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8").read()
# 不引入 PyYAML：只把每个语言块里 description 的折叠标量抠出来数长度
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
