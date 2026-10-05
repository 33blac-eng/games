#!/usr/bin/env python3
r"""bench/score.py — скорер матриці Додатка A (T1, bake-off A vs B).

Вхід: директорія прогонів
    results/<S#>/<A|B>/run<n>.ndjson    (n = 1,2,3 — три повтори на трасу/кандидата)

Для кожної (траса, кандидат) пари бере МЕДІАНУ з 3 прогонів по метриках, які
дає bench/analyze.py, звіряє з гейтами Додатка A і будує markdown-таблицю
PASS/FAIL, потім підсумковий зважений бал A проти B.

ВАЖЛИВА ПРИМІТКА T1 (відкритий хвіст R4#4, зафіксовано тут явно):
    Додаток A вимагає "p95 capture→render". У T1 немає справжнього
    camera-capture timestamp (корпус — записаний Annex-B файл, першим-класним
    моментом є t_arrival_ms viewer'а, тобто коли WebRTC/WebTransport стек
    видав кадр з мережі в JS). Тому тут вважаємо:

        capture→render(T1)  :=  arrival→render  =  t_rendered_ms - t_arrival_ms

    Це ЗАНИЖЕНА оцінка справжнього capture→render (не бачить капчер→агент→
    мережа до arrival на viewer, а на форматі A де arrival взагалі не
    пишеться в NDJSON — оцінка відсутня, показуємо null і не рахуємо
    latency-компонент гейта). Справжній end-to-end capture→render вимагає
    hub-side агент-timestamp таврування, це не в T1-обсязі. Гейти
    S1/S2/S3-S6/S7 з Додатка A звіряються проти цієї арival→render проксі —
    результат PASS слід читати як "PASS за проксі-метрикою", не як
    остаточний доказ гейта капчер→рендер.

Формули (докстрінг = джерело правди для скорингу):

    Нехай для кандидата X (A або B) на трасі S:
        lat95(X,S)   = медіана p95(arrival→render) з 3 прогонів
        gate_lat(S)  = поріг p95 з Додатка A для цієї траси
        latency_score(X,S) = clamp(0, 100, 100 * gate_lat(S) / lat95(X,S))
            (менша латентність за той самий поріг = вищий бал; на гейті = 100)

        freeze95(X,S) = медіана freeze.per_min з 3 прогонів
        gate_freeze(S) = поріг freeze/хв з Додатка A (S1-S2: 1, S3-S6: 4;
                         S7/S8 без явного числа в Додатку A -> той самий 4,
                         як для "складних" трас — явно позначено приміткою)
        freeze_score(X,S) = clamp(0, 100, 100 * gate_freeze(S) / max(freeze95(X,S), eps))

        success_score(X,S): для T1 у нас немає живого 30x-reconnect тесту —
            приймається як аргумент --success-a/--success-b (частка, 0..1,
            за трасу; дефолт 1.0 якщо не задано і трасу PASS за іншими
            гейтами, інакше 0.0). Це MANUAL INPUT, не з NDJSON.
            success_score = 100 * success_fraction

        ops_score(X) = 100 * ops_rating(X) / 5   (ops_rating: 1..5 від --ops-a/--ops-b,
            суб'єктивна оцінка складності конфігурації, фіксується письмово
            викликаючим; однакова для всіх трас одного кандидата)

    Підсумковий бал траси:
        trace_score(X,S) = 0.40*latency_score + 0.25*freeze_score
                          + 0.20*success_score + 0.15*ops_score

    Підсумковий бал кандидата:
        total_score(X) = mean_over_traces( trace_score(X,S) )

PASS/FAIL таблиці (окремо від скорингу, PASS-first за Додатком A):
    trace PASS(X,S) = (lat95 <= gate_lat) AND (freeze/хв <= gate_freeze)
    (SSIM/OCR/CPU/memory гейти з Додатка A тут НЕ рахуються — вони поза
    обсягом NDJSON viewer-метрик; позначені як "N/A (T1)" у таблиці.)

Selftest: див. --selftest у analyze.py. Тут для гейту завдання створюється
структура results/ на 3 синтетичні траси, з одним свідомо провальним гейтом
(траса з latency вище порогу), щоб довести, що скорер уміє FAIL.
"""
import sys
import os
import json
import argparse
import statistics
import glob

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import analyze as analyze_mod  # noqa: E402

EPS = 1e-9

# Гейти Додатка A: p95 arrival->render (мс), freeze/хв
GATES = {
    "S1": {"p95_ms": 120, "freeze_per_min": 1, "label": "clean"},
    "S2": {"p95_ms": 120, "freeze_per_min": 1, "label": "loss-low"},
    "S3": {"p95_ms": 200, "freeze_per_min": 4, "label": "loss-mid"},
    "S4": {"p95_ms": 200, "freeze_per_min": 4, "label": "loss-burst"},
    "S5": {"p95_ms": 200, "freeze_per_min": 4, "label": "reorder"},
    "S6": {"p95_ms": 200, "freeze_per_min": 4, "label": "far"},
    "S7": {"p95_ms": 350, "freeze_per_min": 4, "label": "bw-collapse"},
    "S8": {"p95_ms": None, "freeze_per_min": None, "label": "udp-blocked"},
}


def clamp(v, lo, hi):
    return max(lo, min(hi, v))


def median(vals):
    vals = [v for v in vals if v is not None]
    if not vals:
        return None
    return statistics.median(vals)


def load_run_metrics(path):
    with open(path, "r", encoding="utf-8") as f:
        rows = analyze_mod.parse_ndjson(f.read())
    return analyze_mod.analyze(rows)


def collect_trace_candidate(results_dir, trace, candidate):
    """Повертає (p95_median, freeze_per_min_median, n_runs_found)."""
    pattern = os.path.join(results_dir, trace, candidate, "run*.ndjson")
    files = sorted(glob.glob(pattern))
    p95s = []
    freezes = []
    for fp in files:
        m = load_run_metrics(fp)
        p95s.append(m["arrival_to_render_ms"]["p95"])
        fz = m["freeze"]["per_min"] if m["freeze"]["per_min"] is not None else 0.0
        # Оклюзія вікна тротлить rVFC (формат A): rendered-потік голодує при
        # здоровому декодері -> rendered-freeze недійсний, беремо decode-stall
        # (дельти framesDecoded, незалежні від видимості вікна).
        ds = m.get("decode_stall")
        rf = m.get("rendered_fps")
        if ds and isinstance(rf, (int, float)) and ds.get("decoded_fps") and rf < 0.5 * ds["decoded_fps"]:
            fz = ds["decode_freeze_per_min"]
        freezes.append(fz)
    return median(p95s), median(freezes), len(files)


def score_trace(gate, p95_med, freeze_med, success_fraction, ops_rating):
    if gate["p95_ms"] is None:
        latency_score = None
    elif p95_med is None:
        latency_score = None
    else:
        latency_score = clamp(100.0 * gate["p95_ms"] / max(p95_med, EPS), 0.0, 100.0)

    if gate["freeze_per_min"] is None:
        freeze_score = None
    elif freeze_med is None:
        freeze_score = None
    else:
        freeze_score = clamp(100.0 * gate["freeze_per_min"] / max(freeze_med, EPS), 0.0, 100.0)

    success_score = 100.0 * success_fraction
    ops_score = 100.0 * ops_rating / 5.0

    parts = []
    weights = []
    if latency_score is not None:
        parts.append(latency_score); weights.append(0.40)
    if freeze_score is not None:
        parts.append(freeze_score); weights.append(0.25)
    parts.append(success_score); weights.append(0.20)
    parts.append(ops_score); weights.append(0.15)

    wsum = sum(weights)
    trace_score = sum(p * w for p, w in zip(parts, weights)) / wsum if wsum else None

    return {
        "latency_score": latency_score,
        "freeze_score": freeze_score,
        "success_score": success_score,
        "ops_score": ops_score,
        "trace_score": trace_score,
    }


def load_quality_verdict(results_dir, trace, candidate):
    """R4#3 (Додаток A блок ЯКІСТЬ): опційний results/<S>/<cand>/quality.json
    з bench/quality_score.py (SSIM/OCR/distinct-FPS). Якщо файла нема —
    None (якість не рахується, як і раніше — зворотна сумісність з T1
    прогонами, зробленими до цього гейту). Якщо є — PASS-first: FAIL за
    якістю переважає latency/freeze PASS (інакше агресивний frame-drop
    "виграє" гейти огидною картинкою)."""
    path = os.path.join(results_dir, trace, candidate, "quality.json")
    if not os.path.exists(path):
        return None
    try:
        with open(path, "r", encoding="utf-8") as f:
            q = json.load(f)
        return q.get("verdict")
    except (OSError, json.JSONDecodeError):
        return None


def pass_fail(gate, p95_med, freeze_med, quality_verdict=None):
    if quality_verdict == "FAIL":
        return "FAIL (quality)"
    if gate["p95_ms"] is None or gate["freeze_per_min"] is None:
        return "N/A (T1)" if quality_verdict is None else quality_verdict
    if p95_med is None or freeze_med is None:
        return "NO DATA"
    ok = (p95_med <= gate["p95_ms"]) and (freeze_med <= gate["freeze_per_min"])
    return "PASS" if ok else "FAIL"


def build_report(results_dir, traces, candidates, success_by_trace_cand, ops_rating_by_cand):
    rows = []
    cand_totals = {c: [] for c in candidates}
    for trace in traces:
        gate = GATES[trace]
        for cand in candidates:
            p95_med, freeze_med, n = collect_trace_candidate(results_dir, trace, cand)
            quality_verdict = load_quality_verdict(results_dir, trace, cand)
            verdict = pass_fail(gate, p95_med, freeze_med, quality_verdict)
            success_fraction = success_by_trace_cand.get((trace, cand), 1.0 if verdict == "PASS" else 0.0)
            sc = score_trace(gate, p95_med, freeze_med, success_fraction, ops_rating_by_cand[cand])
            if sc["trace_score"] is not None:
                cand_totals[cand].append(sc["trace_score"])
            rows.append({
                "trace": trace, "label": gate["label"], "candidate": cand,
                "n_runs": n, "p95_ms": p95_med, "freeze_per_min": freeze_med,
                "gate_p95_ms": gate["p95_ms"], "gate_freeze_per_min": gate["freeze_per_min"],
                "verdict": verdict, **sc,
            })

    totals = {c: (sum(v) / len(v) if v else None) for c, v in cand_totals.items()}
    return rows, totals


def render_markdown(rows, totals, candidates):
    lines = []
    lines.append("| Trace | Label | Cand | Runs | p95 arr->render (ms) | gate p95 | freeze/min | gate freeze | Verdict | Score |")
    lines.append("|---|---|---|---|---|---|---|---|---|---|")
    for r in rows:
        p95_str = f"{r['p95_ms']:.1f}" if r["p95_ms"] is not None else "-"
        freeze_str = f"{r['freeze_per_min']:.2f}" if r["freeze_per_min"] is not None else "-"
        score_str = f"{r['trace_score']:.1f}" if r["trace_score"] is not None else "-"
        lines.append(
            f"| {r['trace']} | {r['label']} | {r['candidate']} | {r['n_runs']} | "
            f"{p95_str} | {r['gate_p95_ms']} | {freeze_str} | {r['gate_freeze_per_min']} | "
            f"**{r['verdict']}** | {score_str} |"
        )
    lines.append("")
    lines.append("## Підсумковий бал")
    lines.append("")
    lines.append("| Candidate | Total score (mean over traces) |")
    lines.append("|---|---|")
    for c in candidates:
        t = totals.get(c)
        lines.append(f"| {c} | {t:.2f}" + " |" if t is not None else f"| {c} | no data |")
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--results-dir", default="results")
    ap.add_argument("--traces", default=",".join(GATES.keys()))
    ap.add_argument("--candidates", default="A,B")
    ap.add_argument("--ops-a", type=float, default=3.0)
    ap.add_argument("--ops-b", type=float, default=3.0)
    ap.add_argument("--selftest", action="store_true")
    args = ap.parse_args()

    if args.selftest:
        run_selftest()
        return

    traces = args.traces.split(",")
    candidates = args.candidates.split(",")
    ops_rating_by_cand = {"A": args.ops_a, "B": args.ops_b}
    rows, totals = build_report(args.results_dir, traces, candidates, {}, ops_rating_by_cand)
    print(render_markdown(rows, totals, candidates))


# --------------------------------------------------------------------------
# selftest — синтетичні results/ на 3 траси, один гейт свідомо провалено
# --------------------------------------------------------------------------

def _write_synthetic_run(path, seq_count, step_ms, freeze_gap_ms, freeze_at, a2r_ms):
    lines = []
    t = 1000.0
    for i in range(seq_count):
        if i == 0:
            pass
        elif i - 1 == freeze_at:
            t += freeze_gap_ms
        else:
            t += step_ms
        t_arrival = t - a2r_ms
        lines.append(json.dumps({
            "seq": i, "t_arrival_ms": t_arrival, "t_decoded_ms": t_arrival + 1,
            "t_rendered_ms": t, "bytes": 1000, "key": (i == 0),
        }))
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")


def run_selftest():
    import tempfile
    tmpdir = tempfile.mkdtemp(prefix="oo_screen_score_selftest_")
    results_dir = os.path.join(tmpdir, "results")

    # S1 (clean): gate p95<=120ms, freeze<=1/min.
    #   Candidate A: healthy, no freeze, a2r=30ms -> PASS
    #   Candidate B: a2r=150ms (over 120 gate) -> FAIL (свідомо провалено)
    for run in (1, 2, 3):
        _write_synthetic_run(
            os.path.join(results_dir, "S1", "A", f"run{run}.ndjson"),
            seq_count=60, step_ms=16.667, freeze_gap_ms=0, freeze_at=-1, a2r_ms=30.0,
        )
        _write_synthetic_run(
            os.path.join(results_dir, "S1", "B", f"run{run}.ndjson"),
            seq_count=60, step_ms=16.667, freeze_gap_ms=0, freeze_at=-1, a2r_ms=150.0,
        )

    # S3 (loss-mid): gate p95<=200ms, freeze<=4/min. Both candidates healthy PASS.
    for run in (1, 2, 3):
        _write_synthetic_run(
            os.path.join(results_dir, "S3", "A", f"run{run}.ndjson"),
            seq_count=120, step_ms=16.667, freeze_gap_ms=0, freeze_at=-1, a2r_ms=80.0,
        )
        _write_synthetic_run(
            os.path.join(results_dir, "S3", "B", f"run{run}.ndjson"),
            seq_count=120, step_ms=16.667, freeze_gap_ms=0, freeze_at=-1, a2r_ms=90.0,
        )

    traces = ["S1", "S3"]
    candidates = ["A", "B"]
    rows, totals = build_report(results_dir, traces, candidates, {}, {"A": 4.0, "B": 3.0})
    md = render_markdown(rows, totals, candidates)

    print("=== score.py --selftest ===")
    print(md)
    print()

    s1_a = next(r for r in rows if r["trace"] == "S1" and r["candidate"] == "A")
    s1_b = next(r for r in rows if r["trace"] == "S1" and r["candidate"] == "B")
    s3_a = next(r for r in rows if r["trace"] == "S3" and r["candidate"] == "A")

    checks = [
        ("S1/A verdict == PASS", s1_a["verdict"] == "PASS"),
        ("S1/B verdict == FAIL (свідомо провалений гейт)", s1_b["verdict"] == "FAIL"),
        ("S3/A verdict == PASS", s3_a["verdict"] == "PASS"),
        ("S1/A score > S1/B score", s1_a["trace_score"] > s1_b["trace_score"]),
        ("totals present for both candidates", totals["A"] is not None and totals["B"] is not None),
    ]
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


if __name__ == "__main__":
    main()
