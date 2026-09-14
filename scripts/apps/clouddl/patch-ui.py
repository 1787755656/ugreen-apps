#!/usr/bin/env python3
"""把上游界面里的飞牛 fnOS 字样换成绿联的说法。

只改【文案】，一行逻辑都不动 —— 上游是给飞牛 fnOS 写的，界面上写着
"下载到你的 fnOS""在 fnOS 文件管理器中打开""路径示例 /vol1/1000/Downloads"，
在绿联上原样显示会让用户以为装错了应用，下载目录的示例路径更是直接误导。

每条替换都【断言必须命中】：上游改了文案时当场失败，
而不是悄悄跳过留下一半英文一半中文的界面。

用法：patch-ui.py <文件> [<文件> ...]
      每个文件按扩展名套用对应的替换表。
"""
from __future__ import annotations

import sys
from pathlib import Path

# (旧文本, 新文本, 是否必须命中)
HTML_PATCHES: list[tuple[str, str, bool]] = [
    (
        "并将文件快速的下载到你的 fnOS。",
        "并将文件快速的下载到你的绿联 NAS。",
        True,
    ),
    (
        "任务完成后请在 fnOS 文件管理器中打开此目录。",
        "任务完成后请在绿联 NAS 的文件管理器中打开此目录。",
        True,
    ),
    (
        'placeholder="/vol1/1000/Downloads"',
        'placeholder="/volume1/共享文件夹/downloads"',
        True,
    ),
    (
        "请输入 fnOS 中已授权的绝对路径。",
        "请输入已授权给本应用的绝对路径"
        "（在应用中心里给本应用选择「下载保存目录」时就完成了授权，"
        "换到没授权过的目录会写不进去）。",
        True,
    ),
    # ⚠ 这条是功能性的，不是文案：把上游"直接加载 app.js"换成
    #   "先加载 JSSDK 和认证适配层，由适配层认证就绪后再插入 app.js"。
    #   顺序必须是这个 —— 详见 overlay/ugos-auth.js 开头的说明。
    #   上游没有任何认证，缺了这一步 UGOS 网关不会注入用户身份，
    #   后端每个请求都 401，界面表现是右下角不停弹"未通过登录认证"。
    (
        '<script type="module" src="/static/app.js"></script>',
        '<script src="/static/cloudwindow.js"></script>\n'
        '  <script src="/static/ugos-auth.js"></script>',
        True,
    ),
]

ONBOARDING_PATCHES: list[tuple[str, str, bool]] = [
    (
        "并非百度网盘、夸克网盘或飞牛 fnOS 官方产品。",
        "并非百度网盘、夸克网盘、阿里云盘或绿联 UGOS 官方产品。",
        True,
    ),
]

ALIPAN_PATCHES: list[tuple[str, str, bool]] = [
    (
        "请检查 fnOS 的 DNS 或网关设置",
        "请检查 NAS 的 DNS 或网关设置",
        True,
    ),
]

# 按文件名分表，而不是按扩展名 —— 这样每条都能断言"必须命中"，
# 上游改动任何一句都会当场暴露。
PATCH_TABLES: dict[str, list[tuple[str, str, bool]]] = {
    "index.html": HTML_PATCHES,
    "onboarding.py": ONBOARDING_PATCHES,
    "alipan.py": ALIPAN_PATCHES,
}


def patch(path: Path) -> int:
    table = PATCH_TABLES.get(path.name)
    if table is None:
        sys.exit(f"!! patch-ui.py 没有 {path.name} 的替换表")
    text = path.read_text(encoding="utf-8")
    changed = 0
    for old, new, required in table:
        count = text.count(old)
        if count == 0:
            if required:
                sys.exit(
                    f"!! {path.name} 里找不到要替换的文案：{old!r}\n"
                    f"   上游多半改了这句话，去 patch-ui.py 里对一遍再打包。"
                )
            continue
        text = text.replace(old, new)
        changed += count
    if changed:
        path.write_text(text, encoding="utf-8")
    return changed


def main() -> None:
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    total = 0
    for arg in sys.argv[1:]:
        path = Path(arg)
        if not path.is_file():
            sys.exit(f"!! 文件不存在：{path}")
        total += patch(path)
    print(f"    界面文案替换 {total} 处")


if __name__ == "__main__":
    main()
