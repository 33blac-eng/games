#!/usr/bin/env bash
# Локальний тест deploy/agent-rollout.sh: справжні oo-rollout і oo-update-sign
# (go build), згенерований ключ, звіти агентів пишуться прямо в JSONL.
#
#   deploy/test/agent-rollout-test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$here/../.."
script="$here/../agent-rollout.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

(cd "$root" && go build -o "$work/oo-rollout" ./agent/cmd/oo-rollout && go build -o "$work/oo-update-sign" ./agent/cmd/oo-update-sign)
OO_UPDATE_SIGNING_KEY="$("$work/oo-update-sign" -genkey | sed -n 's/^PRIVATE[^:]*: *//p')"
export OO_UPDATE_SIGNING_KEY
echo fake >"$work/agent.exe"
"$work/oo-update-sign" -exe "$work/agent.exe" -version 2.0.0 -url https://hub/a.exe -rollout 0 -out "$work/m.json" >/dev/null

export OO_ROLLOUT_BIN="$work/oo-rollout" OO_ROLLOUT_DIR="$work/st"
export OO_ROLLOUT_PLAN="-stages 1,100 -soak 0s -min-reports 2 -max-fail-rate 0.3 -max-stage-time 0"
export OO_ROLLOUT_ON_HALT="$work/onhalt"
cat >"$work/onhalt" <<EOF
#!/usr/bin/env bash
echo "\$1" >"$work/halted"
EOF
chmod +x "$work/onhalt"

fail() { echo "FAIL: $*" >&2; exit 1; }
pct() { python3 -c 'import json,base64,sys;print(json.loads(base64.b64decode(json.load(open(sys.argv[1]))["manifest"]))["rollout_percent"])' "$work/m.json"; }
rep() { printf '{"node":"%s","version":"2.0.0","result":"%s","at":"%s"}\n' "$1" "$2" "$(date -u +%FT%TZ)" >>"$work/st/reports.jsonl"; }

bash "$script" init "$work/m.json" >/dev/null
[[ "$(pct)" == 1 ]] || fail "canary percent $(pct)"
out="$(bash "$script" step "$work/m.json")"
[[ "$out" == *action=hold* ]] || fail "no reports must hold: $out"
sleep 1
rep a ok; rep b ok
out="$(bash "$script" step "$work/m.json")"
[[ "$out" == *action=advance* && "$(pct)" == 100 ]] || fail "advance: $out pct $(pct)"
out="$(INTERVAL=0 bash "$script" watch "$work/m.json")"
[[ "$out" == *"викатку завершено"* ]] || fail "watch done: $out"

# новий реліз, канарка падає -> halt, rollout 0, хук викликано, код 3
"$work/oo-update-sign" -exe "$work/agent.exe" -version 2.0.0 -url https://hub/a.exe -rollout 0 -out "$work/m.json" >/dev/null
FORCE=1 bash "$script" init "$work/m.json" >/dev/null
rep c fail
rc=0; bash "$script" step "$work/m.json" >/dev/null || rc=$?
[[ $rc == 3 ]] || fail "halt exit code $rc"
[[ "$(pct)" == 0 ]] || fail "halt must set rollout 0, got $(pct)"
[[ -s "$work/halted" ]] || fail "on-halt hook not called"
echo "agent-rollout-test: OK"
