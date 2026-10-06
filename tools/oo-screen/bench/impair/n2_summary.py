#!/usr/bin/env python3
"""Зведення N2: freeze с/хв у вікні вади, mean (min-max) по групі (назва без -rN)."""
import json
import re
import statistics as st
import sys

g = {}
for line in open(sys.argv[1]):
    r = json.loads(line)
    g.setdefault(re.sub(r"-r\d+$", "", r["name"]), []).append(r)
print("| сценарій | n | freeze с/хв mean (min-max) | freezes/хв | decodable % | fec_recovered | Мбіт/с |")
print("|---|---|---|---|---|---|---|")
for k in sorted(g):
    rs = g[k]
    f = [x["imp"]["freeze_sec_per_min"] for x in rs]
    m = lambda fn: st.mean(fn(x) for x in rs)
    print(f"| {k} | {len(rs)} | {st.mean(f):.2f} ({min(f):.2f}-{max(f):.2f}) | "
          f"{m(lambda x: x['imp']['freeze_per_min']):.1f} | "
          f"{m(lambda x: x['imp']['decodable_pct']):.1f} | "
          f"{m(lambda x: x.get('fec_recovered', 0)):.0f} | "
          f"{m(lambda x: x['imp']['recv_mbps']):.2f} |")
