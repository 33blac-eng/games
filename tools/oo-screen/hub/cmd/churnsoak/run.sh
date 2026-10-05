#!/usr/bin/env bash
# Стиснутий soak хаба (R5): свіжий hub-webrtc у ticket-режимі на окремих портах
# + churnsoak (фейковий ERP, агенти/глядачі з високим темпом).
#   DUR=2h OUT=results/churnsoak hub/cmd/churnsoak/run.sh
# Решта прапорців churnsoak — після «--».
set -eu
cd "$(dirname "$0")/../../.."
DUR=${DUR:-10m}
OUT=${OUT:-results/churnsoak}
BIN=${BIN:-build/churnsoak}
mkdir -p "$OUT" "$BIN"
go build -o "$BIN/hub-webrtc" ./hub/cmd/hub-webrtc
go build -o "$BIN/churnsoak" ./hub/cmd/churnsoak

OO_SCREEN_T1_TOKEN=soak-token \
OO_SCREEN_ERP_BASE=http://127.0.0.1:4474 \
OO_SCREEN_HUB_KEY=soak-hub-key \
OO_SCREEN_HUB_ADDR=127.0.0.1:4471 \
OO_SCREEN_METRICS_ADDR=127.0.0.1:4472 \
OO_SCREEN_PPROF_ADDR=127.0.0.1:4473 \
OO_SCREEN_ICE_PORT=${ICE_PORT:-4590} \
OO_SCREEN_OFFER_RATE=1000 OO_SCREEN_OFFER_BURST=1000 \
  "$BIN/hub-webrtc" >"$OUT/hub.log" 2>&1 &
HUB=$!
trap 'kill $HUB 2>/dev/null || true' EXIT
for _ in $(seq 50); do
  curl -sf http://127.0.0.1:4472/metrics >/dev/null && break
  sleep 0.2
done
"$BIN/churnsoak" -dur "$DUR" -out "$OUT" "$@" 2>"$OUT/churnsoak.log" | tee "$OUT/summary.txt"
