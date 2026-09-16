#!/usr/bin/env node
/* 桥接层假 DOM 冒烟测试（照 checkjs 方法论：把页面脚本真跑一遍）。
 *
 * 覆盖：自绘选择器的开框/导航/单选/多选/取消/空根引导、
 *       openFileManager 的 cloudWindow 调用形状与降级、openAppSetting。
 * 抓的是 ReferenceError / TypeError / 事件绑定缺失 —— 语法检查查不出的那类。
 */
"use strict";

/* ---------- 最小假 DOM ---------- */

function makeEl(tag) {
  const el = {
    tagName: tag.toUpperCase(),
    children: [],
    _handlers: {},
    style: {},
    dataset: {},
    value: "",
    textContent: "",
    className: "",
    id: "",
    disabled: false,
    checked: false,
    type: "",
    hidden: false,
    parentNode: null,
    appendChild(child) {
      child.parentNode = el;
      el.children.push(child);
      if (child.id) registry.set(child.id, child);
      return child;
    },
    removeChild(child) {
      const i = el.children.indexOf(child);
      if (i >= 0) el.children.splice(i, 1);
      child.parentNode = null;
      if (child.id) registry.delete(child.id);
    },
    addEventListener(type, fn) {
      (el._handlers[type] = el._handlers[type] || []).push(fn);
    },
    removeEventListener() {},
    dispatch(type) {
      for (const fn of el._handlers[type] || []) fn({ target: el, stopPropagation() {} });
    },
    querySelector(sel) {
      if (sel === "input[type=checkbox]") {
        const stack = [...el.children];
        while (stack.length) {
          const n = stack.shift();
          if (n.tagName === "INPUT" && n.type === "checkbox") return n;
          stack.push(...n.children);
        }
      }
      if (sel.startsWith(".")) {
        const cls = sel.slice(1);
        const stack = [...el.children];
        while (stack.length) {
          const n = stack.shift();
          if (typeof n.className === "string" && n.className.split(/\s+/).includes(cls)) return n;
          stack.push(...n.children);
        }
      }
      return null;
    },
    querySelectorAll(sel) {
      const out = [];
      const stack = [...el.children];
      while (stack.length) {
        const n = stack.shift();
        if (sel === ".ugos-row" && typeof n.className === "string" && n.className.split(/\s+/).includes("ugos-row")) out.push(n);
        stack.push(...n.children);
      }
      return out;
    },
  };
  let _text = "";
  Object.defineProperty(el, "textContent", {
    // 真实 DOM 的 textContent 会聚合所有后代文本；设置则清空子节点后写自身
    get: () => _text + el.children.map((c) => c.textContent).join(""),
    set(v) {
      _text = String(v);
      for (const c of el.children) c.parentNode = null;
      el.children.length = 0;
    },
  });
  let _innerHTML = "";
  Object.defineProperty(el, "innerHTML", {
    get: () => _innerHTML,
    set(v) {
      _innerHTML = v;
      if (v === "") for (const c of el.children) c.parentNode = null;
      el.children.length = 0;
    },
  });
  return el;
}

const registry = new Map();
const document = {
  head: makeEl("head"),
  body: makeEl("body"),
  createElement: makeEl,
  getElementById(id) {
    return registry.get(id) || null;
  },
  addEventListener() {},
};
const sessionStorageStub = { _m: new Map(), getItem(k) { return this._m.has(k) ? this._m.get(k) : null; }, setItem(k, v) { this._m.set(k, v); }, removeItem(k) { this._m.delete(k); } };

globalThis.window = globalThis;
globalThis.document = document;
globalThis.sessionStorage = sessionStorageStub;
globalThis.window.top = globalThis.window;

/* ---------- fetch 桩 ---------- */

const scanFixture = {
  "/volume1/media": [
    { path: "/volume1/media/sub", name: "sub", kind: "dir" },
    { path: "/volume1/media/a.zip", name: "a.zip", kind: "file", size: 1024 },
    { path: "/volume1/media/b.txt", name: "b.txt", kind: "file", size: 3 },
    { path: "/volume1/media/c.7z", name: "c.7z", kind: "file", size: 2048 },
  ],
  "/volume1/media/sub": [],
};
let rootsFixture = ["/volume1/media"];
let fetchLog = [];
globalThis.fetch = async (path, opts) => {
  fetchLog.push({ path, opts });
  if (path === "/api/context") {
    return { ok: true, json: async () => ({ ok: true, data: { accessibleFolders: rootsFixture } }) };
  }
  if (path === "/api/scan") {
    const body = JSON.parse(opts.body);
    const files = scanFixture[body.path.replace(/\/+$/, "")];
    if (!files) return { ok: false, status: 404, json: async () => ({ ok: false, error: { code: "PATH_NOT_FOUND", message: "找不到该文件或目录", hint: "" } }) };
    return { ok: true, json: async () => ({ ok: true, data: { files, root: body.path, total: files.length } }) };
  }
  return { ok: true, json: async () => ({ ok: true, data: {} }) };
};

/* ---------- cloudWindow 桩 ---------- */

const capCalls = [];
globalThis.UGCloudWindow = {
  useCapacity: async (name, data, timeout) => {
    capCalls.push({ name, data, timeout });
    if (name === "openApp" && data.data.filePathOpen === "/boom") throw new Error("bridge gone");
    return { ok: true };
  },
};

/* ---------- 断言工具 ---------- */

let failures = 0;
function ok(cond, msg) {
  if (cond) console.log("  ✓ " + msg);
  else { failures++; console.error("  ✗ " + msg); }
}
const tick = () => new Promise((r) => setImmediate(r));
async function settle() { for (let i = 0; i < 8; i++) await tick(); }

function dlg() { return registry.get("ugosPickDlg"); }
function list() { const d = dlg(); return d && d.querySelector(".ugos-list"); }
function rowByPath(p) {
  const l = list();
  for (const r of l.querySelectorAll(".ugos-row")) if (r.dataset.path === p) return r;
  return null;
}
function buttonByText(text) {
  const d = dlg();
  const stack = [...d.children];
  while (stack.length) {
    const n = stack.shift();
    if (n.tagName === "BUTTON" && n.textContent === text) return n;
    stack.push(...n.children);
  }
  return null;
}

/* ---------- 用例 ---------- */

async function main() {
  const { existsSync } = await import("node:fs");
const bridgeUrl = ["./overlay/fnos-bridge.js", "../overlay/fnos-bridge.js"]
  .map((p) => new URL(p, import.meta.url))
  .find((u) => existsSync(u));
const api = (await import(bridgeUrl.href)).default;
  ok(window.fnosBridge === api, "window.fnosBridge 已挂载");
  ok(api && typeof api.pickDirectory === "function", "契约方法齐全");

  // 1. 目录选择：单根自动进入 → 进子目录 → 选择当前目录
  {
    const p = api.pickDirectory();
    await settle();
    ok(rowByPath("/volume1/media/sub") !== null, "单根自动进入，子目录已列出");
    ok(rowByPath("/volume1/media/a.zip") === null, "目录模式不显示文件");
    rowByPath("/volume1/media/sub").dispatch("click");
    await settle();
    const btn = buttonByText("选择当前目录");
    ok(btn && btn.disabled === false, "进入子目录后确认按钮可用");
    btn.dispatch("click");
    const got = await p;
    ok(JSON.stringify(got) === JSON.stringify(["/volume1/media/sub"]), "目录选择返回所选路径: " + JSON.stringify(got));
    ok(registry.get("ugosPickDlg") === undefined || !dlg(), "对话框已关闭");
  }

  // 2. 目录选择：取消返回空数组
  {
    const p = api.pickDirectory();
    await settle();
    buttonByText("取消").dispatch("click");
    const got = await p;
    ok(Array.isArray(got) && got.length === 0, "取消返回 []");
  }

  // 3. 文件单选 + accept 过滤
  {
    const p = api.pickFiles({ accept: [".zip", ".7z"] });
    await settle();
    ok(rowByPath("/volume1/media/a.zip") !== null, "匹配扩展名的文件已列出");
    ok(rowByPath("/volume1/media/b.txt") === null, "不匹配扩展名的文件被过滤");
    ok(rowByPath("/volume1/media/sub") !== null, "目录仍可导航");
    rowByPath("/volume1/media/a.zip").dispatch("click");
    const got = await p;
    ok(JSON.stringify(got) === JSON.stringify(["/volume1/media/a.zip"]), "单选点击即返回: " + JSON.stringify(got));
  }

  // 4. 文件多选：整行切换勾选，确认返回集合
  {
    const p = api.pickFiles({ multiple: true });
    await settle();
    ok(rowByPath("/volume1/media/b.txt") !== null, "无 accept 时全量列出");
    rowByPath("/volume1/media/a.zip").dispatch("click");
    rowByPath("/volume1/media/c.7z").dispatch("click");
    rowByPath("/volume1/media/c.7z").dispatch("click"); // 再点一次 = 取消勾选
    const btn = buttonByText("确定（已选 1 项）");
    ok(btn && btn.disabled === false, "确认按钮显示已选数量");
    btn.dispatch("click");
    const got = await p;
    ok(JSON.stringify(got) === JSON.stringify(["/volume1/media/a.zip"]), "多选返回勾选集合: " + JSON.stringify(got));
  }

  // 5. 手输路径导航 + 越界报错不崩
  {
    const p = api.pickDirectory();
    await settle();
    const input = dlg().querySelector(".ugos-toolbar") ? findInput() : null;
    function findInput() {
      const stack = [...dlg().children];
      while (stack.length) {
        const n = stack.shift();
        if (n.tagName === "INPUT") return n;
        stack.push(...n.children);
      }
      return null;
    }
    input.value = "/volume1/media/sub/";
    buttonByText("载入").dispatch("click");
    await settle();
    const btn = buttonByText("选择当前目录");
    btn.dispatch("click");
    const got = await p;
    ok(JSON.stringify(got) === JSON.stringify(["/volume1/media/sub"]), "手输路径（去尾斜杠）可选: " + JSON.stringify(got));
  }
  {
    const p = api.pickDirectory();
    await settle();
    const input = (() => { const s = [...dlg().children]; while (s.length) { const n = s.shift(); if (n.tagName === "INPUT") return n; s.push(...n.children); } })();
    input.value = "/volume1/未授权";
    buttonByText("载入").dispatch("click");
    await settle();
    const hint = findHint();
    ok(/找不到|未授权|HTTP/.test(hint.textContent), "越界路径显示后端报错不崩溃: " + hint.textContent);
    buttonByText("取消").dispatch("click");
    await p;
  }

  // 6. 空根引导
  {
    rootsFixture = [];
    const p = api.pickDirectory();
    await settle();
    const l = list();
    ok(/应用中心/.test(l.textContent), "空根显示应用中心引导文案");
    buttonByText("取消").dispatch("click");
    ok((await p).length === 0, "空根取消返回 []");
    rootsFixture = ["/volume1/media"];
  }

  // 7. openFileManager：形状与降级
  {
    ok((await api.openFileManager("/volume1/media/a.zip")) === true, "openFileManager 成功返回 true");
    const call = capCalls.find((c) => c.name === "openApp");
    ok(call && call.data.appId === "com.ugreen.filemgr", "appId 正确");
    ok(call.data.data && call.data.data.filePathOpen === "/volume1/media/a.zip", "filePathOpen 携带完整路径");
    ok(call.timeout === 3000, "给了 3 秒超时（防手机浏览器挂死）");
    ok((await api.openFileManager("/boom")) === false, "桥调用失败降级 false");
  }

  // 8. openAppSetting / describe / 会话存取
  {
    ok((await api.openAppSetting()) === true, "openAppSetting 走 openAppCenterDetail");
    ok(capCalls.some((c) => c.name === "openAppCenterDetail"), "能力名正确");
    const d = api.describe();
    ok(d.via === "ugos-selfdrawn" && d.sdkLoaded === true, "describe 报告 UGOS 桥状态");
    api.setPendingPaths(["/x"]);
    ok(JSON.stringify(api.takePendingPaths()) === JSON.stringify(["/x"]), "pendingPaths 存取");
    ok(api.takePendingPaths().length === 0, "takePendingPaths 取后即清");
    const cb = api.parseCallback("https://nas/app/100zip/callback.html?status=ok&path=/volume1/a");
    ok(cb.paths[0] === "/volume1/a", "parseCallback 解析路径参数");
  }

  console.log(failures ? `\n${failures} 个断言失败` : "\n全部通过");
  process.exit(failures ? 1 : 0);
}

function findHint() {
  const s = [...dlg().children];
  while (s.length) {
    const n = s.shift();
    if (typeof n.className === "string" && n.className.includes("hint")) return n;
    s.push(...n.children);
  }
  return { textContent: "" };
}

main().catch((e) => { console.error(e); process.exit(1); });
