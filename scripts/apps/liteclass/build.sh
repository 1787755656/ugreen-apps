#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 1.1.5> <arch: amd64|arm64>
#
# 轻课堂（LiteClass）—— Node 22 + Fastify + SQLite 的私有云学习管理系统，
# 上游【只发 Docker 镜像，且只有 linux/amd64】，没有公开源码仓库，
# 业务 JS 是 javascript-obfuscator 混淆过的。
#
# 这个 app 和 zhiyinmusic 一样是"解 Docker 镜像"，但多解决两件事：
#
#  1. **补 arm64**。应用本体是纯 JS（架构无关），分架构的只有两样：
#     Node 运行时、sqlite3 的原生模块。两样都取官方对应架构的产物，
#     所以 amd64-only 的上游镜像照样能出 arm64 包（和 litevideo 同一个套路）。
#
#  2. **不用镜像里的 node_modules，按 lock 重装**。镜像里那份 122MB / 307 个包，
#     混着构建期依赖 —— javascript-obfuscator 13MB、node-gyp 2.7MB、eslint-*
#     全在里面，而它们**一个都不在 package-lock.json 里**。
#     按 lock 重装得到干净的 229 个包，顺带把体积砍掉一大半。
#     ⚠ lock 里 228 条 resolved 全指向 registry.npmmirror.com，
#       npm ≥12 把完整 URL 当 "remote" 类型直接拒（EALLOWREMOTE），
#       所以要改写回 registry.npmjs.org —— integrity 哈希原样保留，
#       等于顺带做了一次校验。
#
# 沙箱适配全在 static/ugos-launcher.js（不动上游混淆代码，只做环境重定向）。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

IMAGE_REPO="luodichanagn/liteclass"

# 上游镜像只有 amd64；业务 JS 与架构无关，所以两个目标架构都从这一份镜像取。
IMAGE_ARCH="amd64"

# ---- Node 运行时 ---------------------------------------------------------
# 钉死在上游镜像自带的同一个版本（镜像里 /usr/local/bin/node 是 v22.23.2）：
# 原生模块按 N-API 编译虽然跨版本兼容，但上游只在这个版本上测过，没必要漂。
NODE_VERSION="22.23.2"
NODE_SHA256_amd64="d60acfe00a2932254bb0ad20e01b0d74397a0875595de719654b214f4b03f307"
NODE_SHA256_arm64="fff4078c5def658577f92c88db7db3bc0072924bfb93fe52c1e744a54e94abb8"

# ---- sqlite3 原生模块 ----------------------------------------------------
# 包里唯一的原生模块。**不在 CI 里编译**（要 node-gyp + 交叉编译工具链，
# 而且 amd64 runner 上编不出 arm64 的），直接取上游发布的预编译产物。
# 解出来就是 build/Release/node_sqlite3.node —— 和镜像里那份的位置一模一样，
# 直接落位即可。napi-v6 对 Node 22 是向前兼容的（N-API ABI 稳定）。
SQLITE3_VERSION="5.1.7"
SQLITE3_SHA256_amd64="6d1f7a95e5aca90db1fd6a2839380a021d5ee23d46f2d7c520ded094da813fed"
SQLITE3_SHA256_arm64="0f112c63a74bebdffce298792c264b3af4b85d7fe1975a4bca1227438f531dbb"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  arm64) NODE_ARCH="arm64"; SQLITE3_ARCH="arm64"; ELF_MACHINE="aarch64" ;;
  amd64) NODE_ARCH="x64";   SQLITE3_ARCH="x64";   ELF_MACHINE="x86-64"  ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac
eval "NODE_SHA=\$NODE_SHA256_${ARCH}"
eval "SQLITE3_SHA=\$SQLITE3_SHA256_${ARCH}"

echo "==> Building LiteClass ${VERSION} (linux/${ARCH})"

fetch() { # fetch <url> <out> <sha256>
  local url="$1" out="$2" want="$3" got
  curl -fL --retry 3 --retry-all-errors -o "$out" "$url"
  got=$(sha256sum "$out" | cut -d' ' -f1)
  [ "$got" = "$want" ] || { echo "::error::sha256 mismatch for $url (want $want, got $got)" >&2; exit 1; }
}

rm -rf "$ROOTFS"
mkdir -p "$ROOTFS/bin"

# =========================================================================
# 1. 上游镜像：按 tag 查出 amd64 的 manifest digest，再按 digest 拉层
# =========================================================================
# 刻意不把 digest 写死：这个 app 要跟着上游自动出版本，写死就等于每次上游发新版
# 都要手改脚本。安全性没降低 —— extract-image.py 会逐层用 manifest 里的 digest
# 复核，digest 本身是内容寻址的。
echo "==> Resolving manifest digest for ${IMAGE_REPO}:${VERSION} (${IMAGE_ARCH})"
TOKEN=$(curl -fsSL "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${IMAGE_REPO}:pull" | jq -r .token)
MANIFEST_DIGEST=$(curl -fsSL \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json" \
  "https://registry-1.docker.io/v2/${IMAGE_REPO}/manifests/${VERSION}" \
  | jq -r --arg a "$IMAGE_ARCH" '.manifests[] | select(.platform.os=="linux" and .platform.architecture==$a) | .digest' | head -1)
[ -n "$MANIFEST_DIGEST" ] || {
  echo "::error::镜像 ${IMAGE_REPO}:${VERSION} 里没有 linux/${IMAGE_ARCH} 的 manifest" >&2
  exit 1
}
echo "    ${MANIFEST_DIGEST}"

UP="$WORK_DIR/upstream"
python3 "$SCRIPT_DIR/extract-image.py" "$IMAGE_REPO" "$MANIFEST_DIGEST" "$WORK_DIR/blobs" "$UP" \
  app/server/

SRC="$UP/app/server"
[ -f "$SRC/index.js" ] && [ -f "$SRC/package-lock.json" ] || {
  echo "::error::镜像里没解出 app/server/{index.js,package-lock.json} —— 上游目录结构可能变了" >&2
  exit 1; }

# ---- 上游业务代码（不含 node_modules，那份按 lock 重装） ----
mkdir -p "$ROOTFS/server"
( cd "$SRC" && tar cf - --exclude=./node_modules . ) | ( cd "$ROOTFS/server" && tar xf - )

# ---- 剔除镜像里夹带的运行时残留 ----
# ⚠ 这几样是上游打镜像时把开发/测试产物一起 COPY 进去的，**绝不能进包**：
#   jwt.secret  —— 所有安装共用同一个签名密钥 = 任何人都能伪造任意用户的 token
#   liteclass.db / data/ —— 夹带的账号（含 bcrypt 哈希）与业务数据
#   *.log / transcode_cache —— 无意义的体积
# 运行时这些都由 ugos-launcher.js 重定向到 UGAPP_DATA_DIR 下重新生成。
rm -rf "$ROOTFS/server/data" "$ROOTFS/server/transcode_cache"
rm -f  "$ROOTFS/server/jwt.secret" "$ROOTFS/server/liteclass.db" "$ROOTFS/server"/*.log

# ---- UGOS 沙箱适配层 ----
install -m 0644 "$SCRIPT_DIR/static/ugos-launcher.js" "$ROOTFS/server/ugos-launcher.js"

# =========================================================================
# 2. 依赖：按 lock 重装（丢掉镜像里夹带的构建期依赖）
# =========================================================================
echo "==> npm ci (按 package-lock.json 重装依赖)"
# lock 里的 resolved 指向 registry.npmmirror.com，npm ≥12 会拒（EALLOWREMOTE）。
# 改写回官方源，**integrity 哈希原样保留** —— 等于顺带校验了包内容。
python3 - "$ROOTFS/server/package-lock.json" <<'PY'
import json, sys
path = sys.argv[1]
with open(path) as f:
    lock = json.load(f)
n = 0
for pkg in lock.get("packages", {}).values():
    r = pkg.get("resolved", "")
    if r.startswith("https://registry.npmmirror.com/"):
        pkg["resolved"] = r.replace("https://registry.npmmirror.com/",
                                    "https://registry.npmjs.org/", 1)
        n += 1
with open(path, "w") as f:
    json.dump(lock, f, indent=2)
    f.write("\n")
print("    改写 %d 条 resolved → registry.npmjs.org" % n)
PY

# --ignore-scripts：不让 sqlite3 的 install 脚本去下载/编译原生模块 ——
# 那一步会按【宿主】架构装，正是我们要避免的。原生模块下一节自己放。
( cd "$ROOTFS/server" && npm ci --omit=dev --no-audit --no-fund --ignore-scripts )

# =========================================================================
# 3. 分架构的两样东西：sqlite3 原生模块 + Node 运行时
# =========================================================================
echo "==> Fetching sqlite3 ${SQLITE3_VERSION} prebuilt (${SQLITE3_ARCH})"
fetch "https://github.com/TryGhost/node-sqlite3/releases/download/v${SQLITE3_VERSION}/sqlite3-v${SQLITE3_VERSION}-napi-v6-linux-${SQLITE3_ARCH}.tar.gz" \
      "$WORK_DIR/sqlite3.tar.gz" "$SQLITE3_SHA"
# 包内布局就是 build/Release/node_sqlite3.node，直接解进模块目录
tar xzf "$WORK_DIR/sqlite3.tar.gz" -C "$ROOTFS/server/node_modules/sqlite3"

echo "==> Fetching Node ${NODE_VERSION} (${NODE_ARCH})"
fetch "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz" \
      "$WORK_DIR/node.tar.xz" "$NODE_SHA"
tar xJf "$WORK_DIR/node.tar.xz" -C "$WORK_DIR" \
  "node-v${NODE_VERSION}-linux-${NODE_ARCH}/bin/node"
install -m 0755 "$WORK_DIR/node-v${NODE_VERSION}-linux-${NODE_ARCH}/bin/node" "$ROOTFS/bin/node"

# =========================================================================
# 4. 断言（没有对应架构的机器可测时，这些是唯一的自动化保障）
# =========================================================================
echo "==> Verifying"
python3 - "$ROOTFS" "$ARCH" <<'PY'
import os, struct, sys

rootfs, arch = sys.argv[1], sys.argv[2]
want_machine = {'amd64': 0x3e, 'arm64': 0xb7}[arch]
fail = []

def elf_machine(path):
    try:
        with open(path, 'rb') as f:
            head = f.read(20)
    except OSError:
        return None
    if head[:4] != b'\x7fELF':
        return None
    return struct.unpack('<H', head[18:20])[0]

# 4.1 包里每个 ELF 都得是目标架构（防的是"预编译产物拿错架构"静默进包）
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
if n < 2:
    fail.append('ELF 数量不对：至少该有 node 和 node_sqlite3.node')

# 4.2 该有的东西
for rel, what in [('bin/node', 'Node 运行时'),
                  ('server/index.js', '上游主程序'),
                  ('server/config.js', '上游配置模块'),
                  ('server/ugos-launcher.js', 'UGOS 适配层'),
                  ('server/public', '前端'),
                  ('server/node_modules/fastify', 'fastify'),
                  ('server/node_modules/sqlite3/build/Release/node_sqlite3.node', 'sqlite3 原生模块')]:
    if not os.path.exists(os.path.join(rootfs, rel)):
        fail.append('缺%s（%s）' % (what, rel))

# 4.3 镜像夹带的秘密/数据绝不能进包
#     jwt.secret 尤其致命：所有安装共用一个签名密钥 = token 可被任意伪造。
for rel in ['server/jwt.secret', 'server/liteclass.db', 'server/data', 'server/transcode_cache']:
    if os.path.exists(os.path.join(rootfs, rel)):
        fail.append('夹带了上游镜像的运行时残留，必须剔除：%s' % rel)

# 4.4 镜像 node_modules 里那堆【不在 lock 里】的构建期污染物不该出现在成品里。
#     这条是回归保险：万一哪天改回"直接用镜像里的 node_modules"，这里会当场失败。
#     ⚠ 不要把 node-gyp 列进来 —— 它是 sqlite3 在 lock 里的正经依赖（只在安装期
#       用得到，我们跳过了它的 install 脚本），按 lock 重装本来就会有。
for junk in ['javascript-obfuscator', 'eslint-scope', 'class-validator', 'libphonenumber-js']:
    if os.path.exists(os.path.join(rootfs, 'server', 'node_modules', junk)):
        fail.append('成品里混进了镜像夹带的构建期依赖：node_modules/%s' % junk)

if fail:
    for f in fail:
        print('::error::' + f)
    sys.exit(1)
print('    全部通过 ✓')
PY

echo "==> Done: $ROOTFS"
du -sh "$ROOTFS"
