#!/usr/bin/env python3
"""Render videomode-results.json (videomode_run.py) as the RESULTS-workloads.md table rows (stdout)."""
import json, os, sys

HERE = os.path.dirname(os.path.abspath(__file__))


def f(x, n=2):
    return "—" if x is None or x != x else f"{x:.{n}f}"


def main(path=os.path.join(HERE, "videomode-results.json")):
    res = json.load(open(path))
    print("| навантаження | Мбіт/с | варіант | кадрів/60 с | у Video | PSNR сер. / p5 | кут відео PSNR | "
          "edge-SSIM сер. / p5 | МБ/год |")
    print("|---|---|---|---|---|---|---|---|---|")
    for r in res:
        for k, q in r["rates"].items():
            corner = f"{f(q.get('corner'))} / {f(q.get('corner_p5'))}" if "corner" in q else "—"
            print(f"| {r['kind']} | {int(k) // 1000}M | {r['variant']} | {r['frames']} | {r['video_share'] * 100:.0f} % | "
                  f"{f(q['psnr'])} / {f(q['psnr_p5'])} | {corner} | {f(q['es'], 4)} / {f(q['es_p5'], 4)} | "
                  f"{q['mb_h']:.0f} |")
    print()
    for r in res:
        if r["kind"] == "video":
            for k, q in r["rates"].items():
                print(f"video {r['variant']} {k}: display corner {f(q['disp_psnr'])}, 4:2:0 ceiling corner "
                      f"{f(q['corner_420_ceiling'])} full {f(q['psnr_420_ceiling'])}", file=sys.stderr)


if __name__ == "__main__":
    main(*sys.argv[1:])
