"""bench/quality/workloads.py — synthetic 30 fps workload sequences (SIMULATION) built from bench/corpus.

Each workload is a deterministic, index-addressable frame source: `Workload.frame(i)` returns HxWx3 uint8 RGB.
Kinds: idle, typing, scroll, drag, video, mixed (a–e interleaved with office-hour proportions).
Also: still-run / refine schedule helpers used by workloads_run.py (pure functions, unit-tested)."""
import math, os
import numpy as np
from PIL import Image

HERE = os.path.dirname(os.path.abspath(__file__))
CORPUS = os.path.join(HERE, "..", "corpus")
FPS = 30
W, H = 1920, 1080
KINDS = ["idle", "typing", "scroll", "drag", "video", "mixed"]
# office-hour proportions for "mixed" (fraction of time)
MIX = {"idle": 0.45, "typing": 0.25, "scroll": 0.10, "drag": 0.05, "video": 0.15}


def load(name, corpus=CORPUS):
    return np.asarray(Image.open(os.path.join(corpus, name)).convert("RGB"))


class Sources:
    def __init__(self, corpus=CORPUS, size=(W, H)):
        w, h = size
        def fit(a):
            return np.ascontiguousarray(np.asarray(Image.fromarray(a).resize((w, h), Image.BICUBIC))
                                        if a.shape[:2] != (h, w) else a)
        self.sheet = fit(load("sheet-1080p.png", corpus))
        self.code = fit(load("code-light-1080p.png", corpus))
        self.dark = fit(load("code-dark-1080p.png", corpus))
        self.color = fit(load("colortext-1080p.png", corpus))
        self.w, self.h = w, h


class Idle:
    def __init__(self, s): self.s = s
    def frame(self, i): return self.s.sheet


class Typing:
    """Spreadsheet cell editing: one glyph every ~150 ms, caret blink 530 ms."""
    GW, GH, CELL_CH = 8, 14, 12

    def __init__(self, s, seed=1):
        self.s = s
        r = np.random.default_rng(seed)
        self.glyphs = r.random((64, self.GH, self.GW)) < 0.28
        self.x0, self.y0 = s.w // 6, s.h // 4

    def _pos(self, k):
        row, col = divmod(k, self.CELL_CH)
        return self.x0 + col * (self.GW + 1), self.y0 + (row % 30) * 22

    def frame(self, i):
        f = self.s.sheet.copy()
        n = int(i * 1000 / FPS // 150)  # glyphs typed so far
        start = max(0, n - 30 * self.CELL_CH)
        for k in range(start, n):
            x, y = self._pos(k)
            g = self.glyphs[k % len(self.glyphs)]
            sub = f[y:y + self.GH, x:x + self.GW]
            sub[g] = (20, 20, 20)
        if int(i * 1000 / FPS // 530) % 2 == 0:  # caret
            x, y = self._pos(n)
            f[y:y + self.GH, x:x + 2] = 0
        return f


class Scroll:
    """Continuous vertical scroll 3..10 px/frame over a tall sheet+code canvas."""
    def __init__(self, s, seed=2, n=FPS * 60):
        self.canvas = np.concatenate([s.sheet, s.code, s.dark, s.sheet, s.color], 0)
        r = np.random.default_rng(seed)
        self.off = np.concatenate([[0], np.cumsum(r.integers(3, 11, n))])
        self.h = s.h

    def frame(self, i):
        o = int(self.off[min(i, len(self.off) - 1)]) % (self.canvas.shape[0] - self.h)
        return self.canvas[o:o + self.h]


class Drag:
    """900x600 window (code) dragged over the sheet desktop: 2 s moving (~12 px/frame), 1 s pause."""
    WW, WH = 900, 600

    def __init__(self, s):
        self.s = s
        win = s.code[100:100 + self.WH, 100:100 + self.WW].copy()
        win[:28] = (60, 90, 160)
        win[[0, -1], :] = 40; win[:, [0, -1]] = 40
        self.win = win

    def pos(self, i):
        cyc, ph = divmod(i, 3 * FPS)
        t = cyc * 2 * FPS + min(ph, 2 * FPS)  # time spent moving
        ax, ay = (self.s.w - self.WW) / 2, (self.s.h - self.WH) / 2
        return int(ax + ax * math.sin(t / 47.0)), int(ay + ay * math.sin(t / 31.0))

    def frame(self, i):
        f = self.s.sheet.copy()
        x, y = self.pos(i)
        f[y:y + self.WH, x:x + self.WW] = self.win
        return f


class Video:
    """640x360 natural-like animation (moving gradients + drifting filtered noise) in the bottom-right corner."""
    VW, VH = 640, 360

    def __init__(self, s, seed=3):
        self.s = s
        r = np.random.default_rng(seed)
        self.tex = np.asarray(Image.fromarray(r.integers(0, 256, (90, 160), dtype=np.uint8))
                              .resize((self.VW * 2, self.VH * 2), Image.BICUBIC), np.float32)
        yy, xx = np.mgrid[0:self.VH, 0:self.VW].astype(np.float32)
        self.xx, self.yy = xx, yy
        self.r = np.random.default_rng(seed + 1)

    def region(self, i):
        t = i / FPS
        xx, yy = self.xx, self.yy
        ox, oy = int(160 + 150 * math.sin(t * 0.7)), int(90 + 80 * math.cos(t * 0.5))
        tex = self.tex[oy:oy + self.VH, ox:ox + self.VW]
        rr = 128 + 90 * np.sin(xx / 70 + t * 1.3) + 0.25 * (tex - 128)
        gg = 128 + 90 * np.sin(yy / 50 - t * 0.9 + xx / 200) + 0.25 * (tex - 128)
        bb = 128 + 90 * np.cos((xx + yy) / 90 + t * 1.7)
        cx, cy = 320 + 200 * math.sin(t * 1.1), 180 + 120 * math.cos(t * 1.4)
        blob = 80 * np.exp(-((xx - cx) ** 2 + (yy - cy) ** 2) / (2 * 60.0 ** 2))
        grain = np.random.default_rng(i).normal(0, 4, (self.VH, self.VW)).astype(np.float32)
        return np.clip(np.stack([rr + blob, gg + blob, bb - blob], -1) + grain[..., None], 0, 255).astype(np.uint8)

    def frame(self, i):
        f = self.s.code.copy()
        f[-self.VH - 40:-40, -self.VW - 40:-40] = self.region(i)
        return f


def mixed_schedule(n=FPS * 60, seed=4, mix=MIX):
    """List of (kind, start, length) covering n frames; segment lengths 2–6 s, proportions ~ mix."""
    r = np.random.default_rng(seed)
    kinds = list(mix)
    budget = {k: int(round(mix[k] * n)) for k in kinds}
    segs = []
    while sum(budget.values()) > 0:
        left = [k for k in kinds if budget[k] > 0]
        k = left[r.integers(len(left))]
        if segs and segs[-1][0] == k and len(left) > 1:
            continue
        ln = min(budget[k], int(r.integers(2, 7)) * FPS)
        budget[k] -= ln
        segs.append([k, ln])
    out, t = [], 0
    for k, ln in segs:
        out.append((k, t, ln)); t += ln
    if t < n:
        out[-1] = (out[-1][0], out[-1][1], out[-1][2] + n - t)
    return [(k, s, min(ln, n - s)) for k, s, ln in out if s < n]


class Mixed:
    def __init__(self, s, n=FPS * 60):
        self.parts = {"idle": Idle(s), "typing": Typing(s), "scroll": Scroll(s), "drag": Drag(s), "video": Video(s)}
        self.sched = mixed_schedule(n)

    def frame(self, i):
        for k, st, ln in self.sched:
            if st <= i < st + ln:
                return self.parts[k].frame(i - st)
        k, st, _ = self.sched[-1]
        return self.parts[k].frame(i - st)


def make(kind, s):
    return {"idle": Idle, "typing": Typing, "scroll": Scroll, "drag": Drag, "video": Video, "mixed": Mixed}[kind](s)


# ---- schedule helpers -------------------------------------------------------------------

def changed_flags(frames):
    """changed[i] = frame i differs from frame i-1 (frame 0 always 'changed')."""
    out, prev = [], None
    for f in frames:
        out.append(prev is None or not np.array_equal(f, prev))
        prev = f
    return out


def still_runs(changed):
    """[(start, end_exclusive)] of maximal runs that begin with a changed frame and continue with unchanged ones."""
    runs, s = [], None
    for i, c in enumerate(changed):
        if c:
            if s is not None:
                runs.append((s, i))
            s = i
    if s is not None:
        runs.append((s, len(changed)))
    return runs


def refine_frames(start, end, idle_frames=6, gap_frames=(1,)):
    """Frame indices (within [start,end)) at which refine QP22 then QP18 go out after `idle_frames` of stillness.
    gap_frames[j]: frames between refine j and j+1 (from bytes/peak in the agent). Returns list of indices."""
    out, t = [], start + idle_frames
    for j in range(len(gap_frames) + 1):
        if t >= end:
            break
        out.append(t)
        if j < len(gap_frames):
            t += max(1, gap_frames[j])
    return out


def pctl(a, p):
    return float(np.percentile(np.asarray(a, dtype=np.float64), p)) if len(a) else float("nan")
