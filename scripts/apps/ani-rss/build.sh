#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 3.1.78> <arch: amd64|arm64>
# Assembles rootfs_<arch>/ with:
#   app/ani-rss.jar
#   jre/  (Temurin JRE, glibc linux) + java shell wrapper
#   locale/C.utf8... (Debian libc-bin, for Chinese path encoding)
#   bin/start.sh

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"
UPSTREAM_TAG="v${VERSION}"

# Match official Docker (eclipse-temurin:26-jre). Override with JRE_MAJOR=25 etc.
JRE_MAJOR="${JRE_MAJOR:-26}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64)
    ADOPTIUM_ARCH="x64"
    DEB_ARCH="amd64"
    ;;
  arm64)
    ADOPTIUM_ARCH="aarch64"
    DEB_ARCH="arm64"
    ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building ass/ANI-RSS ${UPSTREAM_TAG} for ${ARCH}"

# ---- 1. jar ----
JAR_URL="https://github.com/wushuo894/ani-rss/releases/download/${UPSTREAM_TAG}/ani-rss.jar"
echo "==> Downloading jar: ${JAR_URL}"
curl -fL --retry 3 -A "Mozilla/5.0" -o "$WORK_DIR/ani-rss.jar" "$JAR_URL"
file "$WORK_DIR/ani-rss.jar" | grep -qi 'java\|zip\|archive' || {
  echo "unexpected jar content" >&2
  file "$WORK_DIR/ani-rss.jar" >&2
  exit 1
}

# ---- 2. Temurin JRE (not JDK) ----
JRE_URL="https://api.adoptium.net/v3/binary/latest/${JRE_MAJOR}/ga/linux/${ADOPTIUM_ARCH}/jre/hotspot/normal/eclipse?project=jdk"
echo "==> Downloading Temurin JRE ${JRE_MAJOR} ${ADOPTIUM_ARCH}"
curl -fL --retry 3 -A "Mozilla/5.0" -o "$WORK_DIR/jre.tar.gz" "$JRE_URL"
mkdir -p "$WORK_DIR/jre"
tar -xzf "$WORK_DIR/jre.tar.gz" -C "$WORK_DIR/jre" --strip-components=1
[ -x "$WORK_DIR/jre/bin/java" ] || { echo "jre/bin/java missing" >&2; exit 1; }
if [ -e "$WORK_DIR/jre/bin/javac" ]; then
  echo "ERROR: got JDK (javac present), need JRE only" >&2
  exit 1
fi
file "$WORK_DIR/jre/bin/java"
# expect Linux ELF + glibc interpreter
file "$WORK_DIR/jre/bin/java" | grep -q 'ELF' || { echo "java is not ELF" >&2; exit 1; }

# ---- 3. Debian C.utf8 locale (Chinese path fix) ----
# Try several libc-bin versions; pool filenames change over time.
fetch_locale() {
  local deb_arch="$1"
  local pool="https://deb.debian.org/debian/pool/main/g/glibc"
  local candidates=(
    "libc-bin_2.36-9+deb12u14_${deb_arch}.deb"
    "libc-bin_2.36-9+deb12u13_${deb_arch}.deb"
    "libc-bin_2.36-9+deb12u12_${deb_arch}.deb"
    "libc-bin_2.36-9+deb12u10_${deb_arch}.deb"
  )
  # Also try scraping pool index for latest bookworm-ish 2.36
  local listed
  listed=$(curl -fsSL "$pool/" | grep -oE "libc-bin_2\\.36-9\\+deb12u[0-9]+_${deb_arch}\\.deb" | sort -u | tail -3 || true)
  if [ -n "$listed" ]; then
    while IFS= read -r name; do
      candidates+=("$name")
    done <<< "$listed"
  fi

  local deb=""
  local tried=()
  for name in "${candidates[@]}"; do
    [ -n "$name" ] || continue
    # skip duplicates
    local skip=0
    for t in "${tried[@]+"${tried[@]}"}"; do [ "$t" = "$name" ] && skip=1 && break; done
    [ "$skip" = 1 ] && continue
    tried+=("$name")
    echo "==> try locale deb: $name"
    if curl -fL --retry 2 -A "Mozilla/5.0" -o "$WORK_DIR/libc-bin.deb" "$pool/$name"; then
      deb="$name"
      break
    fi
  done
  [ -n "$deb" ] || { echo "failed to download libc-bin for locale" >&2; exit 1; }

  mkdir -p "$WORK_DIR/deb"
  (
    cd "$WORK_DIR/deb"
    ar x "$WORK_DIR/libc-bin.deb"
    tar -xf data.tar.*
  )
  [ -d "$WORK_DIR/deb/usr/lib/locale/C.utf8" ] || {
    echo "C.utf8 not found in deb" >&2
    find "$WORK_DIR/deb" -type d -name '*utf8*' | head >&2 || true
    exit 1
  }
}

fetch_locale "$DEB_ARCH"

# ---- 4. assemble rootfs (clean) ----
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS/bin" "$ROOTFS/app" "$ROOTFS/jre" "$ROOTFS/locale"

cp -f "$WORK_DIR/ani-rss.jar" "$ROOTFS/app/ani-rss.jar"
echo "{\"version\":\"${VERSION}\",\"jre\":\"temurin-${JRE_MAJOR}-jre\"}" > "$ROOTFS/app/version.json"

# JRE tree; rename real java then install wrapper
cp -a "$WORK_DIR/jre/." "$ROOTFS/jre/"
mv "$ROOTFS/jre/bin/java" "$ROOTFS/jre/bin/java.real"
cp -f "$SCRIPT_DIR/static/java-wrapper.sh" "$ROOTFS/jre/bin/java"
chmod +x "$ROOTFS/jre/bin/java" "$ROOTFS/jre/bin/java.real"

# locale: entity copies only (ugcli rejects symlinks)
cp -a "$WORK_DIR/deb/usr/lib/locale/C.utf8" "$ROOTFS/locale/C.utf8"
cp -a "$WORK_DIR/deb/usr/lib/locale/C.utf8" "$ROOTFS/locale/C.UTF-8"
cp -a "$WORK_DIR/deb/usr/lib/locale/C.utf8" "$ROOTFS/locale/en_US.utf8"
cp -a "$WORK_DIR/deb/usr/lib/locale/C.utf8" "$ROOTFS/locale/en_US.UTF-8"

cp -f "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/start.sh"

echo "==> Done: $ROOTFS"
du -sh "$ROOTFS" "$ROOTFS/jre" "$ROOTFS/app" "$ROOTFS/locale"
file "$ROOTFS/jre/bin/java.real"
file "$ROOTFS/jre/bin/java"
ls -la "$ROOTFS/bin" "$ROOTFS/locale"
