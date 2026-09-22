#!/usr/bin/env node
/**
 * 假 DOM 运行检查器。
 *
 * `node --check` 只查语法，查不出「调用了页面上不存在的函数/变量」这类错误 ——
 * 而那类错误的真机症状清一色是「点了没反应」。这里造一个最小但够真的 DOM，
 * 把页面脚本真跑一遍，再把每个按钮的点击回调都触发一次。
 *
 * 三条防假阴性的措施：
 *  - fixture 用带内容的数据，不走提前返回分支
 *  - 脚本末尾 if (点击监听数 === 0) 直接判失败（init 是异步的，早点断言容易全绿）
 *  - 把 fetch 的 reject 显式接住，避免安静地什么都没测
 *
 * 用法：node scripts/check-js-runtime.js
 */

const fs = require("fs");
const path = require("path");
const vm = require("vm");

// 用法：node scripts/check-js-runtime.js [可选：要检查的 html 路径]
// 传路径主要是为了做反向验证（故意改坏一份，确认检查器真的会失败）
const HTML = process.argv[2]
  ? path.resolve(process.argv[2])
  : path.resolve(__dirname, "..", "src", "web", "index.html");
const html = fs.readFileSync(HTML, "utf8");

const scripts = [...html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g)].map(m => m[1]);
if (!scripts.length) {
  console.error("没有找到内联 <script>，检查器失效");
  process.exit(1);
}
const code = scripts.join("\n;\n");

/* ------------------------------------------------------------ 假 DOM */

const clickBindings = [];
const allFetchCalls = [];

class FakeClassList {
  constructor(el) { this.el = el; }
  add(c) { this.el._cls.add(c); }
  remove(c) { this.el._cls.delete(c); }
  contains(c) { return this.el._cls.has(c); }
  toggle(c, on) { on ? this.el._cls.add(c) : this.el._cls.delete(c); }
}

class FakeEl {
  constructor(id, tag) {
    this.id = id || "";
    this.tagName = (tag || "div").toUpperCase();
    this._cls = new Set();
    this.classList = new FakeClassList(this);
    this.textContent = "";
    this.value = "0";
    this.attrs = {};
    this._handlers = {};
    this._kids = [];
  }
  addEventListener(type, fn) {
    (this._handlers[type] = this._handlers[type] || []).push(fn);
    if (type === "click") clickBindings.push(this.id);
  }
  removeEventListener() {}
  fire(type, ev) {
    const list = this._handlers[type] || [];
    for (const fn of list) fn(ev || { target: this, preventDefault() {} });
    return list.length;
  }
  getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getElementsByTagName() { return this._kids; }
  querySelector() { return null; }
  closest() { return this; }
  appendChild(c) { this._kids.push(c); return c; }
  get className() { return [...this._cls].join(" "); }
  set className(v) { this._cls = new Set(String(v).split(/\s+/).filter(Boolean)); this.classList = new FakeClassList(this); }
  get style() { return this._style || (this._style = {}); }
  play() { this.paused = false; if (this.onplaystate) this.onplaystate(); return Promise.resolve(); }
  pause() { this.paused = true; }
  load() {}
}

class FakeVideo extends FakeEl {
  constructor() {
    super("v", "video");
    this.paused = true;
    this.muted = true;
    this.currentTime = 0;
    this.duration = 12.5;
    this.loop = false;
    this.volume = 1;
    this.src = "";
  }
}

const videoEl = new FakeVideo();

const elements = new Map();
function el(id, tag) {
  if (id === "v") return videoEl;
  if (!elements.has(id)) elements.set(id, new FakeEl(id, tag));
  return elements.get(id);
}

// 有内容的 fixture：开关的初始态
const docHandlers = {};
const storage = new Map();

const documentStub = {
  getElementById: id => el(id),
  querySelector: () => null,
  addEventListener: (t, fn) => { (docHandlers[t] = docHandlers[t] || []).push(fn); },
  removeEventListener: () => {},
  createElement: tag => new FakeEl("", tag),
  fullscreenElement: null,
  exitFullscreen: () => Promise.resolve(),
  hidden: false,
  body: new FakeEl("body", "body"),
  documentElement: new FakeEl("html", "html"),
};

const windowStub = {
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  addEventListener: () => {},
  setTimeout,
  clearTimeout,
};

const fetchStub = (url, opts) => {
  allFetchCalls.push(String(url));
  const u = String(url);
  if (u.includes("/api/next")) {
    return Promise.resolve({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ id: "fixture-id-1", count: 7 }),
    });
  }
  if (u.includes("/api/heartbeat")) {
    return Promise.resolve({
      ok: true, status: 200,
      json: () => Promise.resolve({ status: "ok", version: "0.1.0", served: 3 }),
    });
  }
  if (u.includes("/api/diag")) {
    return Promise.resolve({
      ok: true, status: 200,
      json: () => Promise.resolve({ ok: true, host: "txmov2.a.kwimgs.com", ms: 120 }),
    });
  }
  return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) });
};

/* ------------------------------------------------------------ 跑脚本 */

const failures = [];
const sandbox = {
  console,
  document: documentStub,
  window: windowStub,
  navigator: { userAgent: "node-check" },
  localStorage: {
    getItem: k => (storage.has(k) ? storage.get(k) : null),
    setItem: (k, v) => storage.set(k, String(v)),
    removeItem: k => storage.delete(k),
  },
  fetch: fetchStub,
  setTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
  Promise,
  Date,
  Math,
  JSON,
  isFinite,
  Number,
  String,
  Object,
  Array,
  Error,
  encodeURIComponent,
  decodeURIComponent,
  URL,
};

sandbox.globalThis = sandbox;
sandbox.self = sandbox;

// 未捕获的 Promise 拒绝 = 页面里静默失败的异步分支，必须报出来
let unhandled = null;
process.on("unhandledRejection", e => { unhandled = e; });

vm.createContext(sandbox);

try {
  vm.runInContext(code, sandbox, { filename: "inline.js" });
} catch (e) {
  failures.push("脚本执行时抛异常：" + e.message);
}

// init 是异步的（内部有 fetch 链），等几轮把队列清空再断言
const flush = () => new Promise(r => setImmediate(r));

async function main() {
  // ⚠ 整段包在 try 里：页面回调里的同步异常是从这里抛出去的，
  // 不接住的话 main() 的 Promise 直接 reject —— 检查器自己就静默退出了（踩过）。
  try {
    await runChecks();
  } catch (e) {
    failures.push("检查过程中抛异常：" + (e && e.message ? e.message : String(e)));
  }

  if (failures.length) {
    console.error("运行检查失败：");
    for (const f of failures) console.error("  ✗ " + f);
    process.exit(1);
  }
  console.log("运行检查通过：绑定 " + clickBindings.length + " 个点击回调，fetch " + allFetchCalls.length + " 次");
}

function fireSafe(el, type, ctx) {
  try {
    return el.fire(type, ctx || { target: el, preventDefault() {} });
  } catch (e) {
    failures.push((el.id || el.tagName) + " 的 " + type + " 回调抛异常：" + (e && e.message ? e.message : String(e)));
    return -1;
  }
}

async function runChecks() {
  for (let i = 0; i < 8; i++) await flush();
  await new Promise(r => setTimeout(r, 10));
  for (let i = 0; i < 8; i++) await flush();

  if (unhandled) failures.push("未捕获的 Promise 异常：" + (unhandled && unhandled.message ? unhandled.message : String(unhandled)));

  // 兜底断言：一个 click 都没绑上，说明脚本根本没跑到绑定那步
  if (clickBindings.length === 0) {
    failures.push("没有任何 click 监听被绑定 —— 脚本很可能在绑定之前就抛异常了");
  }

  // 逐个触发按钮回调
  const clickables = [
    "btn-play", "btn-mute", "btn-fs",
    "btn-settings", "btn-close", "sw-auto", "sw-muted", "tap", "mask",
  ];
  for (const id of clickables) {
    const e = elements.get(id);
    if (!e) { failures.push("HTML 里没有 id=" + id + " 的元素"); continue; }
    const n = fireSafe(e, "click");
    if (n === 0) failures.push(id + " 没有绑定 click 回调");
  }

  // 键盘
  for (const key of [" ", "ArrowDown", "n", "m", "f"]) {
    for (const fn of docHandlers.keydown || []) {
      try { fn({ key, target: { tagName: "BODY" }, preventDefault() {} }); }
      catch (err) { failures.push("keydown(" + key + ") 抛异常：" + err.message); }
    }
  }

  // 播放器事件
  for (const ev of ["playing", "waiting", "canplay", "ended", "error", "timeupdate", "loadedmetadata", "pause"]) {
    try { videoEl.fire(ev); } catch (err) { failures.push("video 的 " + ev + " 回调抛异常：" + err.message); }
  }

  // 断言真的干活了：loadNext 必然请求过 /api/next；首屏必有视频地址
  // 先再清一轮队列：点击回调里的 fetch 链是异步的，不等它跑完断言会看到中间的"测试中…"
  for (let i = 0; i < 8; i++) await flush();
  await new Promise(r => setTimeout(r, 10));

  if (!allFetchCalls.some(u => u.includes("/api/next"))) failures.push("没有请求过 /api/next —— 首屏加载没跑起来");
  if (!/^\/api\/(stream|raw)\?id=fixture-id-1/.test(videoEl.src)) {
    failures.push("视频地址没被设置，实际是：" + JSON.stringify(videoEl.src));
  }
  if (!/^第 [1-9]\d* 个$/.test(el("counter").textContent)) {
    failures.push("顶部计数没有更新，实际是：" + JSON.stringify(el("counter").textContent));
  }

  if (unhandled) failures.push("收尾时仍有未捕获异常：" + String(unhandled));
}

main();
