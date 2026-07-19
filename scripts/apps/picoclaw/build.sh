#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 0.3.1> <arch: amd64|arm64>
# Downloads the official prebuilt static binaries (picoclaw + picoclaw-launcher)
# from sipeed/picoclaw releases and assembles rootfs_<arch>/bin.

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"
UPSTREAM_TAG="v${VERSION}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) PC_ARCH="x86_64";  ELF_PAT="x86-64" ;;
  arm64) PC_ARCH="arm64";   ELF_PAT="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building PicoClaw (${PC_ARCH}) from ${UPSTREAM_TAG}"

ASSET="picoclaw_Linux_${PC_ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/sipeed/picoclaw/releases/download/${UPSTREAM_TAG}/${ASSET}"
echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL -o "$WORK_DIR/picoclaw.tar.gz" "$DOWNLOAD_URL"

mkdir -p "$WORK_DIR/extracted"
tar xzf "$WORK_DIR/picoclaw.tar.gz" -C "$WORK_DIR/extracted"

for BIN_NAME in picoclaw picoclaw-launcher; do
  [ -f "$WORK_DIR/extracted/$BIN_NAME" ] || {
    echo "$BIN_NAME binary not found in tarball" >&2
    exit 1
  }
  # Guard against upstream ever mislabeling an asset: verify ELF arch.
  file "$WORK_DIR/extracted/$BIN_NAME" | grep -q "ELF.*${ELF_PAT}" || {
    echo "$BIN_NAME is not an ELF ${ELF_PAT} binary:" >&2
    file "$WORK_DIR/extracted/$BIN_NAME" >&2
    exit 1
  }
done

mkdir -p "$ROOTFS/bin"
cp "$WORK_DIR/extracted/picoclaw" "$ROOTFS/bin/picoclaw"
cp "$WORK_DIR/extracted/picoclaw-launcher" "$ROOTFS/bin/picoclaw-launcher"
chmod +x "$ROOTFS/bin/picoclaw" "$ROOTFS/bin/picoclaw-launcher"

# Static wrapper script — unrelated to upstream version.
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
