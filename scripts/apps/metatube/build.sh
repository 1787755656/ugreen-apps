#!/bin/bash
set -euo pipefail

# Usage: build.sh <version> <arch: amd64|arm64>
# Downloads the prebuilt binary from metatube-server-releases (upstream's own
# distribution channel — see get-latest-version.sh) instead of compiling
# from source. Simpler and equivalent to the previous manual `go build`
# process: same static, CGO_ENABLED=0 binary, just built by upstream's own
# CI instead of ours.

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Building MetaTube ${VERSION} (${ARCH})"

DOWNLOAD_URL="https://github.com/metatube-community/metatube-server-releases/releases/download/v${VERSION}/metatube-server-linux-${ARCH}.zip"
echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL -o "$WORK_DIR/metatube-server.zip" "$DOWNLOAD_URL"

unzip -o -q "$WORK_DIR/metatube-server.zip" -d "$WORK_DIR/extracted"
EXTRACTED_BIN=$(find "$WORK_DIR/extracted" -maxdepth 1 -name "metatube-server-linux-*" -type f | head -1)
[ -n "$EXTRACTED_BIN" ] || { echo "metatube-server binary not found in zip" >&2; exit 1; }

mkdir -p "$ROOTFS/bin"
cp "$EXTRACTED_BIN" "$ROOTFS/bin/metatube-server"
chmod +x "$ROOTFS/bin/metatube-server"

# Static wrapper script — unrelated to upstream version, see
# scripts/apps/metatube/static/start.sh's own comments.
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
