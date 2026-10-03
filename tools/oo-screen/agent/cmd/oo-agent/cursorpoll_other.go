//go:build !windows

package main

import "github.com/organicoils/oo-screen/internal/cursorproto"

// readCursorPos — опитувача вказівника поза Windows нема: кадровий цикл
// лишається єдиним джерелом позиції.
func readCursorPos() (cursorproto.Reading, bool) { return cursorproto.Reading{}, false }
