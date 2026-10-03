//go:build !windows

// oo-agent потребує Windows (DXGI Desktop Duplication + Media Foundation
// MFT). Цей стаб лише тримає `GOOS=linux go build ./agent/...` зеленим.
package main

import "fmt"

func main() { fmt.Println("oo-agent requires Windows (DXGI + Media Foundation)") }
