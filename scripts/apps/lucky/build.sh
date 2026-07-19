#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 2.27.2> <arch: amd64|arm64>
# Downloads the official prebuilt static binary from gdy666/lucky releases
# and assembles rootfs_<arch>/bin (lucky + start.sh wrapper).

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
  amd64) LUCKY_ARCH="x86_64" ;;
  arm64) LUCKY_ARCH="arm64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building Lucky (${LUCKY_ARCH}) from ${UPSTREAM_TAG}"

ASSET="lucky_${VERSION}_Linux_${LUCKY_ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/gdy666/lucky/releases/download/${UPSTREAM_TAG}/${ASSET}"
echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL -o "$WORK_DIR/lucky.tar.gz" "$DOWNLOAD_URL"

mkdir -p "$WORK_DIR/extracted"
tar xzf "$WORK_DIR/lucky.tar.gz" -C "$WORK_DIR/extracted"
EXTRACTED_BIN="$WORK_DIR/extracted/lucky"
[ -f "$EXTRACTED_BIN" ] || { echo "lucky binary not found in tarball" >&2; exit 1; }

mkdir -p "$ROOTFS/bin"
cp "$EXTRACTED_BIN" "$ROOTFS/bin/lucky"
chmod +x "$ROOTFS/bin/lucky"

# Static wrapper script — unrelated to upstream version.
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
