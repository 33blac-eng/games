#!/usr/bin/env python3
"""bench/quality/refine_after_idr.py — TASK.md крок 4: refine після IDR на нерухомому екрані (СИМУЛЯЦІЯ, x264).

Сценарій: екран стоїть, refine уже дошліфував його (QP 18), і тут приходить ключовий кадр від rate control —
новий глядач (keyframe_request), PLI або періодичний GOP MFT, що впав на keepalive. IDR кодується на поточній
ставці під HRD (x264 ABR + VBV 1.5×/0.5 с, як PCVBR агента) — на низькій ставці це мило. Без політики руху
нема, і refine (internal/refine) не заводиться: мило лишається до наступної зміни екрана.
З OO_SCREEN_REFINE_AFTER_IDR (refine.Config.AfterKeyframe) через 200 мс ідуть refine QP22 → QP18.

IDR: x264rc (bench/quality/x264rc) — 1 с keepalive після стартового IDR (буфер VBV повний, як у простої),
далі примусовий IDR. Refine: 2-кадровий енкод x264 -qp N [декодований стан, оригінал] — як у
workloads_run.py. Окремий енкод, бо row-level VBV x264 перекриває примусовий QP кадру (MB до QP 51), тоді як
MFT агента на refine затискає MaxQP = QP (mft.c oos_enc_set_refine_qp).
Друкує JSON і таблицю markdown."""
import argparse, json, os, subprocess, tempfile
import numpy as np
import metrics as M
import pipeline as P
import workloads as WL
import workloads_run as R
import ratecontrol_run as RC

IMAGES = ["sheet-1080p.png", "code-light-1080p.png", "code-dark-1080p.png", "colortext-1080p.png"]


def idr_state(tool, yuv, kbps):
    """(bytes IDR на запит, декодований стан після нього) — x264rc, VBV повний."""
    td = tempfile.mkdtemp()
    d = os.path.join(td, "d.yuv")
    p = subprocess.Popen([tool, str(WL.W), str(WL.H), str(WL.FPS), os.path.join(td, "o.h264"), d],
                         stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    sizes = []
    for i in range(32):
        newpix, idr, dump = int(i == 0), int(i in (0, 31)), int(i == 31)
        p.stdin.write(f"{kbps} {newpix} 1 {idr} 0 {dump}\n".encode() + (yuv if newpix else b""))
        p.stdin.flush()
        sizes.append(p.stdout.readline().decode().split())
    p.stdin.close(); p.wait()
    buf = open(d, "rb").read()
    os.remove(d)
    return int(sizes[31][0]), int(sizes[31][2]), R.split(buf)


def to_rgb(planes):
    y, u, v = planes
    return P.yuv_to_rgb(y.astype(np.float64), P.up420(u.astype(np.float64), WL.H, WL.W),
                        P.up420(v.astype(np.float64), WL.H, WL.W))


def qual(ref, planes):
    out = to_rgb(planes)
    es, _ = M.text_sharpness(ref, out)
    return {"psnr": M.psnr(ref, out), "ssim": M.ssim(ref, out), "edge": es}


def one(args):
    name, kbps, corpus, tool = args
    rgb = WL.load(name, corpus)
    yuv = RC.to_yuv(rgb)
    nb, qp, st = idr_state(tool, yuv, kbps)
    res = {"image": name, "kbps": kbps, "idr_kb": nb / 1000, "idr_qp": qp, "after_idr": qual(rgb, st)}
    tot = 0
    for q in (22, 18):
        b, st = R.refine_pass(st, rgb, q)
        tot += b
        res[f"refine{q}"] = qual(rgb, st)
    res["refine_kb"] = tot / 1000
    return res


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=WL.CORPUS)
    ap.add_argument("--rates", default="8000,4000,2000")
    ap.add_argument("--jobs", type=int, default=os.cpu_count() or 2)
    a = ap.parse_args()
    tool = RC.build(tempfile.mkdtemp())
    jobs = [(im, int(r), a.corpus, tool) for r in a.rates.split(",") for im in IMAGES]
    from concurrent.futures import ProcessPoolExecutor
    with ProcessPoolExecutor(a.jobs) as ex:
        res = list(ex.map(one, jobs))
    print(json.dumps(res, indent=1))
    print("| кадр | ставка | IDR КБ (QP) | після IDR PSNR / edge-SSIM | + refine 22→18 PSNR / edge-SSIM | refine КБ |")
    print("|---|---|---|---|---|---|")
    for r in res:
        b, f = r["after_idr"], r["refine18"]
        print(f"| {r['image'].replace('-1080p.png', '')} | {r['kbps'] // 1000}M | {r['idr_kb']:.0f} ({r['idr_qp']}) | "
              f"{b['psnr']:.2f} / {b['edge']:.4f} | {f['psnr']:.2f} / {f['edge']:.4f} | {r['refine_kb']:.0f} |")


if __name__ == "__main__":
    main()
