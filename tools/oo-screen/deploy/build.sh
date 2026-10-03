#!/usr/bin/env bash
# build.sh — збирає ПАРУ з одного коміту: хаб (linux/amd64) і агент
# (windows/amd64, cgo через mingw) у deploy/dist/, плюс VERSION і SHA256SUMS.
#
#   deploy/build.sh                  # збірка з HEAD; брудне дерево = відмова
#   ALLOW_DIRTY=1 deploy/build.sh    # лише для стенда
#
# Змінні: CC_WIN (дефолт x86_64-w64-mingw32-gcc), DIST (дефолт deploy/dist).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mod="$(cd "$here/.." && pwd)"
dist="${DIST:-$here/dist}"
cc_win="${CC_WIN:-x86_64-w64-mingw32-gcc}"

cd "$mod"
sha="$(git rev-parse --short=12 HEAD)"
if [[ -n "$(git status --porcelain -- .)" ]]; then
	if [[ "${ALLOW_DIRTY:-0}" != "1" ]]; then
		echo "build.sh: робоче дерево брудне — пара мусить бути з ОДНОГО коміту (ALLOW_DIRTY=1 щоб обійти)" >&2
		exit 1
	fi
	sha="${sha}-dirty"
fi
command -v "$cc_win" >/dev/null || {
	echo "build.sh: нема $cc_win (apt install gcc-mingw-w64-x86-64)" >&2
	exit 1
}

rm -rf "$dist"
mkdir -p "$dist"
# main.buildVersion у бінарях поки не оголошено — -X тоді просто ігнорується;
# джерело правди про версію — VERSION + SHA256SUMS + імʼя hub.<sha> на сервері.
ldflags="-s -w -X main.buildVersion=$sha"

echo "hub   -> $dist/hub-linux-amd64 ($sha)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -trimpath -ldflags "$ldflags" -o "$dist/hub-linux-amd64" ./hub/cmd/hub-webrtc

echo "token -> $dist/oo-node-token-linux-amd64"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -trimpath -ldflags "$ldflags" -o "$dist/oo-node-token-linux-amd64" ./hub/cmd/oo-node-token

echo "agent -> $dist/oo-agent-windows-amd64.exe ($sha)"
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="$cc_win" \
	go build -trimpath -ldflags "$ldflags -H windowsgui" -o "$dist/oo-agent-windows-amd64.exe" ./agent/cmd/oo-agent

echo "$sha" >"$dist/VERSION"
(cd "$dist" && sha256sum hub-linux-amd64 oo-node-token-linux-amd64 oo-agent-windows-amd64.exe VERSION >SHA256SUMS)
cat "$dist/SHA256SUMS"
