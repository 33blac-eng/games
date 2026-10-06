//go:build windows

package consent

// Windows UI для S3: системно-модальний MessageBox для згоди і постійна
// topmost-плашка «Ваш екран переглядають» з кнопкою «Завершити сесію».
// Лише крос-компіляція перевірена тут; на живому ПК не перевірялось.

import (
	"context"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procMessageBoxTimeou = user32.NewProc("MessageBoxTimeoutW")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procSetWindowPos     = user32.NewProc("SetWindowPos")
	procSetTimer         = user32.NewProc("SetTimer")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmDestroy = 0x0002
	wmClose   = 0x0010
	wmCommand = 0x0111
	wmTimer   = 0x0113

	idYes   = 6
	btnEnd  = 1
	indW    = 380
	indH    = 44
	swpFlag = 0x0001 | 0x0002 | 0x0010 // NOSIZE|NOMOVE|NOACTIVATE
)

// NativeUI — Windows-реалізація UI.
type NativeUI struct{}

// Ask — MessageBoxTimeoutW (Так/Ні, дефолт «Ні», поверх усього).
// Таймаут бере з ctx; мовчання = відмова.
func (NativeUI) Ask(ctx context.Context, text string) bool {
	ms := uint32(30000)
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 {
			ms = uint32(d.Milliseconds())
		}
	}
	res := make(chan bool, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		t, _ := syscall.UTF16PtrFromString(text)
		c, _ := syscall.UTF16PtrFromString("oo-screen — запит доступу")
		const flags = 0x4 | 0x30 | 0x100 | 0x1000 | 0x10000 | 0x40000
		r, _, _ := procMessageBoxTimeou.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags, 0, uintptr(ms))
		res <- r == idYes
	}()
	select {
	case ok := <-res:
		return ok && ctx.Err() == nil
	case <-ctx.Done():
		return false // діалог догорить сам на своєму таймауті; відповідь ігнорується
	}
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type msg struct {
	hwnd    windows.HWND
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

var (
	ind struct {
		sync.Mutex
		hwnd   windows.HWND
		onEnd  func()
		active bool // хтось кликнув Show і ще не Hide
		gen    uint64
	}
	classOnce sync.Once
	className *uint16
	wndProcCB uintptr
)

func wndProc(hwnd, m, wp, lp uintptr) uintptr {
	switch m {
	case wmCommand:
		if wp&0xffff == btnEnd {
			ind.Lock()
			f := ind.onEnd
			ind.Unlock()
			if f != nil {
				go f()
			}
			return 0
		}
	case wmTimer:
		// Повертаємо плашку наверх: інше topmost-вікно не має її сховати.
		procSetWindowPos.Call(hwnd, ^uintptr(0), 0, 0, 0, 0, swpFlag)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, m, wp, lp)
	return r
}

// ShowIndicator — topmost-плашка вгорі по центру основного екрана.
func (NativeUI) ShowIndicator(onEnd func()) {
	ind.Lock()
	ind.onEnd = onEnd
	if ind.active {
		ind.Unlock()
		return
	}
	ind.active = true
	ind.gen++
	gen := ind.gen
	ind.Unlock()
	go runIndicator(gen)
}

// HideIndicator — закрити плашку.
func (NativeUI) HideIndicator() {
	ind.Lock()
	ind.active = false
	h := ind.hwnd
	ind.hwnd = 0
	ind.Unlock()
	if h != 0 {
		procPostMessageW.Call(uintptr(h), wmClose, 0, 0)
	}
}

func runIndicator(gen uint64) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hinst, _, _ := procGetModuleHandleW.Call(0)
	classOnce.Do(func() {
		className, _ = syscall.UTF16PtrFromString("OoScreenWatchIndicator")
		wndProcCB = windows.NewCallback(wndProc)
		wc := wndClassEx{lpfnWndProc: wndProcCB, hInstance: windows.Handle(hinst), lpszClassName: className, hbrBackground: windows.Handle(24 + 1)} // COLOR_INFOBK+1
		wc.cbSize = uint32(unsafe.Sizeof(wc))
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	})
	cx, _, _ := procGetSystemMetrics.Call(0) // SM_CXSCREEN
	x := int32(cx)/2 - indW/2
	title, _ := syscall.UTF16PtrFromString("oo-screen")
	hwnd, _, _ := procCreateWindowExW.Call(0x8|0x80, // TOPMOST|TOOLWINDOW
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		0x80000000|0x10000000|0x00800000, // POPUP|VISIBLE|BORDER
		uintptr(x), 0, indW, indH, 0, 0, hinst, 0)
	if hwnd == 0 {
		return
	}
	static, _ := syscall.UTF16PtrFromString("STATIC")
	button, _ := syscall.UTF16PtrFromString("BUTTON")
	label, _ := syscall.UTF16PtrFromString("● Ваш екран зараз переглядають")
	end, _ := syscall.UTF16PtrFromString("Завершити сесію")
	procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(static)), uintptr(unsafe.Pointer(label)),
		0x40000000|0x10000000, 8, 12, 230, 20, hwnd, 0, hinst, 0)
	procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(button)), uintptr(unsafe.Pointer(end)),
		0x40000000|0x10000000, 245, 7, 125, 28, hwnd, btnEnd, hinst, 0)
	procSetTimer.Call(hwnd, 1, 2000, 0)

	ind.Lock()
	if !ind.active || ind.gen != gen { // Hide встиг до створення
		ind.Unlock()
		procPostMessageW.Call(hwnd, wmClose, 0, 0)
	} else {
		ind.hwnd = windows.HWND(hwnd)
		ind.Unlock()
	}
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}
