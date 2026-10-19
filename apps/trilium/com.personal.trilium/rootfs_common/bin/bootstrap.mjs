// Trilium Notes (UGOS Pro 原生应用) 启动引导
//
// 平台只把 start_cmd 的第一个 token 绝对化，且 systemd 的 WorkingDirectory 是 data/，
// 所以不能写 `bin/node bin/bootstrap.mjs`（第二个 token 会被解析成 <data>/bin/...）。
// 入口固定为 `bin/node -e "<内联 JS>"`，由内联 JS 用 process.execPath 推出包内路径再
// 动态 import 本文件（同一套路已在 UGOS 上验证可用）。
//
// 本文件职责：把平台给的 UGAPP_* 目录翻成 Trilium 认识的环境变量，然后原样加载上游
// 的 app/main.mjs —— 不修改上游任何代码，升级只需换 app/ 目录。

import { mkdirSync, existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const PORT = "28186";

const here = dirname(fileURLToPath(import.meta.url)); // <install>/bin
const installDir = dirname(here); // <install>
const appDir = join(installDir, "app");

function dirOf(envName, fallbackName) {
  const v = (process.env[envName] || "").trim();
  return v || join(installDir, fallbackName);
}

const dataRoot = dirOf("UGAPP_DATA_DIR", "data");
const cacheRoot = dirOf("UGAPP_CACHE_DIR", "cache");

// Trilium 的数据目录（document.db + document/ + backup/ + log/）。
// 单独一层，避免和平台自己的 data/ 目录内容混在一起。
const triliumData = join(dataRoot, "trilium-data");
const tmpDir = join(cacheRoot, "tmp");

for (const d of [triliumData, tmpDir]) {
  try {
    mkdirSync(d, { recursive: true });
  } catch (e) {
    console.error(`[trilium] 创建目录失败 ${d}: ${e && e.message}`);
  }
}

// 沙箱里没有 /tmp，node 与 better-sqlite3 的临时文件都跟着 TMPDIR 走。
process.env.TMPDIR = tmpDir;
process.env.HOME = dataRoot;
process.env.NODE_ENV = "production";

// Trilium 配置：端口来自环境变量（优先级高于 data 目录里的 config.ini），
// 必须监听 0.0.0.0 —— tab 型应用是桌面/手机直接访问 NAS 的该端口。
process.env.TRILIUM_DATA_DIR = triliumData;
// 必填：上游在非 dev/非 Electron 模式下把资源目录算成 dirname(process.argv[1])。
// 而 `node -e` 启动时 argv[1] 是 undefined → 直接 TypeError 崩溃。
// 显式给出资源目录（= app/），同时也绕开了对 cwd 的依赖。
process.env.TRILIUM_RESOURCE_DIR = appDir;
process.env.TRILIUM_PORT = process.env.TRILIUM_PORT || PORT;
process.env.TRILIUM_NETWORK_PORT = process.env.TRILIUM_NETWORK_PORT || PORT;
process.env.TRILIUM_NETWORK_HOST = process.env.TRILIUM_NETWORK_HOST || "0.0.0.0";

const entry = join(appDir, "main.mjs");
if (!existsSync(entry)) {
  console.error(`[trilium] 缺少应用主体: ${entry}`);
  process.exit(1);
}

console.log(
  `[trilium] 启动 install=${installDir} data=${triliumData} tmp=${tmpDir} port=${process.env.TRILIUM_PORT} node=${process.version}`
);

try {
  await import(pathToFileURL(entry).href);
} catch (e) {
  console.error("[trilium] 启动失败", e);
  process.exit(1);
}
