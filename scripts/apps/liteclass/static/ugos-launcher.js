// UGOS Pro 原生沙箱适配层（不动上游混淆代码，只做环境重定向）
// 沙箱特征（真机实测）：
//   - 安装目录只读 -> DATA_DIR / UPLOAD_DIR 必须指向可写数据目录
//   - 没有 /tmp    -> TMPDIR 指向可写缓存目录
//   - 平台注入的授权目录/参数在进程启动前已写入 environ
const path = require('path');
const fs = require('fs');

function mkdirp(p) {
  try { fs.mkdirSync(p, { recursive: true }); } catch (e) {}
}

const dataDir = process.env.UGAPP_DATA_DIR || process.env.DATA_DIR || path.join(__dirname, 'data');
const cacheDir = process.env.UGAPP_CACHE_DIR || path.join(dataDir, 'cache');
const tmpDir = path.join(cacheDir, 'tmp');

mkdirp(dataDir);
mkdirp(cacheDir);
mkdirp(tmpDir);

process.env.DATA_DIR = dataDir;
process.env.UPLOAD_DIR = process.env.UPLOAD_DIR || path.join(dataDir, 'uploads');
process.env.TMPDIR = tmpDir;
process.env.PORT = process.env.PORT || '18083';
process.env.DEFAULT_SHARE_PATH =
  process.env.DEFAULT_SHARE_PATH || path.join(dataDir, 'media');
// transcoder 模块在 index.js 设置该变量之前就被 require，必须提前指到可写目录
process.env.TRANSCODE_CACHE_DIR = path.join(cacheDir, 'transcode');
// 还原上游镜像默认初始管理员（装好后可在应用内改密码）
process.env.ADMIN_USERNAME = process.env.ADMIN_USERNAME || 'admin';
process.env.ADMIN_PASSWORD = process.env.ADMIN_PASSWORD || 'admin123';

mkdirp(process.env.UPLOAD_DIR);
mkdirp(process.env.DEFAULT_SHARE_PATH);
mkdirp(process.env.TRANSCODE_CACHE_DIR);
// 上游把 exam_images 注册成 @fastify/static 的一个 root，而它【只在真正上传
// 考试图片时才建】——注册发生在启动阶段，目录不存在就会打一条
// `"root" path ... must exist` 的告警，之后取考试图片的路由也取不到。
// 其余几个（covers/avatars/note_images…）上游启动时自己会建，只有这个漏了。
mkdirp(path.join(dataDir, 'exam_images'));

// ── 修复上游目录浏览器的可访问路径列表 ──────────────────────────────
// 上游混淆过的 helpers/media.js 实际没有导出 getAccessiblePaths，
// 导致 /api/admin/accessible-paths/browse（新增课程时选"挂载目录"）一调就 500
// （官方 Docker 镜像同样坏）。这里在加载 index.js 之前把实现注入进
// helpers/media 的导出对象 —— Node 按解析路径缓存模块，routes/system.js
// 之后 require('../helpers/media') 拿到的就是同一实例，解构即可取到。
function collectAccessibleRoots() {
  const roots = new Set();
  // 1. 课程媒体库参数（安装时 type:path 授权的真实路径）
  if (process.env.DEFAULT_SHARE_PATH) roots.add(process.env.DEFAULT_SHARE_PATH);
  // 2. 之后在应用设置里追加授权的目录：UGAPP_SHARED_DIR 里镜像成
  //    shared/volume1/<共享文件夹>/<授权目录>，叶子可写；映射回真实路径
  const sharedDir = process.env.UGAPP_SHARED_DIR;
  if (sharedDir) {
    const volRoot = path.join(sharedDir, 'volume1');
    try {
      const walkLeaf = (p) => {
        const entries = fs.readdirSync(p, { withFileTypes: true });
        let hasSubdir = false;
        for (const e of entries) {
          if (e.isDirectory()) {
            hasSubdir = true;
            walkLeaf(path.join(p, e.name));
          }
        }
        if (!hasSubdir) roots.add('/volume1' + p.slice(volRoot.length));
      };
      if (fs.statSync(volRoot).isDirectory()) walkLeaf(volRoot);
    } catch (e) {}
  }
  return [...roots];
}

const media = require('./helpers/media');
media.getAccessiblePaths = async function getAccessiblePaths() {
  return collectAccessibleRoots();
};

require('./index.js');
