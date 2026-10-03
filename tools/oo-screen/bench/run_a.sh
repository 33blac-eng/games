#!/usr/bin/env bash
# bench/run_a.sh — кандидат A (WebRTC/Pion): hub-webrtc + corpus-player-webrtc
# + статика web/ на :4480. Ctrl+C гасить усе.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN_DIR="$(mktemp -d)"
trap 'kill $(jobs -p) 2>/dev/null || true; rm -rf "$BIN_DIR"' EXIT INT TERM

echo "== go build =="
go build -o "$BIN_DIR/hub-webrtc" ./hub/cmd/hub-webrtc
go build -o "$BIN_DIR/corpus-player-webrtc" ./agent/cmd/corpus-player-webrtc

echo "== hub-webrtc :4470 =="
"$BIN_DIR/hub-webrtc" &

sleep 0.5

if ! (exec 3<>/dev/tcp/127.0.0.1/4480) 2>/dev/null; then
  echo "== static web/ :4480 =="
  ( cd web && python3 -m http.server 4480 --bind 127.0.0.1 ) &
else
  exec 3<&- 3>&-
  echo "== :4480 already in use, skipping static server =="
fi

sleep 0.5

echo "== corpus-player-webrtc =="
"$BIN_DIR/corpus-player-webrtc" &

sleep 1
echo
echo "Viewer: http://127.0.0.1:4480/viewer-webrtc.html?token=\${OO_SCREEN_T1_TOKEN:-t1-dev-token}"
echo "Ctrl+C to stop."

wait
