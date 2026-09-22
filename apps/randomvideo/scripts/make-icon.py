#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生成应用图标：256x256 PNG，纯标准库 + 4x 超采样抗锯齿。

画面构成：蓝紫渐变圆角方块 + 白色播放三角 + 两颗"随机"星点，
右下角一个小箭头示意"刷下一个"。
"""
import math
import struct
import zlib
import os

W = H = 256
SS = 4  # 超采样倍数


def lerp(a, b, t):
    return a + (b - a) * t


def mix(c1, c2, t):
    return tuple(lerp(c1[i], c2[i], t) for i in range(3))


C1 = (0x1B, 0x4C, 0xE0)   # 蓝
C2 = (0x7C, 0x3A, 0xED)   # 紫


def rounded_rect_alpha(x, y, w, h, r):
    """返回该点在矩形内的覆盖率（0~1），边缘做 1px 软过渡。"""
    cx = min(max(x, r), w - r)
    cy = min(max(y, r), h - r)
    dx = x - cx
    dy = y - cy
    d = math.hypot(dx, dy)
    return 1.0 if d <= r else max(0.0, min(1.0, r + 0.75 - d))


def triangle_cover(x, y, pts):
    """点在三角形内的覆盖率（用带软边的符号距离判定）。"""
    def sign(p1, p2, p3):
        return (p1[0] - p3[0]) * (p2[1] - p3[1]) - (p2[0] - p3[0]) * (p1[1] - p3[1])
    d1 = sign((x, y), pts[0], pts[1])
    d2 = sign((x, y), pts[1], pts[2])
    d3 = sign((x, y), pts[2], pts[0])
    neg = (d1 < 0) or (d2 < 0) or (d3 < 0)
    pos = (d1 > 0) or (d2 > 0) or (d3 > 0)
    inside = not (neg and pos)
    if not inside:
        return 0.0
    # 圆角：取三条边的最小距离，做 4px 软角
    edges = [(pts[0], pts[1]), (pts[1], pts[2]), (pts[2], pts[0])]
    mind = 1e9
    for (ax, ay), (bx, by) in edges:
        vx, vy = bx - ax, by - ay
        wx, wy = x - ax, y - ay
        t = max(0.0, min(1.0, (vx * wx + vy * wy) / (vx * vx + vy * vy) if (vx * vx + vy * vy) else 0.0))
        px, py = ax + t * vx, ay + t * vy
        mind = min(mind, math.hypot(x - px, y - py))
    return max(0.0, min(1.0, (mind + 0.5) / 3.0)) if mind < 3.0 else 1.0


def disc_cover(x, y, cx, cy, r):
    d = math.hypot(x - cx, y - cy)
    return 1.0 if d <= r - 0.6 else max(0.0, min(1.0, (r + 0.6 - d) / 1.2))


# 播放三角稍微往左偏，给右边的"下一个"箭头留位置
TRI = [(96.0, 74.0), (186.0, 128.0), (96.0, 182.0)]

STARTS = [(206.0, 74.0, 7.0), (218.0, 96.0, 4.5), (196.0, 100.0, 3.0), (216.0, 62.0, 3.0)]

ARROW = [(150.0, 196.0), (188.0, 196.0), (188.0, 182.0), (206.0, 203.0),
         (188.0, 224.0), (188.0, 210.0), (150.0, 210.0)]


def render():
    rows = []
    total = W * SS
    for py in range(H * SS):
        row = bytearray()
        y = (py + 0.5) / SS
        for px in range(W * SS):
            x = (px + 0.5) / SS
            r = g = b = 0
            a = 0.0

            # 背景
            cov = rounded_rect_alpha(x, y, W, H, 58)
            if cov > 0:
                t = (x + y) / (W + H)
                cr, cg, cb = mix(C1, C2, t)
                r, g, b = cr, cg, cb
                a = cov

            # 白色前景图形覆盖上去
            fg = 0.0
            fg = max(fg, triangle_cover(x, y, TRI))
            for cx, cy, rd in STARTS:
                fg = max(fg, disc_cover(x, y, cx, cy, rd))
            # 多边形箭头
            if polygon_cover(x, y, ARROW):
                fg = max(fg, 1.0)
            if fg > 0:
                r = int(lerp(r, 255, fg))
                g = int(lerp(g, 255, fg))
                b = int(lerp(b, 255, fg))
                a = max(a, fg)

            row += bytes((int(r), int(g), int(b), int(a * 255)))
        rows.append(bytes(row))
    return downsample(rows)


def polygon_cover(x, y, pts):
    """射线法判定点是否在多边形内（无抗锯齿，边缘由外侧 grid 提供近似）。"""
    inside = False
    n = len(pts)
    j = n - 1
    for i in range(n):
        xi, yi = pts[i]
        xj, yj = pts[j]
        if ((yi > y) != (yj > y)) and (x < (xj - xi) * (y - yi) / (yj - yi) + xi):
            inside = not inside
        j = i
    return inside


def downsample(rows):
    out = []
    for oy in range(H):
        row = bytearray()
        for ox in range(W):
            r = g = b = a = 0
            for sy in range(SS):
                line = rows[oy * SS + sy]
                for sx in range(SS):
                    i = (ox * SS + sx) * 4
                    r += line[i]
                    g += line[i + 1]
                    b += line[i + 2]
                    a += line[i + 3]
            n = SS * SS
            row += bytes((r // n, g // n, b // n, a // n))
        out.append(bytes(row))
    return out


def write_png(path, rows):
    raw = b"".join(b"\x00" + r for r in rows)

    def chunk(tag, data):
        c = struct.pack(">I", len(data)) + tag + data
        return c + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)

    hdr = struct.pack(">IIBBBBB", W, H, 8, 6, 0, 0, 0)
    png = (b"\x89PNG\r\n\x1a\n"
           + chunk(b"IHDR", hdr)
           + chunk(b"IDAT", zlib.compress(raw, 9))
           + chunk(b"IEND", b""))
    with open(path, "wb") as f:
        f.write(png)


if __name__ == "__main__":
    dest = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                        "..", "com.personal.randomvideo", "rootfs_common", "icon.png")
    dest = os.path.normpath(dest)
    write_png(dest, render())
    size = os.path.getsize(dest)
    print("wrote %s (%d bytes)" % (dest, size))
    assert size < 100 * 1024, "图标必须小于 100KB，实际 %d" % size
