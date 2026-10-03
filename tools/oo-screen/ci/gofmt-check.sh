#!/usr/bin/env bash
# gofmt check tolerant of CRLF line endings: strips CR from each .go file
# into a temp copy and runs gofmt -l on it. Fails only on real formatting issues.
set -euo pipefail
root="${1:-.}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
bad=0
while IFS= read -r -d '' f; do
  tr -d '\r' < "$f" > "$tmp/x.go"
  if [ -n "$(gofmt -l "$tmp/x.go")" ]; then
    echo "needs gofmt: $f"
    bad=1
  fi
done < <(find "$root" -name '*.go' -not -path '*/vendor/*' -print0)
exit "$bad"
