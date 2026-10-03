#!/usr/bin/env python3
"""Markdown-таблиці з results.jsonl netbench (останній запис на назву)."""
import json
import sys

rows = {}
for line in open(sys.argv[1], encoding="utf-8"):
    line = line.strip()
    if line:
        r = json.loads(line)
        rows[r["name"]] = r


def mb(b):
    return "%.2f" % (b / 1e6)


def rec(v):
    return "—" if v < 0 else "%.0f" % v


print("| сценарій | кадри повні % | декодовні % | фриз с/хв | фризів/хв | макс. розрив мс | лат. p50/p95 мс | прийнято Мбіт/с | NACK віднов. % (p95 мс) | kf+PLI агенту /хв | PLI viewer /хв |")
print("|---|---|---|---|---|---|---|---|---|---|---|")
for n, r in rows.items():
    i, nk, p = r["imp"], r["nack"], r["pli"]
    nackc = "—" if nk["shim_dropped"] == 0 else "%.1f (%.0f)" % (nk["recovery_pct"], nk["rec_p95_ms"])
    print("| %s | %.1f | %.1f | %.2f | %.1f | %.0f | %.0f / %.0f | %.2f | %s | %.1f | %.1f |" % (
        n, i["complete_pct"], i["decodable_pct"], i["freeze_sec_per_min"], i["freeze_per_min"],
        i["MaxGapMs"], i["LatP50Ms"], i["LatP95Ms"], i["recv_mbps"], nackc,
        p["imp_kf_per_min"], p["imp_viewer_pli_per_min"]))

print()
print("| сценарій | змін цілі за ваду | перший зріз с | мін. Мбіт/с | ціль на кінці вади | середнє ост. 10 с | ≥90% стелі через, с | ціль на кінці post | черга-дропів | post: декодовні % / фриз с/хв |")
print("|---|---|---|---|---|---|---|---|---|---|")
for n, r in rows.items():
    c, po = r["ctl_imp"], r["post"]
    print("| %s | %d | %s | %s | %s | %s | %s | %s | %d | %.1f / %.2f |" % (
        n, c["changes"], rec(c["first_cut_s"]), mb(c["min_bps"]), mb(c["end_bps"]), mb(c["last10_mean_bps"]),
        rec(c["recover90_s"]), mb(c["post_final_bps"]), r["nack"]["queue_drops"],
        po["decodable_pct"], po["freeze_sec_per_min"]))

if len(sys.argv) > 2:
    print()
    for n in sys.argv[2:]:
        if n in rows:
            pts = " ".join("%.0fs:%s" % (p["t"], mb(p["bps"])) for p in rows[n]["ctl"])
            print("- `%s`: %s" % (n, pts))
