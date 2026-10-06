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
    """2x2 box average; odd width/height are edge-replicated, giving the
    ceil(h/2) x ceil(w/2) chroma plane that yuv420p uses."""
    h, w = c.shape
    if h % 2 or w % 2:
        c = np.pad(c, ((0, h % 2), (0, w % 2)), mode="edge")
        h, w = c.shape
    return c.reshape(h // 2, 2, w // 2, 2).mean(axis=(1, 3))


def up420(c, h, w):
    return np.asarray(Image.fromarray(c.astype(np.float32), "F").resize((w, h), Image.BILINEAR), dtype=np.float64)


def x264(planes, w, h, chroma, kbps, frames=30, fps=30, qp=None):
    """Encode `frames` copies of a static frame, return decoded last frame planes.
    qp: constant-QP mode (x264 -qp) instead of the kbps rate control (static refine)."""
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
                        *(["-qp", str(qp)] if qp is not None else
                          ["-b:v", f"{kbps}k", "-maxrate", f"{kbps}k", "-bufsize", f"{kbps}k"]),
                        "-g", "600", "-pix_fmt", pix, enc], check=True)
        subprocess.run(common + ["-i", enc, "-f", "rawvideo", "-pix_fmt", pix, dec], check=True)
        data = np.fromfile(dec, dtype=np.uint8)
        bits = os.path.getsize(enc) * 8
    cw, ch = ((w + 1) // 2, (h + 1) // 2) if chroma == "420" else (w, h)
    fsz = w * h + 2 * cw * ch
    last = data[-fsz:]
    y = last[:w * h].reshape(h, w)
    u = last[w * h:w * h + cw * ch].reshape(ch, cw)
    v = last[w * h + cw * ch:].reshape(ch, cw)
    return (y, u, v), bits / frames


def run(rgb, chroma="420", scale=None, filt="bicubic", kbps=None, qp=None, frames=30):
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
    if kbps or qp is not None:
        planes, bits = x264(planes, w, h, chroma, kbps, frames=frames, qp=qp)
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


def paste_tiles(dst, src, rects):
    """Overlay lossless original pixels for tile rects (dicts X,Y,W,H) onto dst."""
    out = dst.copy()
    for r in rects:
        x, y, w, h = r["X"], r["Y"], r["W"], r["H"]
        out[y:y + h, x:x + w] = src[y:y + h, x:x + w]
    return out


# ---------------------------------------------------------------- agent-like encode (metrics_run.py)

def sps_profile_level_id(annexb):
    """profile-level-id hex (e.g. '4d4028') of the first SPS in an Annex-B stream, or None."""
    i = 0
    while True:
        i = annexb.find(b"\x00\x00\x01", i)
        if i < 0 or i + 6 > len(annexb):
            return None
        if annexb[i + 3] & 0x1F == 7:
            return annexb[i + 4:i + 7].hex()
        i += 3


def agent_x264(rgb, profile="main", kbps=None, peak=1.5, hrd=0.5, refs=2, gop=300, fps=30,
               frames=30, qp=None, threads=1, ffmpeg="ffmpeg", keep=None):
    """Encode `frames` copies of a static RGB frame the way the agent's MFT is configured
    (agent/encode/mft.c configure_codecapi): BT.709 limited 4:2:0, Main (CABAC) or Constrained
    Baseline (CAVLC), B=0, `refs` reference frames, low-latency, peak-constrained VBR
    (maxrate = peak*mean, VBV buffer = hrd*mean seconds), GOP `gop` frames, no scene-cut IDRs.
    qp: constant QP for every frame (I and P alike: ipratio=1), the refine-frame emulation.

    keep: decoded frame indices to convert back to RGB (default all; negative = from the end).

    Returns dict(frames=[RGB per kept frame], bytes=[per-frame bytes], plid=profile-level-id).
    threads=1: with zerolatency's sliced threads + VBV, x264 is NOT bit-exact run to run
    (measured: 3 runs at threads=4 gave 3 different streams); one thread is, at ~the same speed."""
    H, W = rgb.shape[:2]
    y, cb, cr = rgb_to_yuv(rgb)
    planes = (q8(y), q8(sub420(cb)), q8(sub420(cr)))
    raw = b"".join(p.tobytes() for p in planes)
    cw, ch = (W + 1) // 2, (H + 1) // 2
    xp = ["scenecut=0", "aud=1"]
    if qp is not None:
        rc = ["-qp", str(qp)]
        xp.append("ipratio=1.0")
    else:
        rc = ["-b:v", f"{kbps}k", "-maxrate", f"{int(kbps * peak)}k", "-bufsize", f"{int(kbps * hrd)}k"]
    with tempfile.TemporaryDirectory() as td:
        src, enc, dec = (os.path.join(td, n) for n in ("in.yuv", "o.h264", "out.yuv"))
        with open(src, "wb") as f:
            f.write(raw * frames)
        common = [ffmpeg, "-v", "error", "-y"]
        subprocess.run(common + ["-f", "rawvideo", "-pix_fmt", "yuv420p", "-s", f"{W}x{H}", "-r", str(fps),
                                 "-color_range", "tv", "-colorspace", "bt709", "-color_primaries", "bt709",
                                 "-color_trc", "bt709", "-i", src,
                                 "-c:v", "libx264", "-profile:v", profile, "-preset", "veryfast",
                                 "-tune", "zerolatency", "-threads", str(threads), "-bf", "0", "-refs", str(refs),
                                 "-g", str(gop), "-keyint_min", str(gop), *rc,
                                 "-x264-params", ":".join(xp), "-pix_fmt", "yuv420p", enc], check=True)
        stream = open(enc, "rb").read()
        subprocess.run(common + ["-i", enc, "-f", "rawvideo", "-pix_fmt", "yuv420p", dec], check=True)
        data = np.fromfile(dec, dtype=np.uint8)
    # per-AU sizes: x264 emits an access-unit delimiter (aud=1) in front of every frame
    starts, i = [], stream.find(b"\x00\x00\x00\x01\x09")
    while i >= 0:
        starts.append(i)
        i = stream.find(b"\x00\x00\x00\x01\x09", i + 5)
    sizes = [b - a for a, b in zip(starts, starts[1:] + [len(stream)])]
    fsz = W * H + 2 * cw * ch
    n = len(data) // fsz
    out = []
    for k in (range(n) if keep is None else [k % n for k in keep]):
        f = data[k * fsz:(k + 1) * fsz]
        yy = f[:W * H].reshape(H, W).astype(np.float64)
        u = f[W * H:W * H + cw * ch].reshape(ch, cw).astype(np.float64)
        v = f[W * H + cw * ch:].reshape(ch, cw).astype(np.float64)
        out.append(yuv_to_rgb(yy, up420(u, H, W), up420(v, H, W)))
    j = stream.find(b"x264 - core ")
    x264v = stream[j:stream.find(b" - ", j + 12)].decode("ascii", "replace") if j >= 0 else None
    return {"frames": out, "bytes": sizes, "plid": sps_profile_level_id(stream), "x264": x264v}


def ffmpeg_version(ffmpeg="ffmpeg"):
    try:
        out = subprocess.run([ffmpeg, "-hide_banner", "-version"], capture_output=True, text=True).stdout
        return out.splitlines()[0].split(" Copyright")[0] if out else None
    except OSError:
        return None


def have_libvmaf_at(ffmpeg):
    try:
        out = subprocess.run([ffmpeg, "-hide_banner", "-filters"], capture_output=True, text=True).stdout
    except OSError:
        return False
    return any(l.split()[1:2] == ["libvmaf"] for l in out.splitlines() if l.strip())


def vmaf_rgb(ref, dist, ffmpeg="ffmpeg", threads=4):
    """VMAF (default model, libvmaf) of two RGB arrays, via lossless PNG into `ffmpeg`.
    Both inputs go through the same RGB->YUV conversion, so the matrix choice cancels out."""
    with tempfile.TemporaryDirectory() as td:
        rp, dp = os.path.join(td, "r.png"), os.path.join(td, "d.png")
        Image.fromarray(ref).save(rp, compress_level=1)
        Image.fromarray(dist).save(dp, compress_level=1)
        r = subprocess.run([ffmpeg, "-hide_banner", "-i", dp, "-i", rp, "-lavfi",
                            f"[0:v]format=yuv444p[d];[1:v]format=yuv444p[r];[d][r]libvmaf=n_threads={threads}",
                            "-f", "null", "-"], capture_output=True, text=True)
    for l in r.stderr.splitlines():
        if "VMAF score" in l:
            return float(l.rsplit(":", 1)[1])
    return None
