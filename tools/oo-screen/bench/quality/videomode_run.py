#!/usr/bin/env python3
"""bench/quality/videomode_run.py — SIMULATION of the agent's Video content mode (-video-mode, internal/contentmode).

The screen is sampled at 60 Hz (tick j = content time j/2 in 30 fps workload frames; motion is interpolated, so
scroll/drag/video move half a step per tick). A Python port of the agent's admission decides which ticks are
encoded:
  * no-op skip (a tick whose picture equals the last captured one is not a frame);
  * contentmode.Machine (same constants as internal/contentmode + internal/textmode) fed with the changed-pixel
    fraction of every captured frame and, for window drag, an emulated DXGI move rect (window area);
  * variant gate: "normal" — content frames at most -fps (30); "video" — 30 outside Video, 60 in Video (hardware
    encoder assumed fast enough); "capN" — a fixed N fps in every mode (what-if, not an agent mode).
  * a gated frame is flushed when the gap opens (textPending mechanism), so the last picture always arrives.
All variants are encoded the same way: x264 main 4:2:0 veryfast/zerolatency, -r 60, ABR mean/maxrate 1.5x/VBV 0.5 s
(as workloads_run.py); a non-admitted tick repeats the last admitted picture (P-skip, its bytes are NOT counted:
the agent would not send it). Bitrate boost: the hub raises the target only below the node ceiling; here the
target already IS the ceiling, so the boost is a no-op by construction (reported, not simulated).
Quality: every `step`-th content (30 fps) instant, the last frame actually SENT at or before it vs its own source:
full-frame RGB PSNR, PSNR of the video corner (640x360 rect, `video` only), text edge-SSIM (mean / p5); plus, for
`video`, "display" corner PSNR — the decoded picture on screen vs the source at both 60 Hz phases (a held 30 fps
frame is compared with the moved source, so smoothness shows up here). x264, not MS MFT."""
import argparse, json, math, os, subprocess, sys, tempfile
from concurrent.futures import ProcessPoolExecutor
import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import metrics as M
import workloads as WL
from textfps_run import TextDet

HERE = os.path.dirname(os.path.abspath(__file__))
VF = "scale=out_color_matrix=bt709:out_range=tv:flags=bicubic,format=yuv420p"
TICK = 60  # capture clock, Hz
BASE_FPS = 30

# ---- port of internal/contentmode ---------------------------------------------------------

ENTER_AREA, EXIT_AREA = 0.10, 0.04
ENTER_HOLD, EXIT_HOLD, MAX_GAP = 1.0, 1.5, 0.25  # seconds


class ContentMode:
    """Port of contentmode.Machine (default Config). Times in seconds."""
    NORMAL, TEXT, VIDEO = "normal", "text", "video"

    def __init__(self, no_video=False):
        self.text = TextDet()
        self.no_video = no_video
        self.video, self.streak, self.last, self.mode = False, None, 0.0, self.NORMAL

    def update(self, changed, moved, now):
        self.text.update(changed, moved)
        if not self.no_video:
            a = min(1.0, max(0.0, changed + moved))
            if self.video:
                if a >= EXIT_AREA:
                    self.last = now
            elif a >= ENTER_AREA:
                if self.streak is None or now - self.last > MAX_GAP + 1e-9:
                    self.streak = now
                self.last = now
                if now - self.streak >= ENTER_HOLD - 1e-9:
                    self.video, self.last = True, now
            else:
                self.streak = None
        return self._settle(now)

    def tick(self, now):
        if not self.video and self.streak is not None and now - self.last > MAX_GAP + 1e-9:
            self.streak = None
        return self._settle(now)

    def _settle(self, now):
        if self.video and now - self.last >= EXIT_HOLD - 1e-9:
            self.video, self.streak = False, None
        self.mode = self.VIDEO if self.video else (self.TEXT if self.text.text else self.NORMAL)
        return self.mode


def variant_fps(variant, mode):
    if variant == "normal":
        return BASE_FPS
    if variant == "video":
        return 60 if mode == ContentMode.VIDEO else BASE_FPS
    if variant.startswith("cap"):
        return int(variant[3:])
    raise ValueError(variant)


def admit(fractions, variant, moved=None):
    """fractions[j]: changed fraction of tick j vs j-1 (0 = unchanged); moved[j]: DXGI move-rect fraction
    (emulated for window drag). Returns (admitted[j] -> bool, modes[j]).
    Gate with 0.9 slack (videoModeGap); held content flushed when the gap opens."""
    cm = ContentMode(no_video=(variant != "video"))
    admitted, modes = [], []
    last_adm, pending = -10 ** 9, False
    for j, fr in enumerate(fractions):
        now = j / TICK
        mv = moved[j] if moved else 0.0
        mode = cm.update(fr, mv, now) if fr > 0 else cm.tick(now)
        gap = TICK / variant_fps(variant, mode) * 0.9  # ticks
        ok = False
        if fr > 0 or pending:
            if j - last_adm >= gap - 1e-9:
                ok = True
            else:
                pending = True
        if ok:
            last_adm, pending = j, False
        admitted.append(ok)
        modes.append(mode)
    return admitted, modes


# ---- 60 Hz sources -----------------------------------------------------------------------

class Ticker:
    """frame(j) at 60 Hz: content time t = j/2 workload frames, motion interpolated."""

    def __init__(self, kind, s):
        self.kind, self.s = kind, s
        self.wl = WL.make(kind, s)
        if kind == "mixed":
            self.parts = {k: Ticker(k, s) for k in self.wl.parts}

    def at(self, t):
        k, wl = self.kind, self.wl
        if k == "mixed":
            for kk, st, ln in wl.sched:
                if st <= t < st + ln:
                    return self.parts[kk].at(t - st)
            kk, st, _ = wl.sched[-1]
            return self.parts[kk].at(t - st)
        if k == "scroll":
            i = int(math.floor(t)); fr = t - i
            o0 = wl.off[min(i, len(wl.off) - 1)]; o1 = wl.off[min(i + 1, len(wl.off) - 1)]
            o = int(round(o0 + (o1 - o0) * fr)) % (wl.canvas.shape[0] - wl.h)
            return wl.canvas[o:o + wl.h]
        if k == "drag":
            return wl.frame(t)  # pos() is continuous in t
        if k == "video":
            return video_frame(wl, t)
        return wl.frame(int(math.floor(t)))  # idle/typing: discrete events

    def frame(self, j):
        return self.at(j / 2.0)

    def moved_at(self, t, tp):
        """Move-rect fraction DXGI would report between content times tp -> t (window drag only)."""
        if self.kind == "mixed":
            for kk, st, ln in self.wl.sched:
                if st <= t < st + ln:
                    return self.parts[kk].moved_at(t - st, tp - st) if tp >= st else 0.0
            return 0.0
        if self.kind == "drag":
            w = self.wl
            return w.WW * w.WH / (WL.W * WL.H) if w.pos(t) != w.pos(tp) else 0.0
        return 0.0

    def moved(self, j):
        return self.moved_at(j / 2.0, (j - 1) / 2.0) if j > 0 else 0.0


def video_frame(v, t):
    """WL.Video.region with fractional time (grain reseeded per 60 Hz tick)."""
    ts = t / WL.FPS
    xx, yy = v.xx, v.yy
    ox, oy = int(160 + 150 * math.sin(ts * 0.7)), int(90 + 80 * math.cos(ts * 0.5))
    tex = v.tex[oy:oy + v.VH, ox:ox + v.VW]
    rr = 128 + 90 * np.sin(xx / 70 + ts * 1.3) + 0.25 * (tex - 128)
    gg = 128 + 90 * np.sin(yy / 50 - ts * 0.9 + xx / 200) + 0.25 * (tex - 128)
    bb = 128 + 90 * np.cos((xx + yy) / 90 + ts * 1.7)
    cx, cy = 320 + 200 * math.sin(ts * 1.1), 180 + 120 * math.cos(ts * 1.4)
    blob = 80 * np.exp(-((xx - cx) ** 2 + (yy - cy) ** 2) / (2 * 60.0 ** 2))
    grain = np.random.default_rng(int(round(t * 2))).normal(0, 4, (v.VH, v.VW)).astype(np.float32)
    reg = np.clip(np.stack([rr + blob, gg + blob, bb - blob], -1) + grain[..., None], 0, 255).astype(np.uint8)
    f = v.s.code.copy()
    f[-v.VH - 40:-40, -v.VW - 40:-40] = reg
    return f


CORNER = (slice(WL.H - 360 - 40, WL.H - 40), slice(WL.W - 640 - 40, WL.W - 40))


# ---- encode + measure ----------------------------------------------------------------------

def enc_cmd(out, kbps):
    mx = kbps * 3 // 2
    return ["ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24", "-s", f"{WL.W}x{WL.H}",
            "-r", str(TICK), "-i", "-", "-vf", VF, "-c:v", "libx264", "-profile:v", "main", "-preset", "veryfast",
            "-tune", "zerolatency", "-b:v", f"{kbps}k", "-maxrate", f"{mx}k", "-bufsize", f"{mx // 2}k",
            "-g", "100000", "-pix_fmt", "yuv420p", "-color_range", "tv", "-colorspace", "bt709", out]


def packet_sizes(path):
    out = subprocess.run(["ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "packet=size",
                          "-of", "csv=p=0", path], capture_output=True, text=True, check=True).stdout
    return [int(x) for x in out.split()]


def yuv_rgb_planes(buf):
    import pipeline as P
    w, h = WL.W, WL.H
    a = np.frombuffer(buf, np.uint8)
    cw, ch = w // 2, h // 2
    y = a[:w * h].reshape(h, w).astype(np.float64)
    u = a[w * h:w * h + cw * ch].reshape(ch, cw).astype(np.float64)
    v = a[w * h + cw * ch:].reshape(ch, cw).astype(np.float64)
    return P.yuv_to_rgb(y, P.up420(u, h, w), P.up420(v, h, w))


def roundtrip420(rgb):
    """No-encode 4:2:0 ceiling: RGB -> YCbCr 4:2:0 (8 bit) -> RGB, same conversion as the decode path."""
    import pipeline as P
    y, cb, cr = P.rgb_to_yuv(rgb)
    y, cb, cr = P.q8(y), P.q8(P.sub420(cb)), P.q8(P.sub420(cr))
    h, w = y.shape
    return P.yuv_to_rgb(y.astype(np.float64), P.up420(cb.astype(np.float64), h, w), P.up420(cr.astype(np.float64), h, w))


def run(kind, variant, rates, seconds, step, corpus):
    s = WL.Sources(corpus)
    tk = Ticker(kind, s)
    nt = seconds * TICK
    # pass 1: fractions + admission
    fr, prev = [], None
    for j in range(nt):
        f = tk.frame(j)
        fr.append(1.0 if prev is None else float(np.any(f != prev, axis=2).mean()))
        prev = f
    adm, modes = admit(fr, variant, [tk.moved(j) for j in range(nt)])
    # frame quality: the last ADMITTED tick at or before each 30 fps sample point (what was sent, vs its source);
    # display (video corner only): decoded picture vs source at the sample instants, both 60 Hz phases.
    even = list(range(2 * (step // 4), nt, 2 * step))
    lastadm, la = [], 0
    for j in range(nt):
        if adm[j]:
            la = j
        lastadm.append(la)
    fq = sorted({lastadm[j] for j in even})
    dq = sorted(set(even) | {j + 1 for j in even if j + 1 < nt}) if kind == "video" else []
    samples = set(fq) | set(dq)
    td = tempfile.mkdtemp()
    outs = {k: os.path.join(td, f"{k}.h264") for k in rates}
    encs = {k: subprocess.Popen(enc_cmd(outs[k], k), stdin=subprocess.PIPE) for k in rates}
    ref, last = {}, None
    for j in range(nt):
        f = tk.frame(j)
        if adm[j] or last is None:
            last = np.ascontiguousarray(f).tobytes()
        for p in encs.values():
            p.stdin.write(last)
        if j in samples:
            ref[j] = np.array(f)
    for p in encs.values():
        p.stdin.close(); p.wait()
    sent = [j for j in range(nt) if adm[j]]
    res = {"kind": kind, "variant": variant, "seconds": seconds, "frames": len(sent),
           "video_share": sum(m == "video" for m in modes) / nt, "text_share": sum(m == "text" for m in modes) / nt,
           "rates": {}}
    fs = WL.W * WL.H * 3 // 2
    for k in rates:
        sizes = packet_sizes(outs[k])
        assert len(sizes) == nt, (len(sizes), nt)
        dec = subprocess.Popen(["ffmpeg", "-v", "error", "-i", outs[k], "-f", "rawvideo", "-pix_fmt", "yuv420p", "-"],
                               stdout=subprocess.PIPE)
        rows, disp = [], []
        for j in range(nt):
            buf = dec.stdout.read(fs)
            if j not in ref:
                continue
            out = np.clip(np.round(yuv_rgb_planes(buf)), 0, 255).astype(np.uint8)
            r = ref[j]
            if j in dq:
                disp.append(M.psnr(r[CORNER], out[CORNER]))
            if j in fq:
                mask = M.text_mask(r)
                es, _ = M.text_sharpness(r, out, mask)
                row = [M.psnr(r, out), es]
                if kind == "video":
                    row.append(M.psnr(r[CORNER], out[CORNER]))
                    rt = np.clip(np.round(roundtrip420(r)), 0, 255).astype(np.uint8)
                    row += [M.psnr(r[CORNER], rt[CORNER]), M.psnr(r, rt)]
                rows.append(row)
        dec.wait()
        a = np.array(rows, np.float64); a[np.isinf(a)] = 99.0
        d = np.array(disp, np.float64); d[np.isinf(d)] = 99.0
        q = {"psnr": float(a[:, 0].mean()), "psnr_p5": float(np.percentile(a[:, 0], 5)),
             "es": float(np.nanmean(a[:, 1])), "es_p5": float(np.nanpercentile(a[:, 1], 5)),
             "disp_psnr": float(d.mean()) if len(d) else float("nan")}
        if kind == "video":
            q["corner"] = float(a[:, 2].mean()); q["corner_p5"] = float(np.percentile(a[:, 2], 5))
            q["corner_420_ceiling"] = float(a[:, 3].mean())
            q["psnr_420_ceiling"] = float(a[:, 4].mean())
        nbytes = sum(sizes[j] for j in sent)
        q["mb_h"] = nbytes / 1e6 * 3600 / seconds
        q["avg_mbps"] = nbytes * 8 / seconds / 1e6
        res["rates"][str(k)] = q
        os.remove(outs[k])
    os.rmdir(td)
    print(json.dumps(res), flush=True)
    return res


PLAN = [  # (kind, variant, rates)
    ("video", "normal", [4000, 8000, 12000, 16000, 24000]),
    ("video", "video", [4000, 8000]),
    ("scroll", "normal", [2000, 4000, 8000]),
    ("scroll", "video", [2000, 4000, 8000]),
    ("scroll", "cap20", [2000]),
    ("scroll", "cap15", [2000]),
    ("drag", "normal", [4000, 8000]),
    ("drag", "video", [4000, 8000]),
    ("mixed", "normal", [4000, 8000]),
    ("mixed", "video", [4000, 8000]),
]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=WL.CORPUS)
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--step", type=int, default=15, help="quality sample every N content (30 fps) frames")
    ap.add_argument("--jobs", type=int, default=3)
    ap.add_argument("--only", default="", help="comma list of kinds")
    ap.add_argument("--json", default=os.path.join(HERE, "videomode-results.json"))
    a = ap.parse_args()
    plan = [p for p in PLAN if not a.only or p[0] in a.only.split(",")]
    with ProcessPoolExecutor(a.jobs) as ex:
        res = list(ex.map(run, *zip(*[(k, v, r, a.seconds, a.step, a.corpus) for k, v, r in plan])))
    with open(a.json, "w") as f:
        json.dump(res, f, indent=1)


if __name__ == "__main__":
    main()
