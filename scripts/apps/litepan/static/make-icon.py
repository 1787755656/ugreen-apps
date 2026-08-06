#!/usr/bin/env python3
"""从 LitePan 官方 banner 里抠出 L 标记，合成 256×256 应用图标。

banner 里的标记是"蓝底白字"，所以用亮度当 alpha 抠出白色笔画，
再贴到自己画的圆角蓝卡上（不直接裁 banner，避免带上卡片边缘和噪点）。
"""
import sys
from PIL import Image, ImageDraw

SRC = sys.argv[1] if len(sys.argv) > 1 else "LitePan/docs/pictures/banner.png"
DST = sys.argv[2] if len(sys.argv) > 2 else "icon.png"

# 白色笔画在 banner 中的包围盒（按列扫描量出来的，见 README）
GLYPH_BOX = (87, 85, 158, 159)

SIZE = 256
SS = 4                      # 超采样倍数，用来做抗锯齿
RADIUS_RATIO = 0.22         # 圆角半径 / 边长
GLYPH_RATIO = 0.56          # 标记占画布的比例
TOP = (0x3B, 0x82, 0xF6)    # 品牌蓝（banner 左上角取色）
BOTTOM = (0x60, 0xA5, 0xFA) # 品牌蓝（banner 右下角取色）

banner = Image.open(SRC).convert("RGB")
glyph_src = banner.crop(GLYPH_BOX)

# 亮度即 alpha：蓝底 → 透明，白笔画 → 不透明
lum = glyph_src.convert("L")
lo, hi = 158, 240
alpha = lum.point(lambda v: 0 if v <= lo else (255 if v >= hi else int((v - lo) * 255 / (hi - lo))))
glyph = Image.new("RGBA", glyph_src.size, (255, 255, 255, 0))
glyph.putalpha(alpha)

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
ImageDraw.Draw(mask).rounded_rectangle((0, 0, n - 1, n - 1), radius=int(n * RADIUS_RATIO), fill=255)
card.putalpha(mask)

# 标记按内容包围盒等比缩放并居中
gw, gh = glyph.size
scale = (n * GLYPH_RATIO) / max(gw, gh)
glyph = glyph.resize((max(1, round(gw * scale)), max(1, round(gh * scale))), Image.LANCZOS)
card.alpha_composite(glyph, ((n - glyph.width) // 2, (n - glyph.height) // 2))

card.resize((SIZE, SIZE), Image.LANCZOS).save(DST, optimize=True)
print(DST, Image.open(DST).size)
