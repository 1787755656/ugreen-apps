#!/usr/bin/env node
/*
 * 在 Node 里造一个最小假 DOM，把页面脚本真跑一遍，再逐个调用入口函数。
 *
 * 为什么需要：这个页面是手写 HTML、没有构建流程，最容易出的 bug 是
 * 「调用了未定义的函数」，症状清一色是"点了没反应"，而 `node --check`
 * 只查语法，一个都查不出来。典型的例子是 catch 分支里调用了一个不存在的
 * esc()：异常被 catch 接住 → catch 里又抛一次 → 整个 Promise 静默 reject，
 * 界面就永远停在"处理中"。
 *
 * 两个反过来会咬人的地方：
 *   - 假阴性：fixture 用空数据跑，渲染分支根本没执行 —— 所以下面的
 *     status fixture 【必须】带上内容（警告、日志、局域网地址各来一份），
 *     而且字符串里故意放了单引号，属性转义漏了会当场暴露。
 *   - 假阳性：假 DOM 太假的话，一堆无关的 TypeError 会淹掉真错误。
 *
 * 用法: node checkjs.js <html 文件>
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

// 抽出所有 <script> 里的代码
const scripts = [];
const re = /<script\b[^>]*>([\s\S]*?)<\/script>/gi;
let m;
while ((m = re.exec(html)) !== null) {
  if (m[1].trim()) scripts.push(m[1]);
}
if (scripts.length === 0) {
  console.error("::error:: [checkjs] 页面里没有找到内联脚本");
  process.exit(1);
}

// ---- 最小假 DOM ----
function makeElement(id) {
  const el = {
    id: id || "",
    tagName: "DIV",
    value: "",
    checked: false,
    disabled: false,
    textContent: "",
    innerHTML: "",
    className: "",
    style: {},
    scrollTop: 0,
    scrollHeight: 0,
    onclick: null,
    onchange: null,
    children: [],
    getAttribute(name) { return this._attrs ? this._attrs[name] : null; },
    setAttribute(name, v) { (this._attrs = this._attrs || {})[name] = v; },
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) {
      const i = this.children.indexOf(c);
      if (i >= 0) this.children.splice(i, 1);
      return c;
    },
    addEventListener() {},
    removeEventListener() {},
    select() {},
    focus() {},
    click() { if (typeof this.onclick === "function") this.onclick(); },
  };
  return el;
}

const elements = new Map();
// 页面上真实存在的 id —— 只有这些才返回元素，其余返回 null，
// 这样"引用了不存在的 id"才会像真浏览器一样抛 TypeError。
const declaredIds = new Set();
{
  const idRe = /\bid\s*=\s*["']([A-Za-z0-9_-]+)["']/g;
  let x;
  while ((x = idRe.exec(html)) !== null) declaredIds.add(x[1]);
}

const document = {
  readyState: "complete",
  body: makeElement("body"),
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
  querySelectorAll(sel) {
    // 只支持我们真的用到的 button[data-copy]
    const out = [];
    const mm = /\[([a-z-]+)\]/.exec(sel);
    if (mm) {
      const attr = mm[1];
      const attrRe = new RegExp(attr + '\\s*=\\s*["\']([^"\']+)["\']', "g");
      let y;
      while ((y = attrRe.exec(html)) !== null) {
        const el = makeElement("");
        el.setAttribute(attr, y[1]);
        out.push(el);
      }
    }
    return out;
  },
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; },
  addEventListener() {},
  execCommand() { return true; },
  activeElement: null,
};

// ---- 假的 /api 响应。内容【不能为空】，否则渲染分支不会被执行。----
const FIXTURES = {
  "status": {
    server: {
      state: "running",
      version: "11.4.12-MariaDB",
      started_at: 1754400000,
      last_error: "",
    },
    operation: { kind: "", running: false },
    port: 3306,
    allow_lan: true,
    bind_address: "0.0.0.0",
    // 故意带单引号和尖括号：属性/文本转义漏了会当场暴露
    data_dir: "/volume1/test/it's <data>/mariadb-data",
    socket_path: "/volume1/@appdata/com.personal.mariadb/mariadb.sock",
    error_log: "/volume1/@applog/com.personal.mariadb/mariadb-error.log",
    root_password: "Abc'defGh23jkLmn",
    password_changed_by_user: false,
    lan_addresses: ["192.168.2.51", "10.0.0.7"],
    warnings: ["这是一条测试警告 <b>不该被当成 HTML</b>", "第二条 'warning'"],
  },
  "logs": { content: "2026-08-06 12:00:00 0 [Note] mariadbd: ready for connections.\n" },
  "password": { accepted: true, root_password: "NewPass23xyzABCD" },
  "network": { accepted: true, restarting: true },
  "service": { accepted: true },
};

let fetchCalls = 0;
let sawAuthHeader = 0;
async function fakeFetch(url, opt) {
  fetchCalls++;
  if (opt && opt.headers && opt.headers["Ugreen-Ttk"]) sawAuthHeader++;
  const key = String(url).replace(/^\/api\//, "").split("?")[0];
  if (!(key in FIXTURES)) {
    throw new Error("checkjs: 没有为 /api/" + key + " 准备 fixture，请补一个");
  }
  return {
    ok: true,
    status: 200,
    json: async () => JSON.parse(JSON.stringify(FIXTURES[key])),
  };
}

// 假的 UGOS JSSDK。必须造出来，否则 initAuth() 一进门就因为
// window.UGCloudWindow 不存在而提前返回 —— 取 token、带 Ugreen-Ttk 头这整条
// 路径就一行都不会执行，改坏了也测不出来（就是"假阴性"那个坑）。
const fakeCloudWindow = {
  async useCapacity(name, _data, timeout) {
    // 不给超时的话，真实环境里没有桥接对象时这个 Promise 会永远挂着，
    // 页面就一直卡在"读取中"。这里当成错误报出来。
    if (typeof timeout !== "number" || timeout <= 0) {
      throw new Error("useCapacity 调用没有给超时时间（第三个参数）");
    }
    if (name === "getThirdToken") return { third_token: "fake-third-token" };
    return {};
  },
};

const errors = [];
const sandbox = {
  document,
  window: { isSecureContext: false, UGCloudWindow: fakeCloudWindow },
  navigator: {},
  fetch: fakeFetch,
  console: {
    log() {},
    // 页面在 init 的每一步里 catch 之后会 console.error，那正是我们要抓的
    error(...a) { errors.push("console.error: " + a.map(String).join(" ")); },
    warn() {},
  },
  setTimeout(fn) { return 0; },   // 不真的排期，避免测试挂住
  clearTimeout() {},
  setInterval() { return 0; },    // 同上：页面里有 5 秒轮询
  clearInterval() {},
  JSON, Promise, Math, Date, String, Number, Boolean, Array, Object, Error,
  parseInt, parseFloat, isNaN, encodeURIComponent, decodeURIComponent,
};
sandbox.globalThis = sandbox;
sandbox.window.document = document;

const context = vm.createContext(sandbox);

// ---- 1. 加载脚本 ----
for (const code of scripts) {
  try {
    vm.runInContext(code, context, { filename: path.basename(file) });
  } catch (e) {
    console.error("::error:: [checkjs] 脚本加载即失败: " + (e && e.stack || e));
    process.exit(1);
  }
}

// ---- 2. 逐个调用入口函数 ----
async function main() {
  const entries = ["init"];
  for (const name of entries) {
    const fn = sandbox[name];
    if (typeof fn !== "function") {
      errors.push("找不到入口函数 " + name + "()");
      continue;
    }
    try {
      await fn();
    } catch (e) {
      errors.push(name + "() 抛异常: " + (e && e.stack || e));
    }
  }

  // init 里注册的点击/变更回调，逐个触发一遍
  const handlers = [];
  for (const [id, el] of elements) {
    if (typeof el.onclick === "function") handlers.push([id + ".onclick", el.onclick]);
    if (typeof el.onchange === "function") handlers.push([id + ".onchange", el.onchange]);
  }
  if (handlers.length === 0) {
    errors.push("init() 之后一个事件回调都没注册上 —— 多半是某个步骤静默失败了");
  }
  for (const [name, fn] of handlers) {
    try {
      await fn.call(elements.get(name.split(".")[0]));
    } catch (e) {
      errors.push(name + " 抛异常: " + (e && e.stack || e));
    }
  }

  // 鉴权头必须真的带上。漏了的话页面能打开、但每个请求都 401，
  // 而且那是装到 NAS 上才会暴露的 —— 这里钉死。
  if (sawAuthHeader === 0) {
    errors.push("没有任何一次 fetch 带上 Ugreen-Ttk 头 —— " +
      "UGOS 登录认证会失败，所有 /api 请求都会 401");
  }

  console.log("[checkjs] 执行了 %d 个回调，发起 %d 次 fetch（其中 %d 次带了认证头）",
    handlers.length, fetchCalls, sawAuthHeader);
  if (errors.length) {
    for (const e of errors) console.error("::error:: [checkjs] " + e);
    process.exit(1);
  }
  console.log("[checkjs] 通过");
}

main().catch((e) => {
  console.error("::error:: [checkjs] " + (e && e.stack || e));
  process.exit(1);
});
