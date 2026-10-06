//go:build !windows

// encode-probe is Windows-only: it drives DXGI Desktop Duplication and a Media
// Foundation MFT. This stub keeps `GOOS=linux go build ./agent/...` green.
package main

import "fmt"

func main() { fmt.Println("encode-probe requires Windows (DXGI + Media Foundation)") }
