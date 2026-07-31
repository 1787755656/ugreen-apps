#!/bin/bash
set -euo pipefail

# Usage: fetch-changelog-section.sh <changelog-raw-url> <version>
#
# Prints the section of a Keep-a-Changelog-style file that documents <version>,
# to stdout (empty if not found). For upstreams that keep their changelog in a
# file instead of publishing GitHub Releases — magicpush is one: it has no
# releases and no tags at all, so fetch-upstream-notes.sh always comes back
# empty and the release body ends up saying "无说明源".
#
# Recognised heading shapes (the leading ## level is not enforced, some files
# use ###):
#
#   ## [1.14.0] - 2026-07-30
#   ## 1.14.0
#   ## v1.14.0 (2026-07-30)
#
# The section runs until the next heading of the same kind. A trailing "---"
# rule (used as a separator between entries) is dropped.

URL="${1:-}"
VERSION="${2:-}"

if [ -z "$URL" ] || [ -z "$VERSION" ]; then
  exit 0
fi

BODY=$(curl -sfL --max-time 30 "$URL" 2>/dev/null || true)
if [ -z "$BODY" ]; then
  echo "changelog fetch failed or empty: $URL" >&2
  exit 0
fi

# Match the version literally — the dots must not act as regex wildcards, or
# 1.14.0 would also match a hypothetical 1x14y0.
printf '%s\n' "$BODY" | awk -v want="$VERSION" '
  function heading_level(line,   m) {
    if (line !~ /^#+[[:space:]]/) return 0
    match(line, /^#+/)
    return RLENGTH
  }
  function heading_version(line,   s) {
    s = line
    sub(/^#+[[:space:]]*/, "", s)          # drop the #s
    sub(/^\[/, "", s)                      # drop a leading [
    sub(/^v/, "", s)                       # drop a leading v
    sub(/[^0-9.].*$/, "", s)               # keep just the numeric version
    sub(/\.$/, "", s)                      # a trailing dot is punctuation, not version
    return s
  }
  {
    lvl = heading_level($0)
    if (lvl > 0) {
      if (!grab && heading_version($0) == want) {
        grab = 1
        want_lvl = lvl                     # 标题本身不输出，外层已经写了版本号
        next
      }
      # 停在下一个【同级或更高级】的标题。只认版本号标题是不够的：
      # 文件末尾常有 "## 版本说明" 这种非版本小节，不停就会把它一起抓进来。
      if (grab && lvl <= want_lvl) exit
    }
    if (grab) print
  }
' | awk '
  # 去掉段落首尾的空行和分隔线 ---
  { lines[NR] = $0 }
  END {
    start = 1; end = NR
    while (start <= end && (lines[start] ~ /^[[:space:]]*$/)) start++
    while (end >= start && (lines[end] ~ /^[[:space:]]*$/ || lines[end] ~ /^-{3,}[[:space:]]*$/)) end--
    for (i = start; i <= end; i++) print lines[i]
  }
'
