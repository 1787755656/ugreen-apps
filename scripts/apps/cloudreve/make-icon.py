#!/usr/bin/env python3
"""把 Cloudreve 官方 GitHub 组织头像合成成 256×256 的 UGOS Pro 应用图标。

素材：https://avatars.githubusercontent.com/u/48898462?v=4 （Cloudreve 官方
GitHub 组织的头像，也就是官网/客户端用的那枚「蓝色球体 + 白色云」标志）。

官方标志本身带白色底和浅蓝色圆环，直接放到白底卡片上会糊在一起，
所以卡片用极浅的蓝灰（#F2F7FE）+ 8% 黑细描边，让圆环有边界。

用法：
    python3 make-icon.py [源图] [输出 icon.png]
默认从 GitHub 下载（构建时不跑，图标已提交进仓库）。
"""
import io
import sys
import urllib.request

from PIL import Image, ImageDraw

SRC_URL = "https://avatars.githubusercontent.com/u/48898462?v=4"
SRC = sys.argv[1] if len(sys.argv) > 1 else SRC_URL
DST = sys.argv[2] if len(sys.argv) > 2 else "icon.png"

SIZE = 256
SS = 4                 # 超采样倍数，用来做抗锯齿
RADIUS_RATIO = 0.22    # 圆角半径 / 边长
GLYPH_RATIO = 0.84     # 标志占画布的比例（标志自带留白，所以给得大）
BG = (0xF2, 0xF7, 0xFE)

if SRC.startswith("http"):
    with urllib.request.urlopen(SRC, timeout=30) as r:
        logo = Image.open(io.BytesIO(r.read())).convert("RGBA")
else:
    logo = Image.open(SRC).convert("RGBA")

n = SIZE * SS

# 圆角卡片（先画 4 倍图再降采样，边缘才干净）
card = Image.new("RGBA", (n, n), BG + (255,))
mask = Image.new("L", (n, n), 0)
ImageDraw.Draw(mask).rounded_rectangle(
    (0, 0, n - 1, n - 1), radius=int(n * RADIUS_RATIO), fill=255
)
card.putalpha(mask)

# 8% 黑、0.5px 描边（按 4 倍图算就是 2px）
draw = ImageDraw.Draw(card)
draw.rounded_rectangle(
    (1, 1, n - 2, n - 2),
    radius=int(n * RADIUS_RATIO),
    outline=(0, 0, 0, 20),
    width=SS // 2 or 1,
)

# 标志按内容 alpha 包围盒裁掉留白 —— 头像四周留白并不对称
glyph = logo.crop(logo.split()[3].getbbox())
target = int(n * GLYPH_RATIO)
glyph = glyph.resize((target, target), Image.LANCZOS)
card.alpha_composite(glyph, ((n - target) // 2, (n - target) // 2))

icon = card.resize((SIZE, SIZE), Image.LANCZOS)
icon.save(DST, "PNG", optimize=True)

# 自检：四角透明、中心是标志色、文件体积合规
px = icon.load()
corners = [px[0, 0][3], px[SIZE - 1, 0][3], px[0, SIZE - 1][3], px[SIZE - 1, SIZE - 1][3]]
assert corners == [0, 0, 0, 0], f"四角不透明: {corners}"
assert px[SIZE // 2, SIZE // 2][2] > 120, "中心不像品牌蓝"
import os
size = os.path.getsize(DST)
assert size < 100 * 1024, f"图标 {size} 字节，超过 100KB"
print(f"icon: {DST} {icon.size} {size} bytes")
