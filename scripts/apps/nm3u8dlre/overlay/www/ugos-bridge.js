/*
 * UGOS 宿主桥 —— 替代 fnOS 版 www/vendor/trim-app.js
 *
 * 上游前端（app.js）通过 `import('./vendor/trim-app.js')` 拿 fnOS 的 TrimApp SDK，
 * 用到四个能力：主题、打开文件、打开目录、选择目录。
 * 绿联 UGOS 上没有这套 SDK，这里用 cloudWindow 的 useCapacity 实现同名接口，
 * 因此 app.js 无需改动（保持与上游同步的成本最低）。
 *
 * 能力映射：
 *   openFile / openFileManager → useCapacity('openApp', {appId: com.ugreen.filemgr})
 *   pickUserFile               → 内置目录选择弹窗（后端 ?action=dirs）
 *   getPlatformConfig / $on    → 仅主题，直接返回当前主题
 *
 * 没有宿主桥（普通浏览器直连端口）时不抛错，由 app.js 走"复制路径"兜底。
 */

const FILEMGR_APP_ID = 'com.ugreen.filemgr';
const BRIDGE_TIMEOUT = 8000;

function cloudWindow() {
  const cw = window.UGCloudWindow;
  if (!cw || typeof cw.useCapacity !== 'function') return null;
  return cw;
}

// 在 UGOS 文件管理器中定位到某个路径（传文件会选中文件，传目录直接进目录）
async function openInFileManager(path) {
  const cw = cloudWindow();
  if (!cw) throw new Error('当前不在绿联桌面窗口中，无法调起文件管理器');
  if (!path) throw new Error('路径为空');
  return cw.useCapacity(
    'openApp',
    { appId: FILEMGR_APP_ID, data: { filePathOpen: path } },
    BRIDGE_TIMEOUT
  );
}

/* ---------------- 目录选择弹窗（纯 DOM，无依赖） ---------------- */

const CSS = `
.ugp-mask{position:fixed;inset:0;background:rgba(0,0,0,.55);z-index:99998;display:flex;align-items:center;justify-content:center}
.ugp-box{width:min(620px,92vw);max-height:76vh;background:var(--bs-body-bg,#212529);color:var(--bs-body-color,#dee2e6);
  border:1px solid var(--bs-border-color,#495057);border-radius:12px;display:flex;flex-direction:column;overflow:hidden;
  box-shadow:0 18px 48px rgba(0,0,0,.5);font-size:14px}
.ugp-head{padding:12px 16px;border-bottom:1px solid var(--bs-border-color,#495057);font-weight:600;display:flex;justify-content:space-between;align-items:center}
.ugp-path{padding:8px 16px;font-size:12px;color:var(--bs-secondary-color,#adb5bd);word-break:break-all;border-bottom:1px solid var(--bs-border-color,#343a40)}
.ugp-list{overflow:auto;flex:1;min-height:140px}
.ugp-item{padding:8px 16px;cursor:pointer;display:flex;gap:8px;align-items:center;border-bottom:1px solid rgba(128,128,128,.12)}
.ugp-item:hover{background:rgba(128,128,128,.16)}
.ugp-empty{padding:24px 16px;text-align:center;color:var(--bs-secondary-color,#adb5bd)}
.ugp-foot{padding:10px 16px;display:flex;gap:8px;justify-content:flex-end;border-top:1px solid var(--bs-border-color,#495057)}
.ugp-foot button{border:1px solid var(--bs-border-color,#6c757d);background:transparent;color:inherit;border-radius:6px;padding:6px 14px;cursor:pointer;font-size:13px}
.ugp-foot button.primary{background:#0d6efd;border-color:#0d6efd;color:#fff}
.ugp-foot button:disabled{opacity:.45;cursor:not-allowed}
.ugp-x{border:0;background:transparent;color:inherit;font-size:18px;cursor:pointer;line-height:1}
`;

let cssInjected = false;
function ensureCSS() {
  if (cssInjected) return;
  const st = document.createElement('style');
  st.textContent = CSS;
  document.head.appendChild(st);
  cssInjected = true;
}

async function fetchDirs(path) {
  const url = '/api?' + new URLSearchParams({ action: 'dirs', path: path || '' });
  const res = await fetch(url);
  return res.json();
}

/**
 * 打开目录选择弹窗，resolve 出选中的绝对路径（取消则 reject）。
 * 目录能否真正写入由后端授权根决定，弹窗只负责选。
 */
function pickDir() {
  ensureCSS();
  return new Promise((resolve, reject) => {
    const mask = document.createElement('div');
    mask.className = 'ugp-mask';
    mask.innerHTML = `
      <div class="ugp-box" role="dialog" aria-modal="true">
        <div class="ugp-head"><span>选择输出目录</span><button class="ugp-x" type="button" aria-label="关闭">×</button></div>
        <div class="ugp-path">加载中…</div>
        <div class="ugp-list"></div>
        <div class="ugp-foot">
          <button type="button" class="up">上一级</button>
          <button type="button" class="pick primary" disabled>选择此目录</button>
          <button type="button" class="cancel">取消</button>
        </div>
      </div>`;
    document.body.appendChild(mask);

    const pathEl = mask.querySelector('.ugp-path');
    const listEl = mask.querySelector('.ugp-list');
    const upBtn = mask.querySelector('.up');
    const pickBtn = mask.querySelector('.pick');

    let cur = { path: '', parent: '', selectable: false };
    let settled = false;
    const done = (fn, arg) => {
      if (settled) return;
      settled = true;
      mask.remove();
      document.removeEventListener('keydown', onKey);
      fn(arg);
    };
    const onKey = (e) => { if (e.key === 'Escape') done(reject, new Error('已取消')); };
    document.addEventListener('keydown', onKey);

    mask.querySelector('.ugp-x').onclick = () => done(reject, new Error('已取消'));
    mask.querySelector('.cancel').onclick = () => done(reject, new Error('已取消'));
    mask.addEventListener('click', (e) => { if (e.target === mask) done(reject, new Error('已取消')); });
    pickBtn.onclick = () => { if (cur.path) done(resolve, { path: cur.path }); };
    upBtn.onclick = () => { if (cur.parent) load(cur.parent); else load(''); };

    function render(d) {
      cur = { path: d.path || '', parent: d.parent || '', selectable: !!d.selectable };
      pathEl.textContent = d.path ? d.path + (d.selectable ? '' : '（不在授权范围内，不可选）') : '请选择一个目录';
      upBtn.disabled = !d.parent && !d.path;
      pickBtn.disabled = !cur.selectable || !cur.path;
      if (!d.ok) {
        listEl.innerHTML = `<div class="ugp-empty">${d.error || '无法读取目录'}</div>`;
        return;
      }
      if (!d.dirs || !d.dirs.length) {
        listEl.innerHTML = '<div class="ugp-empty">该目录下没有子目录</div>';
        return;
      }
      listEl.innerHTML = '';
      d.dirs.forEach((it) => {
        const row = document.createElement('div');
        row.className = 'ugp-item';
        row.innerHTML = '<span>📁</span>';
        const span = document.createElement('span');
        span.textContent = it.name;
        row.appendChild(span);
        row.onclick = () => load(it.path);
        listEl.appendChild(row);
      });
    }

    let seq = 0;
    async function load(path) {
      const my = ++seq;
      pathEl.textContent = '加载中…';
      listEl.innerHTML = '';
      try {
        const d = await fetchDirs(path);
        if (my !== seq) return;
        render(d);
      } catch (e) {
        if (my !== seq) return;
        listEl.innerHTML = '<div class="ugp-empty">请求失败：' + (e && e.message ? e.message : e) + '</div>';
      }
    }
    load('');
  });
}

/* ---------------- 对外导出的 SDK 门面 ---------------- */

export class TrimApp {
  constructor() {
    this.isWeb = true;
    // 没有宿主桥时 app.js 会走"独立浏览器"分支（复制路径兜底）
    this.isStandaloneWeb = !cloudWindow();
  }
  async ready() { return this; }
  getWebMethods() { return {}; }
  async getPlatformConfig() {
    return { theme: document.documentElement.dataset.bsTheme || 'dark' };
  }
  $on() { /* 主题订阅：UGOS 注入主题的能力不对外，保持本地记忆即可 */ }
  async openFile(path) { return openInFileManager(path); }
  async openFileManager(path) { return openInFileManager(path); }
  async pickUserFile() { return pickDir(); }
  async openAppAuth() { throw new Error('UGOS 不支持该授权跳转'); }
}

export default TrimApp;

/* ---------------- 下载目录现场横幅 ----------------
 * 后端 /api/?action=status 会带 download{warning}：授权目录还没生效（应用正在
 * 自动重启）、或者当前没有可用目录只能落到应用数据目录（文件管理器看不到）。
 * 这两种状态必须让用户看见 —— 上一版就是静默回退，用户以为下到了配置的目录。
 */
const BANNER_CSS = `
.ugb-warn{display:flex;gap:10px;align-items:flex-start;margin:0 0 14px;padding:11px 14px;border-radius:10px;
  background:rgba(255,193,7,.12);border:1px solid rgba(255,193,7,.45);color:var(--bs-body-color,#dee2e6);
  font-size:13.5px;line-height:1.5}
.ugb-warn .ugb-ico{flex:0 0 auto;width:20px;height:20px;border-radius:50%;background:#ffc107;color:#212529;
  font-weight:700;font-size:13px;display:flex;align-items:center;justify-content:center;margin-top:1px}
`;

function ensureBannerCSS() {
  if (document.getElementById('ugb-warn-css')) return;
  const st = document.createElement('style');
  st.id = 'ugb-warn-css';
  st.textContent = BANNER_CSS;
  document.head.appendChild(st);
}

async function showDownloadWarning() {
  try {
    const res = await fetch('/api/?' + new URLSearchParams({ action: 'status' }));
    const d = await res.json();
    const msg = d && d.download && d.download.warning;
    if (!msg) return;
    ensureBannerCSS();
    const box = document.createElement('div');
    box.className = 'ugb-warn';
    const ico = document.createElement('span');
    ico.className = 'ugb-ico';
    ico.textContent = '!';
    const txt = document.createElement('div');
    txt.className = 'ugb-txt';
    txt.textContent = msg;
    box.append(ico, txt);
    const host = document.querySelector('main') || document.body;
    host.insertBefore(box, host.firstChild);
  } catch (_) { /* 页面还没起好就算了，不影响主流程 */ }
}

if (typeof document !== 'undefined') {
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', showDownloadWarning, { once: true });
  } else {
    showDownloadWarning();
  }
}
