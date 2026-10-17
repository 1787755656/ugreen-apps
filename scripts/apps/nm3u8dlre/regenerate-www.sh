#!/bin/bash
# 从上游 www 重新生成入库的 rootfs_common/www（唯一来源，跑完应当零差异）。
#
# 上游：Youngxj/N_m3u8DL-RE-FN 的 m3u8_down/app/www（fnOS 版前端）
# 处理：patch-ui.py 改造（API 前缀 / 文案 / cloudwindow 引用 / 指纹）
#       + 换掉 fnOS 宿主 SDK（overlay/www/ugos-bridge.js → www/vendor/trim-app.js）
#       + 补绿联宿主桥 SDK（overlay/www/cloudwindow.js → www/cloudwindow.js）
#       + 删掉上游的 www/version（我们不用它，版本号读系统 config.json）
#
# 用法：
#   ./regenerate-www.sh /path/to/upstream/m3u8_down/app/www
#   ./regenerate-www.sh              # 自动从 GitHub 拉上游 tag（需要网络）
#
# 改前端流程：改 overlay/ 或 patch-ui.py → 跑本脚本 → 提交 www + overlay 两份。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

UI_TAG="${UI_TAG:-v0.6.0-beta.25}"   # 上游 fnOS 应用（界面来源）的 tag
DEST="$REPO_ROOT/$PROJECT_DIR/rootfs_common/www"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if [ $# -ge 1 ]; then
  SRC="$1"
else
  echo "==> 拉上游界面 $UI_UPSTREAM@$UI_TAG"
  curl -fL --retry 3 -o "$WORK/ui.tar.gz" \
    "https://codeload.github.com/${UI_UPSTREAM}/tar.gz/refs/tags/${UI_TAG}"
  tar xzf "$WORK/ui.tar.gz" -C "$WORK"
  SRC="$(find "$WORK" -maxdepth 3 -type d -path '*/m3u8_down/app/www' | head -1)"
  [ -n "$SRC" ] || { echo "上游包里没找到 m3u8_down/app/www" >&2; exit 1; }
fi
[ -d "$SRC" ] || { echo "找不到上游 www: $SRC" >&2; exit 1; }

rm -rf "$DEST"
mkdir -p "$DEST"
cp -R "$SRC"/. "$DEST"/
rm -f "$DEST/version"           # 上游的版本文件，本移植不用
find "$DEST" -name '*.gz' -delete   # gzip_static 会把陈旧 .gz 钉死

python3 "$SCRIPT_DIR/patch-ui.py" "$DEST"

install -m 0644 "$SCRIPT_DIR/overlay/www/ugos-bridge.js"   "$DEST/vendor/trim-app.js"
install -m 0644 "$SCRIPT_DIR/overlay/www/cloudwindow.js"   "$DEST/cloudwindow.js"

echo "==> 已生成 $DEST"
find "$DEST" -type f | sort
