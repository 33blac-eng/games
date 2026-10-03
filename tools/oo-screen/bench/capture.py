#!/usr/bin/env python3
"""Знімання per-frame метрик viewer-а у ВИДИМОМУ браузері (Playwright headed).

Прихована вкладка тротлить rAF/rVFC → per-frame метрики недійсні (задокументована
пастка цього репо). Тому офіційні прогони матриці — ЛИШЕ цим драйвером.

  python bench/capture.py --candidate B --seconds 60 \
      --url "http://localhost:4480/viewer-wt.html?token=...&certhash=..." \
      --out results/S1/B/run1.ndjson
  python bench/capture.py --candidate A --seconds 60 \
      --url "http://localhost:4480/viewer-webrtc.html?token=..." --out ...

Вимога: стек кандидата вже запущений (bench/run_a.sh / run_b.sh).
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
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    out = pathlib.Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)

    with sync_playwright() as pw:
        # headed: інакше rVFC/rAF тротляться і метрики брешуть
        browser = pw.chromium.launch(headless=False, args=["--autoplay-policy=no-user-gesture-required"])
        page = browser.new_page(viewport={"width": 1280, "height": 800})
        page.goto(args.url)
        # viewer B чекає кліку Connect (поля префілляться з URL); A конектиться сам
        if args.candidate == "B":
            page.locator("button", has_text="Connect").first.click()
        t0 = time.monotonic()
        # чекати перший кадр
        if args.candidate == "B":
            page.wait_for_function("document.body.innerText.includes('keyframe')", timeout=15000)
        else:
            page.wait_for_function("document.querySelector('video') && document.querySelector('video').videoWidth > 0", timeout=15000)
        first_frame_s = time.monotonic() - t0
        time.sleep(args.seconds)
        if args.candidate == "B":
            text = page.evaluate(
                """() => new Promise(res => {
                     const h = ev => { if (ev.data && ev.data.type === 'metrics-blob' && ev.data.blob) {
                       worker.removeEventListener('message', h); ev.data.blob.text().then(res); } };
                     worker.addEventListener('message', h);
                     worker.postMessage({type: 'download'});
                   })"""
            )
        else:
            text = page.evaluate("() => ndjson.join('\\n') + '\\n'")
        browser.close()

    lines = [l for l in text.strip().splitlines() if l.strip()]
    out.write_text("\n".join(lines) + "\n", encoding="utf-8", newline="")
    print(f"CAPTURE_OK candidate={args.candidate} lines={len(lines)} "
          f"first_frame_s={first_frame_s:.2f} out={out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
