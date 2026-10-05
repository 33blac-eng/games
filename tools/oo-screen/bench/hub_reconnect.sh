#!/usr/bin/env bash
# Реконект агента: kill -9 процесу-агента, перезапуск через DELAY с, і скільки
# глядачі ноди чекають до першого ПОВНОГО IDR від нового агента.
#
#   CORPUS=c.h264 ROUNDS=5 DELAY=0 bench/hub_reconnect.sh
#
# Події пише `hubbench watch` (JSON-рядки, unix ns), моменти kill/старту —
# цей скрипт; зведення рахує python3 наприкінці.
set -eu
cd "$(dirname "$0")/.."
BIN=${BIN:-/tmp/hubbench-bin}
CORPUS=${CORPUS:?CORPUS=шлях до .h264}
ROUNDS=${ROUNDS:-5}
DELAY=${DELAY:-0}
PORT=${PORT:-4471}
OUT=${OUT:-/tmp/hubbench-reconnect}
export OO_SCREEN_T1_TOKEN=${OO_SCREEN_T1_TOKEN:-bench-token-not-default}
mkdir -p "$OUT"
rm -f "$OUT"/*.log "$OUT"/events.txt

"$BIN/hubbench" erp -addr 127.0.0.1:4499 >/dev/null 2>&1 &
erp=$!
env OO_SCREEN_HUB_ADDR=":$PORT" OO_SCREEN_ICE_PORT=4800 \
    OO_SCREEN_ERP_BASE=http://127.0.0.1:4499 OO_SCREEN_HUB_KEY=bench-hub-key \
    OO_SCREEN_OFFER_RATE=1000 OO_SCREEN_OFFER_BURST=1000 \
    "$BIN/hub" >"$OUT/hub.log" 2>&1 &
hub=$!
agent=""
watch=""
trap 'kill $hub $erp $watch $agent 2>/dev/null; wait 2>/dev/null' EXIT
sleep 1
H="http://127.0.0.1:$PORT"
now() { date +%s%N; }

start_agent() {
  "$BIN/hubbench" agent -hub "$H" -node rc -corpus "$CORPUS" >>"$OUT/agent.log" 2>&1 &
  agent=$!
}
start_agent
until grep -q agent_streaming "$OUT/agent.log"; do sleep 0.05; done
"$BIN/hubbench" watch -hub "$H" -node rc -viewers 3 -corpus "$CORPUS" >"$OUT/watch.log" 2>&1 &
watch=$!
sleep 6
for r in $(seq "$ROUNDS"); do
  echo "{\"ev\":\"kill\",\"round\":$r,\"t\":$(now)}" >>"$OUT/events.txt"
  kill -9 "$agent"
  wait "$agent" 2>/dev/null || true
  sleep "$DELAY"
  echo "{\"ev\":\"restart\",\"round\":$r,\"t\":$(now)}" >>"$OUT/events.txt"
  start_agent
  sleep 8
done
python3 - "$OUT" <<'PY'
import json, sys, statistics as st
d = sys.argv[1]
ev = [json.loads(l) for l in open(d + "/events.txt")]
st_ag = [json.loads(l)["t"] for l in open(d + "/agent.log") if l.startswith("{")]
w = [json.loads(l) for l in open(d + "/watch.log") if l.startswith("{")]
res = [x for x in w if x["ev"] == "resume_idr"]
states = [x for x in w if x["ev"] == "pc_state"]
rows = []
for k in (e for e in ev if e["ev"] == "kill"):
    rs = next(e for e in ev if e["ev"] == "restart" and e["round"] == k["round"])
    ag = min((t for t in st_ag if t > rs["t"]), default=None)
    nxt = min((e["t"] for e in ev if e["ev"] == "kill" and e["t"] > k["t"]), default=1 << 62)
    per = [x for x in res if rs["t"] < x["t"] < nxt]
    if not per:
        rows.append((k["round"], None, None, None, None)); continue
    idr = max(x["t"] for x in per)          # найгірший з глядачів
    pkt = max(x["resume_pkt"] for x in per)
    rows.append((k["round"], (idr - k["t"]) / 1e6, (idr - rs["t"]) / 1e6,
                 (ag - rs["t"]) / 1e6 if ag else None, (idr - ag) / 1e6 if ag else None))
print("round  kill->IDR_ms  restart->IDR_ms  restart->agent_connected_ms  agent_connected->IDR_ms")
for r in rows:
    print("  ".join("%8s" % ("-" if x is None else ("%.0f" % x if isinstance(x, float) else x)) for x in r))
ok = [r for r in rows if r[2] is not None]
if ok:
    print("median restart->IDR %.0f ms, kill->IDR %.0f ms (DELAY=%s)" % (
        st.median(r[2] for r in ok), st.median(r[1] for r in ok), "see env"))
print("viewer pc_state events:", [(x["viewer"], x["state"]) for x in states])
PY
