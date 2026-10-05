#!/usr/bin/env python3
"""bench/quality/codec444_motion.py — Q2/F8: chroma PSNR of 4:4:4 codecs (VP9 profile 1,
AV1 profile 1 / libaom) vs H.264 Main 4:2:0 on the corpus, IN MOTION (vertical scroll).

SIMULATION on Linux ffmpeg software encoders; no Windows capture/MF, no browser decode.

Method: each corpus PNG is stacked twice vertically; a 30-frame clip scrolls a 1-frame window
down by SCROLL px/frame (30 fps). Every decoded frame is compared with its source frame in
Y'CbCr BT.709 (8-bit, 4:4:4 reference); chroma PSNR = mean(Cb, Cr) over all frames.
Rate fairness: 4:4:4 encoders overshoot their target and libx264 CBR undershoots its target
(by ~50% on this content), so re-running H.264 with -b:v = the 4:4:4 codec's actual kbps is NOT
rate-matched. "H.264 @matched" therefore bisects libx264 CRF (no rate cap) until H.264's ACTUAL
kbps is >= the 4:4:4 codec's actual kbps (smallest such bitrate found); the table reports both
actual bitrates, so the match can be checked.
Usage: python3 codec444_motion.py [corpus_dir] [--quick]
"""
import glob, os, subprocess, sys, tempfile, time
import numpy as np
from PIL import Image
import metrics as M
import pipeline as P

HERE = os.path.dirname(os.path.abspath(__file__))
FRAMES, FPS, SCROLL = 30, 30, 8

X264 = ["-c:v", "libx264", "-profile:v", "main", "-preset", "veryfast", "-tune", "zerolatency"]
VP9P1 = ["-c:v", "libvpx-vp9", "-profile:v", "1", "-deadline", "realtime", "-cpu-used", "8",
         "-row-mt", "1", "-lag-in-frames", "0", "-error-resilient", "1"]
AV1P1 = ["-c:v", "libaom-av1", "-profile:v", "1", "-usage", "realtime", "-cpu-used", "8",
         "-row-mt", "1", "-lag-in-frames", "0", "-tiles", "2x2"]


def clip(rgb):
    h = rgb.shape[0] - rgb.shape[0] % 16
    w = rgb.shape[1] - rgb.shape[1] % 16
    tall = np.concatenate([rgb, rgb], 0)
    return [tall[i * SCROLL:i * SCROLL + h, :w] for i in range(FRAMES)]


def planes444(frames):
    out = []
    for f in frames:
        y, u, v = P.rgb_to_yuv(f)
        out.append(tuple(P.q8(p) for p in (y, u, v)))
    return out


def encode(ref444, pix, kbps, args, ext, crf=None):
    h, w = ref444[0][0].shape
    with tempfile.TemporaryDirectory() as td:
        src, enc, dec = (os.path.join(td, n) for n in ("in.yuv", "o." + ext, "out.yuv"))
        with open(src, "wb") as f:
            for y, u, v in ref444:
                if pix == "yuv420p":
                    u, v = P.q8(P.sub420(u)), P.q8(P.sub420(v))
                for p in (y, u, v):
                    f.write(np.ascontiguousarray(p).tobytes())
        c = ["ffmpeg", "-v", "error", "-y"]
        t = time.time()
        subprocess.run(c + ["-f", "rawvideo", "-pix_fmt", pix, "-s", f"{w}x{h}", "-r", str(FPS), "-i", src]
                       + args + (["-crf", str(crf)] if crf is not None else
                                 ["-b:v", f"{kbps}k", "-maxrate", f"{kbps}k", "-bufsize", f"{kbps}k"])
                       + ["-g", "600", "-pix_fmt", pix, enc], check=True)
        et = time.time() - t
        subprocess.run(c + ["-i", enc, "-f", "rawvideo", "-pix_fmt", pix, dec], check=True)
        data = np.fromfile(dec, np.uint8)
        bits = os.path.getsize(enc) * 8
    cw, ch = (w // 2, h // 2) if pix == "yuv420p" else (w, h)
    fs = w * h + 2 * cw * ch
    ys, cs = [], []
    for i, (ry, ru, rv) in enumerate(ref444):
        fr = data[i * fs:(i + 1) * fs]
        dy = fr[:w * h].reshape(h, w)
        du = fr[w * h:w * h + cw * ch].reshape(ch, cw).astype(np.float64)
        dv = fr[w * h + cw * ch:].reshape(ch, cw).astype(np.float64)
        if pix == "yuv420p":
            du, dv = P.up420(du, h, w), P.up420(dv, h, w)
        ys.append(M.psnr(ry, dy))
        cs.append((M.psnr(ru, P.q8(du)) + M.psnr(rv, P.q8(dv))) / 2)
    return dict(yp=float(np.mean(ys)), cp=float(np.mean(cs)), cmin=float(np.min(cs)),
                kbps=bits / FRAMES * FPS / 1000, ms=et / FRAMES * 1000)


def match_h264(ref, target_kbps, iters=7):
    """Bisect libx264 CRF so H.264's actual kbps >= target_kbps (as close as possible).
    Returns the smallest-bitrate result that reaches the target, else the highest-bitrate one."""
    lo, hi, best, top = 1, 51, None, None  # crf 0 = lossless, not allowed in Main
    for _ in range(iters):
        crf = (lo + hi) // 2
        r = encode(ref, "yuv420p", 0, X264, "h264", crf=crf)
        r["crf"] = crf
        if top is None or r["kbps"] > top["kbps"]:
            top = r
        if r["kbps"] >= target_kbps:
            if best is None or r["kbps"] < best["kbps"]:
                best = r
            lo = crf + 1
        else:
            hi = crf - 1
        if lo > hi:
            break
    out = best or top
    out["ratio"] = out["kbps"] / target_kbps  # >= 1.0 means truly rate-matched
    return out


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    quick = "--quick" in sys.argv
    corpus = args[0] if args else os.path.join(HERE, "..", "corpus")
    paths = sorted(glob.glob(os.path.join(corpus, "*1080p.png")))
    if quick:
        paths = paths[:1]
    encs = os.popen("ffmpeg -hide_banner -encoders 2>/dev/null").read()
    agg, rows = {}, []
    for path in paths:
        ref = planes444(clip(np.asarray(Image.open(path).convert("RGB"))))
        name = os.path.basename(path)
        for kb in (4000, 8000):
            res = {"H.264 Main 4:2:0": encode(ref, "yuv420p", kb, X264, "h264")}
            for label, a, ext, need in (("VP9 p1 4:4:4", VP9P1, "webm", "libvpx-vp9"),
                                        ("AV1 p1 4:4:4", AV1P1, "mkv", "libaom-av1")):
                if need not in encs:
                    continue
                r = res[label] = encode(ref, "yuv444p", kb, a, ext)
                res[f"H.264 4:2:0 @matched {label[:3]}"] = match_h264(ref, r["kbps"])
            for k, r in res.items():
                key = f"{k} @{kb // 1000}M"
                agg.setdefault(key, []).append(r)
                rows.append((name, key, r))
                print(f"{name:22s} {key:36s} Y {r['yp']:.2f} C {r['cp']:.2f} Cmin {r['cmin']:.2f} "
                      f"{r['kbps']:.0f}kbps {r['ms']:.1f}ms" + (f" crf {r['crf']}" if "crf" in r else ""), flush=True)
    print("\n| variant (target) | luma PSNR dB | chroma PSNR dB (mean) | chroma PSNR worst frame | actual kbit/s | encode ms/frame | H.264 kbps / 4:4:4 kbps (matched rows) |")
    print("|---|---|---|---|---|---|---|")
    for k, rs in agg.items():
        m = lambda f: float(np.mean([r[f] for r in rs]))
        print(f"| {k} | {m('yp'):.2f} | {m('cp'):.2f} | {min(r['cmin'] for r in rs):.2f} | {m('kbps'):.0f} | {m('ms'):.1f} | " + (f"{min(r['ratio'] for r in rs):.2f}..{max(r['ratio'] for r in rs):.2f} |" if 'ratio' in rs[0] else "— |"))
    print("\n| image | variant | chroma PSNR | actual kbit/s |\n|---|---|---|---|")
    for img, k, r in rows:
        print(f"| {img} | {k} | {r['cp']:.2f} | {r['kbps']:.0f} |")


if __name__ == "__main__":
    main()
