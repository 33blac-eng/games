"""Порівняння підходів лідерів до якості тексту (СИМУЛЯЦІЯ технік, не самі продукти).
Однаковий бюджет 8 Мбіт/с, 30 статичних кадрів, корпус bench/corpus.
  Parsec / Sunshine : H.264 4:4:4 (High 4:4:4, x264)  — їхній режим 4:4:4
  MS RDP            : AVC444 (дві підрамки 4:2:0)
  Chrome Remote Desk: VP9 profile 1 4:4:4 (UNVERIFIED, що саме так)
  oo-screen було    : 1080p + 4:2:0 Main
  oo-screen стало   : рідна + 4:2:0 Main + refine QP18 + lossless тайли
"""
import glob, os, sys
import numpy as np
from PIL import Image, ImageDraw, ImageFont
import pipeline as P, metrics as M, avc444 as A, final as F

KB = 8000
def variants(path):
    ref = np.asarray(Image.open(path).convert("RGB"))
    H = ref.shape[0]
    v = {}
    v["oo-screen БУЛО"] = P.run(ref, "420", 1080 / H if H > 1080 else None, "bicubic", KB)[0]
    v["Parsec / Sunshine (4:4:4)"] = P.run(ref, "444", None, "bicubic", KB)[0]
    v["MS RDP (AVC444)"] = A.run_A(ref, KB)[0]
    v["Chrome RD (VP9 4:4:4)*"] = A.run_C(ref, KB)[0]
    out = P.run(ref, "420", None, "bicubic", qp=18, frames=1)[0]
    v["oo-screen СТАЛО"] = P.paste_tiles(out.copy(), ref, F.select_tiles(path))
    return ref, v

def main():
    rows, agg = [], {}
    for path in sorted(glob.glob("../corpus/*.png")):
        ref, v = variants(path)
        mask = M.text_mask(ref)
        for k, out in v.items():
            es, g = M.text_sharpness(ref, out, mask)
            y0, cb0, cr0 = P.rgb_to_yuv(ref); y1, cb1, cr1 = P.rgb_to_yuv(out)
            r = dict(es=es, g=g, cp=(M.psnr(cb0, cb1) + M.psnr(cr0, cr1)) / 2)
            agg.setdefault(k, []).append(r)
        if os.path.basename(path) == "colortext-1440p.png":
            rows = (ref, v)
    ref, v = rows
    x0, y0, x1, y1 = 40, 90, 420, 200
    font = ImageFont.truetype("/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf", 20)
    items = list(v.items()) + [("ОРИГІНАЛ", ref)]
    cw, ch = (x1 - x0) * 2, (y1 - y0) * 2
    img = Image.new("RGB", (2 * cw + 48, 3 * (ch + 40) + 16), (245, 245, 245))
    d = ImageDraw.Draw(img)
    for i, (k, a) in enumerate(items):
        cx, cy = 16 + (i % 2) * (cw + 16), 16 + (i // 2) * (ch + 40)
        crop = Image.fromarray(np.clip(np.asarray(a)[y0:y1, x0:x1], 0, 255).astype("uint8")).resize((cw, ch), Image.NEAREST)
        col = (20, 120, 40) if "СТАЛО" in k else (180, 30, 30) if "БУЛО" in k else (40, 40, 40)
        d.text((cx, cy), k, fill=col, font=font)
        img.paste(crop, (cx, cy + 28))
    os.makedirs("img", exist_ok=True)
    img.save("img/vs-leaders.png")
    print("| Підхід | text edge-SSIM | чіткість тексту | chroma PSNR dB |\n|---|---|---|---|")
    for k, rs in agg.items():
        m = lambda f: np.mean([r[f] for r in rs])
        print(f"| {k} | {m('es'):.4f} | {m('g')*100:.1f} % | {m('cp'):.1f} |")

if __name__ == "__main__":
    main()
