#!/bin/bash
set -euo pipefail

# Usage: build.sh <version, unused> <arch: amd64|arm64>
# version is accepted for interface consistency with the other apps'
# build.sh but unused here — the download URL has no version in it at all
# (see get-latest-version.sh's comment for why).

ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

echo "==> Building natfrp-service (${ARCH})"

LAUNCHER_URL="https://nya.globalslb.net/natfrp/client/launcher-unix/latest/natfrp-service_linux_${ARCH}.tar.zst"
echo "==> Downloading: ${LAUNCHER_URL}"
curl -fL -o "$WORK_DIR/service.tar.zst" "$LAUNCHER_URL"

zstd -d -f -q "$WORK_DIR/service.tar.zst" -o "$WORK_DIR/service.tar"
mkdir -p "$WORK_DIR/extracted"
tar -xf "$WORK_DIR/service.tar" -C "$WORK_DIR/extracted"

[ -f "$WORK_DIR/extracted/natfrp-service" ] || { echo "natfrp-service binary not found in archive" >&2; exit 1; }
[ -f "$WORK_DIR/extracted/frpc" ] || { echo "frpc binary not found in archive" >&2; exit 1; }

mkdir -p "$ROOTFS/bin"
cp "$WORK_DIR/extracted/natfrp-service" "$ROOTFS/bin/natfrp-service"
cp "$WORK_DIR/extracted/frpc" "$ROOTFS/bin/frpc"
chmod +x "$ROOTFS/bin/natfrp-service" "$ROOTFS/bin/frpc"

# Static wrapper script — unrelated to upstream version, see
# scripts/apps/natfrp/static/start.sh's own comments. Notably it never
# overwrites config.json after first install, and hardcodes
# update_interval: -1 to disable natfrp's own self-update (the sandboxed
# install dir is read-only) — both must be preserved across rebuilds.
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS/bin"
