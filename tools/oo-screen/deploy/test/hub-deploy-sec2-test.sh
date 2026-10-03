#!/usr/bin/env bash
# Повторний аудит hub-deploy.sh: інʼєкція через аргументи і журнал пробного
# старту в тимчасовому файлі (не фіксований /tmp/oo-hub-smoke.log).
#
#   deploy/test/hub-deploy-sec2-test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deploy="$here/../hub-deploy.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fakebin="$work/bin"
remote="$work/remote"
tmp="$work/tmp"
mkdir -p "$fakebin" "$remote" "$tmp"
: >"$work/ssh.log"

# ssh HOST -- CMD => журнал + виконати локально (TMPDIR = $tmp)
cat >"$fakebin/ssh" <<EOF
#!/usr/bin/env bash
echo "\$1" >>"$work/ssh.log"
shift; [[ "\$1" == "--" ]] && shift
printf '%s\n' "\$1" >>"$work/cmds.log"
TMPDIR="$tmp" exec bash -c "\$1"
EOF
cat >"$fakebin/scp" <<'EOF'
#!/usr/bin/env bash
[[ "$1" == "-q" ]] && shift
cp "$1" "${2#*:}"
EOF
cat >"$fakebin/systemctl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat >"$fakebin/journalctl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat >"$fakebin/curl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$fakebin"/*
export PATH="$fakebin:$PATH" OO_DEPLOY_SUDO=""

d="$work/dist"
mkdir -p "$d"
printf '#!/bin/sh\nsleep 30\n' >"$d/hub-linux-amd64"
echo "abcdef1" >"$d/VERSION"
(cd "$d" && sha256sum hub-linux-amd64 VERSION >SHA256SUMS)

pass=0
ok() { echo "ok   - $1"; pass=$((pass + 1)); }
fail() { echo "FAIL - $1" >&2; exit 1; }

# 1. host з «-» на початку (ssh-опція) і з пробілом/лапками — відмова до ssh.
for h in "-oProxyCommand=touch $work/pwned" "host'x" "host x" ""; do
	if "$deploy" -n --dist "$d" --remote-dir "$remote" "$h" oo-hub.service >/dev/null 2>&1; then
		fail "host '$h' прийнято"
	fi
done
[[ ! -s "$work/ssh.log" && ! -e "$work/pwned" ]] || fail "ssh викликано з битим host"
ok "битий ssh-host відхилено до ssh"

# 2. --health-url з лапкою розірвав би '...' у віддаленій команді.
if "$deploy" -n --dist "$d" --remote-dir "$remote" --health-url "http://x/'; touch $work/pwned; '" host oo-hub.service >/dev/null 2>&1; then
	fail "--health-url з лапкою прийнято"
fi
[[ ! -e "$work/pwned" ]] || fail "інʼєкція через --health-url"
ok "--health-url з лапкою відхилено"

# 3. Нормальний host (user@host, alias) і URL проходять; журнал smoke — у mktemp
# і прибирається, фіксованого /tmp/oo-hub-smoke.log немає.
"$deploy" --dist "$d" --remote-dir "$remote" --timeout 4 --health-url "http://127.0.0.1:4470/healthz?x=1&y=[2]" deploy@hub-1.example oo-hub.service >/dev/null 2>&1 ||
	fail "чесний деплой провалився"
grep -q '/tmp/oo-hub-smoke' "$work/cmds.log" && fail "журнал smoke у фіксованому /tmp"
grep -q 'mktemp' "$work/cmds.log" || fail "smoke без mktemp"
[[ -z "$(ls -A "$tmp")" ]] || fail "тимчасовий журнал smoke не прибрано: $(ls -A "$tmp")"
[[ "$(readlink "$remote/hub")" == "hub.abcdef1" ]] || fail "симлінк не перемкнуто"
ok "чесний деплой: smoke-журнал у mktemp і прибраний"

echo "усі $pass перевірок пройдено"
