#!/usr/bin/env bash
# Локальний тест hub-deploy.sh без сервера і без bats: ssh/scp/systemctl/
# journalctl/curl підмінено заглушками в PATH, «сервер» — тимчасовий каталог.
#
#   deploy/test/hub-deploy-test.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
deploy="$here/../hub-deploy.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fakebin="$work/bin"
remote="$work/remote"
state="$work/state" # journal, restarts, bad-версія
mkdir -p "$fakebin" "$remote" "$state"
: >"$state/journal"
: >"$state/restarts"

# ssh HOST -- CMD  => виконати CMD локально
cat >"$fakebin/ssh" <<'EOF'
#!/usr/bin/env bash
shift; [[ "$1" == "--" ]] && shift
exec bash -c "$1"
EOF
# scp -q SRC HOST:DST
cat >"$fakebin/scp" <<'EOF'
#!/usr/bin/env bash
[[ "$1" == "-q" ]] && shift
cp "$1" "${2#*:}"
EOF
# systemctl restart|is-active: «запущена» версія = ціль симлінка на момент restart
cat >"$fakebin/systemctl" <<EOF
#!/usr/bin/env bash
case "\$1" in
restart) : >"$state/journal"; t=\$(readlink "$remote/hub"); echo "\$t" >"$state/running"; echo "\$t" >>"$state/restarts"
	if [[ -f "$state/bad" && "\$t" == "\$(cat "$state/bad")" ]]; then echo "panic: test" >>"$state/journal"; fi ;;
is-active) [[ -s "$state/running" ]] ;;
*) exit 1 ;;
esac
EOF
# journalctl: віддає журнал лише поточного «запуску»
cat >"$fakebin/journalctl" <<EOF
#!/usr/bin/env bash
cat "$state/journal"
EOF
# curl: smoke-порт — за FAKE_SMOKE_FAIL; робочий /healthz — якщо запущена не bad-версія
cat >"$fakebin/curl" <<EOF
#!/usr/bin/env bash
for a; do url="\$a"; done
case "\$url" in
*:14470/*) [[ ! -f "$state/smoke_fail" ]] ;;
*) r=\$(cat "$state/running" 2>/dev/null); [[ -n "\$r" && ! ( -f "$state/bad" && "\$r" == "\$(cat "$state/bad")" ) ]] ;;
esac
EOF
chmod +x "$fakebin"/*
export PATH="$fakebin:$PATH" OO_DEPLOY_SUDO=""

# Збірка-заглушка: «хаб» — скрипт, що просто живе.
mkdist() { # $1 = sha
	local d="$work/dist-$1"
	mkdir -p "$d"
	printf '#!/bin/sh\nsleep 30\n' >"$d/hub-linux-amd64"
	echo "$1" >"$d/VERSION"
	(cd "$d" && sha256sum hub-linux-amd64 VERSION >SHA256SUMS)
	echo "$d"
}
run() { "$deploy" --dist "$1" --remote-dir "$remote" --timeout 4 host oo-hub.service "${@:2}"; }
# Журнал «нового запуску» чистий, як після restart.
fresh_journal() { : >"$state/journal"; }

pass=0
ok() { echo "ok   - $1"; pass=$((pass + 1)); }
fail() { echo "FAIL - $1" >&2; exit 1; }

A="$(mkdist aaaaaaa1)"
B="$(mkdist bbbbbbb2)"
C="$(mkdist ccccccc3)"

# 1. dry-run на порожньому сервері нічого не створює
run "$A" --dry-run >/dev/null 2>&1 || fail "dry-run повернув помилку"
[[ -z "$(ls -A "$remote")" ]] || fail "dry-run щось створив: $(ls "$remote")"
ok "dry-run нічого не міняє"

# 2. перше викочування
run "$A" 2>/dev/null || fail "викочування A"
[[ "$(readlink "$remote/hub")" == "hub.aaaaaaa1" ]] || fail "симлінк не на A"
[[ -x "$remote/hub.aaaaaaa1" ]] || fail "бінар A не виконуваний"
ok "перше викочування A"

# 3. повтор тієї самої версії — без restart
n="$(wc -l <"$state/restarts")"
run "$A" 2>/dev/null || fail "повтор A"
[[ "$(wc -l <"$state/restarts")" == "$n" ]] || fail "повтор A зробив restart"
ok "ідемпотентність: повтор A без restart"

# 4. B здорова — перемикання, A лишається поруч
fresh_journal
run "$B" 2>/dev/null || fail "викочування B"
[[ "$(readlink "$remote/hub")" == "hub.bbbbbbb2" && -f "$remote/hub.aaaaaaa1" ]] || fail "B не активна або A зникла"
ok "A -> B, side-by-side"

# 5. C панікує — автоматичний відкат на B, ненульовий код
fresh_journal
echo "hub.ccccccc3" >"$state/bad"
if run "$C" 2>"$work/err"; then fail "нездорова C повернула 0"; fi
[[ "$(readlink "$remote/hub")" == "hub.bbbbbbb2" ]] || fail "відкат не повернув симлінк на B"
[[ "$(tail -1 "$state/restarts")" == "hub.bbbbbbb2" ]] || fail "після відкату не було restart на B"
grep -q "відкочено на hub.bbbbbbb2" "$work/err" || fail "нема повідомлення про відкат"
ok "C з panic -> автоматичний відкат на B"
rm -f "$state/bad"

# 6. пробний старт провалився — прод не чіпали
fresh_journal
touch "$state/smoke_fail"
n="$(wc -l <"$state/restarts")"
D="$(mkdist ddddddd4)"
if run "$D" 2>/dev/null; then fail "провалений smoke повернув 0"; fi
[[ "$(readlink "$remote/hub")" == "hub.bbbbbbb2" && "$(wc -l <"$state/restarts")" == "$n" ]] || fail "smoke-провал зачепив прод"
ok "провал пробного старту не чіпає прод"
rm -f "$state/smoke_fail"

# 7. зіпсований бінар (SHA256SUMS не збігається) — відмова до будь-яких дій
echo junk >>"$D/hub-linux-amd64"
if run "$D" 2>/dev/null; then fail "битий бінар повернув 0"; fi
ok "битий SHA256 — відмова"

echo "усі $pass перевірок пройдено"
