//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("input probe requires Windows")
	os.Exit(2)
}
