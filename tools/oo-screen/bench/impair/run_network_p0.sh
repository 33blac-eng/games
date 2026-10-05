#!/usr/bin/env bash
# Перезамір P0 (B4/B5): «живий енкодер» замість грубої драбини.
#
#   WORK=/tmp/nbp0 [HUB_BIN=...] [TAG=after] [REPS=3] bash bench/impair/run_network_p0.sh [фільтр-regex]
#
# Відмінності від run_network.sh (див. RESULTS-network.md, «Після виправлень P0»):
#   - 16 щаблів ~x1.2 від 0.5 до 8 Мбіт/с замість 5 щаблів x2;
#   - OO_CORPUS_LADDER_ALIGNED=1: ціль перемикає щабель на НАСТУПНОМУ природному
#     IDR (GOP 2 с), без додаткового IDR і стрибка контенту — як справжній
#     агент після P0 (SetBitrate без ForceIDR);
#   - кожен сценарій REPS разів (назва-rN), таблиця — медіана.
# HUB_BIN — інший хаб (напр. зібраний із базового коміту) для порівняння на тому
# самому стенді; TAG потрапляє в назви прогонів.
set -eu
cd "$(dirname "$0")/../.."
WORK="${WORK:?WORK=тека}"
FILTER="${1:-.}"
TAG="${TAG:-after}"
REPS="${REPS:-3}"
mkdir -p "$WORK/bin" "$WORK/logs"
go build -o "$WORK/bin/netbench" ./bench/impair/cmd/netbench
go build -o "$WORK/bin/hub-webrtc" ./hub/cmd/hub-webrtc
go build -o "$WORK/bin/corpus-player-webrtc" ./agent/cmd/corpus-player-webrtc
HUB="${HUB_BIN:-$WORK/bin/hub-webrtc}"
export LADDER_BPS="500000 600000 720000 864000 1037000 1244000 1493000 1792000 2150000 2580000 3096000 3715000 4458000 5350000 6420000 8000000"
bash bench/impair/mkcorpus.sh "$WORK/corpus" 10 >/dev/null
C="$WORK/corpus"
LADDER=""
for b in $LADDER_BPS; do LADDER="${LADDER:+$LADDER,}$b=$C/ladder-$b.h264"; done
export OO_CORPUS_LADDER_ALIGNED=1

port=5481
run() {
  local name="$1"; shift
  echo "$name" | grep -Eq "$FILTER" || return 0
  for r in $(seq 1 "$REPS"); do
    port=$((port+1))
    "$WORK/bin/netbench" -hub-bin "$HUB" -agent-bin "$WORK/bin/corpus-player-webrtc" \
      -ladder "$LADDER" -port $port -udp-min $((20000 + (port-5481)*50)) \
      -name "$TAG/$name/r$r" -out "$WORK/results.jsonl" -logdir "$WORK/logs/$TAG/$name/r$r" "$@" \
      || echo "FAILED $TAG/$name/r$r"
  done
}

for c in 2000000 4000000 8000000; do run "cap-$c" -rtt 40ms -cap $c -imp 40s -post 60s; done
for l in 0 0.01 0.02 0.05 0.10; do run "loss-$l" -loss $l -rtt 20ms; done
run "rtt-200ms" -rtt 200ms -loss 0.01
run "rtt-200ms-nack20" -rtt 200ms -loss 0.01 -nack-interval 20ms
run "loss-0.10-nack20" -loss 0.10 -rtt 20ms -nack-interval 20ms
run "jitter-30ms" -rtt 40ms -jitter 30ms
echo NETWORK_P0_DONE
