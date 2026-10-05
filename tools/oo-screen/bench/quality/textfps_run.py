"""bench/quality/textfps_run.py — SIMULATION of the agent's text-mode FPS cap (-text-fps) and GOP length (-gop-seconds).

Replays a workload (workloads.py) through a Python port of the agent's admission logic:
  * no-op skip (unchanged frames are never encoded),
  * keepalive: after 1 s without an encoded frame the last frame is re-encoded,
  * internal/textmode Detector (same constants) fed with the changed-area fraction of every changed frame,
  * text gate (agent/cmd/oo-agent/textfps.go): in text mode content frames closer than 1/text_fps to the last
    admitted one are held — only frames whose own changed area is < 5 % (textCapApplies); the held frame is
    flushed as soon as the gap opens.
Admitted frames go, in order, into x264 (veryfast/zerolatency, ABR @kbps, -g = gop_seconds*fps ENCODED frames —
the MFT GOP counts encoded frames, like here). Output: encoded frames, IDRs, MB/h, and the worst "content
latency" (how late a changed frame's content reaches the encoder). Not the MS MFT; relative numbers only."""
import argparse, json, os, subprocess, sys, tempfile
import numpy as np

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import workloads as WL

KEEPALIVE_FRAMES = WL.FPS  # keepaliveAfter = 1 s


class TextDet:
    """Port of internal/textmode.Detector with default Config."""
    ALPHA, ENTER_A, EXIT_A, ENTER_M, EXIT_M, MIN_FRAMES = 0.25, 0.05, 0.20, 0.01, 0.05, 8

    def __init__(self):
        self.area = self.motion = 0.0
        self.primed, self.quiet, self.text = False, 0, False

    def update(self, changed, moved=0.0):
        if not self.primed:
            self.area, self.motion, self.primed = changed, moved, True
        else:
            self.area += self.ALPHA * (changed - self.area)
            self.motion += self.ALPHA * (moved - self.motion)
        if self.text:
            if self.area > self.EXIT_A or self.motion > self.EXIT_M:
                self.text, self.quiet = False, 0
        else:
            self.quiet = self.quiet + 1 if (self.area < self.ENTER_A and self.motion < self.ENTER_M) else 0
            if self.quiet >= self.MIN_FRAMES:
                self.text = True
        return self.text


def schedule(fractions, text_fps, fps=WL.FPS):
    """fractions[i]: changed-area fraction of frame i vs i-1 (0 = unchanged). Returns (admitted, text_frames, lat):
    admitted = [(i, src)] encode frame src at tick i; lat = per changed frame, ticks until its content was encoded."""
    gap = 0 if text_fps <= 0 or text_fps >= fps else fps / text_fps  # in ticks
    det, last_adm, last_src, pending = TextDet(), -10 ** 9, None, None
    admitted, text_frames, lat = [], 0, []
    waiting = []  # changed frames whose content is not yet encoded
    for i, fr in enumerate(fractions):
        text = det.text
        src = None
        if fr > 0:
            text = det.update(fr)
            last_src = i
            waiting.append(i)
            g = gap if text and fr < TextDet.ENTER_A else 0  # textCapApplies
            if g and i - last_adm < g - 1e-9:
                pending = i
            else:
                src = i
        elif pending is not None and i - last_adm >= gap - 1e-9:
            src = last_src  # flush the held content frame
        elif last_src is not None and i - last_adm >= KEEPALIVE_FRAMES:
            src = last_src  # keepalive
        text_frames += text
        if src is not None:
            admitted.append((i, src))
            last_adm, pending = i, None
            lat += [i - w for w in waiting]
            waiting = []
    return admitted, text_frames, lat


def fractions_of(wl, n):
    out, prev = [], None
    for i in range(n):
        f = wl.frame(i)
        out.append(1.0 if prev is None else float(np.any(f != prev, axis=2).mean()))
        prev = f
    return out


def encode(wl, admitted, kbps, gop_frames, td, tag):
    out = os.path.join(td, f"{tag}.h264")
    mx = kbps * 3 // 2
    p = subprocess.Popen(["ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24", "-s", f"{WL.W}x{WL.H}",
                          "-r", str(WL.FPS), "-i", "-", "-vf", "format=yuv420p", "-c:v", "libx264", "-preset", "veryfast",
                          "-tune", "zerolatency", "-b:v", f"{kbps}k", "-maxrate", f"{mx}k", "-bufsize", f"{mx // 2}k",
                          "-g", str(gop_frames), "-keyint_min", str(gop_frames), "-sc_threshold", "0", out],
                         stdin=subprocess.PIPE)
    cache = {}
    for _, src in admitted:
        if src not in cache:
            cache.clear(); cache[src] = np.ascontiguousarray(wl.frame(src)).tobytes()
        p.stdin.write(cache[src])
    p.stdin.close(); p.wait()
    r = subprocess.run(["ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "packet=size,flags",
                        "-of", "csv=p=0", out], capture_output=True, text=True, check=True).stdout.split()
    sizes = [int(x.split(",")[0]) for x in r]
    keys = sum(1 for x in r if "K" in x.split(",")[1])
    return sum(sizes), keys


def run(kind, seconds, kbps, variants, corpus):
    n = seconds * WL.FPS
    s = WL.Sources(corpus)
    wl = WL.make(kind, s)
    fr = fractions_of(wl, n)
    td = tempfile.mkdtemp()
    rows = []
    for text_fps, gop_s in variants:
        adm, tf, lat = schedule(fr, text_fps)
        total, keys = encode(wl, adm, kbps, gop_s * WL.FPS, td, f"{kind}-{text_fps}-{gop_s}")
        rows.append({"kind": kind, "text_fps": text_fps, "gop_s": gop_s, "frames": len(adm), "idr": keys,
                     "text_share": tf / n, "mb_h": total / 1e6 * 3600 / seconds,
                     "lat_max_ms": (max(lat) if lat else 0) * 1000 / WL.FPS,
                     "lat_p95_ms": float(np.percentile(lat, 95)) * 1000 / WL.FPS if lat else 0})
        print(json.dumps(rows[-1]), flush=True)
    return rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=WL.CORPUS)
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--kbps", type=int, default=8000)
    ap.add_argument("--kinds", default="typing,typing-fast,mixed")
    ap.add_argument("--variants", default="0:2,15:2,0:10,15:10", help="text_fps:gop_seconds,...")
    ap.add_argument("--json", default="")
    a = ap.parse_args()
    variants = [tuple(int(x) for x in v.split(":")) for v in a.variants.split(",")]
    rows = []
    for k in a.kinds.split(","):
        rows += run(k, a.seconds, a.kbps, variants, a.corpus)
    if a.json:
        with open(a.json, "w") as f:
            json.dump(rows, f, indent=1)


if __name__ == "__main__":
    main()
