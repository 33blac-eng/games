#!/usr/bin/env python3
"""bench/quality_capture.py — знімання PNG-кадрів viewer-а для якість-гейтів
(R4#3, Додаток A блок ЯКІСТЬ: SSIM / OCR / distinct-FPS).

На відміну від bench/capture.py (NDJSON per-frame метрики з JS viewer-а),
цей драйвер знімає РЕАЛЬНІ пікселі з екрану браузера (Playwright headed,
той самий трюк "headed інакше rVFC тротлиться" що й у capture.py) —
щоб зловити композитор-артефакти (розмиття, дропи, кольоропотоки), яких
NDJSON з t_rendered_ms взагалі не бачить.

  python bench/quality_capture.py --candidate B --seconds 60 --fps 5 \
      --url "http://localhost:4480/viewer-wt.html?token=...&certhash=..." \
      --out-dir results/quality/B

  python bench/quality_capture.py --candidate A --seconds 60 --fps 5 \
      --url "http://localhost:4480/viewer-webrtc.html?token=..." \
      --out-dir results/quality/A

Вимога: стек кандидата вже запущений (bench/run_a.sh / run_b.sh), як і для
bench/capture.py.

Межі методу (чесно, як просить завдання): це screenshot композитора
браузера елемента <video>/<canvas>, а НЕ байти з декодера. Він фіксує все,
що бачить користувач (включно з compositor scaling/color-management), але
може додавати power-save/vsync тротлінг самого Playwright-скріншота —
тому --fps тут НЕ те саме, що distinct-rendered-FPS з Додатка A: реальний
distinct-FPS рахується пост-фактум у quality_score.py за унікальними
SEQ-значеннями, знайденими у знятих кадрах (аліасинг знімання PNG сам по
собі FPS не гарантує і не обмежує).
"""
import argparse
import pathlib
import sys
import time

from playwright.sync_api import sync_playwright


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--candidate", choices=["A", "B"], required=True)
    ap.add_argument("--url", required=True)
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--fps", type=float, default=5.0, help="скільки PNG-знімків/сек знімати (не плутати з рендер-FPS viewer-а)")
    ap.add_argument("--out-dir", required=True)
    args = ap.parse_args()

    out_dir = pathlib.Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    for stale in out_dir.glob("frame_*.png"):
        stale.unlink()

    selector = "#v" if args.candidate == "A" else "#canvas"
    interval_s = 1.0 / max(args.fps, 0.001)

    with sync_playwright() as pw:
        # headed: та сама причина що в capture.py — прихована вкладка тротлить
        # rAF/compositor, знімки будуть недійсні для якісних метрик.
        browser = pw.chromium.launch(headless=False, args=["--autoplay-policy=no-user-gesture-required"])
        page = browser.new_page(viewport={"width": 1280, "height": 800})
        page.goto(args.url)
        if args.candidate == "B":
            page.locator("button", has_text="Connect").first.click()
        if args.candidate == "B":
            page.wait_for_function("document.body.innerText.includes('keyframe')", timeout=15000)
        else:
            page.wait_for_function(
                "document.querySelector('video') && document.querySelector('video').videoWidth > 0",
                timeout=15000,
            )

        el = page.locator(selector).first
        t0 = time.monotonic()
        idx = 0
        deadline = t0 + args.seconds
        next_shot = t0
        captured = 0
        while True:
            now = time.monotonic()
            if now >= deadline:
                break
            if now >= next_shot:
                fp = out_dir / f"frame_{idx:04d}.png"
                try:
                    el.screenshot(path=str(fp), timeout=5000)
                    captured += 1
                except Exception as exc:  # element transiently unready — skip, don't abort run
                    print(f"WARN frame {idx} screenshot failed: {exc}", file=sys.stderr)
                idx += 1
                next_shot += interval_s
                if next_shot < now:
                    next_shot = now + interval_s  # не наздоганяти чергу, якщо відстали
            else:
                time.sleep(min(0.01, next_shot - now))
        browser.close()

    meta = out_dir / "meta.json"
    meta.write_text(
        f'{{"candidate":"{args.candidate}","seconds":{args.seconds},"target_fps":{args.fps},'
        f'"selector":"{selector}","frames_attempted":{idx},"frames_captured":{captured}}}\n',
        encoding="utf-8",
    )
    print(f"QUALITY_CAPTURE_OK candidate={args.candidate} frames={captured}/{idx} out_dir={out_dir}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
