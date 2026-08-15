#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 0.8.1> <arch: amd64|arm64>
#
# 知音音乐（Zhiyin Music）—— 上游是 Rust 单二进制 + 自带前端 + Subsonic API，
# 【只发 Docker 镜像】（GitHub 仓库里没有源码）。所以"取上游"这一步是
# **按 manifest digest 把镜像层拉下来解开**，只取真正需要的那几个文件。
#
# 和本仓库其它 app 最大的不同：上游二进制是【动态链接】的（libtag / libstdc++），
# 而绿联沙箱里没有 /usr/lib —— 所以那几个 .so 也得从镜像里一并解出来随包带走。
# 另外上游会 spawn ffmpeg（转码）和 curl（STRM 网盘直链），沙箱里 /usr/bin
# 字面不存在，这两个也要随包带。
#
# 详细的设计取舍见 ~/Desktop/绿联开发/zhiyin-music-ugreen-app/README.md
# （那份是本地迭代用的副本，**这里是权威副本**）。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

IMAGE_REPO="qwex333/zhiyin-music"

# ---- ffmpeg / ffprobe ----------------------------------------------------
# 上游用它做转码（多档音质）和 strm 远程探测。查找顺序是
# PATH → /usr/bin/ffmpeg → /usr/local/bin/ffmpeg，而沙箱里后两个目录字面不存在，
# 所以必须随包带一份并把包内 bin 排在 PATH 最前（见 launcher/env.go）。
# 用全静态构建：沙箱的 /usr/lib 不存在，发行版那种动态链接的 ffmpeg
# 依赖闭包很难凑齐。代价是两个二进制各 50MB。
FFMPEG_VER="7.0.2"
FFMPEG_SHA256_arm64="f4149bb2b0784e30e99bdda85471c9b5930d3402014e934a5098b41d0f7201b1"
FFMPEG_SHA256_amd64="abda8d77ce8309141f83ab8edf0596834087c52467f6badf376a6a2a4c87cf67"

# ---- curl ----------------------------------------------------------------
# ⚠ 不是可选项：上游的 STRM（网盘直链）功能**是 spawn 系统 curl 来转发的**
#   （proxy 模式 `curl -4` 流式转发，redirect 模式也要先用 curl 探 IPv6 连通性）。
#   不带的话 .strm 歌曲一律 "Failed to spawn curl: remote_unavailable"，
#   而且要到运行时才暴露。musl 全静态构建，零依赖。
CURL_VER="8.21.0"
CURL_SHA256_arm64="d3f10502a9c6ead9bc3763fde3d12467db03661a263e11fec2ef2edc70e98e9f"
CURL_SHA256_amd64="e955f211202ded2536164588331acfc987dc4b7857efa3577717b1ffeab22029"

# 管理壳是纯 Go 无第三方依赖，钉死工具链版本，并用 GOTOOLCHAIN=local
# 禁止 go 自己去拉（网络抖动会变成很难懂的失败）。
GO_VERSION="1.26.5"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  # ⚠ Debian 的多架构目录名（gnu 三元组）和 dpkg 架构名不是一回事：
  #   基础库在 usr/lib/aarch64-linux-gnu，而上游 Dockerfile 装 taglib 时用的是
  #   `dpkg --print-architecture` 的输出，落在 usr/lib/arm64-linux-gnu。
  #   两个目录都要取。
  arm64) TRIPLE="aarch64-linux-gnu"; DPKGARCH="arm64"; FF_ARCH="arm64"; CURL_ARCH="aarch64"; ELF_MACHINE="aarch64" ;;
  amd64) TRIPLE="x86_64-linux-gnu";  DPKGARCH="amd64"; FF_ARCH="amd64"; CURL_ARCH="x86_64";  ELF_MACHINE="x86-64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac
eval "FF_SHA=\$FFMPEG_SHA256_${ARCH}"
eval "CURL_SHA=\$CURL_SHA256_${ARCH}"

echo "==> Building Zhiyin Music ${VERSION} (linux/${ARCH})"

fetch() { # fetch <url> <out> <sha256>
  local url="$1" out="$2" want="$3" got
  curl -fL --retry 3 --retry-all-errors -o "$out" "$url"
  got=$(sha256sum "$out" | cut -d' ' -f1)
  [ "$got" = "$want" ] || { echo "::error::sha256 mismatch for $url (want $want, got $got)" >&2; exit 1; }
}

rm -rf "$ROOTFS"
mkdir -p "$ROOTFS/bin" "$ROOTFS/lib" "$ROOTFS/app"

# =========================================================================
# 1. 上游镜像：按 tag 查出【本架构的 manifest digest】，再按 digest 拉层
# =========================================================================
# 刻意不把 digest 写死在脚本里：这个 app 要跟着上游自动出版本，写死就等于
# 每次上游发新版都要手改脚本。安全性没有降低 —— manifest 里每个 layer 的
# digest 都会被 extract-image.py 逐个复核，digest 本身是内容寻址的。
echo "==> Resolving manifest digest for ${IMAGE_REPO}:${VERSION} (${ARCH})"
TOKEN=$(curl -fsSL "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${IMAGE_REPO}:pull" | jq -r .token)
MANIFEST_DIGEST=$(curl -fsSL \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json" \
  "https://registry-1.docker.io/v2/${IMAGE_REPO}/manifests/${VERSION}" \
  | jq -r --arg a "$ARCH" '.manifests[] | select(.platform.os=="linux" and .platform.architecture==$a) | .digest' | head -1)
[ -n "$MANIFEST_DIGEST" ] || {
  echo "::error::镜像 ${IMAGE_REPO}:${VERSION} 里没有 linux/${ARCH} 的 manifest" >&2
  exit 1
}
echo "    ${MANIFEST_DIGEST}"

UP="$WORK_DIR/upstream"
python3 "$SCRIPT_DIR/extract-image.py" "$IMAGE_REPO" "$MANIFEST_DIGEST" "$WORK_DIR/blobs" "$UP" \
  usr/local/bin/zhiyin-music \
  app/web/ \
  app/config.toml \
  app/releases/releases.json \
  "usr/lib/${DPKGARCH}-linux-gnu/libtag" \
  "usr/lib/${TRIPLE}/libstdc++.so.6." \
  "usr/lib/${TRIPLE}/libgcc_s.so.1" \
  "usr/lib/${TRIPLE}/libz.so.1."

[ -f "$UP/usr/local/bin/zhiyin-music" ] || {
  echo "::error::镜像里没解出主程序 —— 上游的目录结构可能变了" >&2; exit 1; }

# ---- 上游主程序 + 前端 + 配置模板 + 更新日志 ----
install -m 0755 "$UP/usr/local/bin/zhiyin-music" "$ROOTFS/bin/zhiyin-music"
cp -R "$UP/app/web" "$ROOTFS/app/web"
# 镜像里的 /app/config.toml 就是上游的 config.toml.example（带完整中文注释）。
# 管理壳首次启动按它生成用户的配置，之后只改受管的那几个键（见 launcher/config.go）。
install -m 0644 "$UP/app/config.toml" "$ROOTFS/app/config.toml.default"
# 应用内"更新日志"那一页的数据。上游从【相对 cwd 的】releases/releases.json 读，
# 而我们的 cwd 是数据目录，所以由管理壳每次启动拷过去（见 launcher/paths.go）。
install -m 0644 "$UP/app/releases/releases.json" "$ROOTFS/app/releases.json"

# ---- 随包的共享库 ----
# 主程序是动态链接的：libtag / libtag_c 在镜像里装在 /usr/lib 下，而沙箱里
# 【没有 /usr/lib】；libstdc++ / libgcc_s / libz 大概率能在沙箱的 /lib 里找到，
# 但赌不起 —— 缺一个就是启动即 ENOENT，界面上只显示"未启动"。
# 一共才 3MB，全带上，靠 LD_LIBRARY_PATH 让包内这份优先。
#
# ⚠ 按 SONAME 命名【真实文件】，不要放软链：ugcli pack 会把软链解引用成完整拷贝。
install -m 0644 "$UP/usr/lib/${DPKGARCH}-linux-gnu/libtag.so.2.0.2"   "$ROOTFS/lib/libtag.so.2"
install -m 0644 "$UP/usr/lib/${DPKGARCH}-linux-gnu/libtag_c.so.2.0.2" "$ROOTFS/lib/libtag_c.so.2"
install -m 0644 "$UP/usr/lib/${TRIPLE}"/libstdc++.so.6.*              "$ROOTFS/lib/libstdc++.so.6"
install -m 0644 "$UP/usr/lib/${TRIPLE}/libgcc_s.so.1"                 "$ROOTFS/lib/libgcc_s.so.1"
install -m 0644 "$UP/usr/lib/${TRIPLE}"/libz.so.1.*                   "$ROOTFS/lib/libz.so.1"

# =========================================================================
# 2. ffmpeg / ffprobe / curl
# =========================================================================
echo "==> Fetching ffmpeg ${FFMPEG_VER} + curl ${CURL_VER} (${ARCH})"
fetch "https://johnvansickle.com/ffmpeg/releases/ffmpeg-${FFMPEG_VER}-${FF_ARCH}-static.tar.xz" \
      "$WORK_DIR/ffmpeg.tar.xz" "$FF_SHA"
tar xJf "$WORK_DIR/ffmpeg.tar.xz" -C "$WORK_DIR" \
  "ffmpeg-${FFMPEG_VER}-${FF_ARCH}-static/ffmpeg" "ffmpeg-${FFMPEG_VER}-${FF_ARCH}-static/ffprobe"
install -m 0755 "$WORK_DIR/ffmpeg-${FFMPEG_VER}-${FF_ARCH}-static/ffmpeg"  "$ROOTFS/bin/ffmpeg"
install -m 0755 "$WORK_DIR/ffmpeg-${FFMPEG_VER}-${FF_ARCH}-static/ffprobe" "$ROOTFS/bin/ffprobe"

fetch "https://github.com/stunnel/static-curl/releases/download/${CURL_VER}/curl-linux-${CURL_ARCH}-musl-${CURL_VER}.tar.xz" \
      "$WORK_DIR/curl.tar.xz" "$CURL_SHA"
mkdir -p "$WORK_DIR/curl"
tar xJf "$WORK_DIR/curl.tar.xz" -C "$WORK_DIR/curl" curl
install -m 0755 "$WORK_DIR/curl/curl" "$ROOTFS/bin/curl"

# =========================================================================
# 3. Go 管理壳
# =========================================================================
# 按【宿主】的 OS/架构取工具链（目标架构由 GOARCH 决定，两者无关）。
# CI 上宿主永远是 linux-amd64；自动探测是为了这个脚本在 macOS 上也能直接跑，
# 和本仓库其它 app 保持一致。
HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) HOST_ARCH="amd64" ;;
  arm64|aarch64) HOST_ARCH="arm64" ;;
  *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac
echo "==> Installing Go ${GO_VERSION} (host ${HOST_OS}/${HOST_ARCH})"
curl -fL -o "$WORK_DIR/go.tar.gz" "https://go.dev/dl/go${GO_VERSION}.${HOST_OS}-${HOST_ARCH}.tar.gz"
tar xzf "$WORK_DIR/go.tar.gz" -C "$WORK_DIR"
export GOROOT="$WORK_DIR/go"
export PATH="$GOROOT/bin:$PATH"
go version

# 管理壳的单元测试就是这个 app 的主要保障（配置合并、参数解析、启动诊断），
# CI 的 workflow 不会单独跑 go test，所以在这里跑。
echo "==> go test (launcher)"
( cd "$SCRIPT_DIR/launcher" && GOWORK=off GOTOOLCHAIN=local go test ./... )

echo "==> go build (launcher, linux/${ARCH})"
( cd "$SCRIPT_DIR/launcher" && GOWORK=off GOTOOLCHAIN=local CGO_ENABLED=0 \
    GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags="-s -w" -o "$ROOTFS/bin/zhiyin-launcher" . )

# =========================================================================
# 4. 断言（没有对应架构的机器可测时，这些是唯一的自动化保障）
# =========================================================================
echo "==> Verifying"
python3 - "$ROOTFS" "$ARCH" "$VERSION" <<'PY'
import os, struct, sys

rootfs, arch, version = sys.argv[1], sys.argv[2], sys.argv[3]
want_machine = {'amd64': 0x3e, 'arm64': 0xb7}[arch]
fail = []

def elf_machine(path):
    with open(path, 'rb') as f:
        head = f.read(20)
    if head[:4] != b'\x7fELF':
        return None
    return struct.unpack('<H', head[18:20])[0]

# 4.1 包里每个 ELF 都得是目标架构
n = 0
for dirpath, _, names in os.walk(rootfs):
    for name in names:
        p = os.path.join(dirpath, name)
        m = elf_machine(p)
        if m is None:
            continue
        n += 1
        if m != want_machine:
            fail.append('架构不对：%s 是 0x%x' % (os.path.relpath(p, rootfs), m))
print('    %d 个 ELF' % n)

# 4.2 上游二进制里要能找到版本串 —— 防的是"digest 解析到了别的 tag"
blob = open(os.path.join(rootfs, 'bin', 'zhiyin-music'), 'rb').read()
if version.encode() not in blob:
    fail.append('主程序里找不到版本串 %s' % version)

# 4.3 主程序的动态依赖要么随包带了、要么是沙箱 /lib 里一定有的基础件。
#     这一条防的是"上游哪天多链了一个库，装到 NAS 上才 ENOENT"。
def needed(path):
    d = open(path, 'rb').read()
    is64, le = d[4] == 2, d[5] == 1
    end = '<' if le else '>'
    shoff, = struct.unpack_from(end + ('Q' if is64 else 'I'), d, 0x28 if is64 else 0x20)
    shentsize, shnum, _ = struct.unpack_from(end + 'HHH', d, 0x3a if is64 else 0x2e)
    secs = []
    fmt = end + ('IIQQQQIIQQ' if is64 else 'IIIIIIIIII')
    for i in range(shnum):
        vals = struct.unpack_from(fmt, d, shoff + i * shentsize)
        secs.append({'typ': vals[1], 'off': vals[4], 'size': vals[5], 'link': vals[6]})
    dyn = [s for s in secs if s['typ'] == 6]
    if not dyn:
        return []
    dyn, out = dyn[0], []
    strtab = secs[dyn['link']]
    step = 16 if is64 else 8
    for off in range(dyn['off'], dyn['off'] + dyn['size'], step):
        tag, val = struct.unpack_from(end + ('qQ' if is64 else 'iI'), d, off)
        if tag == 0:
            break
        if tag == 1:
            s = strtab['off'] + val
            out.append(d[s:d.index(b'\0', s)].decode())
    return out

# 沙箱的 unit 里有 BindReadOnlyPaths=/lib（含 lib64），glibc 那几个一定在。
base = {'libc.so.6', 'libm.so.6', 'libdl.so.2', 'libpthread.so.0', 'librt.so.1',
        'ld-linux-aarch64.so.1', 'ld-linux-x86-64.so.2', 'libgcc_s.so.1'}
deps = needed(os.path.join(rootfs, 'bin', 'zhiyin-music'))
print('    DT_NEEDED: ' + ' '.join(deps))
for dep in deps:
    if dep not in base and not os.path.exists(os.path.join(rootfs, 'lib', dep)):
        fail.append('依赖 %s 既没随包带、也不属于沙箱一定有的基础库' % dep)

# 4.4 包里该有的东西
for rel, what in [('app/web/index.html', '前端'),
                  ('app/config.toml.default', '配置模板'),
                  ('app/releases.json', '更新日志'),
                  ('bin/ffmpeg', 'ffmpeg'), ('bin/ffprobe', 'ffprobe'), ('bin/curl', 'curl'),
                  ('bin/zhiyin-launcher', '管理壳'),
                  ('lib/libtag.so.2', 'TagLib')]:
    if not os.path.exists(os.path.join(rootfs, rel)):
        fail.append('缺%s（%s）' % (what, rel))

if fail:
    for f in fail:
        print('::error::' + f)
    sys.exit(1)
print('    全部通过 ✓')
PY

echo "==> Done: $ROOTFS"
du -sh "$ROOTFS"
