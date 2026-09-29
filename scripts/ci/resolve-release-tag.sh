#!/bin/bash
set -euo pipefail

# Adapted from conversun/fnos-apps' scripts/ci/resolve-release-tag.sh.
# Differences from the original:
#   - Drops fpk_version (that was for fnOS's .fpk filename versioning;
#     ugcli's upk versioning works differently — see build_num below).
#   - Adds build_num: a monotonically-increasing count across ALL of this
#     app's previous tags (any version), used as `ugcli pack --build N`.
#     ugcli requires the build number to strictly increase within the same
#     x.y.z and never repeat — deriving it from "how many releases this app
#     has ever had" satisfies that trivially regardless of how many
#     distinct upstream versions or manual -rN revisions came before.

APP_SLUG="${1:-}"
VERSION="${2:-}"
EVENT_NAME="${3:-}"
REVISION="${4:-}"

error() {
  echo "[ERROR] $1" >&2
  exit 1
}

emit_output() {
  local key="$1"
  local value="$2"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    echo "${key}=${value}" >> "${GITHUB_OUTPUT}"
  else
    echo "${key}=${value}"
  fi
}

[ -z "${APP_SLUG}" ] && error "APP_SLUG is required"
[ -z "${VERSION}" ] && error "VERSION is required"
[ -z "${EVENT_NAME}" ] && error "EVENT_NAME is required"

BASE_TAG="${APP_SLUG}/v${VERSION}"

# ---- build_num: must never repeat a number ugcli has already handed out ----
# A pure "tag count + 1" used to guarantee that trivially, but the release
# job's cleanup step deletes superseded -rN tags (--cleanup-tag), so the
# count goes DOWN between releases and numbers get reused — magicpush
# shipped 0003 twice (v1.14.0-r6 and -r7) exactly this way. Release asset
# filenames survive for every kept revision and encode the build number
# (<arch>[_<infix>]_<appid>_<x.y.z.N>.upk), so take the max of the tag
# count and every build number still visible in assets, then add one.
ALL_APP_TAGS=$(git ls-remote --tags origin "refs/tags/${APP_SLUG}/*" 2>/dev/null | \
  awk '{print $2}' | sed 's|^refs/tags/||' | sed 's|\^{}$||' | sort -u || true)
EXISTING_TAG_COUNT=$(printf '%s\n' "${ALL_APP_TAGS}" | grep -c . || true)

APP_ID="${APP_ID:-}"
META_FILE="scripts/apps/${APP_SLUG}/meta.env"
if [ -f "${META_FILE}" ]; then
  # shellcheck disable=SC1090
  . "${META_FILE}"
fi
# Escape dots for grep -E; empty APP_ID (no meta.env) degrades to matching
# every .upk in the repo — an over-count, which is the safe direction.
APP_ID_RE=$(printf '%s' "${APP_ID}" | sed 's/\./\\./g')
MAX_ASSET_BUILD=$(gh api --paginate 'repos/{owner}/{repo}/releases' --jq '.[].assets[].name' 2>/dev/null \
  | grep -E "${APP_ID_RE}.*\.upk$" \
  | sed -E 's/.*\.([0-9]+)\.upk$/\1/' \
  | sort -n | tail -1 || true)
# 10# forces base-10: asset names zero-pad ("0003"), and $(( )) would read
# those as octal (0008 → error, 0010 → 8).
MAX_ASSET_BUILD=$((10#${MAX_ASSET_BUILD:-0}))
BUILD_NUM=$((EXISTING_TAG_COUNT > MAX_ASSET_BUILD ? EXISTING_TAG_COUNT : MAX_ASSET_BUILD + 1))

# ---- Existing tags for THIS specific version (base tag + any -rN) ----
# Tag names can contain regex-special characters, so exact string
# membership is used instead of grep -E to avoid false negatives.
EXISTING_TAGS=$(
  {
    git ls-remote --tags origin "refs/tags/${BASE_TAG}" "refs/tags/${BASE_TAG}-r*" 2>/dev/null | \
      awk '{print $2}' | sed 's|^refs/tags/||' | sed 's|\^{}$||'
    gh release list --limit 1000 --json tagName -q ".[] | .tagName | select(startswith(\"${APP_SLUG}/v${VERSION}\"))" 2>/dev/null
  } | awk -v base="${BASE_TAG}" '$0==base || index($0, base"-r")==1' | sort -u
)

if [ -n "${REVISION}" ]; then
  RELEASE_TAG="${BASE_TAG}-${REVISION}"
  echo "Manual revision specified: ${RELEASE_TAG}"
elif [ "${EVENT_NAME}" = "schedule" ]; then
  if [ -n "${EXISTING_TAGS}" ]; then
    # Scheduled runs are for NEW upstream versions only — skip if this
    # version (possibly as -rN after cleanup deleted the base tag) is
    # already released.
    echo "Scheduled run: version ${VERSION} already released (${EXISTING_TAGS}), skipping"
    emit_output "release_tag" "${BASE_TAG}"
    emit_output "should_build" "false"
    emit_output "build_num" "${BUILD_NUM}"
    exit 0
  fi
  RELEASE_TAG="${BASE_TAG}"
  echo "Scheduled run: new version ${RELEASE_TAG}"
else
  if [ -n "${EXISTING_TAGS}" ]; then
    HIGHEST_REV=$(
      printf '%s\n' "${EXISTING_TAGS}" | awk -v base="${BASE_TAG}" '
        index($0, base"-r")==1 {
          rev = substr($0, length(base) + 3)   # strip prefix + "-r"
          if (rev ~ /^[0-9]+$/) print rev + 0
        }
      ' | sort -n | tail -1
    )
    if [ -n "${HIGHEST_REV}" ]; then
      NEXT_REV=$((HIGHEST_REV + 1))
    else
      NEXT_REV=1
    fi
    RELEASE_TAG="${BASE_TAG}-r${NEXT_REV}"
    echo "Version exists, using revision: ${RELEASE_TAG}"
  else
    RELEASE_TAG="${BASE_TAG}"
    echo "New version: ${RELEASE_TAG}"
  fi
fi

if gh release view "${RELEASE_TAG}" &>/dev/null; then
  SHOULD_BUILD="false"
  echo "Release ${RELEASE_TAG} already exists, skipping"
else
  SHOULD_BUILD="true"
fi

emit_output "release_tag" "${RELEASE_TAG}"
emit_output "should_build" "${SHOULD_BUILD}"
emit_output "build_num" "${BUILD_NUM}"
