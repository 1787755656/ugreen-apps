#!/bin/bash
set -euo pipefail

# DeepSeek Harness（DSH）是 Docker 型应用：包里内置 smanx/deepseek-harness
# 镜像（官方 npm 包 @deepseek-ai/dsh + Node 反代），版本探测以【镜像】为准，
# 不查 DSH 官方 release —— 镜像的重打节奏由 smanx 手动触发，可能滞后于
# DSH 官方发版（DSH 已发 0.2.2-alpha 时镜像还停在 0.2.1-rc.4，属正常）。
#
# 判据（qbittorrent-ng6 同款）：Docker Hub tags API 里 `devtools-min-latest`
# 与它的版本号 tag `devtools-min-<version>` 共享同一个 manifest digest，
# 按 digest 反查版本 —— 比"按 last_updated 取最新"准，上游补发旧分支镜像
# 不会选错。打包钉到具体版本 tag，不用 latest。
#
# 版本号形如 0.2.1-rc.4（上游目前只发 rc/alpha 预发布）。project.yaml 的
# version 只能是 x.y.z，所以：
#   VERSION         = 0.2.1-rc.4   （release tag 与镜像 tag 用全称）
#   PROJECT_VERSION = 0.2.1        （同一 x.y.z 的不同 rc 靠构建号递增区分，
#                                    构建号由 resolve-release-tag.sh 单调分配）
#
# UPSTREAM_TAG 指向 DSH 官方仓库的 release tag（dsh-v<VERSION>），供
# release 步骤抓上游更新说明。

INPUT_VERSION="${1:-}"
HUB_API="https://hub.docker.com/v2/repositories/smanx/deepseek-harness/tags"
FAMILY_PREFIX="devtools-min-"

if [ -n "$INPUT_VERSION" ]; then
  # 手动指定版本：接受全称（0.2.1-rc.4）、镜像 tag（devtools-min-0.2.1-rc.4）
  # 或 project 短版（0.2.1，按当前 latest 的 rc 补全交给 build.sh 断言）。
  VERSION="$INPUT_VERSION"
  case "$VERSION" in
    "$FAMILY_PREFIX"*) VERSION="${VERSION#"$FAMILY_PREFIX"}" ;;
  esac
else
  echo "==> Querying Docker Hub tags API (version detection)"
  # devtools-min-latest 与每个发布批次同时推送，恒在第一页；按 digest 反查
  TAGS_JSON=$(curl -fsS "${HUB_API}?page_size=100")
  LATEST_DIGEST=$(printf '%s' "$TAGS_JSON" \
    | jq -r '.results[] | select(.name == "devtools-min-latest") | .digest' | head -1)
  if [ -z "$LATEST_DIGEST" ]; then
    echo "::error::Docker Hub 上找不到 devtools-min-latest tag" >&2
    exit 1
  fi
  # 注意：devtools-min-latest 自己也以 devtools-min- 开头且 digest 必然相同，
  # 必须排除 -latest 结尾的 tag，否则反查到的就是它自己
  VERSION=$(printf '%s' "$TAGS_JSON" \
    | jq -r --arg d "$LATEST_DIGEST" \
      '.results[] | select((.name | startswith("'"$FAMILY_PREFIX"'")) and (.name | endswith("-latest") | not) and .digest == $d) | .name' \
    | sed "s/^${FAMILY_PREFIX}//" | head -1)
fi

if [ -z "$VERSION" ] || [ "$VERSION" = "null" ]; then
  echo "Failed to resolve version for deepseek-harness" >&2
  exit 1
fi

if ! printf '%s' "$VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.]+)?$'; then
  echo "::error::镜像版本号不符合预期格式: '${VERSION}'（期望形如 0.2.1-rc.4）" >&2
  exit 1
fi

# project.yaml 的 version 只要 x.y.z（去掉 -rc.N / -alpha.N 后缀）
PROJECT_VERSION="${VERSION%%-*}"
if ! printf '%s' "$PROJECT_VERSION" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "::error::无法从 '${VERSION}' 得到合法的 x.y.z（得到 '${PROJECT_VERSION}'）" >&2
  exit 1
fi

UPSTREAM_TAG="dsh-v${VERSION}"

echo "VERSION=$VERSION"
echo "PROJECT_VERSION=$PROJECT_VERSION"
echo "UPSTREAM_TAG=$UPSTREAM_TAG"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  echo "version=$VERSION" >> "$GITHUB_OUTPUT"
  echo "project_version=$PROJECT_VERSION" >> "$GITHUB_OUTPUT"
  echo "upstream_tag=$UPSTREAM_TAG" >> "$GITHUB_OUTPUT"
fi
