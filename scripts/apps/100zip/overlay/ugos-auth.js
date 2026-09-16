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
    booting = getToken().then(function (token) {
      state.token = token || null;
      return exchangeSession(token);
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
        if (res.status === 401 && !retried) {
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
        "position:fixed;left:0;right:0;top:0;z-index:99999;padding:10px 16px;" +
        "background:#c0392b;color:#fff;font-size:13px;line-height:1.6;" +
        'font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif';
      bar.textContent =
        "未通过 UGOS 登录认证：" +
        state.diag +
        "。请从 NAS 桌面的应用图标打开本应用（直接敲 IP:端口 是不行的）。";
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
