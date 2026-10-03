#!/usr/bin/env bash
# Прогін стелі: N глядачів на одну ноду, свіжий хаб на кожне N.
# Друкує CPU-секунди ХАБА і КЛІЄНТА окремо — без цього не відрізниш, хто впав.
set -u
cd "$(dirname "$0")/../../.."
OUT=results/ceiling
PORT=${PORT:-4471}
# Свій блок UDP-портів: на цій машині поруч крутиться ще один хаб на дефолтному
# 4544-4607, і ділити з ним блок означало б міряти чужі колізії. UMIN/UMAX за
# замовчуванням — рівно 64 порти, стільки ж, скільки в шитому дефолті.
UMIN=${UMIN:-4736}
UMAX=${UMAX:-4799}
# TAG розділяє прогони «як у проді» (вузький блок портів) і «з широким блоком»,
# щоб логи одного не затирали інший.
TAG=${TAG:-default}

cpu() { powershell -NoProfile -Command "\$p=Get-Process -Name '$1' -ErrorAction SilentlyContinue; if(\$p){[math]::Round((\$p|Measure-Object CPU -Sum).Sum,2)}else{0}"; }

for n in "$@"; do
  taskkill //F //IM hub-webrtc-test.exe >/dev/null 2>&1
  # Чекаємо, поки порт справді звільниться: інакше новий хаб тихо падає на
  # bind, а вимірювач іде говорити з привидом попереднього прогону.
  for _ in $(seq 30); do
    powershell -NoProfile -Command "if(Get-NetTCPConnection -LocalPort $PORT -State Listen -EA SilentlyContinue){exit 1}else{exit 0}" && break
    sleep 1
  done
  OO_SCREEN_HUB_ADDR=":$PORT" OO_SCREEN_UDP_PORT_MIN=$UMIN OO_SCREEN_UDP_PORT_MAX=$UMAX ./build/hub-webrtc-test.exe > "$OUT/hub-$TAG-n$n.log" 2>&1 &
  for _ in $(seq 30); do
    powershell -NoProfile -Command "if(Get-NetTCPConnection -LocalPort $PORT -State Listen -EA SilentlyContinue){exit 0}else{exit 1}" && break
    sleep 1
  done
  grep -q 'bind:' "$OUT/hub-$TAG-n$n.log" && { echo "N=$n: хаб не піднявся"; continue; }

  h0=$(cpu hub-webrtc-test)
  # Семплер CPU обох процесів: без нього не відрізниш «упрів хаб» від «упрів
  # вимірювач», а клієнт на цій же машині платить за SRTP на кожну ногу теж.
  : > "$OUT/cpu-$TAG-n$n.txt"
  ( while :; do
      powershell -NoProfile -Command "\$h=(Get-Process -Name hub-webrtc-test -EA SilentlyContinue).CPU; \$l=(Get-Process -Name loadgen -EA SilentlyContinue).CPU; if(\$l){'hub={0:N1} loadgen={1:N1}' -f \$h,\$l}" >> "$OUT/cpu-$TAG-n$n.txt" 2>/dev/null
      sleep 2
    done ) & sampler=$!
  ./build/loadgen.exe -hub "http://127.0.0.1:$PORT" -viewers "$n" -dur 20s > "$OUT/loadgen-$TAG-n$n.txt" 2>&1
  kill "$sampler" 2>/dev/null
  h1=$(cpu hub-webrtc-test)

  echo "===== N=$n ====="
  cat "$OUT/loadgen-$TAG-n$n.txt"
  echo "hub CPU за прогін: $(awk -v a="$h0" -v b="$h1" 'BEGIN{printf "%.2f", b-a}') c"
  echo "рядків nack-телеметрії в лозі хаба: $(grep -c '"leg":"nack"' "$OUT/hub-$TAG-n$n.log" 2>/dev/null || echo 0)"
  echo "CPU (остання проба): $(tail -1 "$OUT/cpu-$TAG-n$n.txt" 2>/dev/null)"
  echo "viewer leg dropped: $(grep -c 'viewer leg dropped' "$OUT/hub-$TAG-n$n.log" 2>/dev/null || echo 0)"
done
taskkill //F //IM hub-webrtc-test.exe >/dev/null 2>&1
