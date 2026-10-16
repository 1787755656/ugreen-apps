#!/usr/bin/env bash
# 主题美化中心（Docker 型，com.personal.ugtheme）build.sh。
#
# 镜像【本机构建、Release 托管】，CI 不重建（与 idphotos 同套路）：
#   ugtheme/ugtheme:<x.y.z>  = alpine:3.20 + python3/curl + 应用代码 + 5 套内置图标主题，
#   发布在【本仓库】的 Release 上：
#     tag   ughtheme-img/<x.y.z>
#     资产  img-amd64.tar / img-arm64.tar   （docker save 出来的 OCI-layout tar）
#
# ⚠ tag 前缀故意用 ughtheme-img/ 而不是 ughtheme/：
#   catalog 生成器（scripts/catalog/generate-catalog.py）按 tag 的 "/" 前段
#   把 release 归到应用 slug 上，并只取【最新】那一个。若镜像 release 与
#   ughtheme/vX.Y.Z 同前缀且发布更晚，生成器会挑到它、发现没有 .upk 资产，
#   然后把这个应用整个跳过（apps.json 里直接消失）。换前缀等于换 slug，互不干扰。
#
# 这个脚本只做三件事：下载对应架构镜像 tar → 铺 rootfs_<arch>/images/ →
# 把 support_arch 收窄到当前架构（ugcli check 要求「每个声明架构」都有镜像 tar，
# 而 matrix 里每个 job 只打一个架构）。随后由可复用 workflow 统一
# ugcli check / pack / 发布。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
VERSION="${1:?usage: build.sh <version> <arch>}"
ARCH="${2:?usage: build.sh <version> <arch>}"
case "$ARCH" in
  amd64|arm64) ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac
# shellcheck disable=SC1091
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/meta.env"

APP_DIR="$REPO_ROOT/$PROJECT_DIR"
IMG_DIR="$APP_DIR/rootfs_${ARCH}/images"
COMPOSE="$APP_DIR/rootfs_common/docker-compose.yaml"
IMAGE="ugtheme/ugtheme:${VERSION}"
IMG_TAG="ugtheme-img/${VERSION}"
ASSET="img-${ARCH}.tar"
URL="https://github.com/1787755656/ugreen-apps/releases/download/${IMG_TAG}/${ASSET}"
TAR="$IMG_DIR/ugtheme-${VERSION}.tar"

mkdir -p "$IMG_DIR"
# 换版本时把上一版的 tar 清掉，免得 ugcli 挑到旧镜像
rm -f "$IMG_DIR"/ugtheme-*.tar

echo "==> downloading ${URL}"
if ! curl -fL --retry 3 --retry-delay 5 -o "$TAR" "$URL"; then
  echo "::error::镜像 tar 下载失败：${URL}" >&2
  echo "::error::请先在本机 docker build 镜像并发布到 Release ${IMG_TAG}（资产名 ${ASSET}），见 scripts/apps/ugtheme/README.md" >&2
  exit 1
fi

SIZE=$(stat -c%s "$TAR" 2>/dev/null || stat -f%z "$TAR")
if [ "$SIZE" -lt 20000000 ]; then
  echo "::error::镜像 tar 太小（${SIZE} 字节），下载到的多半不是镜像" >&2
  exit 1
fi
echo "==> image tar ready: ${SIZE} bytes"

# 硬断言：tar 里真的是这个架构、且镜像名就是 compose 引用的 ugtheme/ugtheme:<version>。
# （镜像在别的机器上构建，标签/架构错配了只有到真机安装那一刻才会暴露，太晚。）
python3 - "$TAR" "$VERSION" "$ARCH" <<'PY'
import json, sys, tarfile

tar_path, version, arch = sys.argv[1], sys.argv[2], sys.argv[3]
want = f"ugtheme/ugtheme:{version}"


def fail(msg):
    print(f"::error::{msg}", file=sys.stderr)
    sys.exit(1)


with tarfile.open(tar_path) as t:
    def load(name):
        try:
            f = t.extractfile(name)
        except KeyError:
            return None
        return json.load(f) if f else None

    def blob(digest):
        name = "blobs/sha256/" + digest.split(":")[1]
        f = t.extractfile(name)
        return json.load(f) if f else None

    idx = load("index.json")
    if not idx:
        fail("tar 里没有 index.json —— 不是 docker save 出来的 OCI-layout tar")

    names, archs = [], set()
    for m in idx.get("manifests", []):
        name = (m.get("annotations") or {}).get("io.containerd.image.name")
        if name:
            names.append(name)
        sub = blob(m["digest"])
        for inner in (sub or {}).get("manifests", []):
            p = inner.get("platform") or {}
            if p.get("os") == "linux":
                archs.add(p.get("architecture"))

    if want not in names and f"docker.io/{want}" not in names:
        fail(f"镜像名不符：期望含 {want}，tar 里是 {names}")
    if arch not in archs:
        fail(f"架构不符：期望 {arch}，tar 里是 {sorted(archs)}")
    print(f"==> verified image {want} ({arch}), index: {names}")
PY

# compose 的镜像 tag 跟着版本走（project.yaml 的 version 由 workflow sed，
# 但 compose 里的 image tag 得自己对齐，否则真机上 docker 找不到镜像）
sed -i -E "s#^([[:space:]]*image:[[:space:]]*\"?)ugtheme/ugtheme:[^\"[:space:]]*(\"?)[[:space:]]*\$#\1${IMAGE}\2#" "$COMPOSE"
if ! grep -qF "image: \"${IMAGE}\"" "$COMPOSE" && ! grep -qF "image: ${IMAGE}" "$COMPOSE"; then
  echo "::error::compose 镜像 tag 对齐失败，期望 ${IMAGE}，实际：" >&2
  grep -n 'image:' "$COMPOSE" >&2
  exit 1
fi
echo "==> compose image tag aligned: ${IMAGE}"

# 收窄 support_arch 到当前架构（单架构打包；只改 CI workspace，仓库里那份不动）
python3 - "$ARCH" "$APP_DIR/project.yaml" <<'PY'
import re, sys

arch, path = sys.argv[1], sys.argv[2]
s = open(path, encoding="utf-8").read()
s, n = re.subn(r"support_arch:\n(?:  - \w+\n)+", "support_arch:\n  - %s\n" % arch, s)
if n != 1:
    sys.exit("support_arch 收窄失败（没匹配到 support_arch 块）")
open(path, "w", encoding="utf-8").write(s)
print("support_arch narrowed to", arch)
PY

ls -lh "$IMG_DIR"
echo "==> Done: $IMG_DIR"
