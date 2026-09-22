#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""前端静态自查（纯手写 HTML 的老三样）：

1. 抽出 <script> 内容做 JS 语法检查（node --check）
2. HTML 里的 id 与 JS 里 getElementById('...') 的差集 —— 引用了不存在的 id 是真机会炸、
   而假 DOM 检查器永远发现不了的一类 bug
3. div/section/main 等容器的开闭标签配平 —— 少闭合一个浏览器会静默纠错、结构悄悄嵌错

以非 0 退出表示发现问题，方便挂进构建流程。
"""
import os
import re
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
HTML = os.path.normpath(os.path.join(HERE, "..", "src", "web", "index.html"))

NODE_CANDIDATES = [
    os.path.expanduser("~/.workbuddy/binaries/node/versions/22.22.2-3/bin/node"),
    "/opt/homebrew/bin/node",
    "/usr/local/bin/node",
]


def find_node():
    for p in NODE_CANDIDATES:
        if os.path.exists(p):
            return p
    return None


def main():
    html = open(HTML, encoding="utf-8").read()
    problems = []

    # ---- 1. JS 语法
    scripts = re.findall(r"<script(?![^>]*\bsrc=)[^>]*>(.*?)</script>", html, re.S)
    inline = "\n;\n".join(scripts)
    node = find_node()
    if not node:
        print("· 跳过 JS 语法检查（找不到 node）")
    else:
        with tempfile.NamedTemporaryFile("w", suffix=".js", delete=False) as f:
            f.write(inline)
            tmp = f.name
        r = subprocess.run([node, "--check", tmp], capture_output=True, text=True)
        os.unlink(tmp)
        if r.returncode != 0:
            problems.append("JS 语法错误:\n" + r.stderr.strip())
        else:
            print("· JS 语法 OK")

    # ---- 2. id 引用
    declared = set(re.findall(r'id\s*=\s*["\']([A-Za-z0-9_\-]+)["\']', html))
    used = set(re.findall(r"getElementById\(\s*['\"]([A-Za-z0-9_\-]+)['\"]\s*\)", html))
    missing = sorted(used - declared)
    if missing:
        problems.append("JS 引用了 HTML 里不存在的 id: " + ", ".join(missing))
    else:
        print("· id 引用一致（声明 %d 个，引用 %d 个）" % (len(declared), len(used)))

    # ---- 3. 标签配平（只查结构性容器，避免被 void 标签干扰）
    for tag in ("div", "main", "aside", "header", "footer", "section", "style", "script", "html", "body"):
        opened = len(re.findall(r"<%s[\s>]" % tag, html))
        closed = len(re.findall(r"</%s>" % tag, html))
        if opened != closed:
            problems.append("<%s> 开闭不配平: 开 %d / 闭 %d" % (tag, opened, closed))
    if not any("配平" in p for p in problems):
        print("· 容器标签配平 OK")

    if problems:
        print("\n发现 %d 个问题：" % len(problems))
        for p in problems:
            print("  ✗ " + p)
        return 1
    print("\n前端自查通过")
    return 0


if __name__ == "__main__":
    sys.exit(main())
