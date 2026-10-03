"""bench/quality/avc444.py — Stage 3: 4:4:4 colour variants (SIMULATION).

A: AVC444-style split (MS-RDPEGFX 3.3.8.3.2 "YUV420p stream combination for YUV444 mode"):
   main view   = Y444 + chroma at even (2x,2y) positions          (blocks B1..B3)
   aux view    = Y plane: in every 16-row macroblock row, rows 0..7 = U444 odd rows,
                 rows 8..15 = V444 odd rows                          (B4, B5)
                 U plane = U444[even rows, odd cols], V plane = V444[even rows, odd cols] (B6, B7)
   Both views are ordinary 4:2:0 frames -> libx264 Main, decoded, recombined.
   Deviations: (1) main chroma is point-sampled at even positions (lossless round trip);
   Windows encoders may low-pass it and the client then reconstructs U[2y,2x] as
   4*avg - (3 others) with a clamp heuristic — we use the simpler direct mapping; (2) both
   views are encoded every frame as two independent streams (RDP AVC444v1/v2 may send the
   aux view less often, LC field); (3) fixed bitrate split main:aux (AUX_SHARE);
   (4) frames are edge-padded to a multiple of 16.
B: 4:2:0 Main baseline + one-shot lossless full-res Cb/Cr for 16x16 tiles that contain text
   (PNG, optimize), applied on the receiver over decoded chroma. Static content only.
C: VP9 profile 1 yuv444p, libvpx-vp9 realtime.
"""
import io, os, subprocess, tempfile, time
import numpy as np
from PIL import Image
import pipeline as P

AUX_SHARE = 0.3


def pad16(a):
    h, w = a.shape
    return np.pad(a, ((0, -h % 16), (0, -w % 16)), mode="edge")


def split444(y, u, v):
    """uint8 HxW planes (H,W multiples of 16) -> (main(y,u,v), aux(y,u,v)), each 4:2:0."""
    h, w = y.shape
    main = (y, u[0::2, 0::2], v[0::2, 0::2])
    uo, vo = u[1::2], v[1::2]  # odd rows, h/2 x w
    ay = np.empty_like(y)
    for b in range(h // 16):
        ay[b * 16:b * 16 + 8] = uo[b * 8:b * 8 + 8]
        ay[b * 16 + 8:b * 16 + 16] = vo[b * 8:b * 8 + 8]
    aux = (ay, u[0::2, 1::2], v[0::2, 1::2])
    return main, aux


def combine444(main, aux):
    y, mu, mv = main
    ay, au, av = aux
    h, w = y.shape
    u = np.empty((h, w), y.dtype); v = np.empty((h, w), y.dtype)
    u[0::2, 0::2], v[0::2, 0::2] = mu, mv
    u[0::2, 1::2], v[0::2, 1::2] = au, av
    uo, vo = u[1::2], v[1::2]
    for b in range(h // 16):
        uo[b * 8:b * 8 + 8] = ay[b * 16:b * 16 + 8]
        vo[b * 8:b * 8 + 8] = ay[b * 16 + 8:b * 16 + 16]
    return y, u, v


def _ffenc(planes, w, h, pix, kbps, codec_args, ext, frames=30, fps=30):
    raw = b"".join(np.ascontiguousarray(p).tobytes() for p in planes)
    with tempfile.TemporaryDirectory() as td:
        src, enc, dec = (os.path.join(td, n) for n in ("in.yuv", "o." + ext, "out.yuv"))
        with open(src, "wb") as f:
            f.write(raw * frames)
        c = ["ffmpeg", "-v", "error", "-y"]
        t = time.time()
        subprocess.run(c + ["-f", "rawvideo", "-pix_fmt", pix, "-s", f"{w}x{h}", "-r", str(fps), "-i", src]
                       + codec_args + ["-b:v", f"{kbps}k", "-maxrate", f"{kbps}k", "-bufsize", f"{kbps}k",
                                       "-g", "600", "-pix_fmt", pix, enc], check=True)
        et = time.time() - t
        subprocess.run(c + ["-i", enc, "-f", "rawvideo", "-pix_fmt", pix, dec], check=True)
        data = np.fromfile(dec, np.uint8)
        bits = os.path.getsize(enc) * 8
    cw, ch = (w // 2, h // 2) if pix == "yuv420p" else (w, h)
    last = data[-(w * h + 2 * cw * ch):]
    out = (last[:w * h].reshape(h, w), last[w * h:w * h + cw * ch].reshape(ch, cw),
           last[w * h + cw * ch:].reshape(ch, cw))
    return out, bits / frames, et / frames


X264 = ["-c:v", "libx264", "-profile:v", "main", "-preset", "veryfast", "-tune", "zerolatency"]
VP9 = ["-c:v", "libvpx-vp9", "-profile:v", "1", "-deadline", "realtime", "-cpu-used", "8",
       "-row-mt", "1", "-lag-in-frames", "0", "-error-resilient", "1"]


def yuv444(rgb):
    y, u, v = P.rgb_to_yuv(rgb)
    return tuple(pad16(P.q8(p)) for p in (y, u, v))


def to_rgb(y, u, v, H, W):
    return P.yuv_to_rgb(*(np.asarray(p)[:H, :W].astype(np.float64) for p in (y, u, v)))


def run_420(rgb, kbps):
    H, W = rgb.shape[:2]
    y, u, v = yuv444(rgb); h, w = y.shape
    planes = (y, P.q8(P.sub420(u)), P.q8(P.sub420(v)))
    (dy, du, dv), bpf, et = _ffenc(planes, w, h, "yuv420p", kbps, X264, "h264")
    uu, vv = P.up420(du.astype(np.float64), h, w), P.up420(dv.astype(np.float64), h, w)
    return to_rgb(dy, uu, vv, H, W), bpf, et, (dy, uu, vv)


def run_A(rgb, kbps):
    H, W = rgb.shape[:2]
    y, u, v = yuv444(rgb); h, w = y.shape
    main, aux = split444(y, u, v)
    m, b1, t1 = _ffenc(main, w, h, "yuv420p", int(kbps * (1 - AUX_SHARE)), X264, "h264")
    a, b2, t2 = _ffenc(aux, w, h, "yuv420p", int(kbps * AUX_SHARE), X264, "h264")
    return to_rgb(*combine444(m, a), H, W), b1 + b2, t1 + t2


def text_tiles(mask, h, w, t=16):
    m = np.pad(mask, ((0, h - mask.shape[0]), (0, w - mask.shape[1])))
    return m.reshape(h // t, t, w // t, t).any(axis=(1, 3))


def run_B(rgb, kbps, mask):
    """Returns (rgb, video bits/frame, enc s/frame, one-shot refinement bytes, refinement s, tile fraction)."""
    H, W = rgb.shape[:2]
    _, bpf, et, (dy, uu, vv) = run_420(rgb, kbps)
    y, u, v = yuv444(rgb); h, w = y.shape
    t0 = time.time()
    tiles = text_tiles(mask, h, w)
    full = np.repeat(np.repeat(tiles, 16, 0), 16, 1)
    uu = np.where(full, u, uu); vv = np.where(full, v, vv)
    idx = np.argwhere(tiles)
    nbytes = 0
    if len(idx):
        strip = np.concatenate([np.concatenate([p[r * 16:r * 16 + 16, c * 16:c * 16 + 16] for r, c in idx], 1)
                                for p in (u, v)], 0)
        buf = io.BytesIO(); Image.fromarray(strip).save(buf, "PNG", optimize=True)
        nbytes = len(buf.getvalue()) + 4 * len(idx)  # + tile coords
    pt = time.time() - t0
    return to_rgb(dy, uu, vv, H, W), bpf, et, nbytes, pt, float(tiles.mean())


def run_C(rgb, kbps):
    H, W = rgb.shape[:2]
    y, u, v = yuv444(rgb); h, w = y.shape
    (dy, du, dv), bpf, et = _ffenc((y, u, v), w, h, "yuv444p", kbps, VP9, "webm")
    return to_rgb(dy, du, dv, H, W), bpf, et
