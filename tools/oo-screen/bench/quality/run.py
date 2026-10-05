#!/usr/bin/env python3
"""bench/quality/run.py — run corpus through simulated pipeline variants, write RESULTS.md."""
import argparse, glob, math, os, tempfile, time
import numpy as np
from PIL import Image
import metrics as M
import pipeline as P

HERE = os.path.dirname(os.path.abspath(__file__))


def variants(codec):
    v = [("444 raw", "444", None, None), ("420 raw", "420", None, None), ("420 scale0.75 raw", "420", 0.75, None)]
    if codec:
        for kb in (4000, 8000):
            for ch in ("420", "444"):
                for sc in (None, 0.75):
                    v.append((f"{ch}{' scale0.75' if sc else ''} {kb // 1000}M", ch, sc, kb))
    return v


def fmt(x, n=2):
    return "inf" if x == math.inf else ("n/a" if x is None or (isinstance(x, float) and math.isnan(x)) else f"{x:.{n}f}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=os.path.join(HERE, "..", "corpus"))
    ap.add_argument("--out", default=os.path.join(HERE, "RESULTS.md"))
    ap.add_argument("--filter", default="bicubic", choices=list(P.FILTERS))
    a = ap.parse_args()
    codec, vm = P.have_ffmpeg(), P.have_libvmaf()
    rows, agg = [], {}
    for path in sorted(glob.glob(os.path.join(a.corpus, "*.png"))):
        ref = np.asarray(Image.open(path).convert("RGB"))
        mask = M.text_mask(ref)
        for name, ch, sc, kb in variants(codec):
            t = time.time()
            out, bits = P.run(ref, ch, sc, a.filter, kb)
            v = None
            if vm:
                with tempfile.TemporaryDirectory() as td:
                    dp = os.path.join(td, "d.png"); Image.fromarray(out).save(dp); v = P.vmaf(path, dp)
            # chroma error on its own: PSNR of Cb/Cr planes (catches 4:2:0 colour bleed on red/blue text)
            _, cb0, cr0 = P.rgb_to_yuv(ref); _, cb1, cr1 = P.rgb_to_yuv(out)
            cpsnr = (M.psnr(cb0, cb1) + M.psnr(cr0, cr1)) / 2
            es, er = M.text_sharpness(ref, out, mask)
            r = dict(img=os.path.basename(path), var=name, psnr=M.psnr(ref, out), cpsnr=cpsnr,
                     ssim=M.ssim(ref, out), vmaf=v, es=es, er=er,
                     kbf=None if bits is None else bits / 1000)
            rows.append(r); agg.setdefault(name, []).append(r)
            print(f"{r['img']:24s} {name:22s} PSNR {fmt(r['psnr'])} SSIM {fmt(r['ssim'],4)} eSSIM {fmt(es,4)} "
                  f"Gratio {fmt(er,3)} ({time.time()-t:.1f}s)", flush=True)
    hdr = "| image | variant | PSNR dB | chroma PSNR dB | SSIM | VMAF | text edge-SSIM | text grad-energy ratio | kbit/frame |\n|---|---|---|---|---|---|---|---|---|\n"
    L = ["# bench/quality — Stage 0 results (SIMULATION)", "",
         "> **SIMULATION, NOT MEASURED ON REAL HARDWARE.** Synthetic corpus (`bench/quality/gen_corpus.py`), "
         "pipeline emulated in numpy + ffmpeg libx264 on Linux. No Windows capture, no MediaFoundation MFT, "
         "no browser decode/compositor. Numbers are for relative comparison of variants only.", "",
         "## Setup", "",
         "- Colour: RGB -> Y'CbCr BT.709 limited range (16-235/240), 8-bit; 4:2:0 = 2x2 box average, "
         "receiver bilinear chroma upsample; 4:4:4 = no subsampling.",
         f"- Scale: `scale0.75` = agent downscale x0.75 ({a.filter}), viewer bilinear upscale back to native size.",
         "- Codec: " + ("ffmpeg libx264, preset veryfast, tune zerolatency, CBR-ish (b=maxrate=bufsize), "
                        "30 identical frames @30fps, last decoded frame scored. 4:2:0 = profile main; "
                        "4:4:4 uses high444 (Main cannot carry 4:4:4 and WebRTC/Chrome won't accept it — reference only)."
                        if codec else "**ffmpeg not found — codec stage skipped.**"),
         "- VMAF: " + ("ffmpeg libvmaf." if vm else "**n/a** — this ffmpeg build has no libvmaf filter (only vmafmotion)."),
         "- Text sharpness: mask = pixels whose 3x3 luma range in the original > 64, dilated 1px. "
         "edge-SSIM = SSIM of gradient-magnitude maps averaged inside mask; grad-energy ratio = "
         "sum|grad dist|^2 / sum|grad ref|^2 inside mask (1 = as sharp, <1 blurred).",
         "- `raw` = colour/scale chain only, no codec.", "",
         "## Mean over corpus per variant", "", hdr.replace("| image ", "| n ")]
    for name, rs in agg.items():
        mean = lambda k: (None if any(r[k] is None for r in rs) else float(np.mean([r[k] for r in rs])))
        L[-1] += (f"| {len(rs)} | {name} | {fmt(mean('psnr'))} | {fmt(mean('cpsnr'))} | {fmt(mean('ssim'),4)} | {fmt(mean('vmaf'))} | "
                  f"{fmt(mean('es'),4)} | {fmt(mean('er'),3)} | {fmt(mean('kbf'),0)} |\n")
    L += ["## Per image", "", hdr + "".join(
        f"| {r['img']} | {r['var']} | {fmt(r['psnr'])} | {fmt(r['cpsnr'])} | {fmt(r['ssim'],4)} | {fmt(r['vmaf'])} | "
        f"{fmt(r['es'],4)} | {fmt(r['er'],3)} | {fmt(r['kbf'],0)} |\n" for r in rows)]
    with open(a.out, "w") as f:
        f.write("\n".join(L))
    print("wrote", a.out)


if __name__ == "__main__":
    main()
