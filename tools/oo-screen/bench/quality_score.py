#!/usr/bin/env python3
r"""bench/quality_score.py — якість-гейти R4#3 (Додаток A блок ЯКІСТЬ):

    SSIM(rendered, source) >= 0.90                (траси S1..S6)
    OCR-точність тексту в кадрі >= 98%             ("oo-screen T1 corpus
                                                     text probe" 14px і
                                                     "SEQ N" 96px)
    distinct-rendered-FPS >= 0.9x цілі (S1-S2), >= 0.5x цілі (S7)

Без цих гейтів агресивний frame-drop "виграє" latency/freeze-гейти
показуючи глядачу криву картинку — цей скрипт існує щоб таке ловити.

── Метод (чесно, межі задокументовано за вимогою завдання) ────────────────
Вхід — PNG-кадри, зняті bench/quality_capture.py через Playwright
element.screenshot() з композитора браузера (headed). Це НЕ байти
декодера — компоузер-scaling/color-management/dropped-repaint теж
потрапляють у вимір, і це навмисно: саме це бачить людина. Зворотний
бік: сам playwright-screenshot може відставати від vsync viewer-а на
до одного кадру композитора — тому "SSIM/OCR по кадру" тут коректні,
а "точний capture→render timestamp" — ні (те міряє bench/capture.py
через NDJSON, окремим шляхом).

Джерело правди для звірки — bench/corpus/corpus-1080p60.h264. Корпус
зациклений: 600 AU (кадр 0..599), плеєр показує seq mod 600. Кожен
source-кадр несе у собі вбудований текстовий "SEQ N" (96px моноширинний,
top-left) — це і є прив'язка rendered-кадру до source-кадру: рахуємо
матчинг rendered-кадру проти ВСІХ 600 source-кадрів (normalized
cross-correlation по SEQ-регіону, векторизовано numpy) і беремо
найкращий; ключове припущення — що viewer НЕ вставляє власний рандомний
seq, а показує PTS-послідовний кадр з корпусу (вірно для T1 corpus
player).

OCR-метод:
  1) якщо є pytesseract + tesseract у PATH — пробуємо читати "SEQ <N>"
     напряму (найточніше, але на цій машині tesseract відсутній —
     перевірено `import pytesseract` на старті, з graceful fallback);
  2) інакше (фактичний шлях тут) — template-matching: SEQ-регіон
     rendered-кадру проти SEQ-регіону кожного з 600 source-кадрів,
     нормалізована крос-кореляція (NCC) на grayscale. NCC найкращого
     кандидата >= --ocr-ncc-threshold вважається "текст прочитано
     правильно" (бо кадр із заблюреним/биткошкодженим "SEQ N" match-иться
     гірше за чіткий) — це проксі, не посимвольний OCR, чесно позначено
     як CORRELATION fallback у виводі.

SSIM: власна векторизована реалізація (numpy), без cv2/skimage
залежності (перевірено на цій машині — numpy є, PIL є; якщо колись не
буде numpy — впаде з чіткою помилкою, а не мовчки). Стандартна
windowed-SSIM формула (Wang et al. 2004), 7x7 uniform window, на
grayscale luma, у діапазоні [0,1] (L=255).

Selftest:
    python bench/quality_score.py --selftest
показує: (а) чистий кадр 100 проти самого себе -> SSIM~1.0 -> PASS,
(б) той самий кадр з ffmpeg boxblur=10 -> SSIM низький -> FAIL.
"""
import argparse
import glob
import json
import math
import os
import shutil
import subprocess
import sys
import tempfile

import numpy as np
from PIL import Image

try:
    import pytesseract  # type: ignore
    HAVE_TESSERACT = True
except Exception:
    HAVE_TESSERACT = False

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CORPUS_PATH = os.path.join(REPO_ROOT, "bench", "corpus", "corpus-1080p60.h264")
CORPUS_FRAME_COUNT = 600  # 10s @ 60fps, задокументовано в README.md

# SEQ-регіон у базовій системі координат source-кадру (1920x1080),
# виміряний з реального декодування corpus-1080p60.h264 кадру 100
# ("SEQ 100" займає приблизно x:30..410 y:20..115). Беремо із запасом.
SEQ_REGION_BASE = (20, 10, 420, 130)  # x0,y0,x1,y1 у 1920x1080
BASE_W, BASE_H = 1920, 1080

GATES = {
    "S1": {"ssim_min": 0.90, "fps_ratio_min": 0.9},
    "S2": {"ssim_min": 0.90, "fps_ratio_min": 0.9},
    "S3": {"ssim_min": 0.90, "fps_ratio_min": None},
    "S4": {"ssim_min": 0.90, "fps_ratio_min": None},
    "S5": {"ssim_min": 0.90, "fps_ratio_min": None},
    "S6": {"ssim_min": 0.90, "fps_ratio_min": None},
    "S7": {"ssim_min": None, "fps_ratio_min": 0.5},
}
OCR_MIN = 0.98


# --------------------------------------------------------------------------
# SSIM — власна реалізація (numpy only)
# --------------------------------------------------------------------------

def _to_gray(img: Image.Image) -> np.ndarray:
    return np.asarray(img.convert("L"), dtype=np.float64)


def _uniform_filter(a: np.ndarray, win: int) -> np.ndarray:
    """Box filter через 2D cumulative-sum (без scipy), 'same' розмір із
    reflect-паддингом по краях, щоб уникнути темної рамки."""
    pad = win // 2
    ap = np.pad(a, pad, mode="reflect")
    csum = np.cumsum(np.cumsum(ap, axis=0), axis=1)
    csum = np.pad(csum, ((1, 0), (1, 0)), mode="constant")
    h, w = a.shape
    out = (
        csum[win:win + h, win:win + w]
        - csum[0:h, win:win + w]
        - csum[win:win + h, 0:w]
        + csum[0:h, 0:w]
    )
    return out / (win * win)


def compute_ssim(gray_a: np.ndarray, gray_b: np.ndarray, win: int = 7) -> float:
    """Windowed SSIM (Wang et al. 2004), L=255, k1=0.01, k2=0.03."""
    if gray_a.shape != gray_b.shape:
        raise ValueError(f"shape mismatch: {gray_a.shape} vs {gray_b.shape}")
    L = 255.0
    C1 = (0.01 * L) ** 2
    C2 = (0.03 * L) ** 2

    mu_a = _uniform_filter(gray_a, win)
    mu_b = _uniform_filter(gray_b, win)
    mu_a2 = mu_a * mu_a
    mu_b2 = mu_b * mu_b
    mu_ab = mu_a * mu_b

    sigma_a2 = _uniform_filter(gray_a * gray_a, win) - mu_a2
    sigma_b2 = _uniform_filter(gray_b * gray_b, win) - mu_b2
    sigma_ab = _uniform_filter(gray_a * gray_b, win) - mu_ab

    numerator = (2 * mu_ab + C1) * (2 * sigma_ab + C2)
    denominator = (mu_a2 + mu_b2 + C1) * (sigma_a2 + sigma_b2 + C2)
    ssim_map = numerator / np.maximum(denominator, 1e-12)
    return float(np.mean(ssim_map))


# --------------------------------------------------------------------------
# Джерело: рендер/кеш 600 source-кадрів корпусу
# --------------------------------------------------------------------------

def ensure_source_cache(corpus_path: str, cache_dir: str) -> str:
    """Декодує corpus у cache_dir/src_%04d.png (0-599), один прохід ffmpeg
    (НЕ 600 окремих виликів — набагато швидше). Кешується на диску, тому
    повторні виклики quality_score.py на тому самому cache_dir безкоштовні."""
    os.makedirs(cache_dir, exist_ok=True)
    existing = sorted(glob.glob(os.path.join(cache_dir, "src_*.png")))
    if len(existing) == CORPUS_FRAME_COUNT:
        return cache_dir
    for f in existing:
        os.remove(f)
    cmd = [
        "ffmpeg", "-y", "-i", corpus_path,
        "-vsync", "0", "-frames:v", str(CORPUS_FRAME_COUNT),
        os.path.join(cache_dir, "src_%04d.png"),
    ]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(f"ffmpeg decode failed: {proc.stderr[-2000:]}")
    got = sorted(glob.glob(os.path.join(cache_dir, "src_*.png")))
    if len(got) != CORPUS_FRAME_COUNT:
        raise RuntimeError(f"expected {CORPUS_FRAME_COUNT} source frames, got {len(got)}")
    return cache_dir


def source_frame_path(cache_dir: str, seq: int) -> str:
    n = seq % CORPUS_FRAME_COUNT
    return os.path.join(cache_dir, f"src_{n + 1:04d}.png")


def load_source_frame(cache_dir: str, seq: int) -> Image.Image:
    return Image.open(source_frame_path(cache_dir, seq)).convert("RGB")


class SourceIndex:
    """Кешовані у пам'яті сірошкальні SEQ-crop-и всіх 600 source-кадрів,
    для швидкого векторизованого NCC-матчингу (600 корельованих кандидатів
    на кожен знятий кадр обходиться недорого саме тому, що crop маленький
    і всі 600 уже лежать у одному ndarray)."""

    def __init__(self, cache_dir: str, crop_size=(400, 110)):
        self.cache_dir = cache_dir
        self.crop_size = crop_size
        cw, ch = crop_size
        stack = np.empty((CORPUS_FRAME_COUNT, ch, cw), dtype=np.float64)
        x0, y0, x1, y1 = SEQ_REGION_BASE
        for seq in range(CORPUS_FRAME_COUNT):
            img = load_source_frame(cache_dir, seq)
            crop = img.crop((x0, y0, x1, y1)).resize(crop_size, Image.BILINEAR)
            stack[seq] = _to_gray(crop)
        self.stack = stack
        # Precompute per-candidate mean/std for NCC.
        flat = stack.reshape(CORPUS_FRAME_COUNT, -1)
        self.mean = flat.mean(axis=1)
        centered = flat - self.mean[:, None]
        self.centered = centered
        self.norm = np.sqrt((centered ** 2).sum(axis=1))

    def match(self, query_gray: np.ndarray):
        """Повертає (best_seq, best_ncc) — найкращий кандидат серед 600
        source-кадрів за нормалізованою крос-кореляцією SEQ-регіону."""
        q = query_gray.reshape(-1).astype(np.float64)
        q = q - q.mean()
        qn = math.sqrt(float((q ** 2).sum()))
        if qn < 1e-9:
            return 0, 0.0
        num = self.centered @ q
        denom = self.norm * qn
        denom = np.where(denom < 1e-9, 1e-9, denom)
        ncc = num / denom
        best = int(np.argmax(ncc))
        return best, float(ncc[best])


def crop_seq_region(img: Image.Image, crop_size=(400, 110)) -> np.ndarray:
    """Виріз SEQ-регіону з rendered-кадру довільного розміру: масштабує
    базові координати (виміряні на 1920x1080) під фактичний розмір
    знятого елемента (screenshot video/canvas у браузері може бути менший
    за 1920x1080 через viewport)."""
    w, h = img.size
    sx, sy = w / BASE_W, h / BASE_H
    x0, y0, x1, y1 = SEQ_REGION_BASE
    box = (int(x0 * sx), int(y0 * sy), int(x1 * sx), int(y1 * sy))
    crop = img.crop(box).resize(crop_size, Image.BILINEAR)
    return _to_gray(crop)


def ocr_seq_pytesseract(img: Image.Image):
    box_w, box_h = int(420 * img.size[0] / BASE_W), int(130 * img.size[1] / BASE_H)
    crop = img.crop((0, 0, box_w, box_h))
    try:
        text = pytesseract.image_to_string(crop, config="--psm 7 -c tessedit_char_whitelist=SEQ0123456789 ")
    except Exception:
        return None
    text = text.strip().upper()
    if text.startswith("SEQ"):
        digits = "".join(ch for ch in text[3:] if ch.isdigit())
        if digits:
            try:
                return int(digits)
            except ValueError:
                return None
    return None


# --------------------------------------------------------------------------
# Скоринг набору знятих кадрів (bench/quality_capture.py output)
# --------------------------------------------------------------------------

def score_capture_dir(frames_dir: str, cache_dir: str, target_fps: float,
                       ocr_ncc_threshold: float = 0.90):
    frame_paths = sorted(glob.glob(os.path.join(frames_dir, "frame_*.png")))
    if not frame_paths:
        raise RuntimeError(f"no frame_*.png in {frames_dir}")

    index = SourceIndex(cache_dir)
    ssim_vals = []
    ocr_hits = 0
    ocr_total = 0
    distinct_seqs = set()
    method_used = "pytesseract" if HAVE_TESSERACT else "ncc-template-match"

    for fp in frame_paths:
        img = Image.open(fp).convert("RGB")
        q_crop = crop_seq_region(img, index.crop_size)
        best_seq, ncc = index.match(q_crop)

        tess_seq = ocr_seq_pytesseract(img) if HAVE_TESSERACT else None
        if tess_seq is not None:
            ocr_total += 1
            if tess_seq % CORPUS_FRAME_COUNT == best_seq:
                ocr_hits += 1
            match_seq = tess_seq % CORPUS_FRAME_COUNT
        else:
            ocr_total += 1
            if ncc >= ocr_ncc_threshold:
                ocr_hits += 1
            match_seq = best_seq

        distinct_seqs.add(match_seq)

        src = load_source_frame(cache_dir, match_seq)
        src_resized = src.resize(img.size, Image.BILINEAR)
        ssim_vals.append(compute_ssim(_to_gray(img), _to_gray(src_resized)))

    # тривалість зйомки — з meta.json (пишеться quality_capture.py), інакше
    # оцінюється з кількості кадрів / номінального fps знімання
    meta_path = os.path.join(frames_dir, "meta.json")
    if os.path.exists(meta_path):
        meta = json.loads(open(meta_path, encoding="utf-8").read())
        duration_s = meta.get("seconds", len(frame_paths))
    else:
        duration_s = len(frame_paths)

    distinct_fps = len(distinct_seqs) / max(duration_s, 1e-9)
    distinct_fps_ratio = distinct_fps / max(target_fps, 1e-9)

    ssim_arr = np.array(ssim_vals)
    return {
        "method": {"ocr": method_used, "ssim": "numpy-windowed-ssim(7x7,reflect)",
                    "note": "screenshot композитора браузера, не байти декодера"},
        "n_frames": len(frame_paths),
        "ssim_mean": float(np.mean(ssim_arr)),
        "ssim_p05": float(np.percentile(ssim_arr, 5)),
        "ocr_accuracy": ocr_hits / max(ocr_total, 1),
        "distinct_seq_count": len(distinct_seqs),
        "duration_s": duration_s,
        "target_fps": target_fps,
        "distinct_fps": distinct_fps,
        "distinct_fps_ratio": distinct_fps_ratio,
    }


def apply_gate(metrics: dict, scenario: str) -> dict:
    gate = GATES.get(scenario, {"ssim_min": 0.90, "fps_ratio_min": None})
    checks = {}
    ok = True

    if gate.get("ssim_min") is not None:
        c = metrics["ssim_mean"] >= gate["ssim_min"]
        checks["ssim"] = {"value": metrics["ssim_mean"], "gate": gate["ssim_min"], "pass": c}
        ok = ok and c

    c = metrics["ocr_accuracy"] >= OCR_MIN
    checks["ocr"] = {"value": metrics["ocr_accuracy"], "gate": OCR_MIN, "pass": c}
    ok = ok and c

    if gate.get("fps_ratio_min") is not None:
        c = metrics["distinct_fps_ratio"] >= gate["fps_ratio_min"]
        checks["distinct_fps_ratio"] = {"value": metrics["distinct_fps_ratio"], "gate": gate["fps_ratio_min"], "pass": c}
        ok = ok and c

    return {"scenario": scenario, "checks": checks, "verdict": "PASS" if ok else "FAIL", **metrics}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--frames-dir", help="results/quality/<cand> з bench/quality_capture.py")
    ap.add_argument("--scenario", default="S1", help="S1..S7 — обирає гейти Додатка A")
    ap.add_argument("--target-fps", type=float, default=60.0)
    ap.add_argument("--corpus", default=CORPUS_PATH)
    ap.add_argument("--cache-dir", default=None, help="кеш декодованих source-кадрів (дефолт: system temp)")
    ap.add_argument("--out", default=None, help="куди писати quality.json (дефолт: <frames-dir>/quality.json)")
    ap.add_argument("--ocr-ncc-threshold", type=float, default=0.90)
    ap.add_argument("--selftest", action="store_true")
    args = ap.parse_args()

    if args.selftest:
        return run_selftest(args.corpus)

    if not args.frames_dir:
        ap.error("--frames-dir required (or --selftest)")

    cache_dir = args.cache_dir or os.path.join(tempfile.gettempdir(), "oo_screen_quality_srccache")
    ensure_source_cache(args.corpus, cache_dir)
    metrics = score_capture_dir(args.frames_dir, cache_dir, args.target_fps, args.ocr_ncc_threshold)
    result = apply_gate(metrics, args.scenario)

    out_path = args.out or os.path.join(args.frames_dir, "quality.json")
    with open(out_path, "w", encoding="utf-8") as f:
        json.dump(result, f, indent=2, ensure_ascii=False)

    print(json.dumps(result, indent=2, ensure_ascii=False))
    print(f"\nQUALITY_SCORE_OK verdict={result['verdict']} out={out_path}")
    return 0 if result["verdict"] == "PASS" else 1


# --------------------------------------------------------------------------
# Selftest — без capture.py: бере source-кадр 100 сам із себе (SSIM=1.0,
# PASS) і той самий кадр після ffmpeg boxblur=10 (SSIM низький, FAIL).
# --------------------------------------------------------------------------

def run_selftest(corpus_path: str) -> int:
    print("=== quality_score.py --selftest ===")
    tmpdir = tempfile.mkdtemp(prefix="oo_screen_quality_selftest_")
    try:
        cache_dir = os.path.join(tmpdir, "srccache")
        ensure_source_cache(corpus_path, cache_dir)

        seq = 100
        clean_path = source_frame_path(cache_dir, seq)
        clean_img = Image.open(clean_path).convert("RGB")

        # (а) чистий кадр проти самого себе -> SSIM ~1.0 -> PASS
        gray_clean = _to_gray(clean_img)
        ssim_clean = compute_ssim(gray_clean, gray_clean)
        print(f"[a] clean frame {seq} vs itself: SSIM={ssim_clean:.4f}")
        pass_a = ssim_clean >= GATES["S1"]["ssim_min"]
        print(f"    verdict: {'PASS' if pass_a else 'FAIL'} (gate >= {GATES['S1']['ssim_min']})")

        # (б) той самий кадр, заблюрений ffmpeg boxblur=10, проти чистого
        blurred_path = os.path.join(tmpdir, "blurred.png")
        cmd = [
            "ffmpeg", "-y", "-i", clean_path,
            "-vf", "boxblur=40:6", blurred_path,
        ]
        proc = subprocess.run(cmd, capture_output=True, text=True)
        if proc.returncode != 0:
            print(f"ffmpeg boxblur failed: {proc.stderr[-1000:]}", file=sys.stderr)
            return 1
        blurred_img = Image.open(blurred_path).convert("RGB")
        ssim_blur = compute_ssim(gray_clean, _to_gray(blurred_img))
        print(f"[b] frame {seq} vs itself boxblur=40:6: SSIM={ssim_blur:.4f}")
        pass_b_expected_fail = ssim_blur < GATES["S1"]["ssim_min"]
        print(f"    verdict: {'FAIL' if pass_b_expected_fail else 'unexpectedly PASS'} "
              f"(gate >= {GATES['S1']['ssim_min']})")

        ok = pass_a and pass_b_expected_fail and (ssim_clean > ssim_blur)
        print()
        print(f"[{'PASS' if pass_a else 'FAIL'}] clean-vs-self SSIM passes gate")
        print(f"[{'PASS' if pass_b_expected_fail else 'FAIL'}] blurred SSIM correctly fails gate")
        print(f"[{'PASS' if ssim_clean > ssim_blur else 'FAIL'}] clean SSIM > blurred SSIM (monotonicity sanity)")

        if not ok:
            print("\nSELFTEST FAILED", file=sys.stderr)
            return 1
        print("\nSELFTEST OK")
        return 0
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
