#!/usr/bin/env bash
# run_b.sh — піднімає повний T1-стенд кандидата B (WebTransport):
# hub-wt + статика web/ + corpus-player-wt. Ctrl+C гасить усе.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

HUB_QUIC_PORT=4460
HUB_WT_PORT=4461
WEB_PORT=4480
CORPUS="${CORPUS:-bench/corpus/corpus-1080p60.h264}"
TOKEN="${OO_SCREEN_T1_TOKEN:-t1-dev-token}"

BUILD_DIR="$(mktemp -d)"
HUB_LOG="$BUILD_DIR/hub-wt.log"
AGENT_LOG="$BUILD_DIR/corpus-player-wt.log"
WEB_LOG="$BUILD_DIR/web.log"

PIDS=()
cleanup() {
  echo "run_b.sh: shutting down..." >&2
  for pid in "${PIDS[@]:-}"; do
    [ -n "${pid:-}" ] && kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

echo "run_b.sh: building hub-wt and corpus-player-wt..."
go build -o "$BUILD_DIR/hub-wt" ./hub/cmd/hub-wt
go build -o "$BUILD_DIR/corpus-player-wt" ./agent/cmd/corpus-player-wt

echo "run_b.sh: starting hub-wt (QUIC ingest :$HUB_QUIC_PORT, WebTransport :$HUB_WT_PORT)..."
OO_SCREEN_T1_TOKEN="$TOKEN" "$BUILD_DIR/hub-wt" > "$HUB_LOG" 2>&1 &
PIDS+=("$!")

# Чекаємо на CERT_HASH= у stdout hub-wt (до 5с).
CERT_HASH=""
for _ in $(seq 1 50); do
  if grep -q '^CERT_HASH=' "$HUB_LOG" 2>/dev/null; then
    CERT_HASH="$(grep '^CERT_HASH=' "$HUB_LOG" | head -1 | cut -d= -f2)"
    break
  fi
  sleep 0.1
done
if [ -z "$CERT_HASH" ]; then
  echo "run_b.sh: ПОМИЛКА — hub-wt не надрукував CERT_HASH= за 5с, дивись $HUB_LOG" >&2
  exit 1
fi
echo "run_b.sh: CERT_HASH=$CERT_HASH"

echo "run_b.sh: serving web/ on :$WEB_PORT..."
( cd web && python3 -m http.server "$WEB_PORT" > "$WEB_LOG" 2>&1 ) &
PIDS+=("$!")

sleep 1

echo "run_b.sh: starting corpus-player-wt against localhost:$HUB_QUIC_PORT..."
"$BUILD_DIR/corpus-player-wt" -hub "localhost:$HUB_QUIC_PORT" -corpus "$CORPUS" > "$AGENT_LOG" 2>&1 &
PIDS+=("$!")

VIEWER_URL="http://localhost:$WEB_PORT/viewer-wt.html?url=https://localhost:$HUB_WT_PORT/wt&token=$TOKEN&certhash=$CERT_HASH"
echo ""
echo "=================================================================="
echo "run_b.sh: стенд піднято. Відкрий у браузері (Chrome/Edge з підтримкою"
echo "WebTransport serverCertificateHashes):"
echo ""
echo "  $VIEWER_URL"
echo ""
echo "Логи: hub=$HUB_LOG agent=$AGENT_LOG web=$WEB_LOG"
echo "Ctrl+C — зупинити все."
echo "=================================================================="

wait
