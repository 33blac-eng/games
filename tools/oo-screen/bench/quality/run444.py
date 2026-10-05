#!/usr/bin/env python3
"""bench/quality/run444.py — Stage 3 4:4:4 variants on the corpus; prints markdown tables
(pasted into STAGE3-444.md). SIMULATION."""
import glob, os, sys
import numpy as np
from PIL import Image
import metrics as M
import pipeline as P
import avc444 as A

HERE = os.path.dirname(os.path.abspath(__file__))


def score(ref, out, mask):
    y0, cb0, cr0 = P.rgb_to_yuv(ref); y1, cb1, cr1 = P.rgb_to_yuv(out)
    return dict(yp=M.psnr(y0, y1), cp=(M.psnr(cb0, cb1) + M.psnr(cr0, cr1)) / 2,
                ssim=M.ssim(ref, out), es=M.text_sharpness(ref, out, mask)[0])


def main():
    vp9 = "libvpx-vp9" in os.popen("ffmpeg -hide_banner -encoders 2>/dev/null").read()
    corpus = sys.argv[1] if len(sys.argv) > 1 else os.path.join(HERE, "..", "corpus")
    agg, per = {}, []
    for path in sorted(glob.glob(os.path.join(corpus, "*.png"))):
        ref = np.asarray(Image.open(path).convert("RGB"))
        mask = M.text_mask(ref)
        for kb in (4000, 8000):
            res = {}
            out, b, t, _ = A.run_420(ref, kb); res["420 Main (baseline)"] = (out, b, t, 0)
            out, b, t = A.run_A(ref, kb); res["A AVC444 2xMain"] = (out, b, t, 0)
            out, b, t, nb, pt, frac = A.run_B(ref, kb, mask); res["B 420+lossless chroma tiles"] = (out, b, t, nb)
            if vp9:
                out, b, t = A.run_C(ref, kb); res["C VP9 p1 444"] = (out, b, t, 0)
            for name, (out, b, t, nb) in res.items():
                r = score(ref, out, mask); r.update(kbf=b / 1000, ms=t * 1000, kb1=nb * 8 / 1000)
                key = f"{name} @{kb // 1000}M"
                agg.setdefault(key, []).append(r)
                per.append((os.path.basename(path), key, r))
                print(f"{os.path.basename(path):22s} {key:36s} Y {r['yp']:.2f} C {r['cp']:.2f} "
                      f"SSIM {r['ssim']:.4f} eSSIM {r['es']:.4f} {r['kbf']:.0f}kb/f {r['ms']:.1f}ms "
                      f"oneshot {r['kb1']:.0f}kb", flush=True)
    print("\n| variant | luma PSNR dB | chroma PSNR dB | SSIM | text edge-SSIM | kbit/frame | one-shot kbit | encode ms/frame |")
    print("|---|---|---|---|---|---|---|---|")
    for k, rs in sorted(agg.items(), key=lambda kv: kv[0][-3:] + kv[0]):
        m = lambda f: float(np.mean([r[f] for r in rs]))
        print(f"| {k} | {m('yp'):.2f} | {m('cp'):.2f} | {m('ssim'):.4f} | {m('es'):.4f} | {m('kbf'):.0f} | "
              f"{m('kb1'):.0f} | {m('ms'):.1f} |")
    print("\n| image | variant | luma PSNR | chroma PSNR | edge-SSIM | kbit/frame | one-shot kbit |\n|---|---|---|---|---|---|---|")
    for img, k, r in per:
        print(f"| {img} | {k} | {r['yp']:.2f} | {r['cp']:.2f} | {r['es']:.4f} | {r['kbf']:.0f} | {r['kb1']:.0f} |")


if __name__ == "__main__":
    main()
