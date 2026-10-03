"""bench/quality/pipeline.py — simulate agent chain without Windows:
RGB -> [downscale] -> YUV BT.709 limited -> 4:2:0|4:4:4 -> [x264 enc/dec] -> RGB -> [viewer upscale]."""
import os, shutil, subprocess, tempfile
import numpy as np
from PIL import Image

KR, KB = 0.2126, 0.0722
KG = 1 - KR - KB
FILTERS = {"bilinear": Image.BILINEAR, "bicubic": Image.BICUBIC, "lanczos": Image.LANCZOS}


def have_ffmpeg():
    return shutil.which("ffmpeg") is not None


def have_libvmaf():
    if not have_ffmpeg():
        return False
    out = subprocess.run(["ffmpeg", "-hide_banner", "-filters"], capture_output=True, text=True).stdout
    return any(l.split()[1:2] == ["libvmaf"] for l in out.splitlines() if l.strip())


def rgb_to_yuv(rgb):
    a = rgb.astype(np.float64) / 255.0
    r, g, b = a[..., 0], a[..., 1], a[..., 2]
    y = KR * r + KG * g + KB * b
    cb = (b - y) / (2 * (1 - KB))
    cr = (r - y) / (2 * (1 - KR))
    return 16 + 219 * y, 128 + 224 * cb, 128 + 224 * cr


def yuv_to_rgb(y, cb, cr):
    y = (y - 16) / 219.0; cb = (cb - 128) / 224.0; cr = (cr - 128) / 224.0
    r = y + 2 * (1 - KR) * cr
    b = y + 2 * (1 - KB) * cb
    g = (y - KR * r - KB * b) / KG
    return np.clip(np.rint(np.stack([r, g, b], -1) * 255), 0, 255).astype(np.uint8)


def q8(p):
    return np.clip(np.rint(p), 0, 255).astype(np.uint8)


def sub420(c):
    h, w = c.shape
    return c.reshape(h // 2, 2, w // 2, 2).mean(axis=(1, 3))


def up420(c, h, w):
    return np.asarray(Image.fromarray(c.astype(np.float32), "F").resize((w, h), Image.BILINEAR), dtype=np.float64)


def x264(planes, w, h, chroma, kbps, frames=30, fps=30):
    """Encode `frames` copies of a static frame, return decoded last frame planes."""
    pix = "yuv420p" if chroma == "420" else "yuv444p"
    profile = "main" if chroma == "420" else "high444"  # Main can't carry 4:4:4
    raw = b"".join(p.tobytes() for p in planes)
    with tempfile.TemporaryDirectory() as td:
        src, enc, dec = (os.path.join(td, n) for n in ("in.yuv", "o.h264", "out.yuv"))
        with open(src, "wb") as f:
            f.write(raw * frames)
        common = ["ffmpeg", "-v", "error", "-y"]
        subprocess.run(common + ["-f", "rawvideo", "-pix_fmt", pix, "-s", f"{w}x{h}", "-r", str(fps),
                        "-color_range", "tv", "-colorspace", "bt709", "-i", src,
                        "-c:v", "libx264", "-profile:v", profile, "-preset", "veryfast", "-tune", "zerolatency",
                        "-b:v", f"{kbps}k", "-maxrate", f"{kbps}k", "-bufsize", f"{kbps}k",
                        "-g", "600", "-pix_fmt", pix, enc], check=True)
        subprocess.run(common + ["-i", enc, "-f", "rawvideo", "-pix_fmt", pix, dec], check=True)
        data = np.fromfile(dec, dtype=np.uint8)
        bits = os.path.getsize(enc) * 8
    cw, ch = (w // 2, h // 2) if chroma == "420" else (w, h)
    fsz = w * h + 2 * cw * ch
    last = data[-fsz:]
    y = last[:w * h].reshape(h, w)
    u = last[w * h:w * h + cw * ch].reshape(ch, cw)
    v = last[w * h + cw * ch:].reshape(ch, cw)
    return (y, u, v), bits / frames


def run(rgb, chroma="420", scale=None, filt="bicubic", kbps=None):
    """Returns (reconstructed RGB at original size, avg bits/frame or None)."""
    H, W = rgb.shape[:2]
    img = rgb
    if scale:
        w, h = int(W * scale) // 2 * 2, int(H * scale) // 2 * 2
        img = np.asarray(Image.fromarray(rgb).resize((w, h), FILTERS[filt]))
    h, w = img.shape[:2]
    y, cb, cr = rgb_to_yuv(img)
    if chroma == "420":
        cb, cr = sub420(cb), sub420(cr)
    planes = (q8(y), q8(cb), q8(cr))
    bits = None
    if kbps:
        planes, bits = x264(planes, w, h, chroma, kbps)
    y, cb, cr = (p.astype(np.float64) for p in planes)
    if chroma == "420":
        cb, cr = up420(cb, h, w), up420(cr, h, w)
    out = yuv_to_rgb(y, cb, cr)
    if scale:  # viewer stretches back to the physical size (browser bilinear)
        out = np.asarray(Image.fromarray(out).resize((W, H), Image.BILINEAR))
    return out, bits


def vmaf(ref_png, dist_png):
    r = subprocess.run(["ffmpeg", "-hide_banner", "-i", dist_png, "-i", ref_png, "-lavfi", "libvmaf", "-f", "null", "-"],
                       capture_output=True, text=True)
    for l in r.stderr.splitlines():
        if "VMAF score" in l:
            return float(l.rsplit(":", 1)[1])
    return None
