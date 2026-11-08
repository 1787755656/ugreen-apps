#!/bin/bash
set -euo pipefail

# Usage: build.sh <version e.g. 8.0.0> <arch: amd64|arm64>
#
# 上游资产：https://github.com/jason5ng32/MyIP tag tarball（v 前缀 tag）。
# 上游不发任何二进制/dist 资产 —— 前端 (vite) 与运行时依赖闭包都在本脚本
# 现场构建，配方与 myip-ugreen-app 独立仓库的 scripts/build.sh 同源
# （0002 双架构真机冒烟全绿；上过大版本时补丁断言会当场暴露行变动）。
#
# 包结构：
#   rootfs_common/app/     跨架构业务树（纯 JS）：backend-server.js +
#                          frontend-server.js + api/ common/ server/ + dist/
#                          + npm 生产依赖 + ugos-start.cjs 引导层
#   rootfs_<arch>/bin/node 官方 glibc Node 24 运行时（零编译依赖）
#
# start_cmd 不经过 shell（bin/node -e 内联入口 → app/ugos-start.cjs），
# 不需要 EXEC 权限；查 IP / 下载数据集必须出公网 → 仅 INTERNET 权限。

VERSION="${1:?VERSION is required}"
ARCH="${2:?ARCH is required (amd64|arm64)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/meta.env"

APPDIR="$REPO_ROOT/$PROJECT_DIR"
ROOTFS="$APPDIR/rootfs_${ARCH}"
COMMON="$APPDIR/rootfs_common"
WORK_DIR=$(mktemp -d)
trap 'rm -rf "$WORK_DIR"' EXIT

NODE_VER="26.13.0" # 上游 8.1.0 起 engines 收紧为 >=26.0.0，运行时跟着走 LTS；工具链如需下载也用它

case "$ARCH" in
  amd64) NARCH="x64";  ELF_MACHINE="x86-64"  ;;
  arm64) NARCH="arm64"; ELF_MACHINE="aarch64" ;;
  *) echo "Unsupported arch: $ARCH" >&2; exit 1 ;;
esac

CURL=(curl -fL --http1.1 --retry 5 --retry-all-errors --retry-delay 3 --connect-timeout 20)

# sed -i 在 GNU/BSD 上参数形状不同（CI=GNU，macOS 本地调试=BSD）
if sed --version >/dev/null 2>&1; then SED_I=(-i); else SED_I=(-i ''); fi

echo "==> Building MyIP ${VERSION} (${ARCH})"

# ---- 1. 上游源码（tag tarball，断言 tag 与内容版本一致） ----
echo "==> Downloading upstream source: v${VERSION}"
"${CURL[@]}" -o "$WORK_DIR/src.tar.gz" \
  "https://github.com/jason5ng32/MyIP/archive/refs/tags/v${VERSION}.tar.gz"
mkdir -p "$WORK_DIR/src"
tar xzf "$WORK_DIR/src.tar.gz" -C "$WORK_DIR/src"
SRC=$(ls -d "$WORK_DIR/src"/*/ | head -1)
SRC="${SRC%/}"
UPSTREAM_VER=$(node -p "require('$SRC/package.json').version")
[ "$UPSTREAM_VER" = "$VERSION" ] || {
  echo "::error::tag v${VERSION} 内容版本是 ${UPSTREAM_VER}，tag 与内容不一致" >&2; exit 1;
}
# 8.0.0 起后端从 api/ common/ 扩到 server/（数据集都在 server/datasets/）
for p in backend-server.js frontend-server.js api common server; do
  [ -e "$SRC/$p" ] || { echo "::error::上游缺 $p —— 结构变动，检查补丁与闭包扫描范围" >&2; exit 1; }
done

# ---- 2. 主机工具链（vite/pnpm 要新 Node；Linux 一律用下载的官方 Node） ----
HOST_NODE_OK=$(node -p "parseInt(process.versions.node) >= 22" 2>/dev/null || echo 0)
if [ "$HOST_NODE_OK" != "1" ]; then
  HOST_OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in
    x86_64|amd64) HOST_NARCH="x64" ;;
    arm64|aarch64) HOST_NARCH="arm64" ;;
    *) echo "Unsupported host arch: $(uname -m)" >&2; exit 1 ;;
  esac
  echo "==> Installing Node ${NODE_VER} for host toolchain"
  # linux-x64 的包与 amd64 运行时是同一份 —— 存公共名，第 9 步复用免重下
  if [ "$HOST_OS" = "linux" ] && [ "$HOST_NARCH" = "x64" ]; then
    HOST_TARBALL="$WORK_DIR/node-linux-x64.tar.xz"
  else
    HOST_TARBALL="$WORK_DIR/host-node.tar.xz"
  fi
  "${CURL[@]}" -o "$HOST_TARBALL" \
    "https://nodejs.org/dist/v${NODE_VER}/node-v${NODE_VER}-${HOST_OS}-${HOST_NARCH}.tar.xz"
  mkdir -p "$WORK_DIR/host-node"
  tar xJf "$HOST_TARBALL" -C "$WORK_DIR/host-node" --strip-components=1
  export PATH="$WORK_DIR/host-node/bin:$PATH"
fi
node --version

command -v pnpm >/dev/null 2>&1 || npm install -g --silent pnpm@12.14.0
pnpm --version

# ---- 3. 前端构建（上游 dist 不随发，现场 vite build） ----
echo "==> pnpm install (frozen lockfile)"
( cd "$SRC" && pnpm install --frozen-lockfile --prefer-offline )
echo "==> vite build"
( cd "$SRC" && pnpm run build )
[ -f "$SRC/dist/index.html" ] || { echo "::error::vite build 未产出 dist/index.html" >&2; exit 1; }

# ---- 4. 运行时依赖闭包（后端实际 import 的顶层包 ∩ 上游 prod 依赖） ----
echo "==> 计算运行时依赖闭包"
pnpm ls --prod --depth 0 --json --dir "$SRC" > "$WORK_DIR/prod-deps.json"
node -e '
const fs = require("fs");
const path = require("path");
const [srcDir, lsPath, outPath] = process.argv.slice(1);
const pkg = JSON.parse(fs.readFileSync(path.join(srcDir, "package.json"), "utf8"));
const ls = JSON.parse(fs.readFileSync(lsPath, "utf8"));
// pnpm ls --json 顶层是项目数组（单包项目取 [0]）
const proj = Array.isArray(ls) ? ls[0] : ls;
const parsed = {};
for (const [name, info] of Object.entries(proj.dependencies || {})) {
  parsed[name] = info.version;
}
// 后端运行时文件：两个 server 入口 + api/ common/ server/ 递归全扫
// （含静态 import 和 await import()）。前端 frontend/ 由 vite 打进 dist，
// 不进运行时树。全部按绝对路径收集。
const files = ["backend-server.js", "frontend-server.js"].map((f) => path.join(srcDir, f));
for (const d of ["api", "common", "server"]) {
  const walk = (dir) => {
    for (const f of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, f.name);
      if (f.isDirectory()) walk(p);
      else if (f.name.endsWith(".js")) files.push(p);
    }
  };
  walk(path.join(srcDir, d));
}
const BUILTIN = new Set(("assert,async_hooks,buffer,child_process,cluster,console,constants,crypto,dgram," +
  "diagnostics_channel,dns,domain,events,fs,http,http2,https,inspector,module,net,os,path,perf_hooks," +
  "process,punycode,querystring,readline,repl,stream,string_decoder,timers,tls,trace_events,tty,url," +
  "util,v8,vm,wasi,worker_threads,zlib").split(","));
const used = new Set();
for (const rel of files) {
  let code = fs.readFileSync(rel, "utf8");
  // 先剥模板字符串再剥注释：模板里的日志文案（8.1.0 guards.js 的
  // `from "${host}"`）会被下面的 import 正则误认成真实导入。真包名不会
  // 落在反引号里（模板型动态 import 本就不被引号正则捕获，剥掉无损失）。
  code = code.replace(/`(?:\\.|[^`\\])*`/g, " ");
  code = code.replace(/\/\*[\s\S]*?\*\//g, " ").replace(/(^|[^:])\/\/[^\n]*/g, "$1 ");
  const re = /(?:from\s+|\bimport\s+|\bimport\(\s*)(["\x27])([^"\x27]+)\1/g;
  let m;
  while ((m = re.exec(code))) {
    const spec = m[2];
    if (spec.startsWith(".") || spec.startsWith("node:")) continue;
    // 包名字符集兜底：剥离边界残留（含空格/${} 等的假 spec）一律不算 import
    if (!/^[@a-zA-Z0-9][@a-zA-Z0-9._/-]*$/.test(spec)) continue;
    const top = spec.startsWith("@") ? spec.split("/").slice(0, 2).join("/") : spec.split("/")[0];
    if (BUILTIN.has(top)) continue;
    used.add(top);
  }
}
const missing = [...used].filter((p) => !parsed[p]);
if (missing.length) throw new Error("import 到但 pnpm 解析集没有: " + missing.join(", "));
const deps = {};
for (const p of used) deps[p] = parsed[p];
const out = {
  name: pkg.name, version: pkg.version, private: pkg.private,
  type: pkg.type, engines: pkg.engines, dependencies: deps,
};
fs.writeFileSync(outPath, JSON.stringify(out, null, 2) + "\n");
console.log("runtime deps(后端 import 闭包):", Object.keys(deps).length, "→", Object.keys(deps).join(", "));
' "$SRC" "$WORK_DIR/prod-deps.json" "$WORK_DIR/package.json"

# ---- 5. npm 装生产依赖（扁平树，无 pnpm 软链） ----
echo "==> npm install 生产依赖"
mkdir -p "$WORK_DIR/staging"
cp "$WORK_DIR/package.json" "$WORK_DIR/staging/package.json"
( cd "$WORK_DIR/staging" && npm install --omit=dev --ignore-scripts --no-audit --no-fund --loglevel=error )

# ---- 6. 组装业务树到 rootfs_common/app ----
STAGING="$WORK_DIR/staging"
rm -rf "$COMMON/app"
mkdir -p "$COMMON/app"
cp "$SRC/backend-server.js" "$SRC/frontend-server.js" "$SRC/package.json" "$COMMON/app/"
cp -R "$SRC/api" "$SRC/common" "$SRC/server" "$SRC/dist" "$COMMON/app/"
cp "$SCRIPT_DIR/ugos-start.cjs" "$COMMON/app/"
cp -R "$STAGING/node_modules" "$COMMON/app/node_modules"
# .bin 全是相对软链（pack 解引用徒增体积），运行时用不到，全删
find "$COMMON/app/node_modules" -name .bin -type d -exec rm -rf {} + 2>/dev/null || true

# ---- 7. 打包补丁（每个都断言命中，上游改行时立刻暴露） ----
APP="$COMMON/app"
patch_file() { # <file> <旧串(断言在)> <sed表达式> <新串(断言生效)>
  local f="$1" old="$2" sedexpr="$3" new="$4"
  if [ ! -f "$APP/$f" ]; then
    echo "  skip: $f 不存在（上游删了这块，补丁作废）"; return 0
  fi
  grep -q "$old" "$APP/$f" || { echo "补丁目标未命中: $f :: $old"; exit 1; }
  sed "${SED_I[@]}" "$sedexpr" "$APP/$f"
  grep -q "$new" "$APP/$f" || { echo "补丁未生效: $f :: $new"; exit 1; }
  echo "  patch ok: $f"
}

echo "== 打补丁 =="
# 7.1 frontend 反代 target：沙箱无 /etc/hosts，localhost 不可解析 → 127.0.0.1
patch_file frontend-server.js \
  'http://localhost:${backEndPort}/api' \
  's|http://localhost:${backEndPort}/api|http://127.0.0.1:${backEndPort}/api|' \
  'http://127.0.0.1:${backEndPort}/api'

# 7.2 backend 监听地址可配（ugos-start.cjs 设 127.0.0.1，不对外）
# 8.1.0 起 listen 回调抽到 server/listen-callback.js（createListenCallback）
patch_file backend-server.js \
  'app.listen(backEndPort, createListenCallback({ port: backEndPort, logger }));' \
  's|app.listen(backEndPort, createListenCallback({ port: backEndPort, logger }));|app.listen(backEndPort, process.env.BACKEND_HOST \|\| undefined, createListenCallback({ port: backEndPort, logger }));|' \
  'app.listen(backEndPort, process.env.BACKEND_HOST'

# 7.3 Referer 白名单加禁用开关（NAS 无固定域名，白名单必挡正常访问）
patch_file backend-server.js \
  "app.use('/api', requireReferer);" \
  "s|app.use('/api', requireReferer);|app.use('/api', process.env.DISABLE_REFERER_CHECK === 'true' ? (req, res, next) => next() : requireReferer);|" \
  'DISABLE_REFERER_CHECK'

# 7.4-7.8 五套离线数据集目录 env 覆盖（8.0.0 起在 server/datasets/，全部写死
# 源码相对路径；安装区只读 → ugos-start.cjs 把它们指到 data/ 下）
patch_file server/datasets/maxmind-service.js \
  "export const MAXMIND_DB_DIR = path.resolve(__dirname, '../../common/maxmind-db');" \
  "s|export const MAXMIND_DB_DIR = path.resolve(__dirname, '../../common/maxmind-db');|export const MAXMIND_DB_DIR = path.resolve(process.env.MAXMIND_DB_DIR \|\| path.join(__dirname, '../../common/maxmind-db'));|" \
  'process.env.MAXMIND_DB_DIR'
patch_file server/datasets/as-org-db.js \
  "export const AS_ORG_DB_DIR = path.join(__dirname, '../../common/as-org-db');" \
  "s|export const AS_ORG_DB_DIR = path.join(__dirname, '../../common/as-org-db');|export const AS_ORG_DB_DIR = process.env.CAIDA_AS_ORG_DB_DIR \|\| path.join(__dirname, '../../common/as-org-db');|" \
  'process.env.CAIDA_AS_ORG_DB_DIR'
patch_file server/datasets/as-rel-db.js \
  "export const AS_REL_DB_DIR = path.join(__dirname, '../../common/as-rel-db');" \
  "s|export const AS_REL_DB_DIR = path.join(__dirname, '../../common/as-rel-db');|export const AS_REL_DB_DIR = process.env.CAIDA_AS_REL_DB_DIR \|\| path.join(__dirname, '../../common/as-rel-db');|" \
  'process.env.CAIDA_AS_REL_DB_DIR'
patch_file server/datasets/peeringdb-db.js \
  "export const PEERINGDB_DB_DIR = path.join(__dirname, '../../common/peeringdb-db');" \
  "s|export const PEERINGDB_DB_DIR = path.join(__dirname, '../../common/peeringdb-db');|export const PEERINGDB_DB_DIR = process.env.PEERINGDB_DB_DIR \|\| path.join(__dirname, '../../common/peeringdb-db');|" \
  'process.env.PEERINGDB_DB_DIR'
patch_file server/datasets/oui-db.js \
  "export const OUI_DB_DIR = path.join(__dirname, '../../common/oui-db');" \
  "s|export const OUI_DB_DIR = path.join(__dirname, '../../common/oui-db');|export const OUI_DB_DIR = process.env.OUI_DB_DIR \|\| path.join(__dirname, '../../common/oui-db');|" \
  'process.env.OUI_DB_DIR'

# ---- 8. 打包树断言 ----
echo "== 断言 =="
DANGLING=$(find "$APP" -type l || true)
[ -z "$DANGLING" ] || { echo "存在软链（pack 会解引用/报错）: $DANGLING"; exit 1; }
for m in $(node -p "Object.keys(require('$WORK_DIR/package.json').dependencies).join(' ')"); do
  [ -f "$APP/node_modules/$m/package.json" ] || { echo "缺依赖: $m"; exit 1; }
done
[ -f "$APP/dist/index.html" ] || { echo "缺 dist/index.html"; exit 1; }
[ -f "$APP/server/datasets/datasets.js" ] || { echo "缺 server/datasets/ —— 后端结构变动"; exit 1; }
( cd "$APP" && node --check backend-server.js && node --check frontend-server.js && node --check ugos-start.cjs )
echo "  业务树 ok（$(du -sh "$APP" | cut -f1)）"

# ---- 9. Node 运行时（官方 glibc 构建，双架构各一份） ----
RUNTIME_TARBALL="$WORK_DIR/node-linux-${NARCH}.tar.xz"
if [ ! -f "$RUNTIME_TARBALL" ]; then
  echo "==> 下载运行时 Node v${NODE_VER} (linux/${NARCH})"
  "${CURL[@]}" -o "$RUNTIME_TARBALL" \
    "https://nodejs.org/dist/v${NODE_VER}/node-v${NODE_VER}-linux-${NARCH}.tar.xz"
fi
BIN="$ROOTFS/bin"
rm -rf "$BIN"; mkdir -p "$BIN"
tar -xJf "$RUNTIME_TARBALL" -C "$BIN" --strip-components=2 "node-v${NODE_VER}-linux-${NARCH}/bin/node"
chmod +x "$BIN/node"
file "$BIN/node" | grep -qi 'ELF 64-bit' || { echo "::error::node 不是 ELF: $(file -b "$BIN/node")" >&2; exit 1; }
file "$BIN/node" | grep -qi "$ELF_MACHINE" || { echo "::error::node 架构不是 ${ELF_MACHINE}" >&2; exit 1; }

# ---- 10. icon（随仓库提交，断言在且不超限） ----
ICON="$COMMON/icon.png"
[ -f "$ICON" ] || { echo "::error::缺 $ICON" >&2; exit 1; }
ICON_SIZE=$(stat -f%z "$ICON" 2>/dev/null || stat -c%s "$ICON")
[ "$ICON_SIZE" -lt 102400 ] || { echo "icon 超 100KB: $ICON_SIZE"; exit 1; }
echo "  icon ok（${ICON_SIZE}B）"

# ---- 11. 启动冒烟（runner 是 linux/x64：amd64 产物直接真机跑一遍启动链） ----
# 合并安装视图，按 UGAPP_* 环境变量、与 unit 完全一致的方式（bin/node -e
# 引导层，不经过 shell）启动，断言静态页 / SPA fallback / API 反代。
if [ "$ARCH" = "amd64" ] && [ "$(uname -s)" = "Linux" ]; then
  echo "== 启动冒烟（amd64 真实引导链） =="
  VIEW="$WORK_DIR/view"
  mkdir -p "$VIEW"
  cp -a "$COMMON/." "$VIEW/"
  cp -a "$ROOTFS/." "$VIEW/"
  export UGAPP_INSTALL_DIR="$VIEW"
  export UGAPP_DATA_DIR="$VIEW/data"
  export UGAPP_LOG_DIR="$VIEW/log"
  export UGAPP_CACHE_DIR="$VIEW/cache"
  mkdir -p "$UGAPP_DATA_DIR"
  ( cd "$UGAPP_DATA_DIR" && "$VIEW/bin/node" -e "require(require('path').join(require('path').dirname(process.execPath),'..','app','ugos-start.cjs'))" ) > "$WORK_DIR/boot.out" 2>&1 &
  SPID=$!
  trap 'kill $SPID 2>/dev/null || true; rm -rf "$WORK_DIR"' EXIT

  wait_url() { # <url> <次数> <标签>
    local url="$1" n="$2" label="$3" i
    for ((i = 1; i <= n; i++)); do
      code=$(node -e "fetch('$url').then(r=>{console.log(r.status);process.exit(0)}).catch(()=>console.log(0))" 2>/dev/null || echo 0)
      if [ "$code" = "200" ]; then echo "  $label → 200（第 ${i} 次轮询）"; return 0; fi
      if ! kill -0 "$SPID" 2>/dev/null; then
        echo "::error::引导进程已退出（$label）"; tail -40 "$WORK_DIR/boot.out" >&2; exit 1
      fi
      sleep 2
    done
    echo "::error::$label 在预算内未就绪"; tail -40 "$WORK_DIR/boot.out" >&2; exit 1
  }
  wait_url "http://127.0.0.1:28966/" 60 "GET /（静态页）"
  wait_url "http://127.0.0.1:28966/tools/whois" 30 "GET /tools/whois（SPA fallback）"
  wait_url "http://127.0.0.1:28966/api/configs" 90 "GET /api/configs（经 frontend 反代）"
  kill "$SPID" 2>/dev/null || true
  wait "$SPID" 2>/dev/null || true
  echo "  启动冒烟通过"
else
  echo "== 跳过启动冒烟（仅 amd64 on Linux 做） =="
fi

echo "==> Done: $ROOTFS + $COMMON/app"
