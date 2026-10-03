#!/usr/bin/env python3
"""bench/mkresults.py — утиліта: створює results/<S#>/<A|B>/run<n>.ndjson
структуру, очікувану score.py, і копіює туди NDJSON-файл, завантажений з
браузера (кнопка "Download metrics" з README.md).

Використання:
    python bench/mkresults.py --scenario S3 --candidate B --run 2 \
        --src "C:\\Users\\Administrator\\Downloads\\metrics.ndjson"

    (опційно) --results-dir results   (за замовчуванням "results" у cwd)
    (опційно) --force                 перезаписати, якщо файл вже існує

Валідація:
    --scenario має бути S1..S8 (як у Додатку A);
    --candidate має бути A або B;
    --run — ціле число >=1 (План: 3 повтори на трасу/кандидата, 1..3, але
    утиліта не обмежує зверху — індекси прогонів вільні для повторних
    e2e-запусків).
    --src має існувати і бути файлом (не директорією); умисно НЕ парсимо
    NDJSON тут — це відповідальність analyze.py/score.py.
"""
import argparse
import os
import shutil
import sys
import re

VALID_SCENARIOS = {f"S{i}" for i in range(1, 9)}
VALID_CANDIDATES = {"A", "B"}


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--scenario", required=True, help="S1..S8 (Додаток A)")
    ap.add_argument("--candidate", required=True, help="A або B")
    ap.add_argument("--run", required=True, type=int, help="номер прогону (>=1)")
    ap.add_argument("--src", required=True, help="шлях до завантаженого NDJSON-файлу")
    ap.add_argument("--results-dir", default="results")
    ap.add_argument("--force", action="store_true", help="перезаписати наявний run-файл")
    args = ap.parse_args()

    scenario = args.scenario.upper()
    candidate = args.candidate.upper()

    if scenario not in VALID_SCENARIOS:
        print(f"error: --scenario має бути одне з {sorted(VALID_SCENARIOS)}, отримано {args.scenario!r}", file=sys.stderr)
        sys.exit(2)
    if candidate not in VALID_CANDIDATES:
        print(f"error: --candidate має бути A або B, отримано {args.candidate!r}", file=sys.stderr)
        sys.exit(2)
    if args.run < 1:
        print(f"error: --run має бути >=1, отримано {args.run}", file=sys.stderr)
        sys.exit(2)
    if not os.path.isfile(args.src):
        print(f"error: --src не знайдено або не файл: {args.src}", file=sys.stderr)
        sys.exit(2)

    dest_dir = os.path.join(args.results_dir, scenario, candidate)
    os.makedirs(dest_dir, exist_ok=True)
    dest_path = os.path.join(dest_dir, f"run{args.run}.ndjson")

    if os.path.exists(dest_path) and not args.force:
        print(f"error: {dest_path} вже існує (передай --force для перезапису)", file=sys.stderr)
        sys.exit(1)

    shutil.copyfile(args.src, dest_path)
    print(f"OK: {args.src} -> {dest_path}")


if __name__ == "__main__":
    main()
