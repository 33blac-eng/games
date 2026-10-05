#!/usr/bin/env bash
# Свіжий fake-ERP + hub-webrtc на кожен прогін, потім bench/hubbench <режим>.
#
#   bench/hub_bench.sh scale -corpus c.h264 -nodes 10 -viewers 4
#   HUB_ENV="OO_SCREEN_GOP_SPAN=12s" bench/hub_bench.sh ttff -corpus g10.h264
#
# Хаб і вимірювач розведені по ядрах (taskset): HUB_CPUS / CLIENT_CPUS. Інакше
# клієнт, що сам платить за SRTP на кожну ногу, краде CPU у хаба і стеля
# виходить стелею пари, а не хаба.
set -eu
cd "$(dirname "$0")/.."
BIN=${BIN:-/tmp/hubbench-bin}
HUB_CPUS=${HUB_CPUS:-0,1}
CLIENT_CPUS=${CLIENT_CPUS:-2,3}
PORT=${PORT:-4471}
LOG=${LOG:-/tmp/hubbench-hub.log}
export OO_SCREEN_T1_TOKEN=${OO_SCREEN_T1_TOKEN:-bench-token-not-default}

mkdir -p "$BIN"
[ -x "$BIN/hub" ] || go build -o "$BIN/hub" ./hub/cmd/hub-webrtc
[ -x "$BIN/hubbench" ] || go build -o "$BIN/hubbench" ./bench/hubbench

"$BIN/hubbench" erp -addr 127.0.0.1:4499 >/dev/null 2>&1 &
erp=$!
env OO_SCREEN_HUB_ADDR=":$PORT" OO_SCREEN_ICE_PORT=4800 \
    OO_SCREEN_ERP_BASE=http://127.0.0.1:4499 OO_SCREEN_HUB_KEY=bench-hub-key \
    OO_SCREEN_PPROF_ADDR=127.0.0.1:6061 OO_SCREEN_OFFER_RATE=1000 OO_SCREEN_OFFER_BURST=1000 \
    ${HUB_ENV:-} taskset -c "$HUB_CPUS" "$BIN/hub" >"$LOG" 2>&1 &
hub=$!
trap 'kill $hub $erp 2>/dev/null; wait 2>/dev/null' EXIT
for _ in $(seq 50); do
  curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
mode=$1; shift
taskset -c "$CLIENT_CPUS" "$BIN/hubbench" "$mode" -hub "http://127.0.0.1:$PORT" -pid "$hub" -pprof 127.0.0.1:6061 "$@"
