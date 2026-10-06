#!/usr/bin/env python3
"""N4 з детектором затримки: python3 n4_summary.py results.jsonl.

Назви прогонів: i<варіант>-r<N>/cap-<bps>/r1 (run_n4_delayprobe.sh; варіанти
перемежовано на одному стенді). Для кожної стелі/варіанта: ≥90% після зняття
стелі (recover90_s, усі прогони і максимум), фриз під стелею, прийнято, декодовні.
"""
import json,sys,statistics as st
from collections import defaultdict
g=defaultdict(list)
for l in open(sys.argv[1]):
    r=json.loads(l); n=r['name']
    if not n.startswith('i'): continue
    tag=n.split('/')[0].split('-')[0][1:]; sc=n.split('/')[1]
    g[(sc,tag)].append(r)
print("| сценарій | варіант | N | ≥90% після зняття, с (усі) | макс | фриз с/хв (усі) | прийнято Мбіт/с | декодовні % |")
print("|---|---|---|---|---|---|---|---|")
for (sc,tag),rs in sorted(g.items(), key=lambda k:(int(k[0][0].split('-')[1]),k[0][1])):
    rec=[r['ctl_imp']['recover90_s'] for r in rs]; fr=[r['imp']['freeze_sec_per_min'] for r in rs]
    recv=[r['imp']['recv_mbps'] for r in rs]; dec=[r['imp']['decodable_pct'] for r in rs]
    f=lambda x:"ні" if x<0 else "%.1f"%x
    print("| %s | %s | %d | %s (%s) | %s | %.2f (%s) | %.2f | %.1f |"%(sc,tag,len(rs),f(st.median(rec)),", ".join(f(x) for x in rec),f(max(rec)),st.median(fr),", ".join("%.1f"%x for x in fr),st.median(recv),st.median(dec)))
