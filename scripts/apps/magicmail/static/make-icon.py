#!/usr/bin/env python3
"""生成 UGOS Pro 应用图标 rootfs_common/icon.png。

素材是上游仓库里的 public/images/icon_512.png —— 一个透明底的黑色信封 logo，
本来就是给白底用的。绿联的图标规范是「正方形 + 平滑圆角 + 浅色背景 + 8% 黑描边」，
所以这里把 logo 按内容 alpha 包围盒（不是画布中心，素材四周留白不对称）
居中合成到一张满幅圆角白卡上。

4 倍超采样后降采样做抗锯齿；末尾的自检比肉眼看缩略图靠谱得多。
"""
import sys
from pathlib import Path

from PIL import Image, ImageDraw

# 一次性工具：图标是【提交进仓库】的（apps/.../rootfs_common/icon.png），
# CI 不跑这个脚本。上游换 logo 时手动重跑一次即可：
#   python3 scripts/apps/magicmail/static/make-icon.py [素材.png] [输出.png]
REPO_ROOT = Path(__file__).resolve().parents[4]
SRC = Path(sys.argv[1]) if len(sys.argv) > 1 else REPO_ROOT / ".cache/magicmail/public/images/icon_512.png"
OUT = Path(sys.argv[2]) if len(sys.argv) > 2 else REPO_ROOT / "apps/magicmail/com.magiccode.magicmail/rootfs_common/icon.png"

SIZE = 256
SS = 4                      # 超采样倍数
CANVAS = SIZE * SS
RADIUS = int(CANVAS * 0.22)  # 圆角半径（占边长的比例，和绿联自带应用观感接近）
CARD = (255, 255, 255, 255)  # 浅色背景 —— logo 是黑色的，必须给白底
STROKE = (0, 0, 0, 20)       # 8% 黑描边
STROKE_W = max(1, int(CANVAS * 0.002))
LOGO_FRAC = 0.76             # logo 内容占卡片边长的比例


def main() -> int:
    if not SRC.exists():
        print(f"素材缺失：{SRC}\n从上游仓库取 public/images/icon_512.png，或把路径作为第一个参数传进来。", file=sys.stderr)
        return 1

    # --- 圆角白卡 ---------------------------------------------------------
    card = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
    d = ImageDraw.Draw(card)
    d.rounded_rectangle([0, 0, CANVAS - 1, CANVAS - 1], radius=RADIUS, fill=CARD)
    d.rounded_rectangle(
        [STROKE_W / 2, STROKE_W / 2, CANVAS - 1 - STROKE_W / 2, CANVAS - 1 - STROKE_W / 2],
        radius=RADIUS, outline=STROKE, width=STROKE_W,
    )

    # --- logo：按内容包围盒裁剪再等比缩放居中 -----------------------------
    logo = Image.open(SRC).convert("RGBA")
    bbox = logo.split()[3].getbbox()
    if bbox is None:
        print("素材是全透明的？", file=sys.stderr)
        return 1
    logo = logo.crop(bbox)

    target = int(CANVAS * LOGO_FRAC)
    scale = min(target / logo.width, target / logo.height)
    logo = logo.resize(
        (max(1, round(logo.width * scale)), max(1, round(logo.height * scale))),
        Image.LANCZOS,
    )
    card.alpha_composite(logo, ((CANVAS - logo.width) // 2, (CANVAS - logo.height) // 2))

    icon = card.resize((SIZE, SIZE), Image.LANCZOS)
    OUT.parent.mkdir(parents=True, exist_ok=True)
    icon.save(OUT, "PNG", optimize=True)

    # --- 自检 -------------------------------------------------------------
    # 光看缩略图看不出"四角没透明"这类问题（桌面上会顶着一个白方块）。
    px = icon.load()
    e = 3
    for name, xy in [("左上", (e, e)), ("右上", (SIZE - 1 - e, e)),
                     ("左下", (e, SIZE - 1 - e)), ("右下", (SIZE - 1 - e, SIZE - 1 - e))]:
        if px[xy][3] > 8:
            print(f"自检失败：{name}角不透明 alpha={px[xy][3]}", file=sys.stderr)
            return 1
    for name, xy in [("上", (SIZE // 2, 1)), ("下", (SIZE // 2, SIZE - 2)),
                     ("左", (1, SIZE // 2)), ("右", (SIZE - 2, SIZE // 2))]:
        if px[xy][3] < 250:
            print(f"自检失败：{name}边中点不是实心 alpha={px[xy][3]}", file=sys.stderr)
            return 1

    kb = OUT.stat().st_size / 1024
    if kb >= 100:
        print(f"自检失败：{kb:.1f}KB 超过 100KB 上限", file=sys.stderr)
        return 1

    print(f"✓ {OUT}  {SIZE}×{SIZE}  {kb:.1f}KB")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
