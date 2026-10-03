#!/usr/bin/env bash
# Cross-compile oo-screen for windows/amd64 with cgo (mingw), vet agent
# packages, and compile (not run) every agent test binary.
set -euo pipefail
export CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="${CC:-x86_64-w64-mingw32-gcc}"
go build ./...
go vet ./agent/...
fmt='{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}'
for p in $(go list -f "$fmt" ./agent/...); do
  echo "go test -c $p"
  go test -c -o /dev/null "$p"
done
