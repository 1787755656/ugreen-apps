#!/usr/bin/env python3
"""把 OpenList 官方 logo 合成成 256×256 的 UGOS Pro 应用图标。

素材：https://github.com/OpenListTeam/Logo 的 logo/512x512.png
      （CC BY-NC-SA 4.0，见该仓库 LICENSE —— 非商业+署名+相同方式共享，
        本仓库是免费的社区打包，符合；若日后要正式上架应用中心需重新确认）

官方 logo 是薄荷绿的环 + 天蓝的斜杠，颜色很浅，放在浅色底上会糊成一团，
所以底色用深石板蓝渐变（OpenList 官网也是深色调）。

两个细节（都踩过，见 ugos-pro-app-dev skill）：
  - 按【内容 alpha 包围盒】居中，不是按画布中心 —— 这张图的留白左右不对称
    （bbox=(71,58,454,454)），照画布中心贴会明显偏。
  - 别用 qlmanage 栅格化 SVG，它会忽略指定的宽高。

用法：python3 make-icon.py [源图 512x512.png] [输出 icon.png]
"""
import sys

from PIL import Image, ImageDraw

SRC = sys.argv[1] if len(sys.argv) > 1 else "512x512.png"
DST = sys.argv[2] if len(sys.argv) > 2 else "icon.png"

SIZE = 256
SS = 4  # 超采样倍数，用来做抗锯齿
RADIUS_RATIO = 0.22  # 圆角半径 / 边长
GLYPH_RATIO = 0.62  # 标记（按内容包围盒）占画布的比例
TOP = (0x0F, 0x17, 0x2A)  # slate-900
BOTTOM = (0x1E, 0x29, 0x3B)  # slate-800

logo = Image.open(SRC).convert("RGBA")
glyph = logo.crop(logo.split()[3].getbbox())  # 按内容裁掉不对称留白

n = SIZE * SS

# 对角渐变底色
card = Image.new("RGBA", (n, n))
px = card.load()
for y in range(n):
    for x in range(n):
        t = (x + y) / (2 * (n - 1))
        px[x, y] = (
            round(TOP[0] + (BOTTOM[0] - TOP[0]) * t),
            round(TOP[1] + (BOTTOM[1] - TOP[1]) * t),
            round(TOP[2] + (BOTTOM[2] - TOP[2]) * t),
            255,
        )

mask = Image.new("L", (n, n), 0)
ImageDraw.Draw(mask).rounded_rectangle(
    (0, 0, n - 1, n - 1), radius=int(n * RADIUS_RATIO), fill=255
)
card.putalpha(mask)

gw, gh = glyph.size
scale = (n * GLYPH_RATIO) / max(gw, gh)
glyph = glyph.resize((max(1, round(gw * scale)), max(1, round(gh * scale))), Image.LANCZOS)
card.alpha_composite(glyph, ((n - glyph.width) // 2, (n - glyph.height) // 2))

card.resize((SIZE, SIZE), Image.LANCZOS).save(DST, optimize=True)

out = Image.open(DST)
print(DST, out.size, out.mode)
