#!/usr/bin/env python3
"""静态检查单页 HTML 里的 id 引用与标签闭合。

为什么需要它：假 DOM 检查器（checkjs.js）抓不到这一类问题 —— 它的
getElementById 对任何 id 都返回一个假元素，永远不会是 null。而真浏览器里
    document.getElementById('已经删掉的按钮').onclick = ...
会直接抛 TypeError，把 init() 里它【后面】的所有初始化步骤一起带走，
表现出来的却是另一个完全不相干的功能失灵。

用法: python3 checkids.py <html 文件>
"""

import re
import sys

# JS 里取元素的写法：$("x")、document.getElementById("x")，
# 以及把 id 当第一个参数传进去的那几个辅助函数 —— 后者不加进来的话，
# showMsg("msg-netwrok", ...) 这种把 id 拼错的调用查不出来（它才是真会出事的）。
REF_PATTERNS = [
    re.compile(r'\$\(\s*["\']([A-Za-z0-9_-]+)["\']\s*\)'),
    re.compile(r'getElementById\(\s*["\']([A-Za-z0-9_-]+)["\']\s*\)'),
    re.compile(r'(?:showMsg|clearMsg|watchOperation)\(\s*["\']([A-Za-z0-9_-]+)["\']'),
]
ID_PATTERN = re.compile(r'\bid\s*=\s*["\']([A-Za-z0-9_-]+)["\']')
# querySelector("button[data-copy]") 这类属性选择器引用的是属性不是 id，
# 单独收集一下，确认页面上真有这个属性。
ATTR_SELECTOR = re.compile(r'querySelectorAll?\(\s*["\'][^"\']*\[([a-z-]+)\][^"\']*["\']\s*\)')

VOID_TAGS = {"area", "base", "br", "col", "embed", "hr", "img", "input",
             "link", "meta", "param", "source", "track", "wbr", "!doctype"}
TAG_PATTERN = re.compile(r'<\s*(/?)\s*([a-zA-Z][a-zA-Z0-9]*)([^>]*?)(/?)>')


def check_ids(text):
    declared = set(ID_PATTERN.findall(text))
    referenced = set()
    for pat in REF_PATTERNS:
        referenced.update(pat.findall(text))
    missing = sorted(referenced - declared)
    unused = sorted(declared - referenced)
    return declared, referenced, missing, unused


def check_attrs(text):
    problems = []
    for attr in set(ATTR_SELECTOR.findall(text)):
        if not re.search(r'\b%s\s*=' % re.escape(attr), text):
            problems.append("选择器用到属性 [%s]，但页面上没有任何元素带这个属性" % attr)
    return problems


def check_tags(text):
    """标签闭合检查。

    少闭一个 <div> 浏览器会自动纠错、页面照常显示，但结构会悄悄嵌错一层，
    肉眼很难发现。
    """
    # 把 <script> / <style> 的内容挖掉，避免 JS 里的 "<" 比较符号被当成标签
    body = re.sub(r'<script\b[^>]*>.*?</script>', '<script></script>',
                  text, flags=re.S | re.I)
    body = re.sub(r'<style\b[^>]*>.*?</style>', '<style></style>',
                  body, flags=re.S | re.I)
    body = re.sub(r'<!--.*?-->', '', body, flags=re.S)

    stack = []
    problems = []
    for closing, name, _attrs, selfclose in TAG_PATTERN.findall(body):
        lname = name.lower()
        if lname in VOID_TAGS or selfclose:
            continue
        if closing:
            if not stack:
                problems.append("多出一个 </%s>" % lname)
            elif stack[-1] != lname:
                problems.append("</%s> 对不上，当前最近未闭合的是 <%s>" % (lname, stack[-1]))
                if lname in stack:
                    while stack and stack[-1] != lname:
                        stack.pop()
                    stack.pop()
            else:
                stack.pop()
        else:
            stack.append(lname)
    for name in stack:
        problems.append("<%s> 没有闭合" % name)
    return problems


def main(argv):
    if len(argv) != 2:
        print(__doc__)
        return 2
    path = argv[1]
    with open(path, encoding="utf-8") as f:
        text = f.read()

    declared, referenced, missing, unused = check_ids(text)
    problems = []
    for mid in missing:
        problems.append("JS 引用了页面上不存在的 id: %s" % mid)
    problems += check_attrs(text)
    problems += check_tags(text)

    print("[checkids] %s：声明 %d 个 id，JS 引用 %d 个" % (path, len(declared), len(referenced)))
    if unused:
        # 只是提示，不算错误：有些 id 是给 CSS 或 label for= 用的
        print("[checkids] 提示：以下 id 声明了但 JS 没用到 —— %s" % ", ".join(unused))
    if problems:
        for p in problems:
            print("::error:: [checkids] %s" % p, file=sys.stderr)
        return 1
    print("[checkids] 通过")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
