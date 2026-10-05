#!/usr/bin/env bash
# Офіційні прогони S1 (clean, RTT~0 локально): 3×10хв на кандидата, ПОСЛІДОВНО.
# Використання: bash bench/run_s1.sh [seconds]  (дефолт 600)
set -u
cd "$(dirname "$0")/.."
SECS="${1:-600}"
TMPD="${TEMP:-/tmp}"
go build -o "$TMPD/hub-wt.exe" ./hub/cmd/hub-wt
go build -o "$TMPD/player-wt.exe" ./agent/cmd/corpus-player-wt
go build -o "$TMPD/hub-webrtc.exe" ./hub/cmd/hub-webrtc
go build -o "$TMPD/player-webrtc.exe" ./agent/cmd/corpus-player-webrtc

# статик-сервер, якщо ще не слухає
if ! netstat -ano | grep -q ":4480 .*LISTEN"; then
  (python -m http.server 4480 --directory web >/dev/null 2>&1 &)
  sleep 1
fi

stop_all() { for p in "$@"; do kill "$p" 2>/dev/null; done; sleep 1; }

for run in 1 2 3; do
  echo "=== S1 B run$run ==="
  "$TMPD/hub-wt.exe" > "$TMPD/s1-hubwt-$run.log" 2>&1 & HB=$!
  sleep 1
  "$TMPD/player-wt.exe" > /dev/null 2>&1 & PB=$!
  sleep 2
  CH=$(grep -o 'CERT_HASH=.*' "$TMPD/s1-hubwt-$run.log" | head -1 | cut -d= -f2-)
  CHENC=$(python -c "import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1],safe=''))" "$CH")
  python bench/capture.py --candidate B --seconds "$SECS" \
    --url "http://localhost:4480/viewer-wt.html?token=t1-dev-token&certhash=$CHENC" \
    --out "results/S1/B/run$run.ndjson" || echo "B run$run FAILED"
  stop_all "$HB" "$PB"
done

for run in 1 2 3; do
  echo "=== S1 A run$run ==="
  "$TMPD/hub-webrtc.exe" > "$TMPD/s1-hubrtc-$run.log" 2>&1 & HA=$!
  sleep 1
  "$TMPD/player-webrtc.exe" > /dev/null 2>&1 & PA=$!
  sleep 2
  python bench/capture.py --candidate A --seconds "$SECS" \
    --url "http://localhost:4480/viewer-webrtc.html?token=t1-dev-token" \
    --out "results/S1/A/run$run.ndjson" || echo "A run$run FAILED"
  stop_all "$HA" "$PA"
done
echo S1_RUNS_DONE
