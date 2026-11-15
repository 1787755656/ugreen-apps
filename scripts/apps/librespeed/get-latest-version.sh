#!/bin/bash
set -euo pipefail

# LibreSpeed（speedtest-go）版本探测：上游 github.com/librespeed/speedtest-go
# 发标准 GitHub Release，tag 形如 v1.1.6 —— 去掉 v 正好是 project.yaml 要的
# x.y.z（ugcli 对中段“最多两位数”的限制这里天然满足：minor=1）。
#
# 上游只发 tag 不带预发布后缀，所以不需要 BASE_VERSION_ONLY 归一化。

INPUT_VERSION="${1:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

if [ -n "$INPUT_VERSION" ]; then
  VERSION="${INPUT_VERSION#v}"
else
  TAG=$(bash "$REPO_ROOT/scripts/ci/github-latest-release-tag.sh" "librespeed/speedtest-go")
  VERSION="${TAG#v}"
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for librespeed" >&2
  exit 1
fi

# 上游 tag 就是 x.y.z，project.yaml 直接用
PROJECT_VERSION="$VERSION"
UPSTREAM_TAG="v$VERSION"

echo "version=$VERSION"
echo "project_version=$PROJECT_VERSION"
echo "upstream_tag=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
