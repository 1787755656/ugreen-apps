#!/usr/bin/env python3
"""把上游界面里的飞牛 fnOS 字样和路径示例换成绿联的说法。

只改【文案】和【授权引导的宿主说明】，一行业务逻辑都不动 ——
上游是给飞牛 fnOS 写的，界面上写着 fnOS 的授权步骤和 /vol1/ 路径示例，
在绿联上原样显示会让用户以为装错了应用、照着步骤点也走不通。

每条替换都【断言必须命中】：上游改了文案时当场失败，
而不是悄悄跳过留下一半飞牛一半绿联的界面。

用法：patch-ui.py <文件> [<文件> ...]
"""
from __future__ import annotations

import sys
from pathlib import Path

# (旧文本, 新文本, 是否必须命中)
HTML_PATCHES: list[tuple[str, str, bool]] = [
    # ⚠ 这条是功能性的，不是文案：把上游"直接加载 app.js"换成
    #   "先加载 JSSDK 和认证适配层，由适配层认证就绪后再插入 app.js"。
    #   顺序必须是这个 —— 详见 overlay/ugos-auth.js 开头的说明。
    #   上游没有任何认证，缺了这一步 UGOS 网关不会注入用户身份，
    #   后端每个请求都 401，界面表现是右下角不停弹"未通过登录认证"。
    #   URL 带上 __WWW_VER__（build.sh 替换成完整构建号）破浏览器缓存 ——
    #   网关静态服务没有 cache-control，升级后浏览器会拿旧 JS（真机踩过）。
    #   css 同理。
    (
        '<link rel="stylesheet" href="css/app.css" />',
        '<link rel="stylesheet" href="css/app.css?v=__WWW_VER__" />',
        True,
    ),
    (
        '<script type="module" src="js/fnos-bridge.js"></script>\n'
        '  <script type="module" src="js/app.js"></script>',
        '<script src="js/cloudwindow.js?v=__WWW_VER__"></script>\n'
        '  <script src="js/ugos-auth.js?v=__WWW_VER__"></script>',
        True,
    ),
    # 授权引导对话框：宿主说明与手动授权步骤换成绿联的说法
    (
        "权限由飞牛系统控制：授权后，应用只能访问你选择的目录（可随时在应用设置里撤销）。",
        "权限由绿联系统控制：安装或设置本应用时选择的文件夹才会授权给应用访问。",
        True,
    ),
    (
        '手动授权步骤（共 8 步）：<br />\n'
        '          1. 打开「<b>应用中心</b>」；<br />\n'
        '          2. 点击左侧「<b>已安装</b>」；<br />\n'
        '          3. 点击「<b>100解压</b>」；<br />\n'
        '          4. 点击「<b>应用设置</b>」；<br />\n'
        '          5. 点击「<b>访问权限</b>」标签；<br />\n'
        '          6. 打开「允许普通用户授权应用访问其文件和文件夹」，点击「<b>+ 添加</b>」；<br />\n'
        '          7. 选择要允许的文件夹，权限选「<b>读写</b>」；<br />\n'
        '          8. 回到本页，点击「<b>我已授权，刷新</b>」。',
        '在绿联 NAS 上授权目录的步骤：<br />\n'
        '          1. 打开「<b>应用中心</b>」；<br />\n'
        '          2. 找到「<b>100解压</b>」，点「<b>设置</b>」（齿轮图标）；<br />\n'
        '          3. 在「<b>工作目录</b>」里添加你要处理的文件夹（可多个）；<br />\n'
        '          4. 保存后回到本应用，<b>停止再启动一次</b>（应用中心里操作），再点「我已授权，刷新」。<br />\n'
        '          提示：刚安装完第一次打开时参数还没注入，重启一次即可。',
        True,
    ),
    # 路径示例：飞牛是 /vol1/、/vol2/，绿联是 /volume1/
    (
        'placeholder="或粘贴压缩包路径，例如 /vol2/1000/05_文件下载/movie.7z.001"',
        'placeholder="或粘贴压缩包路径，例如 /volume1/ downloads/movie.7z.001"',
        True,
    ),
    (
        'placeholder="输出目录，例如 /vol1/1000/media"',
        'placeholder="输出目录，例如 /volume1/media"',
        True,
    ),
    (
        'placeholder="文件夹路径，例如 /vol1/1000/media（可展开选择内部内容）"',
        'placeholder="文件夹路径，例如 /volume1/media（可展开选择内部内容）"',
        True,
    ),
]

APP_JS_PATCHES: list[tuple[str, str, bool]] = [
    # 选择器不可用时的降级提示：上游会引导手输路径，话术里的宿主名换掉
    (
        'toast("飞牛选择器未就绪，请稍候重试或手动输入路径", true);',
        'toast("系统选择器不可用，请直接粘贴路径（例如 /volume1/media）", true);',
        True,
    ),
    # ⚠ 这条是功能性的：safePick 用 15 秒超时保护飞牛系统选择器；
    #   UGOS 版的自绘选择器是用户交互式对话框（浏览目录、勾文件），
    #   15 秒会把还开着的选择器判超时、承诺丢失（表现：选着选着突然报
    #   "选择器响应超时"，选择结果作废）。放宽到 10 分钟。
    (
        'timer = setTimeout(() => reject(new Error("选择器响应超时，请重试或手动输入路径")), 15000);',
        'timer = setTimeout(() => reject(new Error("选择器超时，请重试或直接粘贴路径")), 600000);',
        True,
    ),
    (
        '"授权 SDK 不可用（未检测到宿主授权能力）。请点「如何手动授权？」按步骤操作。";',
        '"系统选择器不可用。请点「如何手动授权？」按步骤操作（在应用中心的本应用设置里添加文件夹）。";',
        True,
    ),
    (
        '"已打开应用设置：请在「访问权限」中添加目录，完成后回到本页点「我已授权，刷新」。";',
        '"已打开应用设置：请在「工作目录」里添加文件夹，保存后重启应用再点「我已授权，刷新」。";',
        True,
    ),
]

PROBE_HTML_PATCHES: list[tuple[str, str, bool]] = [
    (
        "本页用于确认飞牛宿主是否注入了文件授权 SDK。",
        "本页用于确认宿主是否注入了文件授权 SDK（绿联版宿主不注入飞牛 SDK，属正常现象）。",
        True,
    ),
]


def patch(path: Path) -> int:
    table_key = path.name
    if table_key == "index.html":
        table = HTML_PATCHES
    elif table_key == "app.js":
        table = APP_JS_PATCHES
    elif table_key == "probe.html":
        table = PROBE_HTML_PATCHES
    else:
        sys.exit(f"!! patch-ui.py 没有 {table_key} 的替换表")
    text = path.read_text(encoding="utf-8")
    changed = 0
    for old, new, required in table:
        count = text.count(old)
        if count == 0:
            if required:
                sys.exit(
                    f"!! {table_key} 里找不到要替换的文案：{old[:60]!r}...\n"
                    f"   上游多半改了这句话，去 patch-ui.py 里对一遍再打包。"
                )
            continue
        text = text.replace(old, new)
        changed += count
    if changed:
        path.write_text(text, encoding="utf-8")
    return changed


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit("用法: patch-ui.py <文件> [<文件> ...]")
    total = 0
    for arg in sys.argv[1:]:
        total += patch(Path(arg))
    print(f"patch-ui: 共替换 {total} 处")
