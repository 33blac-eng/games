#!/usr/bin/env python3
"""bench/analyze.py — аналізатор NDJSON viewer-метрик (T1, план Додаток A/README).

Вхід (stdin або --file): NDJSON. Два формати, толерантний до обох:

  Формат B (per-frame повний, як описано в README):
    {"seq":123,"t_arrival_ms":F,"t_decoded_ms":F,"t_rendered_ms":F,"bytes":N,"key":false}
    Кожен рядок = один rendered кадр з повним циклом arrival->decoded->rendered.

  Формат A (getStats-стиль): суміш rendered-подій і періодичних stats-семплів
    {"type":"rendered","seq":123,"t_rendered_ms":F,"bytes":N,"key":false}
    {"type":"stats","t_ms":F,"framesDecoded":N,"framesDropped":N}
  (Якщо рядок формату A не має "type", але має t_rendered_ms без t_arrival_ms —
   трактуємо як rendered-подію без arrival-виміру.)

Опційно: --hub HUB.ndjson — hub per-leg лог
  {"leg":"agent"|"viewer","seq":N,"t_ms":...}
  Використовується лише щоб доповнити capture->render оцінку, коли viewer сам
  не має t_arrival_ms (arrival на hub ≈ найближче наближення до capture у T1,
  бо справжнього camera-capture timestamp на записаному корпусі немає — це
  arrival на агента/hub, не capture. Позначаємо приміткою в score.py).

Вихід: JSON на stdout з полями:
  frame_count, rendered_fps
  inter_rendered_ms: {p50,p95,p99,mean,min,max,n}      — міжкадрові інтервали t_rendered
  arrival_to_render_ms: {p50,p95,p99,mean,min,max,n}   — (t_rendered - t_arrival), лише формат B
  freeze: {threshold_ms, episodes, count, total_ms, per_min}
  bytes_total
  format_detected: "A" | "B" | "mixed"
  stats_a: {framesDecoded_last, framesDropped_last, framesDropped_delta} | null

Selftest: `python bench/analyze.py --selftest` генерує синтетичний NDJSON з
відомими властивостями (2 freeze-епізоди, відомі перцентилі) і звіряє, що
analyze() повертає очікувані числа. Ненульовий exit code при розбіжності.
"""
import sys
import json
import argparse
import io

FREEZE_THRESHOLD_MS = 500.0


def _pctl(sorted_vals, p):
    """Перцентиль методом nearest-rank (проста, детермінована, без numpy)."""
    if not sorted_vals:
        return None
    if len(sorted_vals) == 1:
        return sorted_vals[0]
    k = (len(sorted_vals) - 1) * (p / 100.0)
    f = int(k)
    c = min(f + 1, len(sorted_vals) - 1)
    if f == c:
        return sorted_vals[f]
    d = k - f
    return sorted_vals[f] + (sorted_vals[c] - sorted_vals[f]) * d


def _summary(vals):
    if not vals:
        return {"p50": None, "p95": None, "p99": None, "mean": None,
                "min": None, "max": None, "n": 0}
    s = sorted(vals)
    return {
        "p50": _pctl(s, 50),
        "p95": _pctl(s, 95),
        "p99": _pctl(s, 99),
        "mean": sum(s) / len(s),
        "min": s[0],
        "max": s[-1],
        "n": len(s),
    }


def _is_stats_row(row):
    if row.get("type") == "stats":
        return True
    if "framesDecoded" in row or "framesDropped" in row:
        if "t_rendered_ms" not in row:
            return True
    return False


def _is_rendered_row(row):
    if row.get("type") == "rendered":
        return True
    if "t_rendered_ms" in row:
        return True
    return False


def parse_ndjson(text):
    rows = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            rows.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return rows

def decode_stall_stats(rows, freeze_threshold_ms=500.0):
    """Формат A: freeze-оцінка з дельт framesDecoded у 1/с stats-рядках.
    Незалежна від rVFC (той тротлиться при оклюзії вікна — артефакт
    спостерігача, спійманий на S2/A: 35905 декодованих кадрів при 1375
    rendered-рядках). Стала дельта 0 кадрів за >=threshold -> freeze."""
    stats = [(r["t_ms"], r["stats"].get("framesDecoded")) for r in rows
             if isinstance(r.get("stats"), dict) and isinstance(r.get("t_ms"), (int, float))
             and isinstance(r["stats"].get("framesDecoded"), (int, float))]
    if len(stats) < 3:
        return None
    stats.sort()
    episodes = []
    stall_start = None
    for i in range(1, len(stats)):
        dt = stats[i][0] - stats[i-1][0]
        dd = stats[i][1] - stats[i-1][1]
        if dd <= 0:
            if stall_start is None:
                stall_start = stats[i-1][0]
        else:
            if stall_start is not None:
                dur = stats[i-1][0] - stall_start
                if dur >= freeze_threshold_ms:
                    episodes.append(dur)
                stall_start = None
    if stall_start is not None:
        dur = stats[-1][0] - stall_start
        if dur >= freeze_threshold_ms:
            episodes.append(dur)
    span_min = (stats[-1][0] - stats[0][0]) / 60000.0
    total_frames = stats[-1][1] - stats[0][1]
    return {
        "decode_freeze_count": len(episodes),
        "decode_freeze_total_ms": sum(episodes),
        "decode_freeze_per_min": len(episodes) / span_min if span_min > 0 else 0.0,
        "decoded_fps": total_frames / ((stats[-1][0] - stats[0][0]) / 1000.0) if stats[-1][0] > stats[0][0] else 0.0,
    }



def analyze(rows, freeze_threshold_ms=FREEZE_THRESHOLD_MS):
    rendered = [r for r in rows if _is_rendered_row(r)]
    stats_rows = [r for r in rows if _is_stats_row(r)]

    has_full_b = any(("t_arrival_ms" in r) for r in rendered)
    has_a_stats = len(stats_rows) > 0
    if has_full_b and not has_a_stats:
        fmt = "B"
    elif has_a_stats and not has_full_b:
        fmt = "A"
    else:
        fmt = "mixed"

    # null t_rendered_ms трапляється в живих даних (кадр декодовано, але не
    # презентовано на момент запису) — такі рядки не мають часу рендера
    rendered_sorted = sorted(
        (r for r in rendered if isinstance(r.get("t_rendered_ms"), (int, float))),
        key=lambda r: r["t_rendered_ms"],
    )
    t_rendered_list = [r["t_rendered_ms"] for r in rendered_sorted]

    inter = []
    for i in range(1, len(t_rendered_list)):
        inter.append(t_rendered_list[i] - t_rendered_list[i - 1])

    a2r = []
    for r in rendered_sorted:
        if "t_arrival_ms" in r and "t_rendered_ms" in r:
            a2r.append(r["t_rendered_ms"] - r["t_arrival_ms"])
        elif isinstance(r.get("receiveTime"), (int, float)) and isinstance(r.get("presentationTime"), (int, float)):
            # формат A (rVFC metadata): receiveTime = прихід останнього пакета
            # кадру, presentationTime = момент презентації — еквівалентна
            # arrival→render межа для кандидата A (Codex T1 #13 зіставність)
            a2r.append(r["presentationTime"] - r["receiveTime"])

    # freeze episodes: gaps between consecutive rendered frames > threshold
    episodes = []
    for gap in inter:
        if gap > freeze_threshold_ms:
            episodes.append(gap)
    total_ms = sum(episodes)
    duration_ms = (t_rendered_list[-1] - t_rendered_list[0]) if len(t_rendered_list) >= 2 else 0.0
    duration_min = duration_ms / 60000.0 if duration_ms > 0 else None
    per_min = (len(episodes) / duration_min) if duration_min else None

    bytes_total = sum(r.get("bytes", 0) or 0 for r in rendered)

    rendered_fps = None
    if duration_ms > 0:
        rendered_fps = (len(t_rendered_list) - 1) / (duration_ms / 1000.0)

    stats_a = None
    if stats_rows:
        decoded_vals = [r.get("framesDecoded") for r in stats_rows if r.get("framesDecoded") is not None]
        dropped_vals = [r.get("framesDropped") for r in stats_rows if r.get("framesDropped") is not None]
        stats_a = {
            "framesDecoded_last": decoded_vals[-1] if decoded_vals else None,
            "framesDropped_last": dropped_vals[-1] if dropped_vals else None,
            "framesDropped_delta": (dropped_vals[-1] - dropped_vals[0]) if len(dropped_vals) >= 2 else None,
        }

    return {
        "frame_count": len(t_rendered_list),
        "rendered_fps": rendered_fps,
        "inter_rendered_ms": _summary(inter),
        "arrival_to_render_ms": _summary(a2r),
        "freeze": {
            "threshold_ms": freeze_threshold_ms,
            "episodes": episodes,
            "count": len(episodes),
            "total_ms": total_ms,
            "per_min": per_min,
        },
        "bytes_total": bytes_total,
        "format_detected": fmt,
        "decode_stall": decode_stall_stats(rows, freeze_threshold_ms),
        "stats_a": stats_a,
    }


# --------------------------------------------------------------------------
# selftest
# --------------------------------------------------------------------------

def _make_synthetic_ndjson():
    """
    Синтетичний потік формату B, 60 кадрів @ ~16.667мс (60fps), плюс:
      - 2 свідомі freeze-епізоди: розрив 600мс (кадр 20->21) і 900мс (кадр 40->41)
      - arrival_to_render: конструюємо так, щоб медіана = 30, а max = 200
        (останній кадр перед кожним freeze має підвищену затримку рендеру)
    Повертає (text, expected) де expected — очікувані числа для звірки.
    """
    lines = []
    t = 0.0
    seq = 0
    a2r_values = []
    inter_values = []
    prev_t = None
    FREEZE_AT = {20: 600.0, 40: 900.0}  # gap AFTER this seq index (0-based) to next
    base_step = 1000.0 / 60.0  # 16.667ms

    for i in range(60):
        if i == 0:
            t = 1000.0
        else:
            gap = FREEZE_AT.get(i - 1, base_step)
            t += gap
        # arrival is base_step*0.5 before render normally, but for the frame
        # right after a freeze gap, arrival->render latency is larger.
        if i - 1 in FREEZE_AT:
            a2r = 200.0
        else:
            a2r = 25.0 + (i % 3) * 5.0  # varies 25,30,35 -> median 30
        t_arrival = t - a2r
        lines.append(json.dumps({
            "seq": seq, "t_arrival_ms": t_arrival, "t_decoded_ms": t_arrival + 2,
            "t_rendered_ms": t, "bytes": 1200 + (seq % 5) * 10, "key": (seq % 30 == 0),
        }))
        a2r_values.append(a2r)
        if prev_t is not None:
            inter_values.append(t - prev_t)
        prev_t = t
        seq += 1

    text = "\n".join(lines) + "\n"

    expected_freeze_count = 2
    expected_freeze_total = 600.0 + 900.0
    a2r_sorted = sorted(a2r_values)
    expected_a2r_p50 = _pctl(a2r_sorted, 50)
    expected_a2r_max = max(a2r_values)
    expected_frame_count = 60

    return text, {
        "frame_count": expected_frame_count,
        "freeze_count": expected_freeze_count,
        "freeze_total_ms": expected_freeze_total,
        "a2r_p50": expected_a2r_p50,
        "a2r_max": expected_a2r_max,
    }


def run_selftest():
    text, expected = _make_synthetic_ndjson()
    rows = parse_ndjson(text)
    result = analyze(rows)

    checks = []
    checks.append(("format_detected == B", result["format_detected"] == "B"))
    checks.append(("frame_count", result["frame_count"] == expected["frame_count"]))
    checks.append(("freeze.count == 2", result["freeze"]["count"] == expected["freeze_count"]))
    checks.append(("freeze.total_ms", abs(result["freeze"]["total_ms"] - expected["freeze_total_ms"]) < 1e-6))
    checks.append(("arrival_to_render.p50", abs(result["arrival_to_render_ms"]["p50"] - expected["a2r_p50"]) < 1e-6))
    checks.append(("arrival_to_render.max", abs(result["arrival_to_render_ms"]["max"] - expected["a2r_max"]) < 1e-6))
    checks.append(("bytes_total > 0", result["bytes_total"] > 0))
    checks.append(("rendered_fps not None", result["rendered_fps"] is not None))

    print("=== analyze.py --selftest ===")
    print(json.dumps(result, indent=2))
    print()
    ok = True
    for name, passed in checks:
        status = "PASS" if passed else "FAIL"
        if not passed:
            ok = False
        print(f"[{status}] {name}")

    if not ok:
        print("\nSELFTEST FAILED", file=sys.stderr)
        sys.exit(1)
    print("\nSELFTEST OK")
    sys.exit(0)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--file", help="шлях до NDJSON viewer-метрик (за замовчуванням stdin)")
    ap.add_argument("--hub", help="опційний hub NDJSON лог (per-leg arrival/viewer)")
    ap.add_argument("--freeze-threshold-ms", type=float, default=FREEZE_THRESHOLD_MS)
    ap.add_argument("--selftest", action="store_true", help="запустити вбудований selftest на синтетичних даних")
    args = ap.parse_args()

    if args.selftest:
        run_selftest()
        return

    if args.file:
        with io.open(args.file, "r", encoding="utf-8") as f:
            text = f.read()
    else:
        text = sys.stdin.read()

    rows = parse_ndjson(text)
    result = analyze(rows, freeze_threshold_ms=args.freeze_threshold_ms)

    if args.hub:
        with io.open(args.hub, "r", encoding="utf-8") as f:
            hub_rows = parse_ndjson(f.read())
        agent_arrival = {r["seq"]: r["t_ms"] for r in hub_rows if r.get("leg") == "agent" and "seq" in r}
        result["hub_agent_arrival_samples"] = len(agent_arrival)
        result["hub_note"] = (
            "Hub 'agent' leg t_ms — момент прийому кадру hub-ом від агента, "
            "НЕ справжній camera-capture timestamp (його в T1-корпусі немає). "
            "Використовується як найближче наближення capture->render; "
            "справжній capture->render гейт з Додатка A недоступний у T1."
        )

    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
