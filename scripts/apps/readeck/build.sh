#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 0.22.3> <arch: amd64|arm64>
# VERSION is the upstream version as emitted by get-latest-version.sh —
# for Readeck that's a plain x.y.z (no v prefix), which is also the middle
# of the release asset name: readeck-0.22.3-linux-amd64.
# Assets are hosted on Codeberg:
#   https://codeberg.org/readeck/readeck/releases/download/<ver>/readeck-<ver>-linux-<arch>

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Building Readeck ${VERSION} (${ARCH})"

ASSET="readeck-${VERSION}-linux-${ARCH}"
DOWNLOAD_URL="https://codeberg.org/readeck/readeck/releases/download/${VERSION}/${ASSET}"
echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL -o "$WORK_DIR/readeck" "$DOWNLOAD_URL"

echo "==> Verifying ELF (expect statically linked Linux ${ARCH})"
file "$WORK_DIR/readeck"
case "$ARCH" in
  amd64) file "$WORK_DIR/readeck" | grep -q 'x86-64' ;;
  arm64) file "$WORK_DIR/readeck" | grep -q 'aarch64' ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

mkdir -p "$ROOTFS/bin"
cp "$WORK_DIR/readeck" "$ROOTFS/bin/readeck"
chmod +x "$ROOTFS/bin/readeck"

# Static wrapper script — unrelated to upstream version, see
# scripts/apps/readeck/static/start.sh's own comments.
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
