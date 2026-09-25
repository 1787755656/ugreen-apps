"use strict";

const $ = (id) => document.getElementById(id);

let targets = []; // [{name, url}]
const seenURLs = new Set();
let appStarted = false;

async function api(method, path, body) {
  const opt = { method, headers: {}, credentials: "same-origin" };
  if (body !== undefined) {
    opt.headers["Content-Type"] = "application/json";
    opt.body = JSON.stringify(body);
  }
  const res = await fetch("/api" + path, opt);
  if (res.status === 403) {
    showMsg("未通过本机访问校验，请通过 NAS 桌面应用图标打开本应用。", "err");
    throw new Error("forbidden");
  }
  if (res.status === 401) {
    // session missing/expired -> show login form
    showAuth("login");
    throw new Error("unauthorized");
  }
  let data = null;
  try { data = await res.json(); } catch (e) { /* ignore */ }
  if (!res.ok) {
    const err = (data && data.error) || ("HTTP " + res.status);
    throw new Error(err);
  }
  return data;
}

function showMsg(text, kind) {
  const el = $("msg");
  el.textContent = text || "";
  el.className = "msg" + (kind ? " " + kind : "");
}

function renderTargets() {
  const box = $("targets");
  box.innerHTML = "";
  targets.forEach((t, i) => {
    const row = document.createElement("div");
    row.className = "target-row";
    const name = document.createElement("input");
    name.className = "name"; name.type = "text"; name.placeholder = "名称(可选)";
    name.value = t.name || "";
    name.oninput = () => { targets[i].name = name.value; };
    const url = document.createElement("input");
    url.className = "url"; url.type = "text"; url.placeholder = "http://127.0.0.1:8080";
    url.value = t.url || "";
    url.oninput = () => { targets[i].url = url.value; };
    const del = document.createElement("button");
    del.className = "del"; del.type = "button"; del.textContent = "×";
    del.onclick = () => { targets.splice(i, 1); renderTargets(); };
    row.append(name, url, del);
    box.append(row);
  });
}

function addTarget() {
  targets.push({ name: "", url: "" });
  renderTargets();
}

async function loadConfig() {
  try {
    const cfg = await api("GET", "/config");
    $("key").value = cfg.key || "";
    targets = (cfg.targets || []).map((t) => ({ name: t.name || "", url: t.url || "" }));
    if (targets.length === 0) targets.push({ name: "", url: "" });
    $("tlsInsecure").checked = !!cfg.tlsInsecure;
    renderTargets();
  } catch (e) { /* leave defaults */ }
}

async function saveConfig() {
  const payload = {
    key: $("key").value.trim(),
    targets: targets
      .map((t) => ({ name: (t.name || "").trim(), url: (t.url || "").trim() }))
      .filter((t) => t.url !== ""),
    tlsInsecure: $("tlsInsecure").checked,
  };
  try {
    await api("POST", "/config", payload);
    showMsg("配置已保存。", "ok");
  } catch (e) {
    showMsg("保存失败：" + e.message, "err");
  }
}

async function startProxy() {
  showMsg("");
  try {
    await api("POST", "/start");
    showMsg("已启动。", "ok");
    pollStatus();
  } catch (e) {
    showMsg("启动失败：" + e.message, "err");
  }
}

async function stopProxy() {
  showMsg("");
  try {
    await api("POST", "/stop");
    showMsg("已停止。", "ok");
    pollStatus();
  } catch (e) {
    showMsg("停止失败：" + e.message, "err");
  }
}

function setBadge(running) {
  const b = $("statusBadge");
  b.className = "badge " + (running ? "on" : "off");
  b.textContent = running ? "运行中" : "未运行";
  $("start").disabled = running;
  $("stop").disabled = !running;
}

function renderAccessURLs(urls) {
  urls.forEach((u) => seenURLs.add(u));
  const list = Array.from(seenURLs);
  const box = $("accessURLs");
  if (list.length === 0) {
    box.innerHTML = '<p class="empty">尚未获取到访问地址。启动后请稍候，或到 ' +
      '<a href="https://p2pee.cn/dashboard/p2p" target="_blank" rel="noopener">P2Pee 控制台</a> 查看。</p>';
    return;
  }
  box.innerHTML = "";
  list.forEach((u) => {
    const card = document.createElement("div");
    card.className = "url-card";
    const a = document.createElement("a");
    a.href = u; a.target = "_blank"; a.rel = "noopener"; a.textContent = u;
    const open = document.createElement("a");
    open.className = "open"; open.href = u; open.target = "_blank"; open.rel = "noopener";
    open.textContent = "打开";
    card.append(a, open);
    box.append(card);
  });
}

function appendLog(text) {
  const log = $("log");
  const line = document.createElement("div");
  if (/https?:\/\//.test(text)) line.className = "url-line";
  line.textContent = text;
  log.append(line);
  log.scrollTop = log.scrollHeight;
  while (log.childElementCount > 600) log.removeChild(log.firstChild);
}

async function pollStatus() {
  try {
    const st = await api("GET", "/status");
    setBadge(!!st.running);
    renderAccessURLs(st.accessURLs || []);
  } catch (e) { /* ignore transient / auth redirect */ }
}

function connectLogs() {
  const es = new EventSource("/api/logs");
  es.addEventListener("open", () => { /* ok */ });
  es.onmessage = (ev) => {
    const text = ev.data;
    appendLog(text.replace(/^\d+\t/, ""));
  };
  es.addEventListener("url", (ev) => {
    renderAccessURLs([ev.data]);
  });
  es.onerror = () => { /* browser auto-reconnects */ };
}

// ---- auth (login / first-run setup) ----

function showAuth(mode) {
  const ov = $("authOverlay");
  ov.classList.remove("hidden");
  $("logout").style.display = "none";
  if (mode === "setup") {
    $("authTitle").textContent = "设置管理密码";
    $("authHint").textContent = "首次使用，请设置一个管理密码以保护控制台。";
  } else {
    $("authTitle").textContent = "登录";
    $("authHint").textContent = "请输入管理密码。";
  }
  $("authMsg").textContent = "";
  $("authMsg").className = "msg";
  $("authPw").value = "";
  $("authPw").focus();
  $("authSubmit").onclick = () => doAuth(mode);
  $("authPw").onkeydown = (e) => { if (e.key === "Enter") doAuth(mode); };
}

async function doAuth(mode) {
  const pw = $("authPw").value;
  if (pw.length < 4) {
    $("authMsg").textContent = "密码至少 4 位。";
    $("authMsg").className = "msg err";
    return;
  }
  try {
    const path = mode === "setup" ? "/setup" : "/login";
    const res = await fetch("/api" + path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "same-origin",
      body: JSON.stringify({ password: pw }),
    });
    if (res.ok) {
      $("authOverlay").classList.add("hidden");
      $("logout").style.display = "";
      enterApp();
    } else {
      let d = null;
      try { d = await res.json(); } catch (e) { /* ignore */ }
      $("authMsg").textContent = (d && d.error) || ("HTTP " + res.status);
      $("authMsg").className = "msg err";
    }
  } catch (e) {
    $("authMsg").textContent = "请求失败：" + e.message;
    $("authMsg").className = "msg err";
  }
}

async function doLogout() {
  try {
    await fetch("/api/logout", { method: "POST", credentials: "same-origin" });
  } catch (e) { /* ignore */ }
  showAuth("login");
}

function enterApp() {
  if (appStarted) {
    pollStatus();
    return;
  }
  appStarted = true;
  loadConfig();
  pollStatus();
  setInterval(pollStatus, 3000);
  connectLogs();
}

async function initAuth() {
  try {
    const a = await api("GET", "/auth");
    if (a.setup) {
      showAuth("setup");
    } else if (!a.authenticated) {
      showAuth("login");
    } else {
      $("authOverlay").classList.add("hidden");
      $("logout").style.display = "";
      enterApp();
    }
  } catch (e) {
    // /api/auth unavailable (older build) -> fall through to the app.
    $("authOverlay").classList.add("hidden");
    enterApp();
  }
}

function init() {
  renderTargets();
  $("addTarget").onclick = addTarget;
  $("save").onclick = saveConfig;
  $("start").onclick = startProxy;
  $("stop").onclick = stopProxy;
  $("logout").onclick = doLogout;
  initAuth();
}

init();
