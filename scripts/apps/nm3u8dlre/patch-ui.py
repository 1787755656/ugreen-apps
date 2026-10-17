#!/usr/bin/env python3
"""
把上游 fnOS 版前端（m3u8_down/app/www）改造成绿联 UGOS 版。

上游前端的设计目标是"跑在 fnOS 的 index.cgi 相对路径下"，绿联这边是
「网关 serve www/ 静态页 + /api 反代到后端」，所以只需要两处改动：

  1. API 前缀：'?action=...' → '/api?action=...'
     （网关只反代 project.yaml 里 proxy_path 声明的 /api，根路径永远给静态页，
       写成相对路径的 '?action=' 会被 nginx 当成首页，永远拿不到 JSON）
  2. fnOS 专有文案/路径 → 绿联（含打开文件失败提示、目录选择器提示）
  3. 补 <script src="cloudwindow.js">：openFile/openFileManager 靠 UGCloudWindow
  4. 资源指纹 ?v= 统一改成本文件里的 UI_VERSION（网关对静态资源带长缓存）

fnOS 的宿主 SDK（vendor/trim-app.js）不在本脚本里改，而是整份替换成
overlay/www/ugos-bridge.js —— 它导出同名 TrimApp 门面，app.js 一行都不用动。

用法：python3 patch-ui.py <www 目录>
"""
import pathlib
import re
import sys

UI_VERSION = "0.6.0.2"   # 前端资源指纹版本（改 js/css 必须加）

def die(msg):
    print(f"patch-ui: {msg}", file=sys.stderr)
    sys.exit(1)

def sub_once(text, pattern, repl, what):
    """断言式替换：必须恰好命中一次，否则说明上游改版了，宁可失败也不静默放过。"""
    new, n = re.subn(pattern, repl, text, count=1, flags=re.M)
    if n != 1:
        die(f"未命中：{what}（pattern={pattern!r}, 命中 {n} 次）")
    return new

def main():
    if len(sys.argv) != 2:
        die("用法: patch-ui.py <www 目录>")
    www = pathlib.Path(sys.argv[1])
    app_js = www / "app.js"
    index = www / "index.html"
    for f in (app_js, index):
        if not f.is_file():
            die(f"缺少文件：{f}")

    # ---- app.js ----
    js = app_js.read_text(encoding="utf-8")
    # ⚠ 必须是 /api/ 带尾斜杠：网关的 server.d 里是 `location ~ ^/api/`，
    #   /api?action=xxx（无斜杠）不匹配，会被当成静态路径 404。
    js = sub_once(
        js,
        r"^const API = \(params\) => '\?' \+ new URLSearchParams\(params\)\.toString\(\);$",
        "const API = (params) => '/api/?' + new URLSearchParams(params).toString();",
        "app.js 的 API 前缀",
    )
    js = sub_once(
        js,
        r"^// 所有 API 通过相对 URL '\?action=\.\.\.' 调用当前 CGI（页面位于 \.\.\./index\.cgi/ 下）$",
        "// 所有 API 走 /api/（UGOS 网关反代到后端端口；静态页由网关从 www/ 直接提供）",
        "app.js 的注释",
    )
    # fnOS 的提示语在绿联这边不成立，逐条换成绿联的说法
    for a, b in [
        ("当前为独立浏览器环境或 fnOS 宿主桥接不可用，无法直接打开",
         "当前不在绿联桌面窗口中（或宿主桥未就绪），无法直接调起文件管理器"),
        ("'已通过 fnOS 打开'", "'已在文件管理器中打开'"),
        ("'fnOS 打开失败：'", "'打开失败：'"),
        ("无法打开文件管理器（需 fnOS 1.2.0401+ 并从桌面/应用中心打开本应用），请手动输入路径",
         "无法打开文件管理器（请从绿联桌面或应用中心打开本应用），请手动输入路径"),
        ("// 未手动选择过 → 跟随 fnOS 主题（复用 SDK 单例），默认暗黑",
         "// 未手动选择过 → 跟随宿主主题（复用 SDK 单例），默认暗黑"),
    ]:
        if a not in js:
            die(f"未命中（app.js 文案）：{a}")
        js = js.replace(a, b)
    # 剩下的 fnOS 字样全在注释与 console 标签里（用户可见的提示上面已逐条换过），
    # 统一抹掉，避免以后看代码的人以为这还是 fnOS 版
    js = js.replace("fnOS", "绿联")
    app_js.write_text(js, encoding="utf-8")

    # ---- index.html ----
    html = index.read_text(encoding="utf-8")
    html = sub_once(
        html,
        r"· DASH / HLS / MSS 流媒体下载 · fnOS",
        "· DASH / HLS / MSS 流媒体下载 · 绿联 UGOS",
        "副标题里的 fnOS 字样",
    )
    html = sub_once(
        html,
        r'placeholder="/vol1/Users/admin/Downloads"',
        'placeholder="/volume1/<共享文件夹>/Downloads"',
        "自定义目录的示例路径（fnOS 用 /vol1，绿联用 /volume1）",
    )
    html = sub_once(
        html,
        r'<option value="default">默认共享目录（文件管理器中可见）</option>',
        '<option value="default">默认下载目录（应用设置里选择的文件夹）</option>',
        "默认目录选项文案",
    )
    html = sub_once(
        html,
        r"(<span id=\"foot-version\"></span>)",
        r"\1 · 绿联移植：穿斯帕沃特",
        "页脚加移植署名",
    )
    # 宿主桥 SDK 必须排在 app.js 之前：openFile/openFileManager 走 UGCloudWindow
    if 'cloudwindow.js' not in html:
        html = sub_once(
            html,
            r'^(\s*)<script src="app\.js\?v=[^"]*"></script>$',
            r'\1<!-- 绿联宿主桥 SDK：openFile/openFileManager 走 UGCloudWindow.useCapacity -->\n'
            r'\1<script src="cloudwindow.js?v=UI_VERSION"></script>\n\1<script src="app.js?v=UI_VERSION"></script>',
            "index.html 引入 cloudwindow.js",
        )
    # 资源指纹：网关对静态资源带长缓存（Cache-Control: $cache_control），
    # 改了 js/css 不换指纹，用户会"看不到更新"。改前端时把 UI_VERSION 一起加。
    html = html.replace('?v=UI_VERSION', '?v=' + UI_VERSION)
    html = re.sub(r'\?v=[0-9A-Za-z.]+', '?v=' + UI_VERSION, html)
    index.write_text(html, encoding="utf-8")
    print("patch-ui: ok")

if __name__ == "__main__":
    main()
