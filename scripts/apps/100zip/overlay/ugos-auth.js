/* UGOS 登录认证适配层（绿联移植版新增，上游没有这个文件）。
 *
 * 【要解决的问题】
 * 上游前端是给飞牛 fnOS 写的，那边不需要任何认证，所以它就是裸 fetch。
 * 搬到绿联之后，inner 应用的 /api 请求要先过 UGOS 网关：前端必须用 JSSDK 取一个
 * third_token、以 Ugreen-Ttk 头带上，网关校验后才会把用户身份注入给后端。
 * 缺了这一步，后端每个请求都判定"未通过登录认证"，界面上就是右下角不停弹提示。
 *
 * 【为什么还要换会话 Cookie】
 * 上游界面的"导出诊断报告"是浏览器 <a> 直接触发的下载（100zip 的报告走
 * blob，但保险起见 Cookie 通道也一并配上 —— 后端两条路都认）。fetch 能带
 * 自定义头，<a>/<img> 带不了；开场先用 token 换 Cookie，之后这些请求靠
 * Cookie 过闸。
 *
 * 【手机端（无桥环境）怎么活】
 * 手机上没有宿主桥，拿不到 Ttk，登录靠认证横幅里的表单：管理壳下发
 * 一次性 RSA 公钥（/api/session/key），密码加密后提交（/api/session 的
 * password_enc 通道）——手机端打开应用落在 HTTP 入口上，明文密码在那边
 * 是被拒收的，加密通道是唯一合法形态（2026-09 修复）。开场探测先发一个
 * 不带 Ttk 的空 POST /api/session：带有效会话 Cookie 时管理壳直接 200
 * 确认，手机端 reload 后即刻进入应用，不用再等 5 秒的桥超时。
 *
 * 【加载顺序是刻意的】
 * 本文件是普通脚本（会立即执行），上游的 app.js 是 module（会被推迟到最后）。
 * 但"推迟"仍然可能早于我们异步换完 Cookie，那样第一批 /api 请求会 401。
 * 所以干脆由本文件在认证就绪【之后】再把 app.js 插进来 —— 顺序就确定了。
 * 认证失败也照样插，让用户看到界面和上面那条错误说明，而不是一片空白。
 */
(function () {
  "use strict";

  var state = {
    token: null,
    session: false,
    diag: "尚未开始",
  };
  // 排查用：控制台里敲 __100zipAuth 就能看到这一侧发生了什么，
  // 后端的 /api/diag 只知道"没收到身份"，看不到这边为什么没取到 token。
  window.__100zipAuth = state;

  var origFetch = window.fetch.bind(window);

  function extractToken(info) {
    if (!info) return "";
    // 不同版本的返回形状不完全一样，几个都试一下比猜一个稳。
    return info.third_token || (info.data && info.data.third_token) || "";
  }

  function getToken() {
    var cw = window.UGCloudWindow;
    if (!cw || typeof cw.useCapacity !== "function") {
      state.diag = "没有加载到绿联 JSSDK（cloudwindow.js）";
      return Promise.resolve("");
    }
    // ⚠ 第三个参数（超时）必须给：不在绿联桌面窗口里跑时根本没有桥接对象，
    //   不给超时这个 Promise 会永远挂着，表现成页面一直空白。
    return cw
      .useCapacity("getThirdToken", {}, 5000)
      .then(function (info) {
        var token = extractToken(info);
        if (!token) {
          state.diag =
            "getThirdToken 没有返回 third_token（响应：" +
            JSON.stringify(info) +
            "）";
        }
        return token;
      })
      .catch(function (e) {
        state.diag = "getThirdToken 失败：" + ((e && e.message) || e);
        return "";
      });
  }

  function exchangeSession(token) {
    var headers = { Accept: "application/json" };
    if (token) headers["Ugreen-Ttk"] = token;
    return origFetch("/api/session", {
      method: "POST",
      headers: headers,
      credentials: "include",
    })
      .then(function (res) {
        state.session = res.ok;
        if (res.ok) {
          state.diag = "已通过认证";
        } else {
          state.diag = "换取会话失败：HTTP " + res.status;
        }
        return res.ok;
      })
      .catch(function (e) {
        state.session = false;
        state.diag = "换取会话失败：" + ((e && e.message) || e);
        return false;
      });
  }

  var booting = null;
  function boot() {
    if (booting) return booting;
    // 探测先行：无 Ttk 的空 POST 在管理壳那边有一条"有效会话 Cookie → 200 确认"
    // 的分支（2026-09 起）。手机端 reload 后这一发立刻 200，不用再等 5 秒桥超时，
    // 认证横幅也不会再弹。探测 401 才去取桥上的 token 走网关身份换 Cookie。
    booting = exchangeSession(null).then(function (ok) {
      if (ok) return true;
      return getToken().then(function (token) {
        state.token = token || null;
        if (!token) return false;
        return exchangeSession(token);
      });
    });
    return booting;
  }
  function reboot() {
    booting = null;
    return boot();
  }

  // 上游 app.js 用 APP_BASE 前缀请求（桌面内嵌时是 /app/100zip 之类），
  // 这里对"同源下任何路径的 /api/ 请求"统一补头。
  function isApiUrl(url) {
    if (typeof url !== "string") return false;
    var i = url.indexOf("/api/");
    if (i < 0) return false;
    if (i === 0) return true;
    // 允许 APP_BASE + /api/... 形式：前缀里不允许再出现 /api/，
    // 且整个串不能是完整外链（外链的 /api 不是我们的）。
    if (/^https?:\/\//i.test(url)) return false;
    return url.slice(0, i).indexOf("/api/") < 0;
  }

  // 给所有 /api 请求补上认证头和 Cookie。
  // 上游传的都是字符串路径 + 普通 init；万一将来传了 Request 对象，原样放行
  // （宁可不处理，也不要把请求改坏）。
  window.fetch = function (input, init) {
    if (!isApiUrl(input)) {
      return origFetch.apply(null, arguments);
    }
    var send = function (retried) {
      var opts = {};
      for (var k in init || {}) opts[k] = init[k];
      opts.credentials = "include";
      var headers = new Headers((init && init.headers) || {});
      if (state.token) headers.set("Ugreen-Ttk", state.token);
      opts.headers = headers;
      return origFetch(input, opts).then(function (res) {
        // 401 多半是 token 过期或应用重启把会话丢了 —— 重新走一遍再试一次。
        // 只重试一次，避免认证真的坏掉时变成死循环打满请求。
        // 没有 token（无桥环境，如手机浏览器）时重试没有意义，直接返回。
        if (res.status === 401 && !retried && state.token) {
          return reboot().then(function () {
            return send(true);
          });
        }
        return res;
      });
    };
    return boot().then(function () {
      return send(false);
    });
  };

  function showAuthBanner() {
    try {
      var bar = document.createElement("div");
      bar.style.cssText =
        "position:fixed;left:0;right:0;top:0;z-index:99999;padding:12px 16px;" +
        "background:#c0392b;color:#fff;font-size:13px;line-height:1.6;" +
        'font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif';
      bar.appendChild(document.createTextNode(
        "未通过 UGOS 登录认证：" + state.diag + "。"));
      var tip = document.createElement("div");
      tip.style.cssText = "opacity:.9;margin-top:2px";
      tip.textContent =
        "在 NAS 桌面里打开本应用可自动认证；手机等无桥环境可用 NAS 账号密码登录" +
        "（密码用一次性公钥加密后提交，不走明文）：";
      bar.appendChild(tip);

      var form = document.createElement("div");
      form.style.cssText = "display:flex;gap:6px;margin-top:8px;flex-wrap:wrap";
      function mkInput(type, ph) {
        var i = document.createElement("input");
        i.type = type;
        i.placeholder = ph;
        i.style.cssText =
          "flex:1;min-width:120px;padding:6px 8px;border:1px solid rgba(255,255,255,.5);" +
          "border-radius:4px;background:rgba(255,255,255,.12);color:#fff;font-size:13px";
        return i;
      }
      var user = mkInput("text", "NAS 用户名");
      var pass = mkInput("password", "NAS 密码");
      var btn = document.createElement("button");
      btn.type = "button";
      btn.textContent = "登录";
      btn.style.cssText =
        "padding:6px 14px;border:1px solid #fff;border-radius:4px;" +
        "background:#fff;color:#c0392b;font-size:13px;font-weight:600;cursor:pointer";
      var msg = document.createElement("span");
      msg.style.cssText = "flex-basis:100%;font-size:12px;opacity:.95";

      // ---- 一次性 RSA 公钥加密（登录表单专用） ----
      // 手机端落在 HTTP 入口上，非安全上下文里 crypto.subtle 不存在，
      // 所以用纯 JS 的 PKCS#1 v1.5 + e=65537（BigInt modpow）。
      // 与管理壳的 rsa.DecryptPKCS1v15 对齐，也和绿联自家登录同一算法。
      function b64ToBytes(b64) {
        var bin = atob(b64), out = new Uint8Array(bin.length);
        for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
        return out;
      }
      function bytesToBig(bytes) {
        var hex = "";
        for (var i = 0; i < bytes.length; i++) {
          hex += (bytes[i] < 16 ? "0" : "") + bytes[i].toString(16);
        }
        return hex ? BigInt("0x" + hex) : 0n;
      }
      function bigToBytes(x, len) {
        var out = new Uint8Array(len);
        for (var i = len - 1; i >= 0; i--) {
          out[i] = Number(x & 0xffn);
          x >>= 8n;
        }
        return out;
      }
      function modPow(base, exp, mod) {
        var result = 1n;
        base %= mod;
        while (exp > 0n) {
          if (exp & 1n) result = (result * base) % mod;
          base = (base * base) % mod;
          exp >>= 1n;
        }
        return result;
      }
      function rsaEncryptPassword(nB64, eB64, password) {
        var nBytes = b64ToBytes(nB64);
        var n = bytesToBig(nBytes);
        var e = bytesToBig(b64ToBytes(eB64));
        var k = nBytes.length; // 模长（字节）：2048 位 → 256
        var msgBytes = new TextEncoder().encode(password);
        if (msgBytes.length > k - 11) throw new Error("密码过长");
        var padLen = k - 3 - msgBytes.length;
        var pad = new Uint8Array(padLen);
        crypto.getRandomValues(pad);
        // PKCS#1 v1.5 的填充字节必须非零
        for (var i = 0; i < padLen; i++) {
          while (pad[i] === 0) crypto.getRandomValues(pad.subarray(i, i + 1));
        }
        var em = new Uint8Array(k);
        em[0] = 0;
        em[1] = 2;
        em.set(pad, 2);
        em[2 + padLen] = 0;
        em.set(msgBytes, 3 + padLen);
        var c = modPow(bytesToBig(em), e, n);
        return btoa(String.fromCharCode.apply(null, bigToBytes(c, k)));
      }

      btn.addEventListener("click", function () {
        var u = user.value;
        var p = pass.value;
        if (!u || !p) {
          msg.textContent = "请输入 NAS 用户名和密码。";
          return;
        }
        btn.disabled = true;
        btn.textContent = "验证中…";
        msg.textContent = "";
        // 领一次性公钥 → 加密密码 → 提交。密钥用后即废：重试要重取，
        // 抓包拿到的密文没有对应私钥、也没有重放窗口。
        origFetch("/api/session/key", { credentials: "include" })
          .then(function (res) {
            if (!res.ok) throw new Error("取登录公钥失败（HTTP " + res.status + "）");
            return res.json();
          })
          .then(function (key) {
            return origFetch("/api/session", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              credentials: "include",
              body: JSON.stringify({
                username: u,
                key_id: key.key_id,
                password_enc: rsaEncryptPassword(key.n, key.e, p),
              }),
            });
          })
          .then(function (res) {
            if (res.ok) {
              msg.textContent = "登录成功，正在刷新…";
              setTimeout(function () {
                location.reload();
              }, 400);
              return;
            }
            return res
              .json()
              .catch(function () {
                return null;
              })
              .then(function (b) {
                msg.textContent =
                  (b && b.error && b.error.message) || ("登录失败（HTTP " + res.status + "）");
                btn.disabled = false;
                btn.textContent = "登录";
              });
          })
          .catch(function (e) {
            msg.textContent = "登录请求失败：" + ((e && e.message) || e);
            btn.disabled = false;
            btn.textContent = "登录";
          });
      });
      form.appendChild(user);
      form.appendChild(pass);
      form.appendChild(btn);
      form.appendChild(msg);
      bar.appendChild(form);
      document.body.appendChild(bar);
    } catch (e) {
      /* 界面提示失败不影响主流程 */
    }
  }

  function loadApp() {
    // 上游 index.html 里的两行 module script 在这里接管：
    // fnos-bridge.js 已替换为 UGOS 版（自绘选择器 + cloudWindow 定位），
    // 仍以 module 加载（probe.html/callback.html 依赖它的 default export）；
    // app.js 在认证就绪后插入。async=false 保证两者按插入顺序执行。
    // ⚠ __WWW_VER__ 由 build.sh 替换成完整构建号：网关的静态服务没有
    //   cache-control 头，浏览器按启发式缓存发旧 JS —— 真机踩过：
    //   0003 升级后 fnos-bridge.js 还是旧版飞牛桥（挂起、定位失效），
    //   症状是“点了半天没反应/提示加载不了”。URL 带版本号，升级即破缓存。
    var bridge = document.createElement("script");
    bridge.type = "module";
    bridge.async = false;
    bridge.src = "js/fnos-bridge.js?v=__WWW_VER__";
    document.body.appendChild(bridge);

    var app = document.createElement("script");
    app.type = "module";
    app.async = false;
    app.src = "js/app.js?v=__WWW_VER__";
    document.body.appendChild(app);
  }

  function start() {
    // 排查锚点：控制台看到这行 = 新版前端已加载；看不到 = 浏览器在发缓存旧版
    console.log("[100zip] www build __WWW_VER__");
    boot().then(function (ok) {
      if (!ok) showAuthBanner();
      loadApp();
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
