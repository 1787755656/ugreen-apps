// MyIP (https://github.com/jason5ng32/MyIP) — UGOS Pro 原生应用引导层
//
// 为什么有这个文件（取代原 bin/start.sh）：
//   原启动链是 #!/bin/sh 脚本，为了让沙箱里存在 /bin/sh 必须声明
//   SYSTEM.EXEC_SYSTEM_COMMAND —— 那条权限的全部作用就是给 unit 加
//   BindReadOnlyPaths=/bin /sbin /usr/bin /usr/sbin。改由 Node 自身完成
//   启动前的环境准备后，启动链不经过 shell，EXEC 权限即可去掉，
//   只保留 NETWORK.ACCESS_INTERNET。
//
// ⚠ 平台只绝对化 start_cmd 的【首个 token】，且 unit 的 WorkingDirectory
//   固定是 data/，不能给 node 传相对脚本路径。start_cmd 为：
//     bin/node -e "require(require('path').join(require('path').dirname(process.execPath),'..','app','ugos-start.cjs'))"
//   即由 process.execPath 反推安装目录（node 位于 <install>/bin/node）。
//
// backend-server.js 与 frontend-server.js 在本进程内先后 import：
// 任一致命错误 = 整进程退出，交给 systemd 重启 —— 与原双进程看门狗
// （任一退出整组拉闸）语义一致。

const path = require('path');
const fs = require('fs');

function mkdirp(p) {
  try {
    fs.mkdirSync(p, { recursive: true });
  } catch (e) {
    // 沙箱下个别路径不可写时忽略，保证服务仍可启动
  }
}

const binDir = path.dirname(process.execPath);
const installDir = path.dirname(binDir);

const DATA_DIR = process.env.UGAPP_DATA_DIR || path.join(installDir, 'data');
const LOG_DIR = process.env.UGAPP_LOG_DIR || path.join(installDir, 'log');
const CACHE_DIR = process.env.UGAPP_CACHE_DIR || path.join(installDir, 'cache');
const APP_DIR = path.join(installDir, 'app');

// 探活端口在加载用户 .env 之前锁定，不允许用户配置破坏探活
const FRONTEND_PORT = process.env.FRONTEND_PORT || '28966';
const BACKEND_PORT = process.env.BACKEND_PORT || '11966';

mkdirp(DATA_DIR);
mkdirp(LOG_DIR);
mkdirp(CACHE_DIR);
// 五套离线数据集（server/datasets/）：上游写死在源码目录（安装区只读），
// 打包补丁允许 env 覆盖后全部挪进可写的 data/。MaxMind 要 GeoLite2 授权
// 才下载，其余（CAIDA/PeeringDB/OUI）免 key 拉公开数据集。
mkdirp(path.join(DATA_DIR, 'maxmind-db'));
mkdirp(path.join(DATA_DIR, 'as-org-db'));
mkdirp(path.join(DATA_DIR, 'as-rel-db'));
mkdirp(path.join(DATA_DIR, 'peeringdb-db'));
mkdirp(path.join(DATA_DIR, 'oui-db'));
mkdirp(path.join(CACHE_DIR, 'tmp'));

// 沙箱没有 /tmp：数据集下载解压用 os.tmpdir()
process.env.TMPDIR = path.join(CACHE_DIR, 'tmp');

// 数据集目录 env 覆盖（对应 build.sh 的五条打包补丁）
process.env.MAXMIND_DB_DIR = process.env.MAXMIND_DB_DIR || path.join(DATA_DIR, 'maxmind-db');
process.env.CAIDA_AS_ORG_DB_DIR = process.env.CAIDA_AS_ORG_DB_DIR || path.join(DATA_DIR, 'as-org-db');
process.env.CAIDA_AS_REL_DB_DIR = process.env.CAIDA_AS_REL_DB_DIR || path.join(DATA_DIR, 'as-rel-db');
process.env.PEERINGDB_DB_DIR = process.env.PEERINGDB_DB_DIR || path.join(DATA_DIR, 'peeringdb-db');
process.env.OUI_DB_DIR = process.env.OUI_DB_DIR || path.join(DATA_DIR, 'oui-db');

// NAS 上没有固定域名，浏览器 Referer 是 http://<NAS_IP>:28966，上游的
// Referer 白名单必挡正常访问 → 打包默认关闭（打包补丁）；
// 高级用户可在 data/.env 里设 DISABLE_REFERER_CHECK=false 恢复。
process.env.DISABLE_REFERER_CHECK = process.env.DISABLE_REFERER_CHECK || 'true';

// backend 只服务 frontend 的本地反代，不对外暴露（打包补丁）
process.env.BACKEND_HOST = process.env.BACKEND_HOST || '127.0.0.1';
process.env.FRONTEND_PORT = FRONTEND_PORT;
process.env.BACKEND_PORT = BACKEND_PORT;
process.env.NODE_ENV = 'production';
process.env.LOG_FORMAT = process.env.LOG_FORMAT || 'json';
process.env.LOG_LEVEL = process.env.LOG_LEVEL || 'info';
process.env.TZ = process.env.TZ || 'Asia/Shanghai';

// 用户自定义配置（MAXMIND_ACCOUNT_ID / MAXMIND_LICENSE_KEY / 各 API key…），
// 每行一条 KEY=值。端口变量已锁定，解析时跳过。
const ENV_FILE = path.join(DATA_DIR, '.env');
if (fs.existsSync(ENV_FILE)) {
  try {
    const raw = fs.readFileSync(ENV_FILE, 'utf8');
    for (const line of raw.split('\n')) {
      const m = line.match(/^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*([\s\S]*?)\s*$/);
      if (!m) continue;
      if (m[1] === 'FRONTEND_PORT' || m[1] === 'BACKEND_PORT') continue;
      let val = m[2];
      if ((val.startsWith('"') && val.endsWith('"')) || (val.startsWith("'") && val.endsWith("'"))) {
        val = val.slice(1, -1);
      }
      process.env[m[1]] = val;
    }
  } catch (e) {
    // .env 解析失败不应阻断启动
  }
}

const backendEntry = path.join(APP_DIR, 'backend-server.js');
const frontendEntry = path.join(APP_DIR, 'frontend-server.js');
for (const f of [backendEntry, frontendEntry]) {
  if (!fs.existsSync(f)) {
    console.error(`myip: entry missing: ${f}`);
    process.exit(1);
  }
}

// 两台服务器都在 app/ 目录下工作（frontend 的 dotenv 读 cwd）
process.chdir(APP_DIR);

(async () => {
  await import(backendEntry); // API：127.0.0.1:BACKEND_PORT，只服务本地反代
  await import(frontendEntry); // 对外：静态 dist + SPA fallback + /api 反代
})().catch((err) => {
  console.error('myip: server exited:', (err && err.stack) || err);
  process.exit(1);
});
