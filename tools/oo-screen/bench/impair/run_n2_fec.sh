#!/usr/bin/env bash
# N2: RTT 200 мс, 1% і 2% рівномірних втрат, з FEC і без, RUNS прогонів кожен.
#   WORK=/tmp/n2 RUNS=3 bash bench/impair/run_n2_fec.sh [фільтр-regex]
# FECENV — env хаба для FEC-прогонів (дефолт OO_SCREEN_FEC=1), TAG — суфікс назви.
# Зведення: python3 bench/impair/n2_summary.py $WORK/results.jsonl
set -eu
cd "$(dirname "$0")/../.."
WORK="${WORK:?WORK=тека}"
RUNS="${RUNS:-3}"
FILTER="${1:-.}"
FECENV="${FECENV:-OO_SCREEN_FEC=1}"
TAG="${TAG:-}"
mkdir -p "$WORK/bin" "$WORK/logs"
go build -o "$WORK/bin/netbench" ./bench/impair/cmd/netbench
go build -o "$WORK/bin/hub-webrtc" ./hub/cmd/hub-webrtc
go build -o "$WORK/bin/corpus-player-webrtc" ./agent/cmd/corpus-player-webrtc
[ -f "$WORK/corpus/ladder-8000000.h264" ] || bash bench/impair/mkcorpus.sh "$WORK/corpus" 10 >/dev/null
C="$WORK/corpus"
LADDER="500000=$C/ladder-500000.h264,1000000=$C/ladder-1000000.h264,2000000=$C/ladder-2000000.h264,4000000=$C/ladder-4000000.h264,8000000=$C/ladder-8000000.h264"
port=${PORT0:-5481}
run() {
  local name="$1"; shift
  echo "$name" | grep -Eq "$FILTER" || return 0
  port=$((port+1))
  "$WORK/bin/netbench" -hub-bin "$WORK/bin/hub-webrtc" -agent-bin "$WORK/bin/corpus-player-webrtc" \
    -ladder "$LADDER" -port $port -udp-min $((9000 + (port-5481)*40)) \
    -name "$name" -out "$WORK/results.jsonl" -logdir "$WORK/logs/$name" "$@" \
    || echo "FAILED $name"
}
for i in $(seq 1 "$RUNS"); do
  for l in 0.01 0.02; do
    run "n2-l$l-nofec$TAG-r$i" -rtt 200ms -loss $l -imp 60s
    run "n2-l$l-fec$TAG-r$i" -rtt 200ms -loss $l -imp 60s -fec -hub-env "$FECENV"
  done
done
echo N2_DONE
