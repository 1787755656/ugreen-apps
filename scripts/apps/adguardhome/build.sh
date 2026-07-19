#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 0.107.78> <arch: amd64|arm64>
# Downloads the official prebuilt static binary from AdguardTeam/AdGuardHome
# releases and assembles rootfs_<arch>/bin (AdGuardHome + start.sh wrapper).

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

echo "==> Building AdGuard Home (${ARCH}) from ${UPSTREAM_TAG}"

ASSET="AdGuardHome_linux_${ARCH}.tar.gz"
DOWNLOAD_URL="https://github.com/AdguardTeam/AdGuardHome/releases/download/${UPSTREAM_TAG}/${ASSET}"
echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL -o "$WORK_DIR/agh.tar.gz" "$DOWNLOAD_URL"

mkdir -p "$WORK_DIR/extracted"
tar xzf "$WORK_DIR/agh.tar.gz" -C "$WORK_DIR/extracted"
EXTRACTED_BIN="$WORK_DIR/extracted/AdGuardHome/AdGuardHome"
[ -f "$EXTRACTED_BIN" ] || { echo "AdGuardHome binary not found in tarball" >&2; exit 1; }

mkdir -p "$ROOTFS/bin"
cp "$EXTRACTED_BIN" "$ROOTFS/bin/AdGuardHome"
chmod +x "$ROOTFS/bin/AdGuardHome"

# Static wrapper script — unrelated to upstream version.
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
