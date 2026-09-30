#!/usr/bin/env node
/*
 * 在 Node 里造一个最小假 DOM，把管理页的脚本真跑一遍，再逐个触发它注册的回调。
 *
 * 为什么需要这个：这个页面是手写 HTML、没有构建流程，最常见的 bug 是
 * 「调用了不存在的函数 / 引用了不存在的 id」，症状清一色是"点了没反应"，
 * 而 `node --check` 只查语法，一个都查不出来。
 *
 * 它真抓到过的东西：
 *   - 危险操作用 confirm() 做二次确认。inner 应用跑在内嵌窗口里，浏览器会把
 *     confirm/alert/prompt 拦掉，于是 `if (!confirm(...)) return;` 永远返回，
 *     删库、删备份、还原三个按钮全是死的。这里把 confirm/alert/prompt 做成
 *     【直接抛异常】，谁碰谁当场失败。
 *   - catch 分支里调用了一个不存在的 esc()：异常被 catch 接住 → catch 里又抛
 *     一次 → 整个 Promise 静默 reject，界面永远停在"处理中"。
 *
 * 两个反过来会咬人的地方：
 *   - 假阴性：fixture 用空数据跑的话渲染分支根本不执行。所以下面的 fixture
 *     【必须】带内容，而且字符串里故意放了单引号和尖括号 —— 转义漏了会当场暴露。
 *   - 假阳性：假 DOM 太假的话，一堆无关的 TypeError 会淹掉真错误。
 *
 * 用法: node scripts/checkjs.js <html 文件>
 */

"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const file = process.argv[2];
if (!file) {
  console.error("用法: node checkjs.js <html 文件>");
  process.exit(2);
}
const html = fs.readFileSync(file, "utf8");

const scripts = [];
{
  // 只取内联脚本；<script src=...> 那个是绿联 JSSDK，下面用假的顶上。
  const re = /<script\b([^>]*)>([\s\S]*?)<\/script>/gi;
  let m;
  while ((m = re.exec(html)) !== null) {
    if (/\bsrc\s*=/.test(m[1])) continue;
    if (m[2].trim()) scripts.push(m[2]);
  }
}
if (scripts.length === 0) {
  console.error("::error:: [checkjs] 页面里没有找到内联脚本");
  process.exit(1);
}

// ---- 最小假 DOM ----------------------------------------------------------

// 从一段 HTML 里把带指定属性的元素抠出来。用于 querySelectorAll("[data-xxx]")：
// 页面在 innerHTML 里拼出表格行、再对着自己 querySelectorAll 绑事件，
// 不支持这条路的话删库/删备份/还原这三个回调一次都不会被执行 —— 那正好是
// 最容易出 bug 的地方。
function scanAttr(source, attr) {
  const out = [];
  const re = new RegExp(attr + '\\s*=\\s*["\']([^"\']*)["\']', "g");
  let m;
  while ((m = re.exec(source)) !== null) out.push(m[1]);
  return out;
}

// 页面在 innerHTML 里动态拼出来、再绑上回调的元素。见 querySelectorAll。
const dynamicElements = [];

function makeElement(id) {
  return {
    id: id || "",
    tagName: "DIV",
    value: "",
    checked: false,
    disabled: false,
    textContent: "",
    innerHTML: "",
    className: "",
    files: [],
    style: {},
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0,
    dataset: {},
    onclick: null,
    onchange: null,
    children: [],
    _listeners: [],
    _attrs: {},
    classList: {
      add() {}, remove() {}, toggle() {}, contains() { return false; },
    },
    getAttribute(name) { return name in this._attrs ? this._attrs[name] : null; },
    setAttribute(name, v) { this._attrs[name] = v; },
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) {
      const i = this.children.indexOf(c);
      if (i >= 0) this.children.splice(i, 1);
      return c;
    },
    addEventListener(ev, fn) { this._listeners.push([ev, fn]); },
    removeEventListener() {},
    select() {},
    focus() {},
    click() { if (typeof this.onclick === "function") this.onclick(); },
    querySelectorAll(sel) {
      const mm = /\[([A-Za-z0-9_-]+)\]/.exec(sel);
      if (!mm) return [];
      return scanAttr(this.innerHTML || "", mm[1]).map((v) => {
        const el = makeElement("");
        el.setAttribute(mm[1], v);
        el.textContent = "按钮";
        // 记下来：页面是在 innerHTML 里拼出表格行、再对着自己 querySelectorAll
        // 绑 onclick 的，不留引用的话这些回调一次都触发不到 —— 而它们恰好是
        // 删库/删备份/还原这几个最容易出事的操作。
        dynamicElements.push([mm[1] + "=" + v, el]);
        return el;
      });
    },
    querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
  };
}

// 页面上真实存在的 id —— 只有这些才返回元素，其余返回 null。
// 这样"引用了不存在的 id"才会像真浏览器一样抛 TypeError。
const declaredIds = new Set(scanAttr(html, "id"));

const elements = new Map();
const document = {
  readyState: "complete",
  body: makeElement("body"),
  activeElement: null,
  getElementById(id) {
    if (!declaredIds.has(id)) return null;
    if (!elements.has(id)) elements.set(id, makeElement(id));
    return elements.get(id);
  },
  createElement(tag) {
    const el = makeElement("");
    el.tagName = String(tag).toUpperCase();
    return el;
  },
  querySelectorAll() { return []; },
  querySelector() { return null; },
  addEventListener() {},
  execCommand() { return true; },
};

// ---- 假的 /api 响应。内容【不能为空】，否则渲染分支不会被执行。----------
const FIXTURES = {
  "me": { id: "42", name: "admin's account", type: "users" },
  "diag": {
    gateway_headers: { "Ugreen-User-ID": "42" },
    authenticated: true, same_host_ok: true, hint: "ok",
  },
  "status": {
    server: { running: true, pid: 1234, phase: "运行中", last_error: "", uptime_sec: 98765 },
    pg_port: 5432,
    allow_lan: true,
    timezone: "Asia/Shanghai",
    app_dir: "/volume1/@appdata/com.personal.postgresql17",
    // 故意带单引号和尖括号：转义漏了会当场暴露
    data_dir: "/volume1/test/it's <data>/postgresql17-data/pgdata",
    initialized: true,
    data_broken: true,
    data_state: "有 PG_VERSION 但没有初始化完成的痕迹",
    app_version: "17.10-1.pgdg12+1",
    version: "17.10",
    migrate_to: "/volume1/test/新位置/postgresql17-data",
    migration: {
      running: false, phase: "迁移完成", from: "/old/pgdata", to: "/new/pgdata",
      error: "", result: "数据已迁移", elapsed_sec: 42, finished: true,
    },
  },
  "connection": {
    user: "postgres", password: "Abc'defGh23jkLmn", port: 5432, database: "postgres",
    roles: { "immich": "Role'Pass23xyz" },
  },
  "databases": {
    items: [
      { name: "postgres", owner: "postgres", size: "8 MB", encoding: "UTF8" },
      { name: "immich", owner: "immich", size: "1200 MB", encoding: "UTF8" },
    ],
    total: 2,
  },
  "databases/drop": { ok: true, warning: "库已删除，但账号删不掉" },
  "backups": {
    items: [{ name: "dumpall-20260808-120000.sql.gz", size: 1048576, mtime: "2026-08-08 12:00:00" }],
    total: 1,
  },
  "backups/create": { name: "dumpall-20260808-130000.sql.gz", size: 2048 },
  "backups/delete": { ok: true },
  "backups/restore": { ok: true, output: "CREATE ROLE\nCREATE DATABASE\n" },
  "backups/upload/init": { session: "0123456789abcdef0123456789abcdef", chunk_size: 8388608 },
  "backups/upload/chunk": { ok: true, index: 0, size: 1 },
  "backups/upload/complete": { ok: true, name: "x.sql.gz", size: 1 },
  "custom-conf": { content: "# shared_buffers = 512MB\n" },
  "logs": { lines: ["2026-08-08 12:00:00 UTC [123] postgres@postgres LOG:  ready"] },
  "superuser-password": { password: "NewPass23xyzABCD" },
  "settings": { ok: true, restarting: true },
  "server": { ok: true },
  "migrate": { ok: true, to: "/volume1/test/新位置/postgresql17-data" },
  "reinit": { ok: true, retired: "/old/pgdata.broken-20260808-120000" },
};

let fetchCalls = 0;
let sawAuthHeader = 0;
const seenPaths = new Set();
async function fakeFetch(url, opt) {
  fetchCalls++;
  if (opt && opt.headers && opt.headers["Ugreen-Ttk"]) sawAuthHeader++;
  const key = String(url).replace(/^\/api\//, "").split("?")[0];
  seenPaths.add(key);
  if (!(key in FIXTURES)) {
    throw new Error("checkjs: 没有为 /api/" + key + " 准备 fixture，请补一个");
  }
  const body = JSON.parse(JSON.stringify(FIXTURES[key]));
  return {
    ok: true,
    status: 200,
    json: async () => body,
    blob: async () => ({ size: 1 }),
  };
}

// 假的 UGOS JSSDK。必须造出来，否则取 token、带 Ugreen-Ttk 头那整条路径
// 一行都不会执行，改坏了也测不出来（就是"假阴性"那个坑）。
const fakeCloudWindow = {
  async useCapacity(name, _data, timeout) {
    // 不给超时的话，真实环境里没有桥接对象时这个 Promise 会永远挂着，
    // 页面就一直卡在"读取中"。当成错误报出来。
    if (typeof timeout !== "number" || timeout <= 0) {
      throw new Error("useCapacity 调用没有给超时时间（第三个参数）");
    }
    if (name === "getThirdToken") return { third_token: "fake-third-token" };
    return {};
  },
};

const errors = [];

// 原生弹窗一律【直接抛】。inner 应用里它们是被拦掉的，用了就等于按钮是死的。
function blockedDialog(which) {
  return function () {
    throw new Error(which + "() 在绿联内嵌窗口里会被浏览器拦掉，" +
      "用它做确认/提示等于按钮点了没反应 —— 请改用页面内的两段式确认");
  };
}

const sandbox = {
  document,
  navigator: {},
  location: { hostname: "192.168.2.51", href: "" },
  fetch: fakeFetch,
  console: {
    log() {},
    // 页面在 refreshAll 的每一步里 catch 之后会 console.error，那正是我们要抓的
    error(...a) { errors.push("console.error: " + a.map(String).join(" ")); },
    warn() {},
  },
  setTimeout() { return 0; },   // 不真的排期，避免测试挂住
  clearTimeout() {},
  setInterval() { return 0; },  // 页面里有 5 秒轮询
  clearInterval() {},
  URL: { createObjectURL() { return "blob:fake"; }, revokeObjectURL() {} },
  JSON, Promise, Math, Date, String, Number, Boolean, Array, Object, Error,
  parseInt, parseFloat, isNaN, encodeURIComponent, decodeURIComponent,
  alert: blockedDialog("alert"),
  confirm: blockedDialog("confirm"),
  prompt: blockedDialog("prompt"),
};
sandbox.window = sandbox;
sandbox.globalThis = sandbox;
sandbox.window.isSecureContext = false;
sandbox.window.UGCloudWindow = fakeCloudWindow;

const context = vm.createContext(sandbox);

for (const code of scripts) {
  try {
    vm.runInContext(code, context, { filename: path.basename(file) });
  } catch (e) {
    console.error("::error:: [checkjs] 脚本加载即失败: " + ((e && e.stack) || e));
    process.exit(1);
  }
}

async function main() {
  const fn = sandbox.init;
  if (typeof fn !== "function") {
    console.error("::error:: [checkjs] 找不到入口函数 init()");
    process.exit(1);
  }
  try {
    await fn();
  } catch (e) {
    errors.push("init() 抛异常: " + ((e && e.stack) || e));
  }
  // boot() 是异步的，让它的微任务链跑完再触发回调
  await new Promise((r) => process.nextTick(r));
  await new Promise((r) => setImmediate(r));

  // init 里注册的回调，逐个触发。
  // 【每个点两次】：危险操作是两段式确认的，只点一次永远停在"再点一次确认"，
  // 真正干活的那段代码一行都不会跑到。
  const handlers = [];
  for (const [id, el] of elements) {
    for (const [ev, h] of el._listeners) handlers.push([id + "." + ev, el, h]);
    if (typeof el.onclick === "function") handlers.push([id + ".onclick", el, el.onclick]);
    if (typeof el.onchange === "function") handlers.push([id + ".onchange", el, el.onchange]);
  }
  if (handlers.length === 0) {
    errors.push("init() 之后一个事件回调都没注册上 —— 多半是某个步骤静默失败了");
  }
  for (const [name, el, h] of handlers) {
    for (let pass = 0; pass < 2; pass++) {
      try {
        await h.call(el, { target: el, preventDefault() {} });
      } catch (e) {
        errors.push(name + "（第 " + (pass + 1) + " 次点击）抛异常: " + ((e && e.stack) || e));
      }
    }
  }

  // 表格行里的按钮（删库 / 下载·还原·删除备份）是 innerHTML 拼出来再绑 onclick 的，
  // 上面那轮按 id 找不到。它们恰好是全页面最危险的几个操作，必须单独跑。
  //
  // ⚠ 必须先拍快照再遍历：点一下「删除」会触发重新加载列表，列表渲染又会
  // querySelectorAll 出一批新按钮塞进 dynamicElements —— 直接 for...of
  // 遍历这个数组就是个自己喂自己的死循环（会一路吃到 Node 堆溢出）。
  const rowSnapshot = dynamicElements.slice();
  dynamicElements.length = 0;
  let rowCalls = 0;
  for (const [label, el] of rowSnapshot) {
    if (typeof el.onclick !== "function") continue;
    rowCalls++;
    for (let pass = 0; pass < 2; pass++) {
      try {
        await el.onclick({ target: el, preventDefault() {} });
      } catch (e) {
        errors.push("表格行按钮 " + label + "（第 " + (pass + 1) + " 次点击）抛异常: " +
          ((e && e.stack) || e));
      }
    }
  }
  if (rowCalls === 0) {
    errors.push("表格行里一个按钮回调都没被触发 —— " +
      "删库/删备份/还原这几条路径没被覆盖到，检查 fixture 是不是空的");
  }

  // 鉴权头必须真的带上。漏了的话页面能打开、但每个请求都 401，
  // 而且那是装到 NAS 上才会暴露的 —— 这里钉死。
  if (sawAuthHeader === 0) {
    errors.push("没有任何一次 fetch 带上 Ugreen-Ttk 头 —— " +
      "UGOS 登录认证会失败，所有 /api 请求都会 401");
  }

  console.log("[checkjs] 触发了 %d 个回调，发起 %d 次 fetch（其中 %d 次带认证头），命中接口: %s",
    handlers.length, fetchCalls, sawAuthHeader,
    Array.from(seenPaths).sort().join(" "));
  if (errors.length) {
    for (const e of errors) console.error("::error:: [checkjs] " + e);
    process.exit(1);
  }
  console.log("[checkjs] 通过");
}

main().catch((e) => {
  console.error("::error:: [checkjs] " + ((e && e.stack) || e));
  process.exit(1);
});
