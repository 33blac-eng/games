#!/usr/bin/env bash
# Синтетичний корпус-драбина для netbench: скрол code-dark-1080p.png, 1080p60,
# IDR/2 с, без B-кадрів, CBR. Справжній corpus-1080p60.h264 у репо не лежить,
# тож рівень 8M — його замінник, а 4M..0.5M — щаблі, між якими corpus-player
# перемикається на bitrate_target хаба (емуляція енкодера, що слухає контролер).
# Використання: bash bench/impair/mkcorpus.sh <тека-виходу> [секунд]
set -eu
OUT="${1:?тека}"; SECS="${2:-10}"
SRC="$(dirname "$0")/../corpus/code-dark-1080p.png"
mkdir -p "$OUT"
# LADDER_BPS — свій набір щаблів (run_network_p0.sh: 16 щаблів ~x1.2).
for b in ${LADDER_BPS:-8000000 4000000 2000000 1000000 500000}; do
  f="$OUT/ladder-$b.h264"
  [ -s "$f" ] && continue
  k=$((b/1000))
  ffmpeg -hide_banner -loglevel error -y -loop 1 -framerate 60 -i "$SRC" \
    -vf "scale=1920:4320,crop=1920:1080:0:'mod(t*240,3240)',noise=alls=4:allf=t,format=yuv420p" -t "$SECS" -r 60 \
    -c:v libx264 -preset veryfast -tune zerolatency -profile:v high -bf 0 \
    -g 120 -keyint_min 120 -sc_threshold 0 -x264-params "nal-hrd=cbr:repeat-headers=1" \
    -b:v ${k}k -minrate ${k}k -maxrate ${k}k -bufsize $((k/4))k -f h264 "$f"
done
ls -l "$OUT"
