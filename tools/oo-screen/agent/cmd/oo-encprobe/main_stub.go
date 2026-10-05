//go:build !windows

// oo-encprobe enumerates Media Foundation H.264 encoder MFTs, which exist only
// on Windows. This stub keeps `go build/test ./agent/...` building on Linux.
package main

import "fmt"

func main() { fmt.Println("oo-encprobe requires Windows (Media Foundation)") }
