/* 下载任务 → 在系统「文件管理器」里定位（绿联移植版新增，上游没有这个文件）。
 *
 * 【为什么是一层外挂脚本，而不是改上游 app.js】
 * 本工程的规矩是上游业务代码一行都不改（跟版本时只需要换 .build-cache/upstream
 * 的 commit）。上游的任务卡片是 renderDownloads() 里用 DOM API 现搓出来的，
 * 没有插件钩子；所以这里不去动它，改成【观察 DOM、给卡片补一个按钮】——
 * 上游改版只要卡片还是 .download-card + .download-save-path 这个形状就继续有效，
 * 形状变了也只是按钮不出现，不会把下载界面弄坏。
 *
 * 【只在「已完成」的任务上出现】
 * 下载中的任务磁盘上只有「.<名字>.clouddl.part」，跳过去等于定位一个不存在的
 * 文件，反而让人以为下载坏了；所以按钮只在 status 为「已完成」时补。
 *
 * 【跳转怎么写才对】
 * 绿联桌面能力是 cloudWindow.useCapacity('openApp', ...)。官方文档里"打开文件
 * 管理器并定位"的示例写的是 data:{path}，那是错的 —— 只会唤醒窗口、不导航。
 * 真机（本项目另一条产品线 100zip / liteparser 验过）能定位的写法是：
 *
 *     openApp + appId='com.ugreen.filemgr' + data.filePathOpen=<绝对路径>
 *
 * 传【文件】路径 → 打开所在文件夹并选中该文件；传【目录】→ 直接进那个目录。
 * openLocalPath 用不上：它操作的是【用户电脑】的本地文件系统，不是 NAS。
 *
 * 【第三个参数（超时）必须给】
 * 不在绿联桌面窗口里跑（独立浏览器标签、手机浏览器）时根本没有桥接对象，
 * 不给超时这个 Promise 会永远挂着，按钮点下去毫无反应。
 *
 * 【桥可用性怎么判断】
 * 不能看 window.UGCloudWindow 存不存在 —— cloudwindow.js 会无条件定义它，
 * 普通浏览器里对象也在。真正的判据是 ugos-auth.js 有没有拿到 third_token
 * （它挂在 window.__clouddlAuth.token 上），拿到才说明确实在桌面窗口内。
 *
 * 【为什么不能有"抖一下"】
 * 上游每秒轮询一次进度、整块 list.replaceChildren() 重画。补按钮这件事如果
 * 被推迟到定时器里（哪怕 60ms），那 60ms 里卡片上就是没有按钮的 —— 表现成
 * 按钮每秒钟闪一下。所以这里**在 MutationObserver 的回调里同步补齐**：回调是
 * 微任务，在本帧绘制之前跑完，按钮和卡片出现在同一帧；再按 save_path 复用同一个
 * <button> 节点，重画时只是把它挪到新卡片上，连 hover 状态都不会掉。
 */
(function () {
  "use strict";

  var FILEMGR_APP_ID = "com.ugreen.filemgr";
  var BTN_CLASS = "clouddl-locate";
  var ROW_CLASS = "clouddl-locate-row";
  var STYLE_ID = "clouddl-locate-style";
  // 上游 statusLabels 里的「已完成」，只有这一种状态才补按钮（理由见文件头）。
  var COMPLETED = "已完成";

  // 应用私有目录（/volume1/@appstore/<appid>/data/downloads）是兜底下载目录，
  // 它不在任何共享文件夹里，文件管理器压根浏览不到 —— 点过去只会让人以为坏了。
  // 这种情况不跳转，直接说清楚该怎么办。
  var PRIVATE_DIR_RE = /\/@app(store|data)\//;

  // save_path → <button>。重画时复用同一个节点，避免按钮被反复销毁重建。
  var buttons = new Map();

  /* ---------- 小工具 ---------- */

  // 样式就地注入：上游 styles.css 每次构建都会被覆盖，改它等于白改；
  // 而且只加两条规则，用它们已有的 .button/.button-secondary 保证跟随主题变量。
  function injectStyle() {
    if (document.getElementById(STYLE_ID)) return;
    var style = document.createElement("style");
    style.id = STYLE_ID;
    style.textContent =
      "." + ROW_CLASS + "{display:flex;justify-content:flex-start;gap:8px;margin-top:10px}" +
      "." + BTN_CLASS + "{min-height:32px;padding:0 12px;font-size:12px;font-weight:600}";
    (document.head || document.documentElement).appendChild(style);
  }

  function notify(message, isError) {
    var region = document.getElementById("toast-region");
    if (!region) {
      // 极端情况下（页面还没铺完）退化成 console，至少排查时看得见
      if (window.console) console.warn("[clouddl] " + message);
      return;
    }
    var item = document.createElement("div");
    item.className = "toast" + (isError ? " is-error" : "");
    item.textContent = message;
    region.append(item);
    window.setTimeout(function () {
      item.remove();
    }, 4200);
  }

  function bridgeReady() {
    var state = window.__clouddlAuth;
    return !!(state && state.token);
  }

  /* ---------- 从卡片上把任务信息读回来 ---------- */

  // 上游在卡片上留了两处线索：
  //   .status-label 的文字来自上游 statusLabels（已完成为「已完成」）
  //   .download-save-path 的 title 属性 = task.save_path（原始值，最可靠），
  //   文本是「保存位置：<path>」—— title 万一没了就退回从文本里截
  // ⚠ 上游没有把 task 对象挂到 DOM 上（也没有 data-id 之类），所以只能这样读。
  function readCard(card) {
    var statusEl = card.querySelector(".status-label");
    if (!statusEl || statusEl.textContent.trim() !== COMPLETED) return null;

    var saveEl = card.querySelector(".download-save-path");
    if (!saveEl) return null;

    var full = (saveEl.getAttribute("title") || "").trim();
    if (!full) {
      var text = saveEl.textContent || "";
      var slash = text.indexOf("/");
      if (slash >= 0) full = text.slice(slash).trim();
    }
    if (!full || full.charAt(0) !== "/") return null;

    return { full: full };
  }

  /* ---------- 打开文件管理器 ---------- */

  function locate(full) {
    if (PRIVATE_DIR_RE.test(full)) {
      notify(
        "这个文件在应用私有目录里，文件管理器看不到它。" +
          "到「设置 - 下载策略」把下载目录改成共享文件夹下的目录，之后的任务就能直接定位了。",
        true
      );
      return;
    }

    var cw = window.UGCloudWindow;
    if (!cw || typeof cw.useCapacity !== "function" || !bridgeReady()) {
      notify(
        "当前不在绿联桌面窗口里，打不开文件管理器。请从 NAS 桌面的应用图标进入本应用。",
        true
      );
      return;
    }

    cw.useCapacity(
      "openApp",
      { appId: FILEMGR_APP_ID, data: { filePathOpen: full } },
      3000
    ).catch(function (error) {
      notify("打开文件管理器失败：" + ((error && error.message) || error), true);
    });
  }

  /* ---------- 给卡片补按钮 ---------- */

  function makeButton(full) {
    var button = document.createElement("button");
    button.type = "button";
    button.className = "button button-secondary " + BTN_CLASS;
    button.textContent = "在文件管理器中打开";
    button.title = "在文件管理器中选中该文件：" + full;
    button.addEventListener("click", function () {
      locate(full);
    });
    return button;
  }

  function decorate(card) {
    var task = readCard(card);
    if (!task) return;

    var button = buttons.get(task.full);
    if (!button) {
      button = makeButton(task.full);
      buttons.set(task.full, button);
    }

    // 有「暂停/取消」那一行就并进去；已完成的任务没有那一行，自己起一行。
    var host = card.querySelector(".inline-actions") || card.querySelector("." + ROW_CLASS);
    if (!host) {
      host = document.createElement("div");
      host.className = "inline-actions " + ROW_CLASS;
      card.appendChild(host);
    }
    // 已经在正确位置就不要动它（避免无意义的 DOM 变更）
    if (button.parentNode !== host) host.appendChild(button);
  }

  function scan() {
    var list = document.getElementById("download-list");
    if (!list) return;
    var cards = list.querySelectorAll(".download-card");
    for (var i = 0; i < cards.length; i += 1) decorate(cards[i]);
    // 「清理已结束」把任务删掉后，对应的按钮跟着卡片一起没了，顺手清掉缓存里的
    // 死引用（不清理也只是很小的泄漏，但没必要留着）。
    buttons.forEach(function (button, key) {
      if (!button.isConnected) buttons.delete(key);
    });
  }

  /* ---------- 观察 ---------- */

  // ⚠ 这里【必须同步】调用 scan：MutationObserver 的回调是微任务，在本帧绘制
  //   之前执行，所以按钮和卡片出现在同一帧里。套一层 setTimeout 就会露出
  //   "卡片已经画出来、按钮下一帧才补上"的空档 —— 那就是用户看到的闪。
  function watch(list) {
    new MutationObserver(scan).observe(list, { childList: true, subtree: true });
  }

  function start() {
    injectStyle();
    scan();
    var list = document.getElementById("download-list");
    if (list) {
      watch(list);
      return;
    }
    // 兜底：列表节点还没进 DOM 就先盯着 body，等它出现再挂上去。
    var bodyObserver = new MutationObserver(function () {
      var found = document.getElementById("download-list");
      if (!found) return;
      bodyObserver.disconnect();
      scan();
      watch(found);
    });
    bodyObserver.observe(document.body, { childList: true, subtree: true });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})();
