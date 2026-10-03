#!/usr/bin/env bash
# Затримка агент -> глядач, коли агент, глядачі і хаб — окремі процеси на
# окремих ядрах (B7, чистий замір). Свіжі ERP + хаб на кожен прогін.
#
#   bench/hub_lat_split.sh -corpus c.h264 [-viewers 16] [-procs 4] [-dur 30s]
#
# Ядра: HUB_CPUS (хаб сам), AGENT_CPUS, VIEWER_CPUS. Глядачі розкладені на
# PROCS процесів по VIEWERS/PROCS. Підсумок — зведені перцентилі по ВСІХ
# семплах усіх процесів-глядачів.
set -eu
cd "$(dirname "$0")/.."
BIN=${BIN:-/tmp/hubbench-bin}
HUB_CPUS=${HUB_CPUS:-0,1}
AGENT_CPUS=${AGENT_CPUS:-2}
VIEWER_CPUS=${VIEWER_CPUS:-2,3}
PORT=${PORT:-4471}
LOG=${LOG:-/tmp/hubbench-hub.log}
WORK=${WORK:-$(mktemp -d)}
CORPUS="" VIEWERS=16 PROCS=4 DUR=30s
while [ $# -gt 0 ]; do
  case $1 in
    -corpus) CORPUS=$2; shift 2 ;;
    -viewers) VIEWERS=$2; shift 2 ;;
    -procs) PROCS=$2; shift 2 ;;
    -dur) DUR=$2; shift 2 ;;
    *) echo "невідомий прапорець $1" >&2; exit 2 ;;
  esac
done
export OO_SCREEN_T1_TOKEN=${OO_SCREEN_T1_TOKEN:-bench-token-not-default}
mkdir -p "$BIN" "$WORK"
[ -x "$BIN/hub" ] || go build -o "$BIN/hub" ./hub/cmd/hub-webrtc
[ -x "$BIN/hubbench" ] || go build -o "$BIN/hubbench" ./bench/hubbench

"$BIN/hubbench" erp -addr 127.0.0.1:4499 >/dev/null 2>&1 &
erp=$!
env OO_SCREEN_HUB_ADDR=":$PORT" OO_SCREEN_ICE_PORT=4800 \
    OO_SCREEN_ERP_BASE=http://127.0.0.1:4499 OO_SCREEN_HUB_KEY=bench-hub-key \
    OO_SCREEN_PPROF_ADDR=127.0.0.1:6061 OO_SCREEN_OFFER_RATE=1000 OO_SCREEN_OFFER_BURST=1000 \
    ${HUB_ENV:-} taskset -c "$HUB_CPUS" "$BIN/hub" >"$LOG" 2>&1 &
hub=$!
pids="$hub $erp"
trap 'kill $pids 2>/dev/null; wait 2>/dev/null' EXIT
for _ in $(seq 50); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
H="http://127.0.0.1:$PORT"
taskset -c "$AGENT_CPUS" "$BIN/hubbench" agent -hub "$H" -corpus "$CORPUS" -node n000 -sendlog "$WORK/sent.bin" >"$WORK/agent.out" 2>&1 &
pids="$pids $!"
sleep 2
per=$((VIEWERS / PROCS))
vp=""
for i in $(seq "$PROCS"); do
  HUBBENCH_LAT_OUT="$WORK/lat$i.txt" taskset -c "$VIEWER_CPUS" "$BIN/hubbench" latwatch -hub "$H" -corpus "$CORPUS" \
    -node n000 -viewers "$per" -dur "$DUR" -sendlog "$WORK/sent.bin" >"$WORK/watch$i.out" 2>&1 &
  vp="$vp $!"
done
for p in $vp; do wait "$p" || true; done
cat "$WORK"/watch*.out | grep LATWATCH || true
sort -n "$WORK"/lat*.txt | awk '{a[NR]=$1} END {
  if (NR==0) {print "SPLIT no samples"; exit}
  printf "SPLIT viewers='"$VIEWERS"' procs='"$PROCS"' samples=%d p50=%.3f p95=%.3f p99=%.3f max=%.3f\n", NR, a[int(NR*0.50)+1], a[int(NR*0.95)+1], a[int(NR*0.99)+1], a[NR] }'
