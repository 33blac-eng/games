//go:build windows

package main

import (
	"syscall"
	"unsafe"

	"github.com/organicoils/oo-screen/internal/cursorproto"
)

// UNVERIFIED на живому Windows: GetCursorInfo у процесі з
// PER_MONITOR_AWARE_V2 (capture.init) повертає фізичні пікселі
// віртуального столу — ті самі одиниці, що DesktopCoordinates DXGI.

var procGetCursorInfo = syscall.NewLazyDLL("user32.dll").NewProc("GetCursorInfo")

const cursorShowing = 0x00000001 // CURSOR_SHOWING

type cursorInfo struct {
	cbSize  uint32
	flags   uint32
	hCursor uintptr
	x, y    int32
}

// readCursorPos — позиція хотспота і чи видно курсор. Безпечно з будь-якої
// горутини: стан капчера не чіпає.
func readCursorPos() (cursorproto.Reading, bool) {
	ci := cursorInfo{cbSize: uint32(unsafe.Sizeof(cursorInfo{}))}
	if r, _, _ := procGetCursorInfo.Call(uintptr(unsafe.Pointer(&ci))); r == 0 {
		return cursorproto.Reading{}, false // напр. secure desktop: віддаємо DXGI
	}
	return cursorproto.Reading{Showing: ci.flags&cursorShowing != 0, X: int(ci.x), Y: int(ci.y)}, true
}
