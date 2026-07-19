#!/usr/bin/env python3
"""Generate rootfs_common/icon.png for the PicoClaw app.

Pure stdlib (zlib + struct) PNG encoder — no PIL/ImageMagick needed.
Design: light rounded card, orange paw print with claw tips ("Claw"),
white lightning bolt on the palm pad (ultra-light / fast).
Rendered at 4x supersampling for anti-aliasing.
"""
import struct
import sys
import zlib

SIZE = 256
SS = 4  # supersample factor

# Colors (RGBA)
BG = (252, 246, 238, 255)
BORDER = (236, 226, 214, 255)
ORANGE = (245, 122, 32, 255)
WHITE = (255, 255, 255, 255)
CLEAR = (0, 0, 0, 0)

# Geometry in 256-space
CARD_MARGIN = 8.0
CARD_RADIUS = 56.0
BORDER_W = 2.0

# Palm pad: ellipse
PAD_CX, PAD_CY = 128.0, 163.0
PAD_A, PAD_B = 52.0, 42.0

# Toes: (cx, cy, r) — center toe higher, side toes lower and splayed
TOES = [(70.0, 112.0, 24.0), (128.0, 92.0, 25.0), (186.0, 112.0, 24.0)]

# Claw tips: wide triangles growing out of each toe circle (base spans the
# toe's diameter at its center), so toe+claw merge into one pointed shape.
CLAW_LEN = 22.0

# Lightning bolt on the palm pad (local units ~78x105, scaled + centered)
_BOLT = [(45, 0), (68, 0), (50, 42), (78, 42), (28, 105), (38, 58), (15, 58)]
BOLT_SCALE = 0.62
_bw = 78 * BOLT_SCALE
_bh = 105 * BOLT_SCALE
BOLT_OX = PAD_CX - _bw / 2 - 2.0
BOLT_OY = PAD_CY - _bh / 2
BOLT_PTS = [(x * BOLT_SCALE + BOLT_OX, y * BOLT_SCALE + BOLT_OY) for x, y in _BOLT]


def _claw_tris():
    tris = []
    # Claw directions: mostly upward, splayed slightly outward (y axis is
    # down, so "up" is negative y) — radial-from-pad looks too horizontal
    # for the side toes.
    dirs = [(-0.42, -0.91), (0.0, -1.0), (0.42, -0.91)]
    for (cx, cy, r), (ux, uy) in zip(TOES, dirs):
        px, py = -uy, ux                 # perpendicular
        half_w = r - 2.0
        tip = (cx + ux * (r + CLAW_LEN), cy + uy * (r + CLAW_LEN))
        b1 = (cx + px * half_w, cy + py * half_w)
        b2 = (cx - px * half_w, cy - py * half_w)
        tris.append([tip, b1, b2])
    return tris


CLAWS = _claw_tris()


def in_poly(pts, x, y):
    inside = False
    n = len(pts)
    for i in range(n):
        x1, y1 = pts[i]
        x2, y2 = pts[(i + 1) % n]
        if (y1 > y) != (y2 > y):
            if x < (x2 - x1) * (y - y1) / (y2 - y1) + x1:
                inside = not inside
    return inside


def card_sdf(x, y):
    """Signed distance to rounded-rect edge (negative = inside)."""
    hw = (SIZE - 2 * CARD_MARGIN) / 2
    cx, cy = SIZE / 2, SIZE / 2
    qx = abs(x - cx) - (hw - CARD_RADIUS)
    qy = abs(y - cy) - (hw - CARD_RADIUS)
    ax, ay = max(qx, 0.0), max(qy, 0.0)
    outside = (ax * ax + ay * ay) ** 0.5
    return outside + min(max(qx, qy), 0.0) - CARD_RADIUS


def sample(x, y):
    # z-order top -> down
    if in_poly(BOLT_PTS, x, y):
        return WHITE
    dx, dy = (x - PAD_CX) / PAD_A, (y - PAD_CY) / PAD_B
    if dx * dx + dy * dy <= 1.0:
        return ORANGE
    for cx, cy, r in TOES:
        if (x - cx) ** 2 + (y - cy) ** 2 <= r * r:
            return ORANGE
    for tri in CLAWS:
        if in_poly(tri, x, y):
            return ORANGE
    sd = card_sdf(x, y)
    if sd <= -BORDER_W:
        return BG
    if sd <= 0:
        return BORDER
    return CLEAR


def main(out_path):
    rows = []
    inv = 1.0 / SS
    n2 = SS * SS
    for py in range(SIZE):
        row = bytearray()
        for px in range(SIZE):
            rs = gs = bs = as_ = 0
            for sy in range(SS):
                for sx in range(SS):
                    c = sample(px + (sx + 0.5) * inv, py + (sy + 0.5) * inv)
                    rs += c[0]; gs += c[1]; bs += c[2]; as_ += c[3]
            row += bytes((rs // n2, gs // n2, bs // n2, as_ // n2))
        rows.append(bytes(row))

    raw = b"".join(b"\x00" + r for r in rows)

    def chunk(tag, data):
        c = tag + data
        return struct.pack(">I", len(data)) + c + struct.pack(">I", zlib.crc32(c) & 0xFFFFFFFF)

    png = (b"\x89PNG\r\n\x1a\n"
           + chunk(b"IHDR", struct.pack(">IIBBBBB", SIZE, SIZE, 8, 6, 0, 0, 0))
           + chunk(b"IDAT", zlib.compress(raw, 9))
           + chunk(b"IEND", b""))
    with open(out_path, "wb") as f:
        f.write(png)
    print(f"wrote {out_path}: {len(png)} bytes")


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "icon.png")
