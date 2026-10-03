#!/usr/bin/env python3
"""bench/quality/final.py — before/after simulation of the implemented changes, writes RESULTS-final.md.

Scenarios (per corpus image):
  before   : old 1080p-forced path — downscale to 1080 lines (x0.75 on 1440p), 4:2:0, x264 main @8M
  native   : native size, 4:2:0, x264 main @8M (30 static frames, converged)
  motion   : native, the single frame right after motion under the 8M CBR budget
  refine22 : native, last frame re-encoded with x264 -qp 22 (static refine, pass 1)
  refine18 : native, last frame re-encoded with x264 -qp 18 (static refine, pass 2)
  tiles    : native @8M + lossless RGB tiles chosen by internal/tiles.Select (Go helper tilesel)
  combined : native, refine QP18 + tiles
"""
import argparse, glob, io, json, math, os, subprocess
import numpy as np
from PIL import Image
import metrics as M
import pipeline as P

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))  # tools/oo-screen (go module)
KBPS = 8000
ORDER = ["before", "native", "motion", "refine22", "refine18", "tiles", "combined"]
LABEL = {
    "before": "До: 1080p-forced (x0.75 на 1440p) 4:2:0 main 8M",
    "native": "Нативний розмір 4:2:0 main 8M",
    "motion": "Нативний, останній кадр руху @8M (до refine)",
    "refine22": "Нативний + refine QP22",
    "refine18": "Нативний + refine QP22→QP18",
    "tiles": "Нативний 8M + текстові тайли",
    "combined": "Після: нативний + refine QP18 + тайли",
}


def select_tiles(path):
    out = subprocess.run(["go", "run", "./bench/quality/tilesel", os.path.abspath(path)], cwd=ROOT,
                         capture_output=True, text=True, check=True).stdout
    return json.loads(out)


def tile_bits(ref, rects):
    total = 0
    for r in rects:
        b = io.BytesIO()
        Image.fromarray(ref[r["Y"]:r["Y"] + r["H"], r["X"]:r["X"] + r["W"]]).save(b, "PNG", optimize=True)
        total += len(b.getvalue()) * 8
    return total


def fmt(x, n=2):
    return "inf" if x == math.inf else ("n/a" if x is None or (isinstance(x, float) and math.isnan(x)) else f"{x:.{n}f}")


def measure(ref, mask, out, kbit):
    es, er = M.text_sharpness(ref, out, mask)
    return dict(psnr=M.psnr(ref, out), ssim=M.ssim(ref, out), es=es, er=er, kbit=kbit)


def scenarios(ref):
    H = ref.shape[0]
    sc = 1080 / H if H > 1080 else None
    before, bb = P.run(ref, "420", sc, "bicubic", KBPS)
    native, nb = P.run(ref, "420", None, "bicubic", KBPS)
    mo, mb = P.run(ref, "420", None, "bicubic", KBPS, frames=1)
    r22, b22 = P.run(ref, "420", None, "bicubic", qp=22, frames=1)
    r18, b18 = P.run(ref, "420", None, "bicubic", qp=18, frames=1)
    return dict(before=(before, bb), native=(native, nb), motion=(mo, mb), refine22=(r22, b22), refine18=(r18, b18))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=os.path.join(HERE, "..", "corpus"))
    ap.add_argument("--out", default=os.path.join(HERE, "RESULTS-final.md"))
    a = ap.parse_args()
    if not P.have_ffmpeg():
        raise SystemExit("ffmpeg required")
    vm = P.have_libvmaf()
    rows = []
    for path in sorted(glob.glob(os.path.join(a.corpus, "*.png"))):
        ref = np.asarray(Image.open(path).convert("RGB"))
        mask = M.text_mask(ref)
        rects = select_tiles(path)
        tb = tile_bits(ref, rects)
        s = scenarios(ref)
        res = {k: measure(ref, mask, o, b / 1000) for k, (o, b) in s.items()}
        res["tiles"] = measure(ref, mask, P.paste_tiles(s["native"][0], ref, rects), (s["native"][1] + tb) / 1000)
        # combined: refine bytes (QP22 + QP18 passes) + one-off tile payload
        res["combined"] = measure(ref, mask, P.paste_tiles(s["refine18"][0], ref, rects),
                                  (s["refine22"][1] + s["refine18"][1] + tb) / 1000)
        for k in ORDER:
            r = res[k]; r.update(img=os.path.basename(path), var=k, ntiles=len(rects))
            rows.append(r)
            print(f"{r['img']:24s} {k:9s} PSNR {fmt(r['psnr'])} SSIM {fmt(r['ssim'],4)} eSSIM {fmt(r['es'],4)} "
                  f"G {fmt(r['er'],3)} kbit {fmt(r['kbit'],0)} tiles {len(rects)}", flush=True)

    def mean(k, key, imgs=None):
        v = [r[key] for r in rows if r["var"] == k and (imgs is None or imgs(r["img"]))]
        return float(np.mean(v)) if v else None

    hdr = ("| сценарій | PSNR dB | SSIM | text edge-SSIM | різкість тексту (G-ratio) | кбіт |\n"
           "|---|---|---|---|---|---|\n")
    def table(imgs=None):
        return hdr + "".join(f"| {LABEL[k]} | {fmt(mean(k,'psnr',imgs))} | {fmt(mean(k,'ssim',imgs),4)} | "
                             f"{fmt(mean(k,'es',imgs),4)} | {fmt(mean(k,'er',imgs),3)} | {fmt(mean(k,'kbit',imgs),0)} |\n"
                             for k in ORDER)
    is1440 = lambda n: "1440" in n
    es_ref, es_all = mean("refine18", "es"), mean("combined", "es")
    er_all = [r["er"] for r in rows if r["var"] == "combined"]
    ok = lambda b: "**виконано**" if b else "**не виконано**"
    L = ["# bench/quality — підсумок до/після (СИМУЛЯЦІЯ)", "",
         "> **Це симуляція, а не вимір на реальному залізі.** Синтетичний корпус `bench/corpus`, конвеєр агента "
         "емульовано в numpy + ffmpeg libx264 на Linux (без DXGI, MediaFoundation MFT і декодера браузера). "
         "Цифри придатні для порівняння варіантів між собою. Відтворення: `python3 bench/quality/final.py`.", "",
         "## Сценарії", "",
         "- **До** — старий шлях: екран 1440p примусово зменшувався до 1080 рядків (x0.75, bicubic), 4:2:0, "
         "x264 profile main, 8 Мбіт/с; глядач розтягує білінійно назад. На 1080p-екранах масштабу не було.",
         "- **Нативний** — без даунскейлу, решта та сама; 30 однакових кадрів, тож x264 встигає «дотягнути» якість (стелю).",
         "- **Останній кадр руху** — один кадр при CBR 8 Мбіт/с (bufsize = 1 с), тобто стан одразу після руху, до refine.",
         "- **Refine** — останній кадр після руху перекодовано з постійним QP (`x264 -qp 22`, далі `-qp 18`). "
         "Байти — розмір одиночного intra-кадру на кожен прохід (верхня межа; у агенті це P-кадр поверх попереднього).",
         "- **Тайли** — `internal/tiles.Select` (через Go-хелпер `bench/quality/tilesel`, налаштування за замовчуванням, "
         "тайл 64×64) обирає тайли з кольоровим текстом; їхні оригінальні пікселі вставлено в декодований кадр. "
         "Байти тайлів — сума PNG (optimize) по тайлах, одноразово.",
         "- **Після (combined)** — нативний розмір + refine QP22→QP18 + тайли.",
         "- кбіт: для 8M-сценаріїв — середнє на кадр серед 30 однакових кадрів; для refine/combined — сумарні байти "
         "дошліфування.",
         "- Метрики: PSNR/SSIM по RGB/яскравості; text edge-SSIM — SSIM карт градієнта в масці тексту; "
         "G-ratio — енергія градієнта в масці відносно оригіналу (1.0 = так само різко).",
         "- VMAF: " + ("ffmpeg libvmaf доступний, але в цьому звіті не рахувався." if vm else
                       "**n/a** — у цій збірці ffmpeg немає libvmaf (лише vmafmotion)."), "",
         "## До / після — середнє по всьому корпусу", "", table(),
         "## До / після — лише 1440p (де діяв примусовий 1080p)", "", table(is1440),
         "## Цільові показники ТЗ", "",
         "| ціль | значення (симуляція) | статус |", "|---|---|---|",
         f"| text SSIM ≥ 0.98 після refine (edge-SSIM, refine QP18) | {fmt(es_ref,4)} | {ok(es_ref >= 0.98)} |",
         f"| text SSIM ≥ 0.98, refine + тайли | {fmt(es_all,4)} | {ok(es_all >= 0.98)} |",
         f"| різкість тексту ≥ 90% оригіналу (G-ratio ≥ 0.90, мінімум по зображеннях, combined) | "
         f"{fmt(min(er_all),3)} | {ok(min(er_all) >= 0.90)} |",
         "| VMAF | n/a | n/a (немає libvmaf) |" if not vm else "| VMAF | не рахувався | n/a |", "",
         "## По зображеннях", "",
         "| зображення | сценарій | тайлів | PSNR | SSIM | edge-SSIM | G-ratio | кбіт |", "|---|---|---|---|---|---|---|---|"]
    L += [f"| {r['img']} | {r['var']} | {r['ntiles']} | {fmt(r['psnr'])} | {fmt(r['ssim'],4)} | {fmt(r['es'],4)} | "
          f"{fmt(r['er'],3)} | {fmt(r['kbit'],0)} |" for r in rows]
    with open(a.out, "w") as f:
        f.write("\n".join(L) + "\n")
    print("wrote", a.out)


if __name__ == "__main__":
    main()
