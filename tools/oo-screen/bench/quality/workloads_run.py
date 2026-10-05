#!/usr/bin/env python3
"""bench/quality/workloads_run.py — workload simulation (SIMULATION, x264 not MS MFT), writes RESULTS-workloads.md.

Per workload (workloads.py, 1920x1080, 30 fps, 60 s) and per rate (8/4/2 Mbit/s, PCVBR approx: -b:v mean,
-maxrate 1.5x mean, -bufsize 0.5 s of max) the sequence is encoded with x264 main 4:2:0 veryfast zerolatency.
Variants derived from that one encode:
  noskip  : every captured frame is encoded and sent
  skip    : unchanged frames (no-op) are not sent (their x264 output is a P-skip anyway, the reference is
            identical, so bytes = encoded size of changed frames only)
  refine  : skip + refine P-frames QP22 (after 200 ms still) and QP18 (after bytes/peak gap), cost/quality from a
            2-frame x264 -qp N encode [decoded state, original]
  tiles   : refine + lossless text tiles (internal/tiles.Select via Go helper tilesel, PNG bytes) once the
            refine is complete
Plus CPU proxy: x264 veryfast encode wall time of the first 600 frames on 1/2/4 threads."""
import argparse, hashlib, io, json, math, os, re, subprocess, tempfile, time
from concurrent.futures import ProcessPoolExecutor
import numpy as np
from PIL import Image
import metrics as M
import pipeline as P
import workloads as WL

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
RATES = [8000, 4000, 2000]
VARS = ["noskip", "skip", "refine", "tiles"]
VF = "scale=out_color_matrix=bt709:out_range=tv:flags=bicubic,format=yuv420p"


def rc_args(kbps):
    mx = kbps * 3 // 2
    return ["-b:v", f"{kbps}k", "-maxrate", f"{mx}k", "-bufsize", f"{mx // 2}k"]


def enc_cmd(out, kbps, threads=None, src_fmt="rgb24", vf=True):
    return (["ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", src_fmt, "-s", f"{WL.W}x{WL.H}",
             "-r", str(WL.FPS), "-i", "-"] + (["-vf", VF] if vf else []) +
            ["-c:v", "libx264", "-profile:v", "main", "-preset", "veryfast", "-tune", "zerolatency",
             *rc_args(kbps), "-g", "100000", "-pix_fmt", "yuv420p", "-color_range", "tv", "-colorspace", "bt709"] +
            (["-threads", str(threads)] if threads else []) + [out])


def packet_sizes(path):
    out = subprocess.run(["ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "packet=size",
                          "-of", "csv=p=0", path], capture_output=True, text=True, check=True).stdout
    return [int(x) for x in out.split()]


def yuv_rgb(y, u, v):
    h, w = y.shape
    return P.yuv_to_rgb(y.astype(np.float64), P.up420(u.astype(np.float64), h, w), P.up420(v.astype(np.float64), h, w))


def split(buf, w=WL.W, h=WL.H):
    a = np.frombuffer(buf, np.uint8)
    cw, ch = w // 2, h // 2
    return a[:w * h].reshape(h, w), a[w * h:w * h + cw * ch].reshape(ch, cw), a[w * h + cw * ch:].reshape(ch, cw)


def rgb_yuv420(rgb):
    y, cb, cr = P.rgb_to_yuv(rgb)
    return P.q8(y), P.q8(P.sub420(cb)), P.q8(P.sub420(cr))


def refine_pass(prev_yuv, orig_rgb, qp):
    """Encode [prev decoded state, original] at -qp; return (bytes of 2nd (P) frame, decoded 2nd frame yuv)."""
    o = rgb_yuv420(orig_rgb)
    raw = b"".join(p.tobytes() for p in prev_yuv) + b"".join(p.tobytes() for p in o)
    with tempfile.TemporaryDirectory() as td:
        enc = os.path.join(td, "r.h264")
        subprocess.run(["ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", "yuv420p", "-s",
                        f"{WL.W}x{WL.H}", "-r", "30", "-i", "-", "-c:v", "libx264", "-profile:v", "main",
                        "-preset", "veryfast", "-tune", "zerolatency", "-qp", str(qp), "-g", "1000", enc],
                       input=raw, check=True)
        sz = packet_sizes(enc)[1]
        dec = subprocess.run(["ffmpeg", "-v", "error", "-i", enc, "-f", "rawvideo", "-pix_fmt", "yuv420p", "-"],
                             capture_output=True, check=True).stdout
    fs = WL.W * WL.H * 3 // 2
    return sz, split(dec[fs:2 * fs])


def tiles_for(rgb, tilesel):
    with tempfile.TemporaryDirectory() as td:
        p = os.path.join(td, "f.png")
        Image.fromarray(rgb).save(p)
        rects = json.loads(subprocess.run([tilesel, p], capture_output=True, text=True, check=True).stdout)
    bits = 0
    for r in rects:
        b = io.BytesIO()
        Image.fromarray(rgb[r["Y"]:r["Y"] + r["H"], r["X"]:r["X"] + r["W"]]).save(b, "PNG", optimize=True)
        bits += len(b.getvalue())
    return rects, bits


def qual(ref, out, mask):
    es, _ = M.text_sharpness(ref, out, mask)
    return [M.psnr(ref, out), M.ssim(ref, out), es]


def block_area(f, prev, b=16):
    """Fraction of b×b blocks that differ from prev (stand-in for DXGI dirty+move area)."""
    if prev is None:
        return 1.0
    h, w = f.shape[0] // b * b, f.shape[1] // b * b
    d = np.any(f[:h, :w] != prev[:h, :w], axis=2).reshape(h // b, b, w // b, b).any(axis=(1, 3))
    return float(d.mean())


def simulate(kind, n, sample_step, tilesel, corpus, per_frame=False):
    s = WL.Sources(corpus)
    wl = WL.make(kind, s)
    samples = set(range(sample_step // 4, n, sample_step))
    td = tempfile.mkdtemp()
    encs = {k: subprocess.Popen(enc_cmd(os.path.join(td, f"{k}.h264"), k), stdin=subprocess.PIPE) for k in RATES}
    changed, prev, keep, last, area = [], None, {}, None, []
    for i in range(n):
        f = wl.frame(i)
        if per_frame:
            area.append(block_area(f, prev))
        changed.append(prev is None or not np.array_equal(f, prev))
        prev = f
        if changed[-1]:
            if last is not None and i - last[0] > 6:
                keep[last[0]] = last[1]
            last = (i, f)
        b = np.ascontiguousarray(f).tobytes()
        for p in encs.values():
            p.stdin.write(b)
        if i in samples:
            keep[i] = f
    for p in encs.values():
        p.stdin.close(); p.wait()
    runs = WL.still_runs(changed)
    # frames needed: samples + start of runs long enough for refine
    ref_runs = [(a, e) for a, e in runs if e - a > 6]
    need_orig = {a for a, _ in ref_runs}
    if last is not None and n - last[0] > 6:
        keep[last[0]] = last[1]
    run_of = {}
    for a, e in runs:
        for i in samples:
            if a <= i < e:
                run_of[i] = (a, e)
    nchanged = sum(changed)
    res = {"kind": kind, "frames": n, "changed": nchanged, "runs_refine": len(ref_runs), "rates": {}}
    tile_cache = {}
    for kbps in RATES:
        enc = os.path.join(td, f"{kbps}.h264")
        sizes = packet_sizes(enc)
        assert len(sizes) == n, (len(sizes), n)
        need_dec = set(samples) | {a + 5 for a, _ in ref_runs}
        dec = subprocess.Popen(["ffmpeg", "-v", "error", "-i", enc, "-f", "rawvideo", "-pix_fmt", "yuv420p", "-"],
                               stdout=subprocess.PIPE)
        fs = WL.W * WL.H * 3 // 2
        recon = {}
        for i in range(n):
            buf = dec.stdout.read(fs)
            if i in need_dec:
                recon[i] = [p.copy() for p in split(buf)]
        dec.wait()
        mx_bps = kbps * 1500
        # refine events per run
        ev = {}  # run start -> dict
        for a, e in ref_runs:
            b22, y22 = refine_pass(recon[a + 5], keep[a], 22)
            gap = math.ceil(b22 * 8 / mx_bps * WL.FPS)
            idx = WL.refine_frames(a, e, 6, (gap,))
            d = {"idx": idx, "bytes": [b22], "yuv": [y22]}
            if len(idx) > 1:
                b18, y18 = refine_pass(y22, keep[a], 18)
                d["bytes"].append(b18); d["yuv"].append(y18)
                if idx[1] + 1 < e:
                    h = hashlib.sha1(keep[a].tobytes()).hexdigest()
                    if h not in tile_cache:
                        tile_cache[h] = tiles_for(keep[a], tilesel)
                    d["tiles_at"] = idx[1] + 1
                    d["tiles"] = tile_cache[h]
            ev[a] = d
        # per-frame sent bytes per variant
        per = {v: np.zeros(n) for v in VARS}
        sent = {v: [] for v in VARS}
        for i, sz in enumerate(sizes):
            per["noskip"][i] += sz; sent["noskip"].append(sz)
            if changed[i]:
                for v in ("skip", "refine", "tiles"):
                    per[v][i] += sz; sent[v].append(sz)
        tile_bytes_total = 0
        for a, d in ev.items():
            for j, t in enumerate(d["idx"][:len(d["bytes"])]):
                for v in ("refine", "tiles"):
                    per[v][t] += d["bytes"][j]; sent[v].append(d["bytes"][j])
            if "tiles_at" in d:
                per["tiles"][d["tiles_at"]] += d["tiles"][1]; sent["tiles"].append(d["tiles"][1])
                tile_bytes_total += d["tiles"][1]
        out = {}
        for v in VARS:
            sec = per[v][:n - n % WL.FPS].reshape(-1, WL.FPS).sum(1) * 8 / 1e6
            out[v] = {"avg_mbps": float(per[v].sum() * 8 / (n / WL.FPS) / 1e6), "p95_mbps": WL.pctl(sec, 95),
                      "max_mbps": float(sec.max()), "mb_hour": float(per[v].sum() / (n / WL.FPS) * 3600 / 1e6),
                      "sent_frames": len(sent[v]), "f50": WL.pctl(sent[v], 50) / 1e3,
                      "f95": WL.pctl(sent[v], 95) / 1e3, "fmax": float(max(sent[v])) / 1e3}
        # quality on sampled frames
        q = {"base": [], "refine": [], "tiles": []}
        for i in sorted(samples):
            ref = keep[i]
            mask = M.text_mask(ref)
            base = yuv_rgb(*recon[i])
            qb = qual(ref, base, mask)
            q["base"].append(qb)
            a = run_of.get(i, (None,))[0]
            d = ev.get(a)
            qr, qt = qb, qb
            if d:
                k = sum(1 for t in d["idx"][:len(d["yuv"])] if t <= i)
                if k:
                    rr = yuv_rgb(*d["yuv"][k - 1])
                    qr = qual(ref, rr, mask)
                    qt = qr
                    if "tiles_at" in d and d["tiles_at"] <= i and d["tiles"][0]:
                        qt = qual(ref, P.paste_tiles(rr, ref, d["tiles"][0]), mask)
            q["refine"].append(qr); q["tiles"].append(qt)
        qs = {}
        for k, rows in q.items():
            a = np.array(rows, dtype=np.float64)
            a[np.isinf(a)] = 99.0
            qs[k] = {nm: {"mean": float(np.nanmean(a[:, j])), "p5": float(np.nanpercentile(a[:, j], 5))}
                     for j, nm in enumerate(["psnr", "ssim", "es"])}
        out["quality"] = qs
        if per_frame:
            out["per"] = {v: per[v].tolist() for v in VARS}
            out["qrows"] = {k: [[i] + [float(x) for x in r] for i, r in zip(sorted(samples), rows)]
                             for k, rows in q.items()}
        out["refine_events"] = sum(len(d["bytes"]) for d in ev.values())
        out["tile_kb_total"] = tile_bytes_total / 1e3
        res["rates"][str(kbps)] = out
        print(kind, kbps, json.dumps({v: round(out[v]["avg_mbps"], 3) for v in VARS}), flush=True)
    if per_frame:
        res["area"] = area
        res["changed_flags"] = [bool(c) for c in changed]
    for k in RATES:
        os.remove(os.path.join(td, f"{k}.h264"))
    os.rmdir(td)
    return res


def cpu_proxy(kind, n, corpus, threads=(1, 2, 4), kbps=8000):
    """Encode the first n frames (pre-converted yuv420p on disk) at 8M with -threads t; wall time."""
    s = WL.Sources(corpus)
    wl = WL.make(kind, s)
    td = tempfile.mkdtemp()
    yuv = os.path.join(td, "src.yuv")
    p = subprocess.Popen(["ffmpeg", "-v", "error", "-y", "-f", "rawvideo", "-pix_fmt", "rgb24", "-s",
                          f"{WL.W}x{WL.H}", "-r", "30", "-i", "-", "-vf", VF, "-f", "rawvideo", yuv],
                         stdin=subprocess.PIPE)
    for i in range(n):
        p.stdin.write(np.ascontiguousarray(wl.frame(i)).tobytes())
    p.stdin.close(); p.wait()
    out = {}
    for t in threads:
        cmd = enc_cmd(os.path.join(td, "o.h264"), kbps, threads=t, src_fmt="yuv420p", vf=False)
        cmd[cmd.index("-i") + 1] = yuv
        subprocess.run(["cat", yuv], stdout=subprocess.DEVNULL)  # warm page cache
        t0 = time.perf_counter()
        subprocess.run(cmd, check=True)
        dt = time.perf_counter() - t0
        out[str(t)] = {"ms_per_frame": dt / n * 1e3, "fps": n / dt}
        print("cpu", kind, t, out[str(t)], flush=True)
    os.remove(yuv); os.remove(os.path.join(td, "o.h264")); os.rmdir(td)
    return out


def fmt(x, n=2):
    return "n/a" if x is None or (isinstance(x, float) and math.isnan(x)) else f"{x:.{n}f}"


NAMES = {"idle": "(a) простій, статичний робочий стіл", "typing": "(b) набір у таблиці",
         "scroll": "(c) скрол таблиці/коду 3–10 px/кадр", "drag": "(d) перетягування вікна",
         "video": "(e) відео 640×360 у куті", "mixed": "(f) змішана офісна година"}
VNAMES = {"noskip": "без skip", "skip": "skip", "refine": "skip+refine", "tiles": "skip+refine+тайли"}


def write_md(data, path, n, step):
    L = ["# bench/quality — робочі навантаження (СИМУЛЯЦІЯ)", "",
         "> **Це симуляція, а не вимір на реальному залізі й не вимір агента.** Кадрові послідовності синтетичні "
         "(зібрані з `bench/corpus`, 1920×1080, 30 fps, "
         f"{n // WL.FPS} с кожна), кодер — ffmpeg libx264 (main, 4:2:0, `-preset veryfast -tune zerolatency`) на Linux, "
         "а **не** Media Foundation MFT агента. Rate control агента (PCVBR) наближено x264 ABR+VBV. "
         "Цифри придатні для порівняння навантажень/варіантів між собою, а не як абсолютні. "
         "Відтворення: `python3 bench/quality/workloads_run.py`.", "",
         "## Методика", "",
         "- Навантаження (`workloads.py`): (a) статичний робочий стіл (таблиця); (b) набір у клітинці — новий "
         "символ кожні ~150 мс + блимання курсора 530 мс; (c) безперервний вертикальний скрол 3–10 px/кадр "
         "(таблиця+код); (d) вікно 900×600 тягнеться ~12 px/кадр 2 с, пауза 1 с; (e) відео 640×360 у куті "
         "(рухомі градієнти + дрейфуючий фільтрований шум + зерно) поверх статичного коду; "
         "(f) суміш a–e сегментами 2–6 с: простій 45%, набір 25%, відео 15%, скрол 10%, перетягування 5%.",
         "- Rate control: `-b:v M -maxrate 1.5·M -bufsize 0.5 с·max` (8M: max 12M, VBV 6 Мбіт; 4M: 6M/3 Мбіт; "
         "2M: 3M/1.5 Мбіт), GOP безкінечний (один IDR на початку).",
         "- **без skip** — кодується й шлеться кожен кадр 30 fps. **skip** — незмінені кадри (no-op) не шлються; "
         "байти = розміри змінених кадрів із того самого енкоду (x264 для ідентичного кадру дає P-skip, "
         "референс той самий). **skip+refine** — після 200 мс нерухомості P-кадр QP22, далі QP18 через "
         "bytes/peak (як `internal/refine`); вартість/якість — 2-кадровий енкод `x264 -qp N` "
         "[декодований стан, оригінал]. Вплив refine на rate control наступних кадрів не моделюється. "
         "**+тайли** — після QP18 `internal/tiles.Select` (Go-хелпер `tilesel`) + PNG тайлів, "
         "на кожен період нерухомості (верхня межа, без дедуплікації між періодами).",
         "- Бітрейт: середній за 60 с; p95/max — по 1-секундних вікнах. МБ/год = середній × 3600. "
         "Розмір кадру p50/p95/max — серед реально відправлених кадрів (включно з refine/тайлами).",
         f"- Якість: кожен {step}-й кадр ({n // step} зразків), PSNR (RGB), SSIM (яскравість), "
         "text edge-SSIM (SSIM карт градієнта в масці тексту); mean і p5 (5-й перцентиль = гірші кадри). "
         "Для skip якість = без skip (відправлені кадри ті самі).",
         "- CPU-проксі: x264 veryfast, перші 600 кадрів кожного навантаження з yuv-файлу, `-threads 1/2/4` на "
         f"цьому контейнері ({os.cpu_count()} vCPU Linux). Це **x264, не MS MFT** (апаратний/софт MFT на "
         "слабкому ПК поводиться інакше); з skip енкодер працює лише на змінених кадрах.",
         "- A/V синхронізація: **n/a** (аудіо немає).", ""]
    L += ["## Зведення @8M (PCVBR mean 8 / max 12 Мбіт/с)", "",
          "| навантаження | змінених кадрів | варіант | сер. Мбіт/с | p95 1с | МБ/год | кадр p50/p95/max КБ |",
          "|---|---|---|---|---|---|---|"]
    for d in data:
        r = d["rates"]["8000"]
        for v in VARS:
            x = r[v]
            L.append(f"| {NAMES[d['kind']]} | {d['changed']}/{d['frames']} | {VNAMES[v]} | {fmt(x['avg_mbps'],3)} | "
                     f"{fmt(x['p95_mbps'],2)} | {fmt(x['mb_hour'],0)} | {fmt(x['f50'],1)} / {fmt(x['f95'],1)} / "
                     f"{fmt(x['fmax'],0)} |")
    L += ["", "## Бітрейт і обсяг на всіх рівнях (варіант skip+refine+тайли / без skip)", "",
          "| навантаження | rate | сер. Мбіт/с | p95 1с | max 1с | МБ/год | max кадр КБ | без skip сер. | без skip МБ/год |",
          "|---|---|---|---|---|---|---|---|---|"]
    for d in data:
        for k in RATES:
            r = d["rates"][str(k)]; x = r["tiles"]; z = r["noskip"]
            L.append(f"| {d['kind']} | {k // 1000}M | {fmt(x['avg_mbps'],3)} | {fmt(x['p95_mbps'],2)} | "
                     f"{fmt(x['max_mbps'],2)} | {fmt(x['mb_hour'],0)} | {fmt(x['fmax'],0)} | {fmt(z['avg_mbps'],3)} | "
                     f"{fmt(z['mb_hour'],0)} |")
    L += ["", "## Якість (mean / p5)", "",
          "| навантаження | rate | варіант | PSNR dB | SSIM | text edge-SSIM |", "|---|---|---|---|---|---|"]
    for d in data:
        for k in RATES:
            qq = d["rates"][str(k)]["quality"]
            for v, nm in (("base", "без/з skip"), ("refine", "+refine"), ("tiles", "+refine+тайли")):
                q = qq[v]
                L.append(f"| {d['kind']} | {k // 1000}M | {nm} | {fmt(q['psnr']['mean'])} / {fmt(q['psnr']['p5'])} | "
                         f"{fmt(q['ssim']['mean'],4)} / {fmt(q['ssim']['p5'],4)} | {fmt(q['es']['mean'],4)} / "
                         f"{fmt(q['es']['p5'],4)} |")
    L += ["", "## CPU-проксі: x264 veryfast @8M, 1080p (НЕ MS MFT)", "",
          "| навантаження | 1 потік мс/кадр (fps) | 2 потоки | 4 потоки | частка змінених кадрів | потрібно fps з skip |",
          "|---|---|---|---|---|---|"]
    for d in data:
        c = d.get("cpu", {})
        cell = lambda t: f"{fmt(c[t]['ms_per_frame'],1)} ({fmt(c[t]['fps'],0)})" if t in c else "n/a"
        fr = d["changed"] / d["frames"]
        L.append(f"| {d['kind']} | {cell('1')} | {cell('2')} | {cell('4')} | {fmt(fr*100,1)}% | {fmt(fr*30,1)} |")
    L += ["", "## Події refine / тайли @8M", "", "| навантаження | періодів нерухомості ≥200 мс | refine-кадрів | тайли, КБ сумарно |",
          "|---|---|---|---|"]
    for d in data:
        r = d["rates"]["8000"]
        L.append(f"| {d['kind']} | {d['runs_refine']} | {r['refine_events']} | {fmt(r['tile_kb_total'],0)} |")
    with open(path, "w") as f:
        f.write("\n".join(L) + "\n")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=WL.CORPUS)
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--step", type=int, default=45, help="quality sample every N frames")
    ap.add_argument("--kinds", default=",".join(WL.KINDS))
    ap.add_argument("--cpu-frames", type=int, default=600)
    ap.add_argument("--jobs", type=int, default=3)
    ap.add_argument("--json", default=os.path.join(HERE, "workloads-results.json"))
    ap.add_argument("--out", default=os.path.join(HERE, "RESULTS-workloads.md"))
    a = ap.parse_args()
    n = a.seconds * WL.FPS
    kinds = a.kinds.split(",")
    tilesel = os.path.join(tempfile.mkdtemp(), "tilesel")
    subprocess.run(["go", "build", "-o", tilesel, "./bench/quality/tilesel"], cwd=ROOT, check=True)
    cpu = {k: cpu_proxy(k, min(a.cpu_frames, n), a.corpus) for k in kinds}  # alone, before the parallel pass
    with ProcessPoolExecutor(a.jobs) as ex:
        data = list(ex.map(simulate, kinds, [n] * len(kinds), [a.step] * len(kinds), [tilesel] * len(kinds),
                           [a.corpus] * len(kinds)))
    for d in data:
        d["cpu"] = cpu[d["kind"]]
    with open(a.json, "w") as f:
        json.dump(data, f, indent=1)
    write_md(data, a.out, n, a.step)
    print("wrote", a.out)


if __name__ == "__main__":
    main()
