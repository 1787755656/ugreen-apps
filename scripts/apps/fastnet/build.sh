#!/bin/bash
set -euo pipefail

# Usage: build.sh <version> <arch: amd64|arm64>
#
# 从 KoolCenter 官方固件下载服务器取 FastNet 的预编译二进制（上游只发裸二进制，
# 没有源码仓库、也没有 GitHub Release）：
#
#   <镜像>/binary/fastnet/FastNet-<版本>.<amd64|arm64|armv7>
#
# 校验和在同目录的 version.txt 里（FASTNET_AMD64_SHA256 / FASTNET_ARM64_SHA256）。
# armv7 不打包 —— UGOS Pro 只有 amd64/arm64 两种机型。
#
# ⚠ amd64 那个二进制是 UPX 加壳的（arm64 的不是，上游两边不一致）。本脚本会
#   解壳，理由见 ugos-pro-app-dev skill 的 phpMyAdmin 那节：自解压要把内存页
#   改成可写+可执行，在绿联那么紧的 systemd 沙箱里能不能过没验证过；加壳还是
#   应用商店安全审核眼里的"检测规避"特征；解开之后 strings 才读得到、断言才做得了。
#   代价只是包大几 MB。注意 version.txt 里的 sha256 是【加壳文件】的，所以顺序
#   必须是"先校验、后解壳"。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

BASE_URL_PRIMARY="https://fw.kspeeder.com/binary/fastnet"
BASE_URL_FALLBACK="https://fw.koolcenter.com/binary/fastnet"

# 解壳用的 UPX：开发机上一般装了，CI runner 上没有就现下一个钉死版本的官方产物
# （比 apt 装快也更确定；runner 永远是 linux amd64，与目标架构无关）。
UPX_VERSION="5.0.2"

case "$ARCH" in
  amd64)
    ARCH_SUFFIX="amd64"
    SHA_KEY="FASTNET_AMD64_SHA256"
    EXPECT_ELF="x86-64"
    ;;
  arm64)
    ARCH_SUFFIX="arm64"
    SHA_KEY="FASTNET_ARM64_SHA256"
    EXPECT_ELF="aarch64"
    ;;
  *)
    echo "Unsupported arch: $ARCH" >&2
    exit 1
    ;;
esac

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Building FastNet ${VERSION} (${ARCH})"

download_with_fallback() {
  local path="$1" dest="$2" nocache="${3:-0}"
  local base url
  for base in "$BASE_URL_PRIMARY" "$BASE_URL_FALLBACK"; do
    url="${base}/${path}"
    echo "==> Downloading: ${url}"
    if [ "$nocache" = "1" ]; then
      curl -fL --max-time 300 -H 'Cache-Control: no-cache' -H 'Pragma: no-cache' \
        -o "$dest" "$url" && return 0
    else
      curl -fL --max-time 300 -o "$dest" "$url" && return 0
    fi
    echo "    failed, trying next mirror" >&2
  done
  echo "Failed to download ${path} from all mirrors" >&2
  return 1
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# ---- 取校验和 -------------------------------------------------------------
# version.txt 只描述【当前最新版】。workflow_dispatch 指定重建某个历史版本时，
# 里面的 sha 对不上要构建的那个版本 —— 这时跳过校验和（历史二进制的哈希上游
# 没有留档，没法核对），但下面的 ELF/静态链接/冒烟断言照做。
EXPECTED_SHA=""
if download_with_fallback "version.txt" "$WORK_DIR/version.txt" 1; then
  TXT_VERSION=$(awk -F= '/^VERSION=/{print $2; exit}' "$WORK_DIR/version.txt" | tr -d '\r')
  if [ "$TXT_VERSION" = "$VERSION" ]; then
    EXPECTED_SHA=$(awk -F= -v key="$SHA_KEY" '$1==key{print $2; exit}' "$WORK_DIR/version.txt" | tr -d '\r')
  else
    echo "::warning::version.txt 现在是 ${TXT_VERSION}，正在构建的是 ${VERSION}（历史版本），跳过 sha256 校验"
  fi
fi

# ---- 下载二进制 -----------------------------------------------------------
BINARY_NAME="FastNet-${VERSION}.${ARCH_SUFFIX}"
RAW_BIN="$WORK_DIR/${BINARY_NAME}"
download_with_fallback "$BINARY_NAME" "$RAW_BIN"

ACTUAL_SHA=$(sha256_of "$RAW_BIN")
if [ -n "$EXPECTED_SHA" ]; then
  if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
    echo "::error::sha256 校验失败 ${BINARY_NAME}: 期望 ${EXPECTED_SHA}，实际 ${ACTUAL_SHA}" >&2
    exit 1
  fi
  echo "==> sha256 OK: ${ACTUAL_SHA}"
else
  echo "==> sha256 (未校验): ${ACTUAL_SHA}"
fi

# ---- UPX 解壳（只在真的加了壳时做）----------------------------------------
if LC_ALL=C grep -aqm1 'UPX!' "$RAW_BIN"; then
  echo "==> 检测到 UPX 壳，解壳中"
  if command -v upx >/dev/null 2>&1; then
    UPX_BIN="upx"
  else
    UPX_TARBALL="$WORK_DIR/upx.tar.xz"
    curl -fL --max-time 300 -o "$UPX_TARBALL" \
      "https://github.com/upx/upx/releases/download/v${UPX_VERSION}/upx-${UPX_VERSION}-amd64_linux.tar.xz"
    tar -xJf "$UPX_TARBALL" -C "$WORK_DIR"
    UPX_BIN="$WORK_DIR/upx-${UPX_VERSION}-amd64_linux/upx"
    chmod +x "$UPX_BIN"
  fi
  SIZE_BEFORE=$(wc -c < "$RAW_BIN")
  "$UPX_BIN" -d -q "$RAW_BIN" >/dev/null
  SIZE_AFTER=$(wc -c < "$RAW_BIN")
  echo "==> 解壳完成: ${SIZE_BEFORE} -> ${SIZE_AFTER} 字节"
  if LC_ALL=C grep -aqm1 'UPX!' "$RAW_BIN"; then
    echo "::error::解壳后仍能匹配到 UPX 标记" >&2
    exit 1
  fi
else
  echo "==> 未加壳，跳过解壳"
fi

# ---- 断言：架构 / 静态链接 -------------------------------------------------
# 拿错架构的表现是"装到 NAS 上才炸"，在这里就拦住。
FILE_OUT=$(file -b "$RAW_BIN")
echo "==> file: ${FILE_OUT}"
case "$FILE_OUT" in
  *ELF*) ;;
  *) echo "::error::不是 ELF 可执行文件: ${FILE_OUT}" >&2; exit 1 ;;
esac
if ! echo "$FILE_OUT" | grep -q "$EXPECT_ELF"; then
  echo "::error::架构不匹配，期望 ${EXPECT_ELF}: ${FILE_OUT}" >&2
  exit 1
fi
# 沙箱里 /usr、/lib 视图极窄，动态链接的东西基本活不了；上游两个架构目前都是
# statically linked，哪天变成动态链接必须当场知道。
if ! echo "$FILE_OUT" | grep -q 'statically linked'; then
  echo "::error::期望静态链接的二进制: ${FILE_OUT}" >&2
  exit 1
fi

# ---- 冒烟测试（只有同架构才跑得起来）---------------------------------------
# CI runner 是 linux/amd64，所以 amd64 这一路能真跑一次 `version`；解壳解坏了、
# 或者上游发了个坏包，会在这里当场暴露，而不是等用户装上去发现应用起不来。
chmod +x "$RAW_BIN"
if [ "$ARCH" = "amd64" ] && [ "$(uname -s)" = "Linux" ] && [ "$(uname -m)" = "x86_64" ]; then
  echo "==> 冒烟测试: FastNet version"
  "$RAW_BIN" version || { echo "::error::解壳后的二进制无法执行" >&2; exit 1; }
fi

# ---- 落位 -----------------------------------------------------------------
mkdir -p "$ROOTFS/bin"
cp "$RAW_BIN" "$ROOTFS/bin/fastnet"
chmod +x "$ROOTFS/bin/fastnet"

# 静态封装脚本，与上游版本无关，注释见 static/start.sh 自身
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
