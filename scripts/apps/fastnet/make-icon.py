#!/usr/bin/env python3
"""生成 FastNet 的 256×256 UGOS Pro 应用图标（不依赖任何外部素材）。

FastNet 是闭源软件，官方没有放出可用的 logo 文件，webui 里也没有 favicon
（实测 index.html 只引了一个 js 一个 css），所以图标是自己画的：深色圆角卡片
上一个测速仪表盘 —— 刻度弧从冷到暖，指针指到高速区，右下角一道速度线。

配色取自 FastNet webui 自己的 CSS（按出现次数取的主色）：
  #2563eb 主蓝 / #0ea5e9 天蓝 / #22c55e 绿 / #f97316 橙 / #0f172a 深底

画法照抄本仓库 openlist/picoclaw 的套路：4 倍超采样 + LANCZOS 降采样做抗锯齿，
只用 Pillow，不碰 SVG 栅格化（qlmanage 会忽略指定宽高，见 skill）。

用法：python3 make-icon.py [输出路径]
      默认写到 apps/fastnet/com.koolcenter.fastnet/rootfs_common/icon.png
"""
import math
import os
import sys

from PIL import Image, ImageDraw

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_DST = os.path.join(
    HERE, "..", "..", "..", "apps", "fastnet",
    "com.koolcenter.fastnet", "rootfs_common", "icon.png",
)
DST = sys.argv[1] if len(sys.argv) > 1 else os.path.normpath(DEFAULT_DST)

SIZE = 256
SS = 4                      # 超采样倍数
RADIUS_RATIO = 0.22         # 圆角半径 / 边长
TOP = (0x0F, 0x17, 0x2A)    # slate-900
BOTTOM = (0x1E, 0x3A, 0x8A) # blue-900

# 仪表盘刻度弧：从左下(210°)扫到右下(330°)，共 240°，分段上色
ARC_START, ARC_END = 150, 390          # PIL 的角度：0° 在 3 点钟方向，顺时针
ARC_SEGMENTS = [
    (0.00, 0.45, (0x38, 0xBD, 0xF8)),  # sky-400   慢~中
    (0.45, 0.75, (0x22, 0xC5, 0x5E)),  # green-500 快
    (0.75, 1.00, (0xF9, 0x73, 0x16)),  # orange-500 很快
]
NEEDLE_T = 0.82                        # 指针位置（落在橙色段，示意"很快"）

n = SIZE * SS
cx = cy = n / 2


def lerp_angle(t):
    return ARC_START + (ARC_END - ARC_START) * t


# ---- 底：对角渐变 + 圆角 ----------------------------------------------------
card = Image.new("RGBA", (n, n))
px = card.load()
for y in range(n):
    t = y / (n - 1)
    row = (
        round(TOP[0] + (BOTTOM[0] - TOP[0]) * t),
        round(TOP[1] + (BOTTOM[1] - TOP[1]) * t),
        round(TOP[2] + (BOTTOM[2] - TOP[2]) * t),
        255,
    )
    for x in range(n):
        px[x, y] = row

d = ImageDraw.Draw(card)

# ---- 刻度弧 ----------------------------------------------------------------
r = n * 0.34
w = n * 0.085
box = (cx - r, cy - r, cx + r, cy + r)

# 底槽（暗一档的整条弧，让未"点亮"的部分也有形）
d.arc(box, ARC_START, ARC_END, fill=(0x1E, 0x29, 0x3B, 255), width=round(w))
for t0, t1, color in ARC_SEGMENTS:
    d.arc(box, lerp_angle(t0), lerp_angle(t1), fill=color + (255,), width=round(w))

# ---- 刻度点：沿弧内侧均匀排 9 个小圆 ----------------------------------------
tick_r = n * 0.245
for i in range(9):
    a = math.radians(lerp_angle(i / 8))
    tx, ty = cx + tick_r * math.cos(a), cy + tick_r * math.sin(a)
    s = n * 0.011
    d.ellipse((tx - s, ty - s, tx + s, ty + s), fill=(0xCB, 0xD5, 0xE1, 190))

# ---- 指针 ------------------------------------------------------------------
a = math.radians(lerp_angle(NEEDLE_T))
tip = (cx + n * 0.285 * math.cos(a), cy + n * 0.285 * math.sin(a))
# 指针做成三角形：尖端 + 转轴两侧
base_w = n * 0.035
left = (cx + base_w * math.cos(a + math.pi / 2), cy + base_w * math.sin(a + math.pi / 2))
right = (cx + base_w * math.cos(a - math.pi / 2), cy + base_w * math.sin(a - math.pi / 2))
tail = (cx - n * 0.055 * math.cos(a), cy - n * 0.055 * math.sin(a))
d.polygon([tip, left, tail, right], fill=(0xFF, 0xFF, 0xFF, 255))

# 转轴
hub = n * 0.045
d.ellipse((cx - hub, cy - hub, cx + hub, cy + hub), fill=(0xFF, 0xFF, 0xFF, 255))
hub2 = n * 0.022
d.ellipse((cx - hub2, cy - hub2, cx + hub2, cy + hub2), fill=(0x25, 0x63, 0xEB, 255))

# ---- 速度线：仪表下方三道，右端渐短，暗示"疾驰" ------------------------------
line_y = cy + n * 0.335
for i, (frac, alpha) in enumerate(((0.30, 255), (0.22, 190), (0.14, 130))):
    y = line_y + i * n * 0.052
    half = n * frac / 2
    lw = n * 0.026
    d.rounded_rectangle(
        (cx - half, y - lw / 2, cx + half, y + lw / 2),
        radius=lw / 2,
        fill=(0x0E, 0xA5, 0xE9, alpha),
    )

# ---- 圆角遮罩 + 降采样 ------------------------------------------------------
mask = Image.new("L", (n, n), 0)
ImageDraw.Draw(mask).rounded_rectangle(
    (0, 0, n - 1, n - 1), radius=int(n * RADIUS_RATIO), fill=255
)
card.putalpha(mask)

icon = card.resize((SIZE, SIZE), Image.LANCZOS)
icon.save(DST, "PNG", optimize=True)

# ---- 自检（照 skill 里那几条：四角必须透明、边中点必须实心、体积达标）--------
a_ch = icon.split()[3]
corners = [a_ch.getpixel(p) for p in ((0, 0), (SIZE - 1, 0), (0, SIZE - 1), (SIZE - 1, SIZE - 1))]
assert max(corners) == 0, f"四角应为全透明，实际 alpha={corners}"
edges = [a_ch.getpixel(p) for p in ((SIZE // 2, 0), (0, SIZE // 2), (SIZE // 2, SIZE - 1), (SIZE - 1, SIZE // 2))]
assert min(edges) == 255, f"四边中点应为不透明（图标要铺满画布），实际 alpha={edges}"
size_kb = os.path.getsize(DST) / 1024
assert size_kb < 100, f"图标 {size_kb:.1f}KB 超过绿联 100KB 上限"
print(f"OK  {DST}  {SIZE}x{SIZE}  {size_kb:.1f}KB")
