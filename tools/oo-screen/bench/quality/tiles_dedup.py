"""bench/quality/tiles_dedup.py — text-tile bytes/hour before vs after cross-episode dedup (SIMULATION).

Replays bench/quality/workloads.py sequences with the agent's episode rules
(agent/cmd/oo-agent/tiles.go + internal/tiles.Episodes):
  - an episode starts once the screen has been still for IDLE frames
    (refine path: refine QP22 after 200 ms = 6 frames; soft path: 500 ms = 15 frames),
  - at most one episode per epoch (any changed frame starts a new epoch),
  - episodes no more often than tiles.DefaultMinInterval (2 s = 60 frames).
Each episode's frame goes to the Go helper `tiledup`, which runs tiles.Build (every
episode resends all selected tiles) and tiles.BuildDedup (shared Held, TypeKeep for
unchanged tiles) and reports wire bytes (headers included, 2 MB episode cap).

Usage: python3 bench/quality/tiles_dedup.py [--seconds 60] [--kinds typing,typing-pauses,mixed,drag,idle]
"""
import argparse, json, os, subprocess, tempfile
import numpy as np
from PIL import Image

import workloads as WL

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
MIN_INTERVAL = 2 * WL.FPS


class TypingPauses(WL.Typing):
    """Typing in bursts: 6–12 glyphs at 150 ms, then a 1.5–4 s pause (reading/thinking); caret blinks."""

    def __init__(self, s, seed=5, n=WL.FPS * 3600):
        super().__init__(s)
        r = np.random.default_rng(seed)
        t, times = 0.0, []
        while t < n:
            for _ in range(int(r.integers(6, 13))):
                times.append(t); t += WL.FPS * 0.150
            t += WL.FPS * float(r.uniform(1.5, 4.0))
        self.times = np.array(times)

    def frame(self, i):
        f = self.s.sheet.copy()
        n = int(np.searchsorted(self.times, i, side="right"))
        start = max(0, n - 30 * self.CELL_CH)
        for k in range(start, n):
            x, y = self._pos(k)
            sub = f[y:y + self.GH, x:x + self.GW]
            sub[self.glyphs[k % len(self.glyphs)]] = (20, 20, 20)
        if int(i * 1000 / WL.FPS // 530) % 2 == 0:
            x, y = self._pos(n)
            f[y:y + self.GH, x:x + 2] = 0
        return f


def make(kind, s):
    return TypingPauses(s) if kind == "typing-pauses" else WL.make(kind, s)


def episodes(kind, n, idle, corpus):
    s = WL.Sources(corpus)
    wl = make(kind, s)
    prev, last_change, started, last_ep, out = None, 0, False, None, []
    for i in range(n):
        f = wl.frame(i)
        if prev is None or not np.array_equal(f, prev):
            last_change, started = i, False
        elif not started and i - last_change >= idle and (last_ep is None or i - last_ep >= MIN_INTERVAL):
            started, last_ep = True, i
            out.append((i, f))
        prev = f
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=WL.CORPUS)
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--kinds", default="typing,typing-pauses,mixed,drag,idle")
    ap.add_argument("--idle", default="6,15", help="still frames before an episode (refine path, soft path)")
    a = ap.parse_args()
    n = a.seconds * WL.FPS
    td = tempfile.mkdtemp()
    tool = os.path.join(td, "tiledup")
    subprocess.run(["go", "build", "-o", tool, "./bench/quality/tiledup"], cwd=ROOT, check=True)
    rows = []
    for kind in a.kinds.split(","):
        for idle in map(int, a.idle.split(",")):
            eps = episodes(kind, n, idle, a.corpus)
            paths = []
            for j, (_, f) in enumerate(eps):
                p = os.path.join(td, f"{kind}-{idle}-{j:04d}.png")
                Image.fromarray(f).save(p, compress_level=1)
                paths.append(p)
            res = json.loads(subprocess.run([tool] + paths, capture_output=True, text=True, check=True).stdout) if paths else []
            full = sum(e["full_bytes"] for e in res)
            dd = sum(e["dedup_bytes"] for e in res)
            k = 3600 / a.seconds
            rows.append({"kind": kind, "idle": idle, "episodes": len(res), "full_mb_h": full * k / 1e6,
                         "dedup_mb_h": dd * k / 1e6, "full_tiles": sum(e["full_sent"] for e in res),
                         "dedup_tiles": sum(e["dedup_sent"] for e in res), "kept": sum(e["dedup_kept"] for e in res)})
            r = rows[-1]
            print(f"| {kind} | {idle} | {r['episodes']} | {r['full_tiles']} | {r['dedup_tiles']} (+{r['kept']} keep) | "
                  f"{r['full_mb_h']:.1f} | {r['dedup_mb_h']:.2f} | "
                  f"{(1 - r['dedup_mb_h'] / r['full_mb_h']) * 100 if r['full_mb_h'] else 0:.1f}% |", flush=True)
    print(json.dumps(rows))


if __name__ == "__main__":
    main()
