#!/usr/bin/env bash
# O4: поетапна викатка агента (канарка -> ... -> 100 %) з воротами здоров'я
# і автоматичною зупинкою. Див. deploy/DEPLOY.md, розділ «O4: викатка агента».
#
#   OO_UPDATE_SIGNING_KEY=... deploy/agent-rollout.sh init  <manifest.json>
#   OO_UPDATE_SIGNING_KEY=... deploy/agent-rollout.sh step  <manifest.json>   # з cron/systemd timer
#   OO_UPDATE_SIGNING_KEY=... deploy/agent-rollout.sh watch <manifest.json>   # цикл кожні $INTERVAL с
#
# Env: OO_ROLLOUT_BIN (oo-rollout), OO_ROLLOUT_DIR (/var/lib/oo-rollout),
#      OO_ROLLOUT_PLAN (прапорці плану, див. DEFAULT_PLAN), INTERVAL (600),
#      OO_ROLLOUT_ON_HALT (команда-сповіщення, отримує рядок рішення в $1).
# Коди: 0 ok, 3 зупинено (halt, маніфест уже з rollout_percent 0), інше — помилка.
set -euo pipefail

LAST=""
BIN="${OO_ROLLOUT_BIN:-oo-rollout}"
DIR="${OO_ROLLOUT_DIR:-/var/lib/oo-rollout}"
DEFAULT_PLAN="-stages 1,10,50,100 -soak 2h -min-reports 3 -max-fail-rate 0.05 -max-stage-time 48h"
read -r -a PLAN <<< "${OO_ROLLOUT_PLAN:-$DEFAULT_PLAN}"
INTERVAL="${INTERVAL:-600}"

cmd="${1:-}"
man="${2:-}"
if [[ -z "$cmd" || -z "$man" ]]; then
  sed -n '2,13p' "$0" >&2
  exit 2
fi
if [[ -z "${OO_UPDATE_SIGNING_KEY:-}" ]]; then
  echo "agent-rollout: OO_UPDATE_SIGNING_KEY не задано" >&2
  exit 2
fi
mkdir -p "$DIR"

step_once() {
  local rc=0
  LAST="$("$BIN" step -manifest "$man" -state "$DIR/state.json" -reports "$DIR/reports.jsonl" "${PLAN[@]}")" || rc=$?
  echo "$(date -u +%FT%TZ) $LAST"
  if [[ $rc -eq 3 && -n "${OO_ROLLOUT_ON_HALT:-}" ]]; then
    "$OO_ROLLOUT_ON_HALT" "$LAST" || true
  fi
  return $rc
}

case "$cmd" in
  init)
    extra=()
    if [[ -n "${FORCE:-}" ]]; then extra=(-force); fi
    "$BIN" init -manifest "$man" -state "$DIR/state.json" "${PLAN[@]}" "${extra[@]}"
    echo "agent-rollout: викатку розпочато, стан $DIR/state.json"
    ;;
  step)
    step_once
    ;;
  watch)
    while true; do
      rc=0
      step_once || rc=$?
      if [[ $rc -ne 0 ]]; then exit $rc; fi
      if [[ "$LAST" == *"action=done"* ]]; then
        echo "agent-rollout: 100 % — викатку завершено"
        exit 0
      fi
      sleep "$INTERVAL"
    done
    ;;
  *)
    echo "agent-rollout: невідома команда $cmd" >&2
    exit 2
    ;;
esac
