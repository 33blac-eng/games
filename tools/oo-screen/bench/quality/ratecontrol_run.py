#!/usr/bin/env python3
"""bench/quality/ratecontrol_run.py — TASK.md крок 4: політики rate control енкодера агента (СИМУЛЯЦІЯ).

Один безперервний енкод x264 (bench/quality/x264rc, veryfast zerolatency main, ABR + VBV 1.5×/0.5 с — як
PCVBR агента) на кожен варіант; кадр за кадром ним керує дзеркало кадрового циклу агента:
  * незмінений кадр не кодується (DXGI no-op), keepalive — повтор останнього кадру раз на 1 с;
  * refine (internal/refine): через 200 мс нерухомості P-кадр QP22, потім QP18, пауза bytes/peak;
  * keyframe_request (новий глядач / PLI) — IDR на наступному відправленому кадрі;
  * періодичний IDR — GOP енкодера в ЗАКОДОВАНИХ кадрах (MFT: -gop-seconds × fps = 300).
Варіанти додають по одній політиці (див. VARIANTS). Це x264, а не MS MFT; навантаження синтетичне
(workloads.py). Цифри — для порівняння варіантів між собою, не абсолютні.

Метрики: середній/макс. 1-с бітрейт, макс. кадр, затримка черги на лінку = піковий бітрейт (1.5× ціль;
проксі «спайку» від IDR), PSNR/SSIM/text edge-SSIM показаного декодером кадру (кожен 30-й кадр), окремо
для «нерухомих» зразків (екран стоїть ≥ 1 с — те, що людина читає).
"""
import argparse, json, math, os, subprocess, tempfile
from concurrent.futures import ProcessPoolExecutor
import numpy as np
import metrics as M
import pipeline as P
import workloads as WL

HERE = os.path.dirname(os.path.abspath(__file__))
FPS = WL.FPS
REFINE_QPS = (22, 18)
REFINE_IDLE = 6          # 200 мс
KEEPALIVE = FPS          # 1 с
GOP = 10 * FPS           # -gop-seconds 10
SAMPLE = 30
WARMUP = 2 * FPS
IDLE_IDR = FPS // 2      # internal/keyframe DefaultIdle 500 мс
JOINS_S = (15.5, 33.2, 47.7)  # keyframe_request: нові глядачі

# Кожен варіант — базова поведінка агента + перелічені політики.
VARIANTS = {
    "base": {},
    "rekey": {"rekey": True},
    "rekey+qa": {"rekey": True, "qaware": True},
    "idleidr": {"rekey": True, "qaware": True, "idle_idr": True},
    "ir": {"ir": 1},
    "qpmax40": {"qpmax": 40},
    "qpmax36": {"qpmax": 36},
    "qpmin16": {"qpmin": 16},
    # C3: top-off до збіжності (refine.Config.Converge, OO_SCREEN_REFINE_CONVERGE): після 22/18 ще
    # refine-кадри з TARGET_QP, доки ВИМІРЯНИЙ QP гірший, ≤ CONV_EXTRA кадрів і ≤ 1 с бітрейту байтів.
    "conv": {"rekey": True, "conv": True},
    # MFT, що ігнорує QP семпла (refine кодує rate control) — без і з Converge.
    "ignqp": {"rekey": True, "ignqp": True},
    "ignqp+conv": {"rekey": True, "ignqp": True, "conv": True},
    # C3: правило великого кадру (OO_SCREEN_LARGE_FRAME_QP=32) + Converge.
    "large32+conv": {"rekey": True, "conv": True, "large": 32},
}
TARGET_QP = 16           # refine.DefaultTargetQP
CONV_EXTRA = 4           # refine.DefaultMaxExtra


def build(out_dir):
    out = os.path.join(out_dir, "x264rc")
    subprocess.run(["gcc", "-O2", "-o", out, os.path.join(HERE, "x264rc", "x264rc.c"), "-lx264"], check=True)
    return out


def to_yuv(rgb):
    y, cb, cr = P.rgb_to_yuv(rgb)
    return P.q8(y).tobytes() + P.q8(P.sub420(cb)).tobytes() + P.q8(P.sub420(cr)).tobytes()


class Agent:
    """Дзеркало політик кадрового циклу агента (Go: internal/refine, internal/keyframe, main.go)."""

    def __init__(self, v):
        self.v = v
        self.armed, self.done, self.next = False, 0, 0
        self.worst = 0           # гірший QP, що лишився на екрані після IDR/refine (0 — невідомо)
        self.since_idr = 0       # закодованих кадрів від останнього IDR
        self.want_idr = False
        self.last_change = -10 ** 9
        self.bytes = 0           # байтів refine за епізод (Converge)
        self.budget = 0          # бюджет епізоду, байтів (1 с бітрейту)

    def motion(self, i):
        self.armed, self.done, self.next = True, 0, i + REFINE_IDLE
        self.last_change = i
        self.bytes = 0

    def converging(self):
        return (self.v.get("conv") and self.done >= len(REFINE_QPS)
                and self.done < len(REFINE_QPS) + CONV_EXTRA
                and self.worst > TARGET_QP and self.bytes < self.budget)

    def refine_due(self, i):
        while self.armed and self.done < len(REFINE_QPS) and i >= self.next:
            qp = REFINE_QPS[self.done]
            if self.v.get("qaware") and self.worst > 0 and qp >= self.worst:
                self.done += 1      # крок нічого не покращить — пропускаємо, не витрачаючи кадр
                continue
            return qp
        if self.armed and i >= self.next and self.converging():
            return TARGET_QP
        if self.done >= len(REFINE_QPS) and not self.converging():
            self.armed = False
        return 0

    def idle_idr_due(self, i):
        # internal/keyframe: GOP вийшов, кадр — keepalive, екран стоїть ≥ Idle (500 мс)
        return self.v.get("idle_idr") and self.since_idr >= GOP and i - self.last_change >= IDLE_IDR

    def coded(self, i, typ, qp, refine_qp, nbytes, peak_bps, motion):
        self.since_idr = 0 if typ == "I" else self.since_idr + 1
        if typ == "I":
            self.worst = qp
            if self.v.get("rekey") and refine_qp == 0:
                # Ключовий кадр перезаписав дошліфований екран якістю rate control — refine знову.
                self.armed, self.done, self.next = True, 0, i + REFINE_IDLE
                self.bytes = 0
        elif refine_qp:
            # як Go: QP з потоку важить більше за запитаний (MFT міг його проігнорувати)
            self.worst = qp if self.worst == 0 else min(self.worst, qp)
        elif motion and self.worst > 0:
            self.worst = max(self.worst, qp)
        # keepalive (повтор без змін) — суцільний P-skip, якість екрана не міняє
        if refine_qp:
            self.done += 1
            gap = math.ceil(nbytes * 8 / peak_bps * FPS) if peak_bps else 1
            self.next = i + max(1, gap)
            self.bytes += nbytes
            if self.done >= len(REFINE_QPS) and not self.converging():
                self.armed = False


def scan(kind, n, corpus):
    wl = WL.make(kind, WL.Sources(corpus))
    changed, frac, prev = [], [], None
    for i in range(n):
        f = wl.frame(i)
        if prev is None:
            changed.append(True); frac.append(1.0)
        else:
            d = np.any(f != prev, axis=2)
            changed.append(bool(d.any())); frac.append(float(d.mean()))
        prev = f
    return changed, frac


def run_variant(args):
    name, v, kind, kbps, n, corpus, tool = args[:7]
    joins_s = args[7] if len(args) > 7 else JOINS_S
    wl = WL.make(kind, WL.Sources(corpus))
    changed, changed_frac = scan(kind, n, corpus)
    td = tempfile.mkdtemp()
    dump = os.path.join(td, "d.yuv")
    opts = [f"keyint={2 * GOP if v.get('idle_idr') else GOP}"]
    for k in ("qpmin", "qpmax", "ir", "aq"):
        if k in v:
            opts.append(f"{k}={v[k]}")
    p = subprocess.Popen([tool, str(WL.W), str(WL.H), str(FPS), os.path.join(td, "o.h264"), dump] + opts,
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    a = Agent(v)
    a.budget = kbps * 1000 // 8
    peak = kbps * 1500
    joins = {int(t * FPS) for t in joins_s if t * FPS < n}
    last_sent, have_pix = -10 ** 9, False
    sizes, types, qps, still_samples = [0] * n, ["-"] * n, [0] * n, []
    samples = list(range(SAMPLE // 2, n, SAMPLE))
    for i in range(n):
        if i in joins:
            a.want_idr = True
        send, newpix, rq, idr = 0, 0, 0, 0
        if changed[i]:
            a.motion(i)
            send, newpix = 1, 1
        else:
            rq = a.refine_due(i)
            if rq:
                send = 1
            elif i - last_sent >= KEEPALIVE:
                send = 1
                idr = int(bool(a.idle_idr_due(i)))
        if send and a.want_idr:
            idr, a.want_idr = 1, False
        if newpix:
            frame_bytes = to_yuv(wl.frame(i))
        dump_it = int(i in samples)
        enc_q = 0 if v.get("ignqp") else rq  # ignqp: QP семпла проігноровано, кодує rate control
        if newpix and v.get("large"):
            # правило великого кадру (refine.LargeFrameMinQP): частка зміни x HRD на змінений піксель
            frac = changed_frac[i]
            if frac >= 0.5 and (kbps * 1000 / 2) / (frac * WL.W * WL.H) < 1.0:
                # x264 не має MinQP на кадр: наближення — примусовий QP правила (дешевий кадр).
                enc_q = v["large"]
        hdr = f"{kbps} {newpix} {send} {idr} {enc_q} {dump_it}\n".encode()
        p.stdin.write(hdr + (frame_bytes if newpix else b""))
        p.stdin.flush()
        nb, typ, qp = p.stdout.readline().decode().split()
        nb, qp = int(nb), int(qp)
        if send:
            last_sent = i
            sizes[i], types[i], qps[i] = nb, typ, qp
            a.coded(i, typ, qp, rq, nb, peak, changed[i])
        if dump_it:
            still_samples.append(i - a.last_change >= FPS)
    p.stdin.close(); p.wait()
    # якість
    fs = WL.W * WL.H * 3 // 2
    q = []
    with open(dump, "rb") as f:
        for i in samples:
            buf = f.read(fs)
            yv, u, vv = split(buf)
            out = P.yuv_to_rgb(yv.astype(np.float64), P.up420(u.astype(np.float64), WL.H, WL.W),
                               P.up420(vv.astype(np.float64), WL.H, WL.W))
            ref = wl.frame(i)
            es, _ = M.text_sharpness(ref, out)
            q.append((M.psnr(ref, out), M.ssim(ref, out), es))
    q = np.array(q)
    st = np.array(still_samples)
    # бітрейт і черга
    per = np.array(sizes, dtype=float)
    sec = per[WARMUP:n - n % FPS].reshape(-1, FPS).sum(1) * 8 / 1e6
    backlog, delays = 0.0, []
    for i in range(n):
        backlog = max(0.0, backlog - peak / FPS)
        if sizes[i]:
            backlog += sizes[i] * 8
            if i >= WARMUP:  # стартовий IDR однаковий для всіх варіантів
                delays.append(backlog / peak * 1000)
    idrs = sum(1 for t in types if t == "I")
    motion_qp = [qp for qp, t, c in zip(qps, types, changed) if t == "P" and c]
    res = {
        "variant": name, "kind": kind, "kbps": kbps,
        "avg_mbps": float(per.sum() * 8 / (n / FPS) / 1e6), "max1s_mbps": float(sec.max()),
        "max_frame_kb": float(per.max() / 1000), "idr": idrs,
        "sent": int(sum(1 for t in types if t != "-")),
        "delay_p99_ms": WL.pctl(delays, 99), "delay_max_ms": float(max(delays)),
        "late_frames": int(sum(1 for d in delays if d > 100)),
        "motion_qp_mean": float(np.mean(motion_qp)) if motion_qp else float("nan"),
        "motion_qp_p95": WL.pctl(motion_qp, 95) if motion_qp else float("nan"),
        "psnr": float(q[:, 0].mean()), "psnr_p5": WL.pctl(q[:, 0], 5),
        "ssim": float(q[:, 1].mean()), "edge": float(q[:, 2].mean()), "edge_p5": WL.pctl(q[:, 2], 5),
        "motion_psnr_p5": WL.pctl(q[~st, 0], 5) if (~st).any() else float("nan"),
        "motion_edge_p5": WL.pctl(q[~st, 2], 5) if (~st).any() else float("nan"),
        "still_n": int(st.sum()),
        "still_psnr": float(q[st, 0].mean()) if st.any() else float("nan"),
        "still_psnr_min": float(q[st, 0].min()) if st.any() else float("nan"),
        "still_edge": float(q[st, 2].mean()) if st.any() else float("nan"),
        "still_edge_min": float(q[st, 2].min()) if st.any() else float("nan"),
    }
    res["samples"] = [(i, bool(x), round(float(r[0]), 2), round(float(r[2]), 4)) for i, x, r in zip(samples, st, q)]
    res["frames"] = [(i, types[i], qps[i], sizes[i]) for i in range(n) if types[i] != "-" and (types[i] == "I" or not changed[i])]
    os.remove(dump)
    return res


def split(buf, w=WL.W, h=WL.H):
    a = np.frombuffer(buf, np.uint8)
    cw, ch = w // 2, h // 2
    return a[:w * h].reshape(h, w), a[w * h:w * h + cw * ch].reshape(ch, cw), a[w * h + cw * ch:].reshape(ch, cw)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=WL.CORPUS)
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--kinds", default="mixed")
    ap.add_argument("--rates", default="8000,2000")
    ap.add_argument("--variants", default=",".join(VARIANTS))
    ap.add_argument("--jobs", type=int, default=os.cpu_count() or 2)
    ap.add_argument("--json", default="")
    ap.add_argument("--joins", default=",".join(map(str, JOINS_S)),
                    help="секунди keyframe_request нових глядачів через кому; порожньо — без них")
    a = ap.parse_args()
    tool = build(tempfile.mkdtemp())
    n = a.seconds * FPS
    joins = tuple(float(x) for x in a.joins.split(",") if x)
    jobs = [(name, VARIANTS[name], k, int(r), n, a.corpus, tool, joins)
            for k in a.kinds.split(",") for r in a.rates.split(",") for name in a.variants.split(",")]
    with ProcessPoolExecutor(a.jobs) as ex:
        res = list(ex.map(run_variant, jobs))
    if a.json:
        with open(a.json, "w") as f:
            json.dump(res, f, indent=1)
    cols = ["variant", "kind", "kbps", "avg_mbps", "max1s_mbps", "max_frame_kb", "idr", "sent", "delay_p99_ms",
            "delay_max_ms", "late_frames", "motion_qp_mean", "psnr", "psnr_p5", "ssim", "edge", "edge_p5", "motion_psnr_p5", "motion_edge_p5",
            "still_n", "still_psnr", "still_psnr_min", "still_edge", "still_edge_min"]
    print("| " + " | ".join(cols) + " |")
    print("|" + "---|" * len(cols))
    for r in res:
        r.pop("samples", None), r.pop("frames", None)
        print("| " + " | ".join(f"{r[c]:.4g}" if isinstance(r[c], float) else str(r[c]) for c in cols) + " |")


if __name__ == "__main__":
    main()
