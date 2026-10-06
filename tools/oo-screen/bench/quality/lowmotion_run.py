#!/usr/bin/env python3
"""bench/quality/lowmotion_run.py — R3: low-motion bitrate cap (agent -lowmotion-cap) on the office-hour workload.

SIMULATION (x264, not MS MFT). The workload is encoded once per rate (8/4/2 Mbit/s) by workloads_run.simulate;
per frame the policy (mirror of internal/contentmode: video detector + Capper, same defaults) picks a multiplier
1 / 0.5 / 0.25 of the 8M target and the frame's bytes (and sampled quality) are taken from the encode at the
matching rate (8M / 4M / 2M). Approximation: a real encoder switching rate mid-stream keeps its own reference
and VBV state; the transient around a switch is NOT modelled. Writes RESULTS-lowmotion.md.
Policy inputs come from the frames only (block-diff area), never from the workload labels."""
import argparse, json, os, subprocess, tempfile
import numpy as np
import workloads as WL
import workloads_run as R

HERE = os.path.dirname(os.path.abspath(__file__))
FPS = WL.FPS

# internal/contentmode defaults
ENTER_AREA, EXIT_AREA, ENTER_HOLD, EXIT_HOLD, MAX_GAP = 0.10, 0.04, 1.0, 1.5, 0.25
FULL_AREA, LOW_FRAC, VIDEO_FRAC, LOW_HOLD = 0.25, 0.25, 0.5, 0.5
RATE_OF = {1.0: 8000, VIDEO_FRAC: 4000, LOW_FRAC: 2000}


class Video:
    """Mirror of contentmode.Machine's video part (Update on changed frames, Tick on unchanged ones)."""

    def __init__(self):
        self.video, self.streak, self.last = False, None, None

    def update(self, a, t):
        if self.video:
            if a >= EXIT_AREA:
                self.last = t
        elif a >= ENTER_AREA:
            if self.streak is None or t - self.last > MAX_GAP:
                self.streak = t
            self.last = t
            if t - self.streak >= ENTER_HOLD:
                self.video, self.last = True, t
        else:
            self.streak = None
        return self.settle(t)

    def tick(self, t):
        if not self.video and self.streak is not None and t - self.last > MAX_GAP:
            self.streak = None
        return self.settle(t)

    def settle(self, t):
        if self.video and t - self.last >= EXIT_HOLD:
            self.video, self.streak = False, None
        return self.video


class Capper:
    """Mirror of contentmode.Capper."""

    def __init__(self):
        self.frac, self.low_since = 1.0, None

    def update(self, video, a, t):
        if a >= FULL_AREA:
            self.frac, self.low_since = 1.0, None
            return self.frac
        want = VIDEO_FRAC if video else LOW_FRAC
        if self.low_since is None:
            self.low_since = t
        if want > self.frac or (want < self.frac and t - self.low_since >= LOW_HOLD):
            self.frac = want
        return self.frac


def policy(area, changed):
    v, c, fr = Video(), Capper(), []
    for i, (a, ch) in enumerate(zip(area, changed)):
        t = i / FPS
        if ch:
            fr.append(c.update(v.update(a, t), a, t))
        else:
            v.tick(t)
            fr.append(c.frac)
    return fr


def compose(d, fracs, v="tiles"):
    n = d["frames"]
    per = np.array([d["rates"][str(RATE_OF[f])]["per"][v][i] for i, f in enumerate(fracs)])
    sec = per[:n - n % FPS].reshape(-1, FPS).sum(1) * 8 / 1e6
    q = {}
    for k in ("base", "tiles"):
        rows = [next(r for r in d["rates"][str(RATE_OF[fracs[r0[0]]])]["qrows"][k] if r[0] == r0[0])
                for r0 in d["rates"]["8000"]["qrows"][k]]
        a = np.array([r[1:] for r in rows])
        a[np.isinf(a)] = 99.0
        q[k] = {nm: {"mean": float(np.nanmean(a[:, j])), "p5": float(np.nanpercentile(a[:, j], 5))}
                for j, nm in enumerate(["psnr", "ssim", "es"])}
    return {"avg_mbps": float(per.sum() * 8 / (n / FPS) / 1e6), "p95_mbps": WL.pctl(sec, 95),
            "max_mbps": float(sec.max()), "mb_hour": float(per.sum() / (n / FPS) * 3600 / 1e6), "quality": q}


def kind_quality(d, kbps):
    return d["rates"][str(kbps)]["quality"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=WL.CORPUS)
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--step", type=int, default=45)
    ap.add_argument("--json", default=os.path.join(HERE, "lowmotion-results.json"))
    ap.add_argument("--out", default=os.path.join(HERE, "RESULTS-lowmotion.md"))
    a = ap.parse_args()
    n = a.seconds * FPS
    tilesel = os.path.join(tempfile.mkdtemp(), "tilesel")
    subprocess.run(["go", "build", "-o", tilesel, "./bench/quality/tilesel"], cwd=R.ROOT, check=True)
    d = R.simulate("mixed", n, a.step, tilesel, a.corpus, per_frame=True)
    fr = policy(d["area"], d["changed_flags"])
    sched = WL.mixed_schedule(n)
    share = {}
    for k, st, ln in sched:
        for i in range(st, st + ln):
            share.setdefault(k, []).append(fr[i])
    res = {"frames": n, "time_share": {str(f): fr.count(f) / n for f in RATE_OF},
           "per_kind_frac": {k: {str(f): v.count(f) / len(v) for f in RATE_OF} for k, v in share.items()},
           "switches": sum(1 for i in range(1, n) if fr[i] != fr[i - 1]),
           "baseline": {v: {x: d["rates"]["8000"][v][x] for x in ("avg_mbps", "p95_mbps", "max_mbps", "mb_hour")}
                        for v in ("skip", "refine", "tiles")},
           "capped": {v: compose(d, fr, v) for v in ("skip", "refine", "tiles")},
           "baseline_quality": d["rates"]["8000"]["quality"]}
    with open(a.json, "w") as f:
        json.dump(res, f, indent=1)
    write_md(res, a.out)
    print(json.dumps({v: round(res["capped"][v]["mb_hour"]) for v in res["capped"]}), "wrote", a.out)


def write_md(r, path):
    f2 = lambda x, k=2: f"{x:.{k}f}"
    L = ["# bench/quality — стеля бітрейту для малорухомого вмісту, R3 (СИМУЛЯЦІЯ)", "",
         "> **Симуляція, не вимір на реальному ПК і не MS MFT.** Змішана офісна година з `workloads.py` (60 с, "
         "1080p30; простій 45 %, набір 25 %, відео в куті 15 %, скрол 10 %, перетягування 5 %), x264 як у "
         "`RESULTS-workloads.md`. Політика (`internal/contentmode.Capper`, прапорець агента `-lowmotion-cap`, "
         "типово ВИМКНЕНО) бачить лише кадри (частка змінених блоків 16×16), не мітки сценарію. Байти кадру беруться "
         "з енкоду на тій ставці, яку обрала політика (8M / 4M / 2M); перехідний процес енкодера при зміні "
         "ставки посеред потоку **не моделюється**. Відтворення: `python3 bench/quality/lowmotion_run.py`.", "",
         "## Політика", "",
         "- рух кадру ≥ 25 % екрана → повна ціль (8M) миттєво;",
         "- режим Video (≥ 10 % 1 с) і рух < 25 % → 50 % (4M);",
         "- інакше → 25 % (2M), після 500 мс безперервно малих кадрів; не нижче 1 Мбіт/с.", "",
         f"Частка часу: 8M {f2(r['time_share']['1.0']*100,1)} %, 4M {f2(r['time_share']['0.5']*100,1)} %, "
         f"2M {f2(r['time_share']['0.25']*100,1)} %; перемикань ставки за 60 с: {r['switches']}.", "",
         "| сегмент | 8M | 4M | 2M |", "|---|---|---|---|"]
    for k, v in r["per_kind_frac"].items():
        L.append(f"| {k} | {f2(v['1.0']*100,0)} % | {f2(v['0.5']*100,0)} % | {f2(v['0.25']*100,0)} % |")
    L += ["", "## Трафік (змішана офісна година)", "",
          "| варіант | без стелі МБ/год | зі стелею МБ/год | сер. Мбіт/с | p95 1с | max 1с |", "|---|---|---|---|---|---|"]
    for v, nm in (("skip", "skip"), ("refine", "skip+refine"), ("tiles", "skip+refine+тайли")):
        b, c = r["baseline"][v], r["capped"][v]
        L.append(f"| {nm} | {f2(b['mb_hour'],0)} | **{f2(c['mb_hour'],0)}** | {f2(b['avg_mbps'],3)} → "
                 f"{f2(c['avg_mbps'],3)} | {f2(b['p95_mbps'])} → {f2(c['p95_mbps'])} | {f2(b['max_mbps'])} → "
                 f"{f2(c['max_mbps'])} |")
    L += ["", "## Якість на зразках (mean / p5)", "",
          "| варіант | PSNR dB | SSIM | text edge-SSIM |", "|---|---|---|---|"]
    for nm, q in (("без стелі, кадри енкодера", r["baseline_quality"]["base"]),
                  ("зі стелею, кадри енкодера", r["capped"]["tiles"]["quality"]["base"]),
                  ("без стелі, +refine+тайли", r["baseline_quality"]["tiles"]),
                  ("зі стелею, +refine+тайли", r["capped"]["tiles"]["quality"]["tiles"])):
        L.append(f"| {nm} | {f2(q['psnr']['mean'])} / {f2(q['psnr']['p5'])} | {f2(q['ssim']['mean'],4)} / "
                 f"{f2(q['ssim']['p5'],4)} | {f2(q['es']['mean'],4)} / {f2(q['es']['p5'],4)} |")
    with open(path, "w") as f:
        f.write("\n".join(L) + "\n")


if __name__ == "__main__":
    main()
