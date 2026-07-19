#!/bin/bash
set -euo pipefail

# natfrp-service has no versioned download URL and no GitHub releases to
# poll — the vendor URL below always serves "whatever is currently latest"
# with no version string anywhere in the path or filename. Rather than
# downloading the full ~12MB tarball on every check just to hash it, this
# uses the server's own `Last-Modified` header (confirmed present via
# `curl -I`) as the change signal, converted to a plain YYYY.M.D version:
#   - It's already a valid x.y.z shape for project.yaml's version field.
#   - Unchanged upstream content -> identical Last-Modified -> identical
#     VERSION -> scripts/ci/resolve-release-tag.sh's existing git-tag
#     dedup check sees the tag already exists and skips the rebuild, with
#     no separate state file needed — same generic mechanism every other
#     app already relies on, just fed a date instead of a real semver.

INPUT_VERSION="${1:-}"
LAUNCHER_URL="https://nya.globalslb.net/natfrp/client/launcher-unix/latest/natfrp-service_linux_amd64.tar.zst"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
else
  LAST_MODIFIED=$(curl -fsSI "$LAUNCHER_URL" | grep -i '^last-modified:' | sed -E 's/^[Ll]ast-[Mm]odified: *//' | tr -d '\r\n')
  if [ -z "$LAST_MODIFIED" ]; then
    echo "Failed to read Last-Modified header for natfrp" >&2
    exit 1
  fi

  # GNU date (Linux/CI) vs BSD date (macOS local dev) parse the same
  # RFC 1123 format differently — try GNU first, fall back to BSD.
  if date -u -d "$LAST_MODIFIED" "+%Y.%m.%d" >/dev/null 2>&1; then
    RAW_DATE=$(date -u -d "$LAST_MODIFIED" "+%Y.%m.%d")
  else
    RAW_DATE=$(date -u -j -f "%a, %d %b %Y %H:%M:%S %Z" "$LAST_MODIFIED" "+%Y.%m.%d")
  fi

  # Strip any leading zeros from month/day (e.g. "2026.03.31" -> "2026.3.31")
  # so it reads as plain decimal integers, not octal-looking padded fields.
  IFS='.' read -r Y M D <<< "$RAW_DATE"
  VERSION="$((10#$Y)).$((10#$M)).$((10#$D))"
fi

if [ -z "$VERSION" ]; then
  echo "Failed to resolve version for natfrp" >&2
  exit 1
fi

# Already a valid x.y.z shape — no separate truncation needed for project.yaml.
PROJECT_VERSION="$VERSION"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
fi
