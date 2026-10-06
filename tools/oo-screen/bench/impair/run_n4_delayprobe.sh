#!/usr/bin/env bash
# N4 з детектором затримки (хвиля 8): перемежовані прогони двох хабів на стелі
# з пробою і детектором (OO_SCREEN_PROBE=1, OO_SCREEN_DELAYBWE=1, глядач -twcc).
#
#   WORK=/tmp/n4 BASE_HUB=... FIX_HUB=... [REPS=4] [CAPS="2000000 4000000 8000000"] \
#     bash bench/impair/run_n4_delayprobe.sh
#   python3 bench/impair/n4_summary.py $WORK/results.jsonl
#
# Стенд той самий, що run_network_p0.sh (драбина 16 щаблів, IDR/2 с, черга
# 100 мс, RTT 40 мс, стеля 40 с + 60 с після). BASE_HUB/FIX_HUB — зібрані
# hub-webrtc двох комітів; без них обидва = поточний код.
set -eu
cd "$(dirname "$0")/../.."
WORK="${WORK:?WORK=тека}"
REPS="${REPS:-4}"
CAPS="${CAPS:-2000000 4000000 8000000}"
mkdir -p "$WORK/bin" "$WORK/logs"
go build -o "$WORK/bin/netbench" ./bench/impair/cmd/netbench
go build -o "$WORK/bin/hub-webrtc" ./hub/cmd/hub-webrtc
go build -o "$WORK/bin/corpus-player-webrtc" ./agent/cmd/corpus-player-webrtc
BASE_HUB="${BASE_HUB:-$WORK/bin/hub-webrtc}"
FIX_HUB="${FIX_HUB:-$WORK/bin/hub-webrtc}"
export LADDER_BPS="500000 600000 720000 864000 1037000 1244000 1493000 1792000 2150000 2580000 3096000 3715000 4458000 5350000 6420000 8000000"
bash bench/impair/mkcorpus.sh "$WORK/corpus" 10 >/dev/null
LADDER=""
for b in $LADDER_BPS; do LADDER="${LADDER:+$LADDER,}$b=$WORK/corpus/ladder-$b.h264"; done
export OO_CORPUS_LADDER_ALIGNED=1
port=7400
for r in $(seq 1 "$REPS"); do
  for cap in $CAPS; do
    for v in base fix; do
      port=$((port+1))
      hub="$BASE_HUB"; [ "$v" = fix ] && hub="$FIX_HUB"
      "$WORK/bin/netbench" -hub-bin "$hub" -agent-bin "$WORK/bin/corpus-player-webrtc" -ladder "$LADDER" \
        -port $port -udp-min $((30000 + (port % 500) * 50)) -name "i$v-r$r/cap-$cap/r1" -out "$WORK/results.jsonl" \
        -logdir "$WORK/logs/i$v-r$r/cap-$cap" -twcc -hub-env OO_SCREEN_PROBE=1,OO_SCREEN_DELAYBWE=1 \
        -rtt 40ms -cap "$cap" -imp 40s -post 60s || echo "FAILED i$v-r$r/cap-$cap"
    done
  done
done
echo N4_DELAYPROBE_DONE
