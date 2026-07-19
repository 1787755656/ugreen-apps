#!/bin/bash
set -euo pipefail

# Usage: build.sh <version> <arch: amd64|arm64>
# Must be run from the repo root (paths below are relative to it).

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Building Jellyfin ${VERSION} (${ARCH})"

# ---- Server tarball (repo.jellyfin.org, not GitHub release assets — see
#      scripts/apps/jellyfin/get-latest-version.sh for why version is
#      resolved via GitHub API but the actual binary is fetched from here) ----
SERVER_URL="https://repo.jellyfin.org/files/server/linux/stable/v${VERSION}/${ARCH}/jellyfin_${VERSION}-${ARCH}.tar.gz"
echo "==> Server: ${SERVER_URL}"
curl -fL -o "$WORK_DIR/jellyfin.tar.gz" "$SERVER_URL"

# ---- FFmpeg (jellyfin's own custom portable GPL build, tracked
#      independently of the server version via repo.jellyfin.org's
#      "latest-7.x" pointer directory) ----
case "$ARCH" in
  amd64) PORTABLE_SUFFIX="linux64" ;;
  arm64) PORTABLE_SUFFIX="linuxarm64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac
FFMPEG_BASE="https://repo.jellyfin.org/files/ffmpeg/linux/latest-7.x/${ARCH}"
FFMPEG_FILE=$(curl -sL "$FFMPEG_BASE/" | grep -oE "jellyfin-ffmpeg_[^\"]*_portable_${PORTABLE_SUFFIX}-gpl\.tar\.[a-z]+" | head -1)
[ -z "$FFMPEG_FILE" ] && { echo "Failed to resolve ffmpeg portable build from $FFMPEG_BASE/" >&2; exit 1; }
echo "==> FFmpeg: ${FFMPEG_FILE}"
curl -fL -o "$WORK_DIR/$FFMPEG_FILE" "${FFMPEG_BASE}/${FFMPEG_FILE}"

# ---- Assemble rootfs ----
rm -rf "$ROOTFS/jellyfin" "$ROOTFS/ffmpeg" "$ROOTFS/lib"
mkdir -p "$ROOTFS/bin" "$ROOTFS/ffmpeg" "$ROOTFS/lib"

tar -xzf "$WORK_DIR/jellyfin.tar.gz" -C "$WORK_DIR"
# The tarball extracts to a single top-level "jellyfin/" dir containing the
# self-contained .NET runtime + jellyfin-web static frontend bundled inside.
mv "$WORK_DIR/jellyfin" "$ROOTFS/jellyfin"

mkdir -p "$WORK_DIR/ffmpeg_extract"
tar -xf "$WORK_DIR/$FFMPEG_FILE" -C "$WORK_DIR/ffmpeg_extract"
FFMPEG_BIN=$(find "$WORK_DIR/ffmpeg_extract" -name ffmpeg -type f -print -quit)
[ -z "$FFMPEG_BIN" ] && { echo "ffmpeg binary not found in portable package" >&2; exit 1; }
cp -a "$(dirname "$FFMPEG_BIN")"/* "$ROOTFS/ffmpeg/"
chmod +x "$ROOTFS/ffmpeg/ffmpeg" "$ROOTFS/ffmpeg/ffprobe" 2>/dev/null || true

# Static files that don't change with upstream version (this repo's own
# wrapper script + the nss_wrapper workaround for the sandboxed uid not
# being in /etc/passwd — see scripts/apps/jellyfin/static/start.sh's own
# comments for why it's needed).
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"
cp "$SCRIPT_DIR/static/lib/libnss_wrapper-${ARCH}.so" "$ROOTFS/lib/libnss_wrapper.so"

echo "==> Done: $ROOTFS"
du -sh "$ROOTFS/jellyfin" "$ROOTFS/ffmpeg"
