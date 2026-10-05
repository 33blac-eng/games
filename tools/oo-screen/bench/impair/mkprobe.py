#!/usr/bin/env python3
"""Медіани P1 (проба/пейсинг) по тегах: python3 mkprobe.py results.jsonl.

Назви прогонів: TAG/сценарій/rN (run_network_p0.sh). Для стелі: декодовні %,
фриз с/хв (усі прогони), перший момент ціль <= стелі (first_le_cap_s, якщо
netbench його дає, інакше з ряду ctl), >=90% після зняття (recover90_s),
kf+PLI агенту/хв.
"""
import json
import statistics
import sys
from collections import defaultdict

runs = defaultdict(list)
for line in open(sys.argv[1], encoding="utf-8"):
    line = line.strip()
    if not line:
        continue
    r = json.loads(line)
    parts = r["name"].split("/")
    if len(parts) < 3:
        continue
    runs[(parts[0], parts[1])].append(r)


def med(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs) if xs else None


def le_cap(r, cap):
    # ряд ctl: t — секунди від початку вади (0..imp_s), bps — ціль із цього моменту
    last = None
    for p in r["ctl"]:
        if p["t"] <= 0:
            last = p["bps"]
            continue
        if last is not None and last <= cap:
            return 0.0
        if p["t"] > r["args"]["imp_s"]:
            return None
        if p["bps"] <= cap:
            return p["t"]
        last = p["bps"]
    return None


def fmt(v, f="%.1f"):
    return "ні" if v is None else f % v


print("| тег | сценарій | N | декодовні % | фриз с/хв (усі) | ціль ≤ стелі, с | ≥90% після зняття, с (усі) | kf+PLI/хв | прийнято Мбіт/с |")
print("|---|---|---|---|---|---|---|---|---|")
for (tag, sc), rs in sorted(runs.items(), key=lambda kv: (kv[0][1], kv[0][0])):
    cap = int(sc.split("-")[1]) if sc.startswith("cap-") else 0
    fr = sorted(r["imp"]["freeze_sec_per_min"] for r in rs)
    rec = [r["ctl_imp"]["recover90_s"] for r in rs]
    recv = [x if x >= 0 else None for x in rec]
    rec_med = statistics.median([x if x >= 0 else 1e9 for x in rec])
    print("| %s | %s | %d | %.1f | %.2f (%s) | %s | %s (%s) | %.0f | %.2f |" % (
        tag, sc, len(rs),
        med([r["imp"]["decodable_pct"] for r in rs]),
        med(fr), ", ".join("%.1f" % x for x in fr),
        fmt(med([le_cap(r, cap) for r in rs]), "%.0f"),
        "ні" if rec_med >= 1e9 else "%.0f" % rec_med,
        ", ".join("ні" if x is None else "%.0f" % x for x in recv),
        med([r["pli"]["imp_kf_per_min"] for r in rs]),
        med([r["imp"]["recv_mbps"] for r in rs])))
