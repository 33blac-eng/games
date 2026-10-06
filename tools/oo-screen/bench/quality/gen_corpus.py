#!/usr/bin/env python3
"""Synthetic desktop-like corpus.

legacy  -> bench/corpus/*.png         (spreadsheet, code dark/light, colored text; 1080p + 1440p)
screens -> bench/corpus/screens/*.png (bench/quality/metrics_run.py, TASK.md step 5): Excel-like
           sheet, 8-9 px fonts, ClearType-like RGB-subpixel text, mixed desktop (wallpaper +
           windows + taskbar + chart). 1080p, deterministic (seeded), committed so every run
           scores the same pixels regardless of the local FreeType build.

    python3 gen_corpus.py [--only legacy|screens|all]
"""
import argparse, os, random
import numpy as np
from PIL import Image, ImageDraw, ImageFilter, ImageFont

OUT = os.path.join(os.path.dirname(__file__), "..", "corpus")
SCREENS = os.path.join(OUT, "screens")
MONO = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
SANS = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"
_LIB = "/usr/share/fonts/truetype/liberation/"
UI = _LIB + "LiberationSans-Regular.ttf"      # metric clone of Arial (stand-in for Segoe UI/Calibri)
UI_B = _LIB + "LiberationSans-Bold.ttf"
CODE_F = _LIB + "LiberationMono-Regular.ttf"  # metric clone of Courier New (stand-in for Consolas)
SERIF = _LIB + "LiberationSerif-Regular.ttf"


def font(path, px):
    for p in (path, SANS):
        try:
            return ImageFont.truetype(p, px)
        except OSError:
            pass
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


# ---------------------------------------------------------------- screens set (metrics_run.py)

# FreeType's FT_LCD_FILTER_DEFAULT taps: the 5-tap FIR that ClearType-style renderers run
# over the 3x horizontal coverage before splitting it into R, G, B subpixels.
LCD_FIR = np.array([8, 77, 86, 77, 8], np.float64) / 256.0


def subpixel_text(im, xy, s, path, px, fill):
    """Draw `s` with RGB-stripe subpixel anti-aliasing (ClearType-like) onto RGB image `im`.

    Glyphs are rasterised at 3x in both axes, box-averaged back to 1x vertically, LCD-filtered
    horizontally, and every 3 horizontal samples become the R, G, B coverage of one pixel.
    Result: coloured fringes on stems, exactly the chroma detail 4:2:0 throws away."""
    f3 = font(path, 3 * px)
    l, t, r, b = f3.getbbox(s)
    w3 = (r + 9) // 3 * 3 + 3
    h3 = (b + 5) // 3 * 3
    m = Image.new("L", (w3, h3))
    ImageDraw.Draw(m).text((3, 0), s, font=f3, fill=255)
    a = np.asarray(m, np.float64).reshape(h3 // 3, 3, w3).mean(axis=1) / 255.0
    p = np.pad(a, ((0, 0), (2, 2)))
    a = sum(c * p[:, i:i + w3] for i, c in enumerate(LCD_FIR))
    cov = a.reshape(h3 // 3, w3 // 3, 3)
    x, y = xy[0] - 1, xy[1]
    arr = np.asarray(im, np.float64)
    H, W = arr.shape[:2]
    x0, y0, x1, y1 = max(x, 0), max(y, 0), min(x + cov.shape[1], W), min(y + cov.shape[0], H)
    if x1 <= x0 or y1 <= y0:
        return
    c = cov[y0 - y:y1 - y, x0 - x:x1 - x]
    reg = arr[y0:y1, x0:x1]
    arr[y0:y1, x0:x1] = reg * (1 - c) + np.asarray(fill, np.float64) * c
    im.paste(Image.fromarray(np.clip(np.rint(arr[y0:y1, x0:x1]), 0, 255).astype(np.uint8)), (x0, y0))


WORDS = ("invoice total balance supplier warehouse quantity amount discount payment status "
         "report quarter region manager approved pending overdue rebate shipment order "
         "рахунок залишок постачальник склад кількість сума знижка оплата звіт квартал").split()


def lorem(rnd, n):
    return " ".join(rnd.choice(WORDS) for _ in range(n))


def excel(w, h):
    """Excel-like workbook at ~80 % zoom: 11 px cells, ribbon, formula bar, banded table."""
    rnd = random.Random(11)
    im = Image.new("RGB", (w, h), "white"); d = ImageDraw.Draw(im)
    fu, fb, f12 = font(UI, 11), font(UI_B, 11), font(UI, 12)
    d.rectangle([0, 0, w, 30], fill=(33, 115, 70))
    d.text((w // 2 - 90, 8), "Budget_2026.xlsx - Excel", font=f12, fill="white")
    d.rectangle([0, 30, w, 122], fill=(243, 243, 243))
    for i, t in enumerate("File Home Insert Draw Page Layout Formulas Data Review View Help".split()):
        d.text((12 + i * 70, 36), t, font=f12, fill=(33, 115, 70) if t == "Home" else (40, 40, 40))
    for i in range(18):  # ribbon buttons: icon + 9 px caption
        x = 12 + i * 100
        d.rectangle([x, 60, x + 22, 82], fill=[(68, 114, 196), (237, 125, 49), (112, 173, 71), (255, 192, 0)][i % 4])
        d.text((x, 92), lorem(rnd, 1)[:12], font=font(UI, 9), fill=(60, 60, 60))
    d.line([0, 122, w, 122], fill=(210, 210, 210))
    d.rectangle([0, 123, w, 147], fill="white")
    d.text((8, 128), "D5", font=fu, fill=(0, 0, 0)); d.text((70, 127), "fx", font=font(SERIF, 13), fill=(90, 90, 90))
    d.text((100, 128), "=SUMIFS(Sales[Amount];Sales[Region];$B5;Sales[Quarter];D$4)", font=fu, fill=(0, 0, 0))
    top, rh, rw = 148, 17, 34
    cols = [rw] + [64 if c % 4 else 110 for c in range(1, 40)]
    xs = np.cumsum([0] + cols)
    d.rectangle([0, top, w, top + rh], fill=(230, 230, 230))
    for c in range(1, len(cols)):
        if xs[c] > w: break
        L = chr(64 + c) if c <= 26 else "A" + chr(64 + c - 26)
        d.text((xs[c] + cols[c] // 2 - 3, top + 3), L, font=fu, fill=(70, 70, 70))
    nrows = (h - top - 46) // rh
    for r in range(1, nrows):
        y = top + r * rh
        band = r > 2 and r % 2 == 0
        if r == 2:
            d.rectangle([rw, y, w, y + rh], fill=(68, 114, 196))
        elif band:
            d.rectangle([rw, y, w, y + rh], fill=(217, 225, 242))
        d.rectangle([0, y, rw, y + rh], fill=(230, 230, 230))
        d.text((4, y + 3), str(r), font=fu, fill=(70, 70, 70))
        for c in range(1, len(cols)):
            if xs[c] > w: break
            x0, cw = xs[c], cols[c]
            if r == 1:
                if c == 1: d.text((x0 + 3, y + 3), "Регіон / Region — Q1..Q4 2026, тис. грн", font=fb, fill=(0, 0, 0))
                continue
            if r == 2:
                d.text((x0 + 3, y + 3), ["Region", "Manager", "Q1", "Q2", "Q3", "Q4", "Total", "Δ %"][c % 8], font=fb, fill="white")
                continue
            if c % 4 == 0:
                v, col = lorem(rnd, 2)[:16].capitalize(), (0, 0, 0)
                d.text((x0 + 3, y + 3), v, font=fu, fill=col); continue
            k = rnd.random()
            if k < 0.12:
                v, col = f"({rnd.uniform(1, 9999):,.2f})", (192, 0, 0)
            elif k < 0.25:
                v, col = f"{rnd.uniform(-30, 60):.1f}%", (0, 97, 0) if k > 0.18 else (156, 0, 6)
            else:
                v, col = f"{rnd.uniform(0, 999999):,.2f}", (0, 0, 0)
            tw = d.textlength(v, font=fu)
            d.text((x0 + cw - 4 - tw, y + 3), v, font=fu, fill=col)
    for c in range(len(cols)):
        d.line([xs[c], top, xs[c], top + nrows * rh], fill=(212, 212, 212))
    for r in range(nrows + 1):
        d.line([0, top + r * rh, w, top + r * rh], fill=(212, 212, 212))
    sx, sy = xs[3], top + 5 * rh  # selected range D5:F12 + active cell border
    d.rectangle([sx, sy, xs[6], sy + 8 * rh], outline=(33, 115, 70), width=2)
    d.rectangle([xs[6] - 3, sy + 8 * rh - 3, xs[6] + 2, sy + 8 * rh + 2], fill=(33, 115, 70))
    d.rectangle([0, h - 46, w, h - 23], fill=(240, 240, 240))
    for i, t in enumerate(["Summary", "Sales", "Q3 detail", "Лист4"]):
        d.text((40 + i * 90, h - 41), t, font=fb if i == 1 else fu, fill=(33, 115, 70) if i == 1 else (60, 60, 60))
    d.rectangle([0, h - 22, w, h], fill=(243, 243, 243))
    d.text((8, h - 18), "Ready     Average: 12 345,67     Count: 32     Sum: 395 061,44", font=fu, fill=(60, 60, 60))
    return im


def tinyfont(w, h):
    """8-9 px UI/body text in sans, serif and mono; dark-on-light, grey, light-on-dark, links."""
    rnd = random.Random(8)
    im = Image.new("RGB", (w, h), "white"); d = ImageDraw.Draw(im)
    colw = w // 3
    styles = [(UI, 8, (0, 0, 0), None), (UI, 9, (0, 0, 0), None), (CODE_F, 8, (30, 30, 30), None),
              (SERIF, 9, (0, 0, 0), None), (UI, 9, (110, 110, 110), None), (UI, 8, (230, 230, 230), (45, 52, 64)),
              (CODE_F, 9, (212, 212, 212), (30, 30, 30)), (UI, 9, (0, 102, 204), None), (UI, 8, (255, 255, 255), (0, 120, 215))]
    for ci in range(3):
        y = 6
        si = ci * 3
        while y < h - 20:
            path, px, fg, bg = styles[si % len(styles)]
            f = font(path, px); lh = px + 3
            blk = 14 * lh
            if bg:
                d.rectangle([ci * colw, y - 2, (ci + 1) * colw - 4, min(y + blk, h)], fill=bg)
            for _ in range(14):
                if y + lh > h - 4: break
                t = f"{px}px " + lorem(rnd, 24)
                while d.textlength(t, font=f) > colw - 16: t = t[:-4]
                d.text((ci * colw + 6, y), t, font=f, fill=fg)
                if fg == (0, 102, 204):
                    d.line([ci * colw + 6, y + px + 1, ci * colw + 6 + d.textlength(t, font=f), y + px + 1], fill=fg)
                y += lh
            y += 8; si += 1
    return im


def cleartype(w, h):
    """ClearType-like subpixel text: light theme (left) and dark syntax-coloured code (right)."""
    rnd = random.Random(3)
    im = Image.new("RGB", (w, h), "white")
    ImageDraw.Draw(im).rectangle([w // 2, 0, w, h], fill=(30, 30, 30))
    y = 8
    for px in (9, 10, 11, 12, 13, 9, 11):
        for path, fg in ((UI, (0, 0, 0)), (SERIF, (40, 40, 40)), (CODE_F, (0, 0, 0)),
                         (UI, (192, 0, 0)), (UI, (0, 0, 192)), (UI, (0, 128, 0))):
            if y > h - px - 6: break
            subpixel_text(im, (8, y), f"{px}px " + lorem(rnd, 16), path, px, fg)
            y += px + 6
    kw, cm, st, fg, num = (86, 156, 214), (106, 153, 85), (206, 145, 120), (212, 212, 212), (181, 206, 168)
    code_lines = [(kw, "func (e *Encoder) Encode(f Frame) ([]AU, error) {"), (cm, "    // NV12 -> H.264 Main, PCVBR, B=0"),
                  (fg, "    if f.W != e.w || f.H != e.h { return nil, ErrSize }"), (st, '    log.Printf("enc %dx%d qp=%d", f.W, f.H, qp)'),
                  (num, "    peak := mean / 2 * 3 // 0x1F & nal[0]"), (fg, "    return e.drain()"), (fg, "}"), (fg, "")]
    y = 8
    i = 0
    while y < h - 22:
        col, ln = code_lines[i % len(code_lines)]
        subpixel_text(im, (w // 2 + 8, y), f"{i + 1:4d}", CODE_F, 12, (133, 133, 133))
        subpixel_text(im, (w // 2 + 52, y), ln, CODE_F, 12, col)
        y += 16; i += 1
    return im


def desktop(w, h):
    """Mixed desktop: photo-like wallpaper, icons, two overlapping windows (code + sheet + chart), taskbar."""
    rng = np.random.default_rng(42)

    def octave(div, amp):
        g = rng.normal(0, 1, (h // div + 2, w // div + 2, 3)).astype(np.float32)
        return np.stack([np.asarray(Image.fromarray(g[..., c], "F").resize((w, h), Image.BICUBIC)) for c in range(3)], -1) * amp

    yy, xx = np.mgrid[0:h, 0:w] / max(w, h)
    base = np.stack([30 + 110 * xx + 20 * np.sin(5 * yy), 70 + 80 * yy + 20 * np.sin(7 * xx + 2),
                     130 + 70 * np.cos(3 * xx * yy + 1)], -1)
    wall = np.clip(base + octave(96, 22) + octave(24, 6), 0, 255).astype(np.uint8)
    im = Image.fromarray(wall); d = ImageDraw.Draw(im)
    fi = font(UI, 11)
    for i, t in enumerate(["Цей ПК", "Кошик", "Звіти 2026", "1С Бухгалтерія", "Budget.xlsx"]):
        x, y = 18, 16 + i * 86
        d.rounded_rectangle([x + 14, y, x + 50, y + 36], 5, fill=[(255, 200, 60), (120, 170, 230), (250, 220, 120), (230, 60, 60), (33, 115, 70)][i])
        d.text((x + 33 - d.textlength(t, font=fi) / 2 + 1, y + 43), t, font=fi, fill=(0, 0, 0))
        d.text((x + 32 - d.textlength(t, font=fi) / 2, y + 42), t, font=fi, fill=(255, 255, 255))

    def window(x0, y0, x1, y1, title, dark):
        sh = Image.new("L", (w, h)); ImageDraw.Draw(sh).rectangle([x0 + 4, y0 + 6, x1 + 4, y1 + 6], fill=110)
        im.paste((0, 0, 0), (0, 0), sh.filter(ImageFilter.GaussianBlur(8)))
        d.rectangle([x0, y0, x1, y1], fill=(30, 30, 30) if dark else (255, 255, 255), outline=(90, 90, 90))
        d.rectangle([x0, y0, x1, y0 + 30], fill=(45, 45, 48) if dark else (243, 243, 243))
        d.text((x0 + 10, y0 + 8), title, font=font(UI, 12), fill=(220, 220, 220) if dark else (30, 30, 30))
        for k, c in enumerate([(232, 17, 35), (120, 120, 120), (120, 120, 120)]):
            d.rectangle([x1 - 30 - k * 40, y0 + 10, x1 - 20 - k * 40, y0 + 20], outline=c)

    window(110, 30, 1010, 760, "agent.go — oo-screen — Visual Studio Code", True)
    fc = font(CODE_F, 12)
    cols = [(86, 156, 214), (212, 212, 212), (106, 153, 85), (206, 145, 120), (78, 201, 176)]
    src = ["package main", "", "import (\"context\"; \"time\")", "// refine: QP 22 then 18 after 200 ms idle",
           "func (s *State) Due(now time.Time) (qp int, ok bool) {", "    if !s.armed { return 0, false }",
           "    return s.cfg.QPs[s.done], true", "}", ""]
    for i, y in enumerate(range(66, 750, 16)):
        ln = src[i % len(src)]
        d.text((122, y), f"{i + 1:3d}", font=fc, fill=(110, 110, 110))
        d.text((160, y), ln, font=fc, fill=cols[i % len(cols)] if ln else cols[1])
    window(700, 120, 1880, 1000, "Sales_Q3.xlsx - Excel", False)
    fs = font(UI, 11)
    rnd = random.Random(5)
    for r, y in enumerate(range(160, 600, 17)):
        d.line([700, y, 1880, y], fill=(212, 212, 212))
        for c, x in enumerate(range(734, 1880, 72)):
            if r == 0:
                d.text((x + 28, y + 3), chr(65 + c), font=fs, fill=(70, 70, 70)); continue
            v = f"{rnd.uniform(-999, 99999):,.0f}"
            d.text((x + 68 - d.textlength(v, font=fs), y + 3), v, font=fs, fill=(192, 0, 0) if v[0] == "-" else (0, 0, 0))
    for x in range(734, 1880, 72):
        d.line([x, 160, x, 600], fill=(212, 212, 212))
    d.rectangle([740, 620, 1290, 980], outline=(200, 200, 200), fill="white")
    for i in range(12):  # bar chart
        v = 40 + rnd.random() * 280
        d.rectangle([760 + i * 42, 960 - v, 788 + i * 42, 960], fill=[(68, 114, 196), (237, 125, 49)][i % 2])
        d.text((762 + i * 42, 964), ["Січ", "Лют", "Бер", "Кві", "Тра", "Чер", "Лип", "Сер", "Вер", "Жов", "Лис", "Гру"][i], font=font(UI, 9), fill=(60, 60, 60))
    pw, ph = 520, 360  # photo-like thumbnail: sky, sun, two ridges, field with grain
    py, px_ = np.mgrid[0:ph, 0:pw].astype(np.float64)
    ph_arr = np.stack([90 + 80 * py / ph, 150 + 60 * py / ph, 235 - 40 * py / ph], -1)
    ph_arr[(px_ - 400) ** 2 + (py - 70) ** 2 < 900] = (255, 236, 170)
    ridge1 = 170 + 30 * np.sin(px_ / 37) + 14 * np.sin(px_ / 11 + 1)
    ridge2 = 230 + 18 * np.sin(px_ / 53 + 2) + 8 * np.sin(px_ / 7)
    ph_arr[py > ridge1] = (95, 105, 125)
    ph_arr[py > ridge2] = (70, 130, 50)
    ph_arr = ph_arr + rng.normal(0, 6, ph_arr.shape)
    photo = Image.fromarray(np.clip(ph_arr, 0, 255).astype(np.uint8)).filter(ImageFilter.GaussianBlur(0.8))
    im.paste(photo, (1320, 620))
    d.text((1320, 985 - 2), "IMG_0412.jpg  520×360", font=font(UI, 9), fill=(60, 60, 60))
    d.rectangle([0, h - 40, w, h], fill=(32, 32, 32))
    for i in range(9):
        d.rounded_rectangle([12 + i * 48, h - 34, 40 + i * 48, h - 6], 4,
                            fill=[(0, 120, 215), (255, 185, 0), (33, 115, 70), (220, 60, 50), (100, 100, 100)][i % 5])
    for t, dy in (("14:32", 6), ("06.10.2026", 21)):
        d.text((w - 90, h - 40 + dy), t, font=font(UI, 11), fill=(240, 240, 240))
    d.text((w - 250, h - 28), "УКР   ENG", font=font(UI, 11), fill=(200, 200, 200))
    return im


SCREEN_SET = {"excel": excel, "tinyfont": tinyfont, "cleartype": cleartype, "desktop": desktop}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", choices=["legacy", "screens", "all"], default="all")
    a = ap.parse_args()
    if a.only in ("legacy", "all"):
        os.makedirs(OUT, exist_ok=True)
        for (w, h), s in [((1920, 1080), 1.0), ((2560, 1440), 1.0)]:
            tag = f"{h}p"
            for name, im in [("sheet", sheet(w, h, s)), ("code-dark", code(w, h, s, True)),
                             ("code-light", code(w, h, s, False)), ("colortext", colored(w, h, s))]:
                p = os.path.join(OUT, f"{name}-{tag}.png")
                im.save(p, optimize=True)
                print(p, os.path.getsize(p))
    if a.only in ("screens", "all"):
        os.makedirs(SCREENS, exist_ok=True)
        for name, fn in SCREEN_SET.items():
            p = os.path.join(SCREENS, f"{name}-1080p.png")
            fn(1920, 1080).save(p, optimize=True)
            print(p, os.path.getsize(p))

if __name__ == "__main__":
    main()
