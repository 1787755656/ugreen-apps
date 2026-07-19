#!/usr/bin/env python3
"""Generate rootfs_common/icon.png for the PicoClaw app from the official logo.

Pure stdlib (zlib + struct) PNG decode + encode — no PIL/ImageMagick needed.
Reads logo.png (official PicoClaw crawfish mascot, RGBA), crops to its
content alpha bounding box, bilinearly upscales, and composites it centered
onto a light rounded card. (Never rasterize via qlmanage — it ignores
declared scaling; see the UGOS dev notes.)

Usage: gen-icon.py [output.png]   (logo.png is expected next to this script)
"""
import os
import struct
import sys
import zlib

SIZE = 256
SS = 3  # supersample factor for card-edge anti-aliasing

# Card design (same family as the other apps in this repo)
BG = (247, 249, 252, 255)
BORDER = (228, 233, 240, 255)
CLEAR = (0, 0, 0, 0)
CARD_MARGIN = 8.0
CARD_RADIUS = 56.0
BORDER_W = 2.0

CONTENT = 182.0  # target size (px) of the logo's content bbox on the card
CX = CY = SIZE / 2.0


def decode_png_rgba(path):
    d = open(path, "rb").read()
    assert d[:8] == b"\x89PNG\r\n\x1a\n", "not a PNG"
    pos, w, h, idat = 8, 0, 0, b""
    while pos < len(d):
        (ln,) = struct.unpack(">I", d[pos:pos + 4])
        tag = d[pos + 4:pos + 8]
        data = d[pos + 8:pos + 8 + ln]
        if tag == b"IHDR":
            w, h, bitd, ct, comp, filt, inter = struct.unpack(">IIBBBBB", data)
            assert (bitd, ct, inter) == (8, 6, 0), \
                f"expected 8-bit RGBA non-interlaced, got depth={bitd} colortype={ct} interlace={inter}"
        elif tag == b"IDAT":
            idat += data
        elif tag == b"IEND":
            break
        pos += 12 + ln
    raw = zlib.decompress(idat)
    stride = w * 4
    px = bytearray(w * h * 4)
    prev = bytearray(stride)
    off = 0
    for y in range(h):
        f = raw[off]
        line = bytearray(raw[off + 1:off + 1 + stride])
        off += 1 + stride
        if f == 1:  # Sub
            for i in range(4, stride):
                line[i] = (line[i] + line[i - 4]) & 0xFF
        elif f == 2:  # Up
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif f == 3:  # Average
            for i in range(stride):
                a = line[i - 4] if i >= 4 else 0
                line[i] = (line[i] + ((a + prev[i]) >> 1)) & 0xFF
        elif f == 4:  # Paeth
            for i in range(stride):
                a = line[i - 4] if i >= 4 else 0
                b = prev[i]
                c = prev[i - 4] if i >= 4 else 0
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                pr = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                line[i] = (line[i] + pr) & 0xFF
        px[y * stride:(y + 1) * stride] = line
        prev = line
    return w, h, px


LOGO_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), "logo.png")
LW, LH, LPX = decode_png_rgba(LOGO_PATH)

# Content alpha bounding box (don't trust canvas centering — logos often
# have asymmetric padding).
x0, y0, x1, y1 = LW, LH, -1, -1
for y in range(LH):
    row = y * LW * 4
    for x in range(LW):
        if LPX[row + x * 4 + 3] > 8:
            if x < x0: x0 = x
            if x > x1: x1 = x
            if y < y0: y0 = y
            if y > y1: y1 = y
assert x1 >= x0 and y1 >= y0, "logo is fully transparent?"
BW, BH = x1 - x0 + 1, y1 - y0 + 1
SCALE = CONTENT / max(BW, BH)
DW, DH = BW * SCALE, BH * SCALE  # drawn size on card


def logo_sample(u, v):
    """Bilinear RGBA sample of the logo at source-space float coords."""
    if u < 0: u = 0.0
    if v < 0: v = 0.0
    if u > LW - 1: u = float(LW - 1)
    if v > LH - 1: v = float(LH - 1)
    ix, iy = int(u), int(v)
    fx, fy = u - ix, v - iy
    ix2 = min(ix + 1, LW - 1)
    iy2 = min(iy + 1, LH - 1)
    out = []
    for c in range(4):
        p00 = LPX[(iy * LW + ix) * 4 + c]
        p10 = LPX[(iy * LW + ix2) * 4 + c]
        p01 = LPX[(iy2 * LW + ix) * 4 + c]
        p11 = LPX[(iy2 * LW + ix2) * 4 + c]
        out.append((p00 * (1 - fx) + p10 * fx) * (1 - fy)
                   + (p01 * (1 - fx) + p11 * fx) * fy)
    return out


def card_sdf(x, y):
    """Signed distance to rounded-rect edge (negative = inside)."""
    hw = (SIZE - 2 * CARD_MARGIN) / 2
    qx = abs(x - CX) - (hw - CARD_RADIUS)
    qy = abs(y - CY) - (hw - CARD_RADIUS)
    ax, ay = max(qx, 0.0), max(qy, 0.0)
    outside = (ax * ax + ay * ay) ** 0.5
    return outside + min(max(qx, qy), 0.0) - CARD_RADIUS


def sample(x, y):
    sd = card_sdf(x, y)
    if sd > 0:
        return CLEAR
    base = BG if sd <= -BORDER_W else BORDER
    # Map card coords -> logo source coords (content bbox centered at CX,CY)
    lx = x0 + (x - (CX - DW / 2)) / SCALE
    ly = y0 + (y - (CY - DH / 2)) / SCALE
    if -1 <= lx <= LW and -1 <= ly <= LH:
        r, g, b, a = logo_sample(lx, ly)
        if a > 0:
            af = a / 255.0
            return (r * af + base[0] * (1 - af),
                    g * af + base[1] * (1 - af),
                    b * af + base[2] * (1 - af), 255)
    return base


def main(out_path):
    rows = []
    inv = 1.0 / SS
    n2 = SS * SS
    for py in range(SIZE):
        row = bytearray()
        for px in range(SIZE):
            rs = gs = bs = as_ = 0.0
            for sy in range(SS):
                for sx in range(SS):
                    c = sample(px + (sx + 0.5) * inv, py + (sy + 0.5) * inv)
                    rs += c[0]; gs += c[1]; bs += c[2]; as_ += c[3]
            row += bytes((int(rs / n2), int(gs / n2), int(bs / n2), int(as_ / n2)))
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
    print(f"wrote {out_path}: {len(png)} bytes "
          f"(logo bbox {BW}x{BH} @ scale {SCALE:.3f} -> {DW:.0f}x{DH:.0f})")


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "icon.png")
