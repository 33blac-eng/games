#!/usr/bin/env python3
"""bench/quality/lowmotion_live.py — R3: office-hour traffic from ONE continuous x264 encode.

Unlike lowmotion_run.py (bytes spliced from separate 8M/4M/2M encodes; switch transients NOT modelled),
here a single libx264 encoder runs over the whole mixed workload and the rate is changed mid-stream with
x264_encoder_reconfig exactly where the policy (mirror of internal/contentmode, from lowmotion_run.py)
switches, and unchanged frames are not fed to the encoder at all (agent skip). So VBV state, references
and rate-control transients are real x264 behaviour. Still x264, not MS MFT; still a synthetic workload.

Pipeline: workload RGB -> ffmpeg (same VF as workloads_run) -> yuv420p -> x264live (bench/quality/x264live).
Needs libx264-dev to build x264live. Prints JSON; --repeat N re-runs (x264 threads => tiny nondeterminism).
"""
import argparse, json, os, subprocess, tempfile
import numpy as np
import workloads as WL
import workloads_run as R
import lowmotion_run as LM

HERE = os.path.dirname(os.path.abspath(__file__))
FPS = WL.FPS


def build():
    out = os.path.join(tempfile.mkdtemp(), "x264live")
    subprocess.run(["gcc", "-O2", "-o", out, os.path.join(HERE, "x264live", "x264live.c"), "-lx264"], check=True)
    return out


def scan(wl, n):
    area, changed, prev = [], [], None
    for i in range(n):
        f = wl.frame(i)
        area.append(R.block_area(f, prev))
        changed.append(prev is None or not np.array_equal(f, prev))
        prev = f
    return area, changed


def encode(tool, wl, n, sched):
    td = tempfile.mkdtemp()
    sp = os.path.join(td, "sched.txt")
    with open(sp, "w") as f:
        for k, s in sched:
            f.write(f"{k} {int(s)}\n")
    ff = subprocess.Popen(["ffmpeg", "-v", "error", "-f", "rawvideo", "-pix_fmt", "rgb24", "-s", f"{WL.W}x{WL.H}",
                           "-r", str(FPS), "-i", "-", "-vf", R.VF, "-f", "rawvideo", "-pix_fmt", "yuv420p", "-"],
                          stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    enc = subprocess.Popen([tool, str(WL.W), str(WL.H), str(FPS), sp], stdin=ff.stdout, stdout=subprocess.PIPE,
                           text=True)
    ff.stdout.close()
    for i in range(n):
        ff.stdin.write(np.ascontiguousarray(wl.frame(i)).tobytes())
    ff.stdin.close()
    sizes = [int(x) for x in enc.stdout.read().split()]
    ff.wait(); enc.wait()
    assert len(sizes) == n, (len(sizes), n)
    per = np.array(sizes, dtype=float)
    sec = per[:n - n % FPS].reshape(-1, FPS).sum(1) * 8 / 1e6
    return {"mb_hour": float(per.sum() / (n / FPS) * 3600 / 1e6), "avg_mbps": float(per.sum() * 8 / (n / FPS) / 1e6),
            "p95_mbps": WL.pctl(sec, 95), "max_mbps": float(sec.max())}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=WL.CORPUS)
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--repeat", type=int, default=1)
    a = ap.parse_args()
    n = a.seconds * FPS
    tool = build()
    wl = WL.make("mixed", WL.Sources(a.corpus))
    area, changed = scan(wl, n)
    fr = LM.policy(area, changed)
    full = [(LM.RATE_OF[1.0], c) for c in changed]
    capped = [(LM.RATE_OF[f], c) for f, c in zip(fr, changed)]
    res = {"frames": n, "changed": sum(changed), "switches": sum(1 for i in range(1, n) if fr[i] != fr[i - 1]),
           "runs": []}
    for _ in range(a.repeat):
        res["runs"].append({"uncapped": encode(tool, wl, n, full), "capped": encode(tool, wl, n, capped)})
    print(json.dumps(res, indent=1))


if __name__ == "__main__":
    main()
