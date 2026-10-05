//go:build windows

package main

import (
	"sync/atomic"
	"testing"
)

// TestQueryEndSessionDoesNotStopAgent — R6-G6: WM_QUERYENDSESSION — лише
// запит; людина ще може скасувати вихід із системи. Агент мусить лишитись
// живим, доки не прийде WM_ENDSESSION з wParam != 0 (вихід вирішено). Шлях
// той самий, що в бою: приховане вікно watchSession і його цикл повідомлень.
// Поверни onEnd() у гілку wmQueryEndSession — перша перевірка почервоніє.
func TestQueryEndSessionDoesNotStopAgent(t *testing.T) {
	var ends atomic.Int32
	w := watchSession(func() { ends.Add(1) })
	if w == nil {
		t.Skip("вікно сесійних подій не піднялось у цьому оточенні")
	}
	hwnd := w.hwnd
	send := user32.NewProc("SendMessageW")
	t.Cleanup(func() { send.Call(hwnd, 0x0010 /* WM_CLOSE */, 0, 0) })

	if r, _, _ := send.Call(hwnd, wmQueryEndSession, 0, 0); r != 1 {
		t.Fatalf("WM_QUERYENDSESSION = %d, want 1 (не блокуємо вихід)", r)
	}
	if n := ends.Load(); n != 0 {
		t.Fatalf("агент згас на ЗАПИТІ виходу (onEnd x%d) — скасований вихід лишив би ПК без OO", n)
	}
	send.Call(hwnd, wmEndSession, 0, 0) // вихід скасовано
	if n := ends.Load(); n != 0 {
		t.Fatalf("агент згас на скасованому виході (WM_ENDSESSION wParam=0), onEnd x%d", n)
	}
	send.Call(hwnd, wmEndSession, 1, 0) // вихід вирішено
	if n := ends.Load(); n != 1 {
		t.Fatalf("WM_ENDSESSION wParam=1: onEnd x%d, want 1", n)
	}
}
