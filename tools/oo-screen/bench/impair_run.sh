#!/usr/bin/env bash
# Один прогін «hub + agent + soak_probe із керованою вадою» на ПРИВАТНОМУ порту.
#
# Навіщо окремий порт: дефолтний :4470 зашитий у soak_probe і в corpus-player, а
# поруч цілком може крутитись чужий хаб — тоді замір мовчки поїде в чужий бінар.
# Хаб з ce10a587 вміє OO_SCREEN_HUB_ADDR, тож беремо свій порт і ПЕРЕВІРЯЄМО, що
# слухач на ньому — процес із НАШОГО exe (verify listener нижче). Те саме з
# UDP-діапазоном: дефолт 4544-4607 теж спільний.
#
# Кожен рядок stdout хаба й проби отримує мітку «секунд від старту»: NDJSON хаба
# ({"leg":"ctl"...}, {"leg":"nack"...}) свого часу не несе, а весь замір — про
# ЧАС (за скільки секунд рампи прийшов перший зріз, за скільки ціль дійшла до
# підлоги, скільки тримається засувка).
#
# Використання:
#   OOS_BIN=... OOS_OUT=... bash bench/impair_run.sh <ім'я> <секунди> [-- аргументи soak_probe]
# Приклад:
#   bash bench/impair_run.sh ramp15 45 -- -delay 600ms -delay-ramp 40s
set -u
cd "$(dirname "$0")/.."

NAME="${1:?назва прогону}"
SECS="${2:?тривалість, секунд}"
shift 2
[ "${1:-}" = "--" ] && shift

BIN="${OOS_BIN:?OOS_BIN=тека з бінарями, зібраними ОДИН раз}"
OUT="${OOS_OUT:?OOS_OUT=тека для артефактів прогонів}"
PORT="${OOS_PORT:-4471}"
BITRATE="${OOS_BITRATE:-8000000}"

D="$OUT/$NAME"
rm -rf "$D"; mkdir -p "$D"

WBIN=$(cygpath -w "$BIN")

# stamp — секунди від старту процесу перед кожним рядком.
stamp() { python -u -c "
import sys,time
t0=time.time()
for l in sys.stdin:
    sys.stdout.write('%8.3f %s' % (time.time()-t0, l)); sys.stdout.flush()
"; }

# launch <лог> <exe> [аргументи...] — пускає exe у фон, кладе його MSYS-pid у
# файл і проганяє вивід через stamp. Пайп ховає $! самого exe, тому pid пишемо
# зсередини групи.
launch() {
  local log="$1"; shift
  local exe="$1"; shift
  { "$exe" "$@" 2>&1 & echo $! > "$log.pid"; wait; } 2>/dev/null | stamp > "$log" 2>/dev/null &
}

# winpid_of <ім'я exe> -> Windows-pid процесу саме з НАШОЇ теки бінарів.
# ps -W віддає повний шлях, тож чужий oo-agent (у нас на машині крутиться
# бойовий із C:\ProgramData) під фільтр не потрапляє — а це рівно та помилка,
# через яку замір поїхав би в чужий процес.
# ПАСТКА, спіймана живцем: у шаблоні grep НЕ МОЖЕ бути "/" — MSYS перетворює
# аргумент, схожий на шлях, у windows-вигляд, і grep -F "scratchpad/bin" мовчки
# не знаходить нічого. Через це killall_ours нікого не вбивав, хаб попереднього
# прогону лишався на порту, новий не біндився — і замір поїхав би в старий бінар
# (рівно та біда, на яку наступив попередній агент). Роздільник — крапка регекспу.
BINMARK="$(basename "$(dirname "$BIN")").$(basename "$BIN")"
winpid_of() { ps -W | grep -iE "$BINMARK" | grep -i "${1%.exe}" | awk '{print $4}' | head -1; }

# killall_ours — прибрати ВСІ наші процеси (у т.ч. від попереднього прогону, який
# урвався: живий чужий... тобто наш же вчорашній хаб на тому самому порту — це
# рівно та пастка, через яку міряють не той бінар).
killall_ours() {
  for n in soak_probe oo-agent corpus-player hub-webrtc; do
    for p in $(ps -W | grep -iE "$BINMARK" | grep -i "$n" | awk '{print $4}'); do
      powershell -NoProfile -Command "Stop-Process -Id $p -Force -ErrorAction SilentlyContinue"
    done
  done
}

killall_ours   # хвости попереднього прогону
sleep 1

export OO_SCREEN_HUB_ADDR=":$PORT"
export OO_SCREEN_UDP_PORT_MIN="${OOS_UDP_MIN:-4700}"
export OO_SCREEN_UDP_PORT_MAX="${OOS_UDP_MAX:-4760}"
launch "$D/hub.log" "$BIN/hub-webrtc.exe"
sleep 3

# --- verify listener --------------------------------------------------------
HUBPID=$(winpid_of hub-webrtc.exe)
LPID=$(netstat -ano | grep -E "TCP.*:$PORT[[:space:]]+.*LISTENING" | awk '{print $NF}' | tr -d '\r' | head -1)
echo "listener_check port=$PORT listen_pid=$LPID our_hub_pid=$HUBPID exe=$WBIN\\hub-webrtc.exe" | tee "$D/listener.txt"
if [ -z "$LPID" ] || [ "$LPID" != "$HUBPID" ]; then
  echo "ABORT: на порту $PORT слухає pid=$LPID, а наш хаб — pid=$HUBPID" | tee -a "$D/listener.txt"
  killall_ours
  exit 3
fi

# Публікатор: agent (справжній DXGI) або corpus (записаний 1080p60).
# ЧОМУ ДВА. Тільки oo-agent відкриває control-канал "oosc-ctl", тож рядки
# {"leg":"ctl"} (рішення контролера бітрейту) є ЛИШЕ з ним. Зате нерухомий екран
# сервера дає ~5 пакетів/с, а для NACK-вікна треба >=16 запитів за 2 с — це
# фізично недосяжно. Корпус дає ~800 пакетів/с, але ctl-каналу не має.
# Тому: RTT міряємо агентом, NACK — корпусом.
case "${OOS_PUB:-agent}" in
  corpus)
    export OO_SCREEN_HUB_URL="http://127.0.0.1:$PORT/offer/agent"
    launch "$D/agent.log" "$BIN/corpus-player-webrtc.exe"
    ;;
  *)
    launch "$D/agent.log" "$BIN/oo-agent.exe" -transport webrtc \
      -hub "http://127.0.0.1:$PORT/offer/agent" -bitrate "$BITRATE"
    ;;
esac
sleep 4

launch "$D/probe.log" "$BIN/soak_probe.exe" \
  -signal "http://127.0.0.1:$PORT/offer/viewer" -state "$D/viewer_state.json" "$@"

sleep "$SECS"

killall_ours
sleep 1
echo "DONE $NAME -> $D"
