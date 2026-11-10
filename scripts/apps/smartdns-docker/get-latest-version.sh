#!/bin/bash
set -euo pipefail

# 上游 tag 通常是 "ReleaseX.Y"（如 Release48.4），而 project.yaml 的 version
# 只接受纯数字 x.y.z，所以 PROJECT_VERSION 要补齐/截断到 3 段。
#
# 2026-11-09 上游把 Release48.5-RC1 发成了【正式】Release（prerelease=false），
# GitHub /releases/latest 于是返回它。旧实现只做 "cut -d. -f1-3 + 补 .0"，
# 得到 48.5-RC1.0，ugcli check 直接判非法版本（run 42674553263 失败）。
# 现在先剥掉所有非数字后缀再规范化：
#   48.4      -> 48.4.0
#   48.5-RC1  -> 48.5.0   （RC 编号不进 project.yaml）
#   48.5.2    -> 48.5.2
#   48        -> 48.0.0
# VERSION 仍保留完整上游版本（48.5-RC1）用于 release tag / 镜像 tag / 上游
# 资产定位；RC 与后续正式版之间靠递增的 build 号区分（与 octop 的 bN 同理）。

INPUT_VERSION="${1:-}"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="$INPUT_VERSION"
  UPSTREAM_TAG="Release${VERSION}"
else
  UPSTREAM_TAG=$(curl -sL ${GH_TOKEN:+-H "Authorization: Bearer $GH_TOKEN"} \
    "https://api.github.com/repos/pymumu/smartdns/releases/latest" | jq -r '.tag_name')
  VERSION=$(echo "$UPSTREAM_TAG" | sed -E 's/^Release//')
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for smartdns-docker" >&2
  exit 1
fi

# 1) 只保留开头的数字段，剥掉 -RC1 / -beta2 / -rc.4 之类的后缀。
#    上游若换成完全不带数字前缀的 tag（如 nightly），这里直接失败，
#    而不是把 nightly.0.0 写进 project.yaml 等 ugcli check 来报错。
NUMERIC=$(printf '%s' "$VERSION" | sed -E 's/^([0-9]+(\.[0-9]+)*).*$/\1/')
if [ -z "$NUMERIC" ] || ! printf '%s' "$NUMERIC" | LC_ALL=C grep -Eq '^[0-9]+(\.[0-9]+)*$'; then
  echo "::error::上游版本 '${VERSION}' 解析不出数字版本号（期望形如 48.4 / 48.5-RC1）" >&2
  exit 1
fi

# 2) 截到 3 段、不足补 .0，并断言结果是合法的 x.y.z
PROJECT_VERSION=$(printf '%s' "$NUMERIC" | cut -d. -f1-3)
while [ "$(printf '%s\n' "$PROJECT_VERSION" | awk -F. '{print NF}')" -lt 3 ]; do
  PROJECT_VERSION="${PROJECT_VERSION}.0"
done
if ! printf '%s' "$PROJECT_VERSION" | LC_ALL=C grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "::error::'${VERSION}' 归一化后不是合法的 x.y.z（得到 '${PROJECT_VERSION}'）" >&2
  exit 1
fi

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
