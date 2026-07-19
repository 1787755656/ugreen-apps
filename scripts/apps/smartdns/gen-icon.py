#!/usr/bin/env python3
"""Generate rootfs_common/icon.png for the SmartDNS app.

Pure stdlib (zlib + struct) PNG encoder — no PIL/ImageMagick needed.
Design: light rounded card, blue globe with white grid (DNS), amber
lightning bolt with white outline (fastest-IP speed testing).
Rendered at 4x supersampling for anti-aliasing.
"""
import struct
import sys
import zlib

SIZE = 256
SS = 4  # supersample factor
W = SIZE * SS

# Colors (RGBA)
BG = (238, 244, 252, 255)
BORDER = (219, 226, 236, 255)
BLUE = (46, 107, 230, 255)
WHITE = (255, 255, 255, 255)
AMBER = (255, 166, 32, 255)
CLEAR = (0, 0, 0, 0)

# Geometry in 256-space
CARD_MARGIN = 8.0
CARD_RADIUS = 56.0
BORDER_W = 2.0
GCX, GCY, GR = 118.0, 122.0, 74.0   # globe center/radius (nudged left/up for bolt)
MERID_A, MERID_B = 33.0, 74.0        # vertical meridian ellipse semi-axes
GRID_T = 6.5                          # grid stroke width
LAT_DY = 36.0                         # latitude line offset from equator

# Lightning bolt polygon (local units, roughly 78 wide x 105 tall),
# translated so it sits over the globe's lower-right.
_BOLT = [(45, 0), (68, 0), (50, 42), (78, 42), (28, 105), (38, 58), (15, 58)]
BOLT_OX, BOLT_OY = 128.0, 88.0  # translation of bolt local origin
BOLT_SCALE = 1.05
OUTLINE_SCALE = 1.22  # white outline = bolt scaled about its centroid


def _poly(scale_about_centroid=1.0):
    pts = [(x * BOLT_SCALE + BOLT_OX, y * BOLT_SCALE + BOLT_OY) for x, y in _BOLT]
    if scale_about_centroid != 1.0:
        cx = sum(p[0] for p in pts) / len(pts)
        cy = sum(p[1] for p in pts) / len(pts)
        pts = [(cx + (x - cx) * scale_about_centroid,
                cy + (y - cy) * scale_about_centroid) for x, y in pts]
    return pts


BOLT_IN = _poly()
BOLT_OUT = _poly(OUTLINE_SCALE)


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
    if in_poly(BOLT_IN, x, y):
        return AMBER
    if in_poly(BOLT_OUT, x, y):
        return WHITE
    dx, dy = x - GCX, y - GCY
    r = (dx * dx + dy * dy) ** 0.5
    if r <= GR:
        # white grid on blue globe
        # meridian ellipse ring (normalized-radius test)
        f = ((dx / MERID_A) ** 2 + (dy / MERID_B) ** 2) ** 0.5
        if abs(f - 1.0) < GRID_T / (2 * MERID_A) * 0.9:
            return WHITE
        # equator + two latitude chords (inset a bit from the rim)
        if r <= GR - 2.5:
            for ly in (0.0, -LAT_DY, LAT_DY):
                if abs(dy - ly) < GRID_T / 2:
                    return WHITE
        return BLUE
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
