#!/usr/bin/env python3
"""Synthetic desktop-like corpus (spreadsheet, code dark/light, colored text) -> bench/corpus/*.png."""
import os, random
from PIL import Image, ImageDraw, ImageFont

OUT = os.path.join(os.path.dirname(__file__), "..", "corpus")
MONO = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
SANS = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"


def font(path, px):
    try:
        return ImageFont.truetype(path, px)
    except OSError:
        return ImageFont.load_default()


def sheet(w, h, s):
    im = Image.new("RGB", (w, h), "white"); d = ImageDraw.Draw(im)
    f = font(SANS, int(13 * s)); rh, cw = int(20 * s), int(90 * s)
    d.rectangle([0, 0, w, rh], fill=(230, 230, 230))
    rnd = random.Random(1)
    for y in range(rh, h, rh):
        d.line([0, y, w, y], fill=(212, 212, 212))
    for x in range(int(40 * s), w, cw):
        d.line([x, 0, x, h], fill=(212, 212, 212))
    for r, y in enumerate(range(rh, h - rh, rh)):
        d.text((4, y + 3), str(r + 1), font=f, fill=(80, 80, 80))
        for c, x in enumerate(range(int(40 * s), w - cw, cw)):
            v = f"{rnd.uniform(-9999, 99999):,.2f}" if (r + c) % 3 else rnd.choice(["Total", "Q3 rev", "Kyiv", "SKU-4471", "Net"])
            col = (192, 0, 0) if v.startswith("-") else (0, 0, 0)
            d.text((x + 4, y + 3), v, font=f, fill=col)
    return im


CODE = ["func (h *Hub) Serve(ctx context.Context, w http.ResponseWriter) error {",
        "    // forward NAL units to every subscribed viewer",
        "    for _, v := range h.viewers { if err := v.Write(nal); err != nil { return err } }",
        "    x := 0x1F & buf[i] // nal_unit_type", "    return fmt.Errorf(\"bad SPS: %w\", err)", "}", ""]


def code(w, h, s, dark):
    bg, fg = ((30, 30, 30), (212, 212, 212)) if dark else ((255, 255, 255), (30, 30, 30))
    kw, cm, st = ((86, 156, 214), (106, 153, 85), (206, 145, 120)) if dark else ((0, 0, 255), (0, 128, 0), (163, 21, 21))
    im = Image.new("RGB", (w, h), bg); d = ImageDraw.Draw(im)
    f = font(MONO, int(13 * s)); lh = int(18 * s)
    for i, y in enumerate(range(4, h - lh, lh)):
        ln = CODE[i % len(CODE)]
        col = cm if "//" in ln[:8] else st if '"' in ln else kw if ln.startswith(("func", "    for")) else fg
        d.text((4, y), f"{i + 1:4d}", font=f, fill=(128, 128, 128))
        d.text((int(60 * s), y), ln, font=f, fill=col)
    return im


def colored(w, h, s):
    im = Image.new("RGB", (w, h), "white"); d = ImageDraw.Draw(im)
    txt = "The quick brown fox jumps over the lazy dog 0123456789 — ЇжакҐ"
    for i, y in enumerate(range(8, h - 30, int(22 * s))):
        px = int((10 + i % 6) * s)
        col = [(220, 0, 0), (0, 0, 220), (0, 140, 0), (128, 0, 128)][i % 4]
        d.text((10, y), txt * 2, font=font(SANS if i % 2 else MONO, px), fill=col)
    return im


def main():
    os.makedirs(OUT, exist_ok=True)
    for (w, h), s in [((1920, 1080), 1.0), ((2560, 1440), 1.0)]:
        tag = f"{h}p"
        for name, im in [("sheet", sheet(w, h, s)), ("code-dark", code(w, h, s, True)),
                         ("code-light", code(w, h, s, False)), ("colortext", colored(w, h, s))]:
            p = os.path.join(OUT, f"{name}-{tag}.png")
            im.save(p, optimize=True)
            print(p, os.path.getsize(p))


if __name__ == "__main__":
    main()
