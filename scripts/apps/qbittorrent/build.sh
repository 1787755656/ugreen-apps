#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 5.2.1.10> <arch: amd64|arm64>
# VERSION here is the full upstream version (same value get-latest-version.sh
# emits as VERSION, NOT the truncated PROJECT_VERSION) — the upstream git
# tag is reconstructed from it as "release-<version>".

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"
UPSTREAM_TAG="release-${VERSION}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) QB_ARCH="x86_64" ;;
  arm64) QB_ARCH="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building qBittorrent Enhanced Edition (${QB_ARCH}) from ${UPSTREAM_TAG}"

ASSET="qbittorrent-enhanced-nox_${QB_ARCH}-linux-musl_static.zip"
DOWNLOAD_URL="https://github.com/c0re100/qBittorrent-Enhanced-Edition/releases/download/${UPSTREAM_TAG}/${ASSET}"
echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL -o "$WORK_DIR/qbittorrent-nox.zip" "$DOWNLOAD_URL"

unzip -o -q "$WORK_DIR/qbittorrent-nox.zip" -d "$WORK_DIR/extracted"
EXTRACTED_BIN=$(find "$WORK_DIR/extracted" -maxdepth 1 -type f | head -1)
[ -n "$EXTRACTED_BIN" ] || { echo "qbittorrent-nox binary not found in zip" >&2; exit 1; }

mkdir -p "$ROOTFS/bin"
cp "$EXTRACTED_BIN" "$ROOTFS/bin/qbittorrent-nox"
chmod +x "$ROOTFS/bin/qbittorrent-nox"

# Static wrapper script — unrelated to upstream version, see
# scripts/apps/qbittorrent/static/start.sh's own comments.
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
