//go:build windows

package capture

import (
	"syscall"
	"testing"
	"unsafe"
)

// GetDeviceCaps indices (wingdi.h) і PROCESS_DPI_AWARENESS (shellscalingapi.h).
const (
	horzres        = 8
	vertres        = 10
	logpixelsx     = 88
	desktopvertres = 117
	desktophorzres = 118

	dpiPerMonitorAware = 2
)

// GDIFrame бере робочий стіл через GetDC(NULL)+BitBlt НА РОЗМІР З DXGI, тобто
// у фізичних пікселях. Процесу, який не оголосив DPI-awareness, Windows віддає
// віртуалізований (масштабований) робочий стіл — і на ноутбуці зі 125-150%
// перший кадр виходить обрізаним та збільшеним.
//
// Дві перевірки, бо вони ловлять різне:
//
//  1. процес справді per-monitor aware. Це негативний контроль, який червоніє
//     на БУДЬ-ЯКІЙ машині, щойно прибрати init() у capture_windows.go —
//     зокрема на цій, де екран не масштабований;
//
//  2. DC не віртуалізований: HORZRES (те, що бачить GDI) == DESKTOPHORZRES
//     (фізичний розмір, який GetDeviceCaps віддає навіть DPI-сліпому процесу).
//     На немасштабованому екрані вони рівні й без фікса, на масштабованому
//     розходяться рівно тоді, коли дефект живий. Тобто це саме та перевірка,
//     що має сенс на Yeva's-Notebook, а не тут.
func TestProcessIsDPIAwareForGDIGrab(t *testing.T) {
	user32 := syscall.NewLazyDLL("user32.dll")
	gdi32 := syscall.NewLazyDLL("gdi32.dll")
	getDC := user32.NewProc("GetDC")
	releaseDC := user32.NewProc("ReleaseDC")
	getDeviceCaps := gdi32.NewProc("GetDeviceCaps")

	dc, _, err := getDC.Call(0)
	if dc == 0 {
		t.Fatalf("GetDC(NULL): %v", err)
	}
	defer releaseDC.Call(0, dc)

	caps := func(index uintptr) int {
		v, _, _ := getDeviceCaps.Call(dc, index)
		return int(int32(v))
	}
	logical := [2]int{caps(horzres), caps(vertres)}
	physical := [2]int{caps(desktophorzres), caps(desktopvertres)}
	scalePct := caps(logpixelsx) * 100 / 96
	t.Logf("GDI бачить %dx%d, фізичний робочий стіл %dx%d, масштаб %d%%",
		logical[0], logical[1], physical[0], physical[1], scalePct)

	if p := syscall.NewLazyDLL("shcore.dll").NewProc("GetProcessDpiAwareness"); p.Find() == nil {
		var awareness int32
		if hr, _, _ := p.Call(0, uintptr(unsafe.Pointer(&awareness))); hr != 0 {
			t.Fatalf("GetProcessDpiAwareness: hr=0x%x", hr)
		}
		if awareness != dpiPerMonitorAware {
			t.Fatalf("процес оголосив DPI-awareness %d (хочемо %d = per-monitor): на масштабованому екрані GetDC(NULL) віддасть віртуалізований робочий стіл і GDIFrame зріже кадр",
				awareness, dpiPerMonitorAware)
		}
	} else {
		t.Log("shcore.dll!GetProcessDpiAwareness недоступний — перевіряємо лише розміри")
	}

	if logical != physical {
		t.Fatalf("DC віртуалізований: GDI бачить %dx%d, а екран насправді %dx%d (масштаб %d%%) — BitBlt на %dx%d зріже й розтягне кадр",
			logical[0], logical[1], physical[0], physical[1], scalePct, physical[0], physical[1])
	}
}
