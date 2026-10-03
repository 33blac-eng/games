#!/usr/bin/env bash
# Матриця RESULTS-network.md. Linux, без root: вади — in-process реле
# bench/impair (tc/netem у контейнері недоступні: немає iproute2 і NET_ADMIN).
#
#   WORK=/tmp/nb bash bench/impair/run_network.sh [фільтр-regex]
#
# Збирає бінарі, генерує корпус-драбину (ffmpeg/libx264), ганяє сценарії
# послідовно (кожен — свій хаб на своєму порту) і дописує $WORK/results.jsonl.
# Таблиці: python3 bench/impair/mktables.py $WORK/results.jsonl
set -eu
cd "$(dirname "$0")/../.."
WORK="${WORK:?WORK=тека для бінарів/корпусу/логів}"
FILTER="${1:-.}"
mkdir -p "$WORK/bin" "$WORK/logs"
go build -o "$WORK/bin/netbench" ./bench/impair/cmd/netbench
go build -o "$WORK/bin/hub-webrtc" ./hub/cmd/hub-webrtc
go build -o "$WORK/bin/corpus-player-webrtc" ./agent/cmd/corpus-player-webrtc
bash bench/impair/mkcorpus.sh "$WORK/corpus" 10 >/dev/null
C="$WORK/corpus"
LADDER="500000=$C/ladder-500000.h264,1000000=$C/ladder-1000000.h264,2000000=$C/ladder-2000000.h264,4000000=$C/ladder-4000000.h264,8000000=$C/ladder-8000000.h264"

port=4481
run() {
  local name="$1"; shift
  echo "$name" | grep -Eq "$FILTER" || return 0
  port=$((port+1))
  "$WORK/bin/netbench" -hub-bin "$WORK/bin/hub-webrtc" -agent-bin "$WORK/bin/corpus-player-webrtc" \
    -ladder "$LADDER" -port $port -udp-min $((4800 + (port-4481)*50)) \
    -name "$name" -out "$WORK/results.jsonl" -logdir "$WORK/logs/$name" "$@" \
    || echo "FAILED $name"
}

# Втрати (рівномірні, обидва боки), RTT 20 мс
for l in 0 0.005 0.01 0.02 0.05 0.10; do run "loss-$l" -loss $l -rtt 20ms; done
# RTT при 1% втрат
for r in 0 20ms 50ms 100ms 200ms; do run "rtt-$r" -rtt $r -loss 0.01; done
# Джитер (RTT 40 мс, без втрат)
for j in 0 10ms 30ms; do run "jitter-$j" -rtt 40ms -jitter $j; done
# Стеля смуги hub->viewer (RTT 40 мс), довгий хвіст — відновлення
for c in 2000000 4000000 8000000; do
  run "cap-$c" -rtt 40ms -cap $c -imp 40s -post 60s
  run "cap-$c-fastup" -rtt 40ms -cap $c -imp 40s -post 60s -fastup
done
# Burst (Gilbert-Elliott), RTT 40 мс: ~2% і ~5% середніх втрат пачками
run "burst-2" -rtt 40ms -ge-p 0.005 -ge-r 0.25
run "burst-5" -rtt 40ms -ge-p 0.0125 -ge-r 0.25
run "burst-5-long" -rtt 40ms -ge-p 0.005 -ge-r 0.1
# Перестановка 2% (RTT 40 мс)
run "reorder-2" -rtt 40ms -reorder 0.02
# Відновлення після втрат 10% (контролер зрізав) — з/без fastup
run "rec-loss10" -rtt 40ms -loss 0.10 -imp 20s -post 90s
run "rec-loss10-fastup" -rtt 40ms -loss 0.10 -imp 20s -post 90s -fastup
# NACK-генератор viewer-а 20 мс замість дефолтних pion 100 мс (ближче до браузера)
run "loss-0.05-nack20" -loss 0.05 -rtt 20ms -nack-interval 20ms
run "rtt-100ms-nack20" -loss 0.01 -rtt 100ms -nack-interval 20ms
run "burst-5-nack20" -rtt 40ms -ge-p 0.0125 -ge-r 0.25 -nack-interval 20ms
run "rtt-200ms-nack20" -loss 0.01 -rtt 200ms -nack-interval 20ms
run "loss-0.10-nack20" -loss 0.10 -rtt 20ms -nack-interval 20ms
echo NETWORK_MATRIX_DONE
