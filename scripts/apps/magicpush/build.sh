#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 1.13.0> <arch: amd64|arm64>
#
# MagicPush is a Node.js app (express server + vite web frontend) with no
# upstream binary releases, so this script builds everything from the main
# branch source and bundles an official Node runtime:
#   rootfs_<arch>/bin/node          — official nodejs.org static build
#   rootfs_<arch>/bin/start.sh      — static wrapper (scripts/.../static/)
#   rootfs_<arch>/app/server/       — package.json + src + prod node_modules
#   rootfs_<arch>/app/web/dist      — vite build output
#   rootfs_<arch>/app/version.json  — upstream version manifest
#
# Cross-arch note: the only native dependency is better-sqlite3, which
# ships prebuilt linux-x64/arm64 binaries on its GitHub releases. Install
# scripts are deliberately disabled (npm ≥12 blocks them by default anyway,
# and prebuild-install is deprecated) — the prebuilt .node is downloaded
# directly by URL instead. The ELF check fails the build if the tarball
# layout or arch coverage ever changes.

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

# Pinned Node runtime (bump deliberately; must stay on an ABI that
# better-sqlite3 ships prebuilds for). NODE_ABI must match NODE_VERSION's
# major (node 20 = ABI 115) — it selects the better-sqlite3 prebuild.
NODE_VERSION="20.18.1"
NODE_ABI="115"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

ROOTFS="$REPO_ROOT/$PROJECT_DIR/rootfs_${ARCH}"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

case "$ARCH" in
  amd64) NODE_ARCH="x64";   ELF_MACHINE="x86-64" ;;
  arm64) NODE_ARCH="arm64"; ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

echo "==> Building MagicPush ${VERSION} (${NODE_ARCH}) from main branch"

# ---- 1. upstream source (main branch tarball) ----
SRC_URL="https://github.com/magiccode1412/magicpush/archive/refs/heads/main.tar.gz"
echo "==> Downloading source: ${SRC_URL}"
curl -fL -o "$WORK_DIR/src.tar.gz" "$SRC_URL"
mkdir -p "$WORK_DIR/src"
tar xzf "$WORK_DIR/src.tar.gz" -C "$WORK_DIR/src" --strip-components=1

# Sanity: the source we fetched should match the version we resolved —
# main may have moved between the version check and this download.
SRC_VERSION=$(jq -r '.version' "$WORK_DIR/src/version.json")
if [ "$SRC_VERSION" != "$VERSION" ]; then
  echo "::warning::main branch version.json ($SRC_VERSION) != resolved version ($VERSION); packaging $SRC_VERSION"
fi

# ---- 2. web frontend (vite build, runs on the CI host arch) ----
echo "==> Building web frontend"
(
  cd "$WORK_DIR/src/web"
  # pnpm-lock.yaml is upstream's lockfile; corepack ships with Node ≥16.
  corepack enable >/dev/null 2>&1 || true
  if command -v pnpm >/dev/null 2>&1; then
    pnpm install --frozen-lockfile || pnpm install
    pnpm run build
  else
    npm install --no-audit --no-fund
    npm run build
  fi
)
[ -d "$WORK_DIR/src/web/dist" ] || { echo "web build produced no dist/" >&2; exit 1; }

# ---- 3. server prod dependencies (target arch) ----
echo "==> Installing server production dependencies (target: linux/${NODE_ARCH})"
(
  cd "$WORK_DIR/src/server"
  # --ignore-scripts: deterministic across npm versions (npm ≥12 blocks
  # install scripts by default anyway). The only script that matters is
  # better-sqlite3's prebuild fetch, replaced by the direct download below.
  npm install --omit=dev --no-audit --no-fund --ignore-scripts
)

# better-sqlite3 prebuilt binary for the TARGET arch, fetched by URL.
BSQL_DIR="$WORK_DIR/src/server/node_modules/better-sqlite3"
BSQL_VER=$(jq -r '.version' "$BSQL_DIR/package.json")
BSQL_URL="https://github.com/WiseLibs/better-sqlite3/releases/download/v${BSQL_VER}/better-sqlite3-v${BSQL_VER}-node-v${NODE_ABI}-linux-${NODE_ARCH}.tar.gz"
echo "==> Downloading better-sqlite3 prebuild: ${BSQL_URL}"
curl -fL "$BSQL_URL" | tar xz -C "$BSQL_DIR"
SQLITE_NODE="$BSQL_DIR/build/Release/better_sqlite3.node"
[ -f "$SQLITE_NODE" ] || { echo "better-sqlite3 native binary missing after prebuild download" >&2; exit 1; }
if ! file "$SQLITE_NODE" | grep -q "ELF.*${ELF_MACHINE}"; then
  echo "better-sqlite3 binary is not linux/${ELF_MACHINE}: $(file "$SQLITE_NODE")" >&2
  exit 1
fi

# ---- 4. Node runtime ----
NODE_TARBALL="node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz"
echo "==> Downloading Node runtime: ${NODE_TARBALL}"
curl -fL -o "$WORK_DIR/$NODE_TARBALL" "https://nodejs.org/dist/v${NODE_VERSION}/${NODE_TARBALL}"
mkdir -p "$WORK_DIR/node"
tar xJf "$WORK_DIR/$NODE_TARBALL" -C "$WORK_DIR/node" --strip-components=1

# ---- 5. assemble rootfs ----
echo "==> Assembling $ROOTFS"
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS/bin" "$ROOTFS/app/web"
cp "$WORK_DIR/node/bin/node" "$ROOTFS/bin/node"
cp "$SCRIPT_DIR/static/start.sh" "$ROOTFS/bin/start.sh"
chmod +x "$ROOTFS/bin/node" "$ROOTFS/bin/start.sh"
cp -R "$WORK_DIR/src/web/dist" "$ROOTFS/app/web/dist"
mkdir -p "$ROOTFS/app/server"
cp "$WORK_DIR/src/server/package.json" "$ROOTFS/app/server/"
cp -R "$WORK_DIR/src/server/src" "$ROOTFS/app/server/src"
cp -R "$WORK_DIR/src/server/node_modules" "$ROOTFS/app/server/node_modules"
cp "$WORK_DIR/src/version.json" "$ROOTFS/app/version.json"

echo "==> Done: $ROOTFS"
du -sh "$ROOTFS"
