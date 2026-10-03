#!/usr/bin/env bash
# hub-deploy.sh — викочує хаб на VPS із перевіркою і автоматичним відкатом.
#
#   deploy/hub-deploy.sh [опції] <ssh-host> <systemd-unit>
#
# Опції:
#   -n, --dry-run        лише показати, що буде зроблено (на сервері нічого не міняється)
#   --dist DIR           каталог зі збіркою (дефолт deploy/dist: hub-linux-amd64, VERSION, SHA256SUMS)
#   --remote-dir DIR     каталог хаба на сервері (дефолт /opt/oo-screen)
#   --health-url URL     перевірка ПІСЛЯ рестарту, з сервера (дефолт http://127.0.0.1:4470/healthz)
#   --timeout SEC        скільки чекати здоровʼя (дефолт 60)
#   --smoke-port PORT    порт пробного старту нового бінаря (дефолт 14470; ICE = PORT+1)
#
# Розкладка на сервері:  $REMOTE_DIR/hub.<sha>  (поруч, старі не видаляються)
#                        $REMOTE_DIR/hub -> hub.<sha>  (симлінк; ExecStart юніта = $REMOTE_DIR/hub)
#
# Кроки: перевірка SHA256 локально -> завантаження hub.<sha> (пропускається, якщо
# вже є з тим самим SHA256) -> пробний старт на альтернативному порту + /healthz ->
# атомарне перемикання симлінка -> restart юніта -> здоровʼя (HTTP + is-active +
# журнал без panic/fatal) -> інакше відкат симлінка + restart.
# Повторний запуск тієї ж версії нічого не перемикає (ідемпотентно).
#
# Env: OO_DEPLOY_SUDO (дефолт "sudo"; "" якщо ssh-користувач root),
#      OO_DEPLOY_SSH / OO_DEPLOY_SCP (дефолт ssh / scp).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DRY=0
DIST="$here/dist"
REMOTE_DIR="/opt/oo-screen"
HEALTH_URL="http://127.0.0.1:4470/healthz"
TIMEOUT=60
SMOKE_PORT=14470
SUDO="${OO_DEPLOY_SUDO-sudo}"
SSH="${OO_DEPLOY_SSH:-ssh}"
SCP="${OO_DEPLOY_SCP:-scp}"

usage() { sed -n '2,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }
die() { echo "hub-deploy: $*" >&2; exit 1; }
log() { echo "hub-deploy: $*" >&2; }

args=()
while [[ $# -gt 0 ]]; do
	case "$1" in
	-n | --dry-run) DRY=1 ;;
	--dist) DIST="$2"; shift ;;
	--remote-dir) REMOTE_DIR="$2"; shift ;;
	--health-url) HEALTH_URL="$2"; shift ;;
	--timeout) TIMEOUT="$2"; shift ;;
	--smoke-port) SMOKE_PORT="$2"; shift ;;
	-h | --help) usage; exit 0 ;;
	-*) usage >&2; die "невідомий прапорець $1" ;;
	*) args+=("$1") ;;
	esac
	shift
done
[[ ${#args[@]} -eq 2 ]] || { usage >&2; die "потрібно рівно два аргументи: <ssh-host> <systemd-unit>"; }
HOST="${args[0]}"
UNIT="${args[1]}"
[[ "$TIMEOUT" =~ ^[0-9]+$ && "$SMOKE_PORT" =~ ^[0-9]+$ ]] || die "--timeout і --smoke-port мають бути числами"
[[ "$UNIT" =~ ^[A-Za-z0-9@._-]+$ ]] || die "дивне імʼя юніта: $UNIT"
[[ "$REMOTE_DIR" =~ ^/[A-Za-z0-9/._-]+$ ]] || die "дивний --remote-dir: $REMOTE_DIR"

# ---- локальна перевірка збірки ----
BIN="$DIST/hub-linux-amd64"
[[ -f "$BIN" && -f "$DIST/VERSION" && -f "$DIST/SHA256SUMS" ]] || die "у $DIST нема hub-linux-amd64/VERSION/SHA256SUMS — спершу deploy/build.sh"
SHA="$(tr -d '[:space:]' <"$DIST/VERSION")"
[[ "$SHA" =~ ^[0-9a-f]{7,40}(-dirty)?$ ]] || die "VERSION не схожий на git sha: $SHA"
(cd "$DIST" && grep ' hub-linux-amd64$' SHA256SUMS | sha256sum -c --quiet -) || die "SHA256 hub-linux-amd64 не збігається з SHA256SUMS"
SUM="$(sha256sum "$BIN" | cut -d' ' -f1)"
NEW="hub.$SHA"
log "версія $SHA, sha256 $SUM -> $HOST:$REMOTE_DIR/$NEW (юніт $UNIT)"

# rsh — виконати на сервері (завжди, і в dry-run: лише читання).
rsh() { "$SSH" "$HOST" -- "$1"; }
# rmut — змінити щось на сервері; у dry-run лише друкує.
rmut() {
	if [[ $DRY -eq 1 ]]; then
		log "[dry-run] ssh $HOST: $1"
	else
		rsh "$1"
	fi
}

# ---- 1. завантаження (ідемпотентно) ----
remote_sum="$(rsh "sha256sum '$REMOTE_DIR/$NEW' 2>/dev/null | cut -d' ' -f1" || true)"
if [[ "$remote_sum" == "$SUM" ]]; then
	log "$NEW вже на сервері з тим самим SHA256 — завантаження пропущено"
else
	[[ -z "$remote_sum" ]] || log "УВАГА: $NEW на сервері має інший SHA256 ($remote_sum) — перезаписую"
	if [[ $DRY -eq 1 ]]; then
		log "[dry-run] scp $BIN $HOST:$REMOTE_DIR/$NEW.tmp"
	else
		rsh "mkdir -p '$REMOTE_DIR'"
		"$SCP" -q "$BIN" "$HOST:$REMOTE_DIR/$NEW.tmp"
	fi
	rmut "cd '$REMOTE_DIR' && echo '$SUM  $NEW.tmp' | sha256sum -c --quiet - && chmod 0755 '$NEW.tmp' && mv -f '$NEW.tmp' '$NEW'"
fi

# ---- 2. preflight: пробний старт на альтернативному порту ----
# Хаб не має -version; натомість піднімаємо НОВИЙ бінар (з одноразовим T1-токеном,
# бо дефолтний хаб відмовляється приймати) на
# 127.0.0.1:SMOKE_PORT (ICE на SMOKE_PORT+1, щоб не зачепити робочий 4544) і чекаємо /healthz.
smoke="cd '$REMOTE_DIR' && OO_SCREEN_T1_TOKEN=smoke-$RANDOM$RANDOM$RANDOM OO_SCREEN_HUB_ADDR=127.0.0.1:$SMOKE_PORT OO_SCREEN_ICE_PORT=$((SMOKE_PORT + 1)) OO_SCREEN_RECORD=0 \
timeout 15 './$NEW' >/tmp/oo-hub-smoke.log 2>&1 & pid=\$!; ok=1; \
for i in 1 2 3 4 5 6 7 8 9 10; do sleep 1; if curl -fsS -m 2 http://127.0.0.1:$SMOKE_PORT/healthz >/dev/null 2>&1; then ok=0; break; fi; done; \
kill \$pid 2>/dev/null; wait \$pid 2>/dev/null; [ \$ok -eq 0 ] || { tail -20 /tmp/oo-hub-smoke.log >&2; exit 1; }"
if [[ $DRY -eq 1 ]]; then
	log "[dry-run] пробний старт $NEW на 127.0.0.1:$SMOKE_PORT"
else
	rsh "$smoke" || die "пробний старт $NEW провалився — прод НЕ чіпали"
	log "пробний старт $NEW: /healthz OK"
fi

# ---- 3. перемикання ----
prev="$(rsh "readlink '$REMOTE_DIR/hub' 2>/dev/null" || true)"
prev="${prev##*/}"
health() {
	local since="$1" deadline=$((SECONDS + TIMEOUT))
	while ((SECONDS < deadline)); do
		if rsh "journalctl -u '$UNIT' --since '$since' --no-pager -q 2>/dev/null | grep -qE '(panic:|fatal error:)'"; then
			log "у журналі $UNIT panic/fatal"
			return 1
		fi
		if rsh "systemctl is-active --quiet '$UNIT' && curl -fsS -m 3 '$HEALTH_URL' >/dev/null"; then
			# Ще пару секунд — паніка на першому підключенні теж не пройде.
			sleep 3
			if rsh "systemctl is-active --quiet '$UNIT' && ! journalctl -u '$UNIT' --since '$since' --no-pager -q 2>/dev/null | grep -qE '(panic:|fatal error:)'"; then
				return 0
			fi
			return 1
		fi
		sleep 2
	done
	log "здоровʼя не настало за ${TIMEOUT}s"
	return 1
}
switch_to() { # $1 = імʼя бінаря в REMOTE_DIR
	rmut "cd '$REMOTE_DIR' && ln -sfn '$1' hub.next && mv -Tf hub.next hub && $SUDO systemctl restart '$UNIT'"
}

if [[ "$prev" == "$NEW" ]]; then
	log "симлінк уже вказує на $NEW — перемикати нічого, лише перевіряю здоровʼя"
	[[ $DRY -eq 1 ]] && exit 0
	health "$(rsh "date '+%Y-%m-%d %H:%M:%S' -d '-1 min'")" || die "$NEW уже активний, але НЕЗДОРОВИЙ — розберіться вручну (попередня версія невідома)"
	log "OK: $NEW здоровий"
	exit 0
fi

log "перемикаю ${prev:-<нема>} -> $NEW"
since="$(rsh "date '+%Y-%m-%d %H:%M:%S'")"
switch_to "$NEW"
[[ $DRY -eq 1 ]] && { log "[dry-run] далі: перевірка $HEALTH_URL до ${TIMEOUT}s, інакше відкат на ${prev:-<нема>}"; exit 0; }

if health "$since"; then
	log "OK: $UNIT працює на $NEW (було ${prev:-<нема>})"
	exit 0
fi

# ---- 4. відкат ----
if [[ -z "$prev" ]]; then
	die "$NEW нездоровий, а попередньої версії нема — відкочувати нікуди"
fi
log "ВІДКАТ на $prev"
since="$(rsh "date '+%Y-%m-%d %H:%M:%S'")"
switch_to "$prev"
if health "$since"; then
	die "$NEW нездоровий; відкочено на $prev, $prev здоровий"
fi
die "$NEW нездоровий; відкат на $prev ТЕЖ нездоровий — потрібне втручання"
