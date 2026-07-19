#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 48.2> <arch: amd64|arm64>
# Downloads the official "-linux-all" tarball from pymumu/smartdns releases.
# That tarball is self-contained: musl-linked smartdns + bundled loader/libs
# (usr/local/lib/smartdns/) + the smartdns_ui.so web UI plugin and its
# wwwroot frontend (usr/share/smartdns/wwwroot/). The whole lib dir must be
# kept together — the binary's ELF interpreter is a RELATIVE path
# (lib/ld-musl-*.so.1), which is why launching goes through run-smartdns
# (it cd's into the dir first).
#
# Note: the asset filename embeds a datestamp (smartdns.1.YYYY.MM.DD-HHMM.
# <arch>-linux-all.tar.gz) that cannot be derived from the tag, so the URL
# is resolved from the release's asset list via the GitHub API.

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"
UPSTREAM_TAG="Release${VERSION}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) SD_ARCH="x86_64" ;;
  arm64) SD_ARCH="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building SmartDNS (${SD_ARCH}) from ${UPSTREAM_TAG}"

# endswith is unambiguous: the x86 (32-bit) asset ends in "x86-linux-all",
# which does not match "x86_64-linux-all".
DOWNLOAD_URL=$(curl -sL "https://api.github.com/repos/pymumu/smartdns/releases/tags/${UPSTREAM_TAG}" | \
  jq -r --arg suffix "${SD_ARCH}-linux-all.tar.gz" \
    '.assets[] | select(.name | endswith($suffix)) | .browser_download_url' | head -1)
[ -n "$DOWNLOAD_URL" ] && [ "$DOWNLOAD_URL" != "null" ] || {
  echo "No ${SD_ARCH}-linux-all.tar.gz asset found in ${UPSTREAM_TAG}" >&2; exit 1
}

echo "==> Downloading: ${DOWNLOAD_URL}"
curl -fL -o "$WORK_DIR/smartdns.tar.gz" "$DOWNLOAD_URL"

mkdir -p "$WORK_DIR/extracted"
tar xzf "$WORK_DIR/smartdns.tar.gz" -C "$WORK_DIR/extracted"
PAYLOAD="$WORK_DIR/extracted/smartdns"

[ -f "$PAYLOAD/usr/local/lib/smartdns/smartdns" ] || { echo "smartdns binary missing in tarball" >&2; exit 1; }
[ -f "$PAYLOAD/usr/local/lib/smartdns/smartdns_ui.so" ] || { echo "smartdns_ui.so missing in tarball" >&2; exit 1; }
[ -d "$PAYLOAD/usr/share/smartdns/wwwroot" ] || { echo "wwwroot missing in tarball" >&2; exit 1; }

rm -rf "$ROOTFS"
mkdir -p "$ROOTFS/bin"

cp -R "$PAYLOAD/usr/local/lib/smartdns" "$ROOTFS/smartdns"
cp -R "$PAYLOAD/usr/share/smartdns/wwwroot" "$ROOTFS/wwwroot"
# Upstream's fully-commented reference config, for users editing conf/smartdns.conf.
cp "$PAYLOAD/etc/smartdns/smartdns.conf" "$ROOTFS/smartdns.conf.sample"
chmod +x "$ROOTFS/smartdns/smartdns" "$ROOTFS/smartdns/run-smartdns"

# Static wrapper script — unrelated to upstream version.
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
ls -lh "$ROOTFS" "$ROOTFS/bin" "$ROOTFS/smartdns"
