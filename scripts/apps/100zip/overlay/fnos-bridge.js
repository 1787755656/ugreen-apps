/* 100zip UGOS bridge —— 替换上游的飞牛 fnOS 桥接层（绿联移植版）。
 *
 * 【保持的契约】
 * 上游 app.js 通过 window.fnosBridge 消费这层，方法签名不变：
 *   pickDirectory(opts) / pickFiles(opts) / authorizePath(path) /
 *   openAppSetting() / openFileManager(path) / getSdk() / describe() /
 *   isStandalone() / parseCallback(href) / takePendingPaths() / setPendingPaths()
 * probe.html / callback.html 还会 `import bridge from "./js/fnos-bridge.js"`，
 * 所以本文件保持 ES Module + default export。
 *
 * 【换掉的实现】
 *  1. 飞牛的系统选择器（@trimjs/web-app 的 pickUserFile）在绿联上没有宿主桥，
 *     调用会永远挂起 —— safePick 的表现就是"提示选择器正在打开，但打不开"。
 *     这里换成【应用内自绘的选择器对话框】：目录数据走上游自己的 /api/scan
 *     （列目录/文件，Guard 保证只能在授权目录内浏览），不依赖任何宿主 SDK。
 *  2. openFileManager（任务页"打开目录"）：按 liteparser 真机验证过的写法，
 *     cloudWindow.useCapacity('openApp', {appId:'com.ugreen.filemgr',
 *     data:{filePathOpen: 完整路径}})。注意官方文档示例的 data:{path} 是错的，
 *     只会唤醒窗口不定位；openLocalPath 操作的是 PC 本地文件系统，也不对。
 *     第三个参数（超时）必须给：不在绿联桌面窗口里跑时没有桥接对象，
 *     不给超时 Promise 会永远挂着。
 *  3. openAppSetting：绿联没有"应用设置"直达能力，best-effort 试
 *     openAppCenterDetail（liteparser 同款），失败返回 false —— 调用方会
 *     展示手动步骤（patch-ui.py 已把那段文案换成绿联的应用中心流程）。
 */

/* ---------- 基础工具 ---------- */

function jsonHeaders() {
  return { "Content-Type": "application/json", Accept: "application/json" };
}

// 统一请求超时：网关或上游偶发卡住时，15 秒内给出明确报错，
// 而不是让用户对着“载入中…”干等（真机反馈过“半天才弹出来提示”）。
function fetchWithTimeout(path, options, ms) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), ms || 15000);
  return fetch(path, Object.assign({}, options, { signal: ctrl.signal }))
    .finally(() => clearTimeout(timer));
}

// 统一走 window.fetch —— ugos-auth.js 已经包过它（自动带 Ugreen-Ttk +
// 401 自愈），这里绝不保存原始引用。
async function apiJson(path, options) {
  let res;
  try {
    res = await fetchWithTimeout(path, Object.assign({ headers: jsonHeaders() }, options || {}));
  } catch (e) {
    if (e && e.name === "AbortError") throw new Error("请求超时（15 秒无响应），请稍候重试");
    throw e;
  }
  const body = await res.json().catch(() => null);
  if (!res.ok || !body || body.ok === false) {
    const er = (body && body.error) || {};
    const msg = er.message ? er.message + (er.hint ? "（" + er.hint + "）" : "") : "HTTP " + res.status;
    const err = new Error(msg);
    err.code = er.code;
    throw err;
  }
  return body.data;
}

function humanSize(n) {
  if (n == null) return "";
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + " MB";
  return (n / 1024 / 1024 / 1024).toFixed(2) + " GB";
}

function parentOf(p) {
  const t = String(p || "").replace(/\/+$/, "");
  if (!t || t === "/") return null;
  const i = t.lastIndexOf("/");
  return i <= 0 ? "/" : t.slice(0, i);
}

/* ---------- 绿联宿主能力（cloudWindow） ---------- */

function hasCloudWindow() {
  const cw = window.UGCloudWindow;
  return !!(cw && typeof cw.useCapacity === "function");
}

// useCapacity(name, data, timeoutMs) —— 第三个参数必须给（见文件头说明 2）。
async function capacity(name, data, timeoutMs) {
  if (!hasCloudWindow()) throw new Error("不在绿联桌面窗口内（无 JSSDK 桥）");
  return window.UGCloudWindow.useCapacity(name, data || {}, timeoutMs || 3000);
}

async function openFileManager(path) {
  if (!path || typeof path !== "string") return false;
  try {
    await capacity("openApp", { appId: "com.ugreen.filemgr", data: { filePathOpen: path } }, 3000);
    return true;
  } catch (e) {
    console && console.warn && console.warn("[ugos-bridge] openApp 失败", e);
    return false;
  }
}

async function openAppSetting() {
  try {
    await capacity("openAppCenterDetail", { appCenterType: 1 }, 3000);
    return true;
  } catch (e) {
    return false;
  }
}

/* ---------- 自绘选择器对话框 ---------- */

const PICK_STYLE_ID = "ugos-pick-style";
const PICK_CSS = [
  "#ugosPickDlg .ugos-toolbar{display:flex;gap:6px;align-items:center;margin:8px 0}",
  "#ugosPickDlg .ugos-toolbar input{flex:1;min-width:0}",
  "#ugosPickDlg .ugos-list{max-height:44vh;overflow:auto;border:1px solid rgba(128,128,128,.35);border-radius:6px;margin:8px 0}",
  "#ugosPickDlg .ugos-row{display:flex;justify-content:space-between;align-items:center;gap:10px;padding:8px 12px;cursor:pointer;border-bottom:1px solid rgba(128,128,128,.18);font-size:13.5px}",
  "#ugosPickDlg .ugos-row:last-child{border-bottom:none}",
  "#ugosPickDlg .ugos-row:hover{background:rgba(128,128,128,.14)}",
  "#ugosPickDlg .ugos-row .ugos-name{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}",
  "#ugosPickDlg .ugos-row .ugos-meta{color:rgba(128,128,128,.9);font-size:12px;white-space:nowrap}",
  "#ugosPickDlg .ugos-row.ugos-dir .ugos-name::before{content:'▸ ';color:rgba(128,128,128,.8)}",
  "#ugosPickDlg .ugos-row.ugos-crumbs{color:rgba(128,128,128,.85);font-size:12.5px;cursor:default}",
  "#ugosPickDlg .ugos-empty{padding:22px 12px;text-align:center;color:rgba(128,128,128,.9);font-size:13px}",
  "#ugosPickDlg .ugos-actions{display:flex;gap:8px;justify-content:flex-end;flex-wrap:wrap;margin-top:4px}",
].join("\n");

function ensurePickStyle() {
  if (document.getElementById(PICK_STYLE_ID)) return;
  const st = document.createElement("style");
  st.id = PICK_STYLE_ID;
  st.textContent = PICK_CSS;
  (document.head || document.body).appendChild(st);
}

// 选择器状态（一次只开一个对话框；重复调用直接复用并重置）
const pickState = {
  open: false,
  mode: "dir", // dir | file
  accept: null, // null = 不过滤；[".zip", ...]
  multiple: false,
  resolve: null,
  roots: [],
  cur: null, // null = 虚拟根层（列出所有授权根）；字符串 = 真实目录
  checked: new Set(),
  el: {}, // 对话框内元素引用
};

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
}

function dirName(p) {
  const t = String(p || "").replace(/\/+$/, "");
  return t.split("/").pop() || t;
}

async function fetchRoots() {
  const ctx = await apiJson("/api/context");
  return (ctx && ctx.accessibleFolders) || [];
}

async function scanDir(path) {
  const d = await apiJson("/api/scan", {
    method: "POST",
    body: JSON.stringify({ path: path, recursive: false, archivesOnly: false }),
  });
  return (d && d.files) || [];
}

function fmtErr(e) {
  return (e && e.message) ? e.message : String(e);
}

function setHint(msg, isError) {
  const h = pickState.el.hint;
  if (!h) return;
  h.textContent = msg || "";
  h.style.color = isError ? "#c0392b" : "";
}

// 渲染虚拟根层（所有授权根作为入口）
function renderVirtual() {
  pickState.cur = null;
  pickState.checked.clear();
  const list = pickState.el.list;
  list.innerHTML = "";
  pickState.el.pathInput.value = "";
  pickState.el.up.disabled = true;
  updateConfirm();
  const crumbs = el("div", "ugos-row ugus-crumbs", "选择一个已授权的工作目录进入：");
  list.appendChild(crumbs);
  if (!pickState.roots.length) {
    list.appendChild(el("div", "ugos-empty",
      "还没有已授权目录。请到 应用中心 → 100解压 → 设置 里添加「工作目录」，保存后在应用中心停止再启动一次本应用。"));
    return;
  }
  for (const r of pickState.roots) {
    const row = el("div", "ugos-row ugos-dir");
    const name = el("span", "ugos-name", r);
    const meta = el("span", "ugos-meta", "工作目录");
    row.appendChild(name);
    row.appendChild(meta);
    row.addEventListener("click", () => {
      navigate(r).catch((e) => setHint(fmtErr(e), true));
    });
    list.appendChild(row);
  }
}

async function navigate(path) {
  const list = pickState.el.list;
  list.innerHTML = "";
  setHint("载入中…");
  try {
    const files = await scanDir(path);
    pickState.cur = path;
    pickState.el.pathInput.value = path;
    pickState.el.up.disabled = false;
    pickState.checked.clear();
    updateConfirm();

    const dirs = [];
    const fileRows = [];
    for (const f of files) {
      if (f.kind === "dir") dirs.push(f);
      else if (pickState.mode === "file") {
        if (pickState.accept && pickState.accept.length) {
          const lower = String(f.name || "").toLowerCase();
          if (!pickState.accept.some((ext) => lower.endsWith(ext.toLowerCase()))) continue;
        }
        fileRows.push(f);
      }
    }
    dirs.sort((a, b) => String(a.name).localeCompare(String(b.name), "zh-CN"));
    fileRows.sort((a, b) => String(a.name).localeCompare(String(b.name), "zh-CN"));

    const crumbs = el("div", "ugos-row ugus-crumbs", path);
    list.appendChild(crumbs);

    if (!dirs.length && !fileRows.length) {
      list.appendChild(el("div", "ugos-empty", pickState.mode === "file" ? "（没有匹配的文件）" : "（空目录）"));
    }
    for (const d of dirs) {
      list.appendChild(buildRow(d, true));
    }
    for (const f of fileRows) {
      list.appendChild(buildRow(f, false));
    }
    setHint(pickState.mode === "dir"
      ? "双击进入文件夹；点「选择当前目录」确认。"
      : (pickState.multiple ? "勾选文件后点「确定」。" : "点击文件名选中。"));
  } catch (e) {
    setHint(fmtErr(e), true);
    // 导航失败时退回原状态：重新画一遍空列表，保留输入框里的路径供修改
    pickState.cur = null;
    pickState.el.up.disabled = true;
    updateConfirm();
  }
}

function buildRow(item, isDir) {
  const row = el("div", "ugos-row" + (isDir ? " ugos-dir" : ""));
  row.dataset.path = item.path;
  row.dataset.kind = isDir ? "dir" : "file";
  if (isDir) {
    const name = el("span", "ugos-name", item.name || dirName(item.path));
    const meta = el("span", "ugos-meta", "文件夹");
    row.appendChild(name);
    row.appendChild(meta);
    row.addEventListener("click", () => {
      navigate(item.path).catch((e) => setHint(fmtErr(e), true));
    });
    return row;
  }
  if (pickState.multiple) {
    const box = el("input");
    box.type = "checkbox";
    box.checked = pickState.checked.has(item.path);
    const sync = () => {
      if (box.checked) pickState.checked.add(item.path);
      else pickState.checked.delete(item.path);
      updateConfirm();
    };
    box.addEventListener("click", (ev) => ev.stopPropagation());
    box.addEventListener("change", sync);
    row.appendChild(box);
  }
  const name = el("span", "ugos-name", item.name || dirName(item.path));
  const meta = el("span", "ugos-meta", humanSize(item.size) + (item.volume && item.missing && item.missing.length ? " · 缺卷" : ""));
  row.appendChild(name);
  row.appendChild(meta);
  if (!pickState.multiple) {
    row.addEventListener("click", () => finish([item.path]));
  } else {
    // 整行点击 = 切换勾选（直接改 checkbox 再同步，与 change 事件共用 sync 逻辑）
    row.addEventListener("click", () => {
      const box = row.querySelector("input[type=checkbox]");
      if (!box) return;
      box.checked = !box.checked;
      if (box.checked) pickState.checked.add(item.path);
      else pickState.checked.delete(item.path);
      updateConfirm();
    });
  }
  return row;
}

function updateConfirm() {
  const btn = pickState.el.confirm;
  if (!btn) return; // 单文件模式没有确认按钮（点行即选中）
  if (pickState.mode === "dir") {
    btn.textContent = "选择当前目录";
    btn.disabled = !pickState.cur;
  } else {
    btn.textContent = "确定（已选 " + pickState.checked.size + " 项）";
    btn.disabled = !pickState.checked.size;
  }
}

function finish(paths) {
  const resolve = pickState.resolve;
  closePick();
  if (resolve) resolve(paths || []);
}

function closePick() {
  pickState.open = false;
  pickState.resolve = null;
  const dlg = pickState.el.dialog;
  if (dlg && dlg.parentNode) dlg.parentNode.removeChild(dlg);
  pickState.el = {};
}

function openPickDialog(mode, opts) {
  return new Promise((resolve, reject) => {
    if (pickState.open) {
      reject(new Error("选择器已打开"));
      return;
    }
    pickState.open = true;
    pickState.mode = mode;
    pickState.accept = (opts && opts.accept) || null;
    pickState.multiple = !!(opts && opts.multiple);
    pickState.resolve = resolve;
    pickState.checked = new Set();
    pickState.roots = [];
    pickState.cur = null;

    ensurePickStyle();

    const dlg = el("div", "dialog");
    dlg.id = "ugosPickDlg";
    const card = el("div", "dialog-card");
    const head = el("div", "dialog-head");
    const h3 = el("h3", null, (opts && opts.title) || (mode === "dir" ? "选择目录" : "选择文件"));
    head.appendChild(h3);
    card.appendChild(head);

    const hint = el("p", "hint", "载入中…");
    card.appendChild(hint);

    const toolbar = el("div", "ugos-toolbar");
    const pathInput = el("input");
    pathInput.type = "text";
    pathInput.placeholder = "也可直接粘贴路径（例如 /volume1/media）";
    const loadBtn = el("button", "btn ghost sm", "载入");
    const upBtn = el("button", "btn ghost sm", "上级");
    upBtn.disabled = true;
    toolbar.appendChild(pathInput);
    toolbar.appendChild(loadBtn);
    toolbar.appendChild(upBtn);
    card.appendChild(toolbar);

    const list = el("div", "ugos-list");
    card.appendChild(list);

    const actions = el("div", "ugos-actions");
    const cancel = el("button", "btn ghost", "取消");
    actions.appendChild(cancel);
    let confirm = null;
    if (mode === "dir" || (mode === "file" && pickState.multiple)) {
      confirm = el("button", "btn primary", mode === "dir" ? "选择当前目录" : "确定");
      confirm.disabled = true;
      actions.appendChild(confirm);
    }
    card.appendChild(actions);

    dlg.appendChild(card);
    document.body.appendChild(dlg);

    pickState.el = { dialog: dlg, hint: hint, list: list, pathInput: pathInput, up: upBtn, confirm: confirm };

    cancel.addEventListener("click", () => finish([]));
    if (confirm) {
      confirm.addEventListener("click", () => {
        if (pickState.mode === "dir") {
          if (pickState.cur) finish([pickState.cur]);
        } else if (pickState.multiple) {
          finish(Array.from(pickState.checked));
        }
      });
    }
    upBtn.addEventListener("click", () => {
      if (!pickState.cur) return;
      if (pickState.roots.indexOf(pickState.cur) >= 0) {
        renderVirtual();
        setHint("");
        return;
      }
      const parent = parentOf(pickState.cur);
      const inside = pickState.roots.some((r) => parent === r || parent.indexOf(r + "/") === 0);
      if (parent && inside) {
        navigate(parent).catch((e) => setHint(fmtErr(e), true));
      } else {
        renderVirtual();
        setHint("");
      }
    });
    const doLoad = () => {
      const v = pathInput.value.trim().replace(/\/+$/, "");
      if (!v) { setHint("请先输入路径", true); return; }
      navigate(v).catch((e) => setHint(fmtErr(e), true));
    };
    loadBtn.addEventListener("click", doLoad);
    pathInput.addEventListener("keydown", (e) => { if (e.key === "Enter") doLoad(); });

    // 载入授权根
    fetchRoots().then((roots) => {
      pickState.roots = (roots || []).map((r) => String(r).replace(/\/+$/, ""));
      if (pickState.roots.length === 1) {
        // 单根直接进入（无论目录还是文件模式，虚拟根层都只是多一次点击）
        navigate(pickState.roots[0]).catch((e) => setHint(fmtErr(e), true));
      } else {
        renderVirtual();
        setHint("");
      }
    }).catch((e) => {
      setHint("读取授权目录失败：" + fmtErr(e), true);
      renderVirtual();
    });
  });
}

/* ---------- 对外 API（保持上游 fnosBridge 的形状） ---------- */

async function pickDirectory(opts) {
  try {
    return await openPickDialog("dir", opts || {});
  } catch (e) {
    if (e && e.message === "选择器已打开") return [];
    throw e;
  }
}

async function pickFiles(opts) {
  try {
    return await openPickDialog("file", opts || {});
  } catch (e) {
    if (e && e.message === "选择器已打开") return [];
    throw e;
  }
}

async function authorizePath(path) {
  try {
    const res = await apiJson("/api/authorize/register", {
      method: "POST",
      body: JSON.stringify({ paths: [path] }),
    });
    return (res && res.registered) || [];
  } catch (e) {
    return [];
  }
}

function describe() {
  return {
    sdkLoaded: hasCloudWindow(),
    via: "ugos-selfdrawn",
    isTop: window === window.top,
    picker: "self-drawn (/api/scan)",
    fileMgr: hasCloudWindow() ? "openApp+filePathOpen" : "unavailable",
    injectedGlobals: { UGCloudWindow: typeof window.UGCloudWindow },
  };
}

function isStandalone() {
  return window === window.top;
}

function parseCallback(href) {
  const url = new URL(href);
  return {
    status: url.searchParams.get("status"),
    paths: url.searchParams.getAll("path").concat(url.searchParams.getAll("paths")),
    state: url.searchParams.get("state"),
  };
}

const STORAGE_KEY = "100zip.authorizedPaths";
function takePendingPaths() {
  const raw = sessionStorage.getItem(STORAGE_KEY);
  sessionStorage.removeItem(STORAGE_KEY);
  if (!raw) return [];
  try { return JSON.parse(raw); } catch (e) { return []; }
}
function setPendingPaths(paths) {
  sessionStorage.setItem(STORAGE_KEY, JSON.stringify(paths || []));
}

const api = {
  getSdk, describe, isStandalone, pickDirectory, pickFiles, authorizePath,
  openAppSetting, openFileManager, parseCallback, takePendingPaths, setPendingPaths,
  APP_NAME: "100zip",
};

function getSdk() {
  return { sdk: api, via: "ugos-selfdrawn" };
}

window.fnosBridge = api;
window.fnosBridge.__guarded = true; // app.js 的 guardPicker 不必再包一层

export default api;
