//go:build windows

// A-39: агент мусить знати, що робить СЕСІЯ Windows під ним.
//
// Дві різні речі, які до цього агент не бачив узагалі:
//
//  1. Logoff/shutdown. Процес просто вбивали: черга відправки не доїжджала,
//     мʼютекс єдиного екземпляра (A-36) звільнявся не нами, а ядром, і на
//     швидкому релогоні наступний агент міг застати його ще живим. Тепер
//     WM_ENDSESSION (вихід вирішено, wParam != 0) гасить ctx тим самим шляхом,
//     що й Ctrl+C, і main виходить через свої defer-и. WM_QUERYENDSESSION —
//     лише запит, його ще можуть скасувати: на ньому не гасимо нічого.
//
//  2. Lock/unlock. На лок-скріні DXGI не віддає дублікацію взагалі
//     (E_ACCESSDENIED), а кадровий цикл цього не знав і молотив reacquire —
//     бек-оф допомагав лише частково, бо він рахує НЕВДАЧІ, а не причину.
//     Лок триває хвилинами: усі ці спроби наперед приречені. WTS-повідомлення
//     дають точний сигнал: поки locked — не пробуємо взагалі (сесію тримає
//     keepalive), на unlock — пробуємо НЕГАЙНО, без залишкового бек-офу.
//
// Вікно тут звичайне (не HWND_MESSAGE) навмисно: message-only вікна не
// отримують широкомовних WM_ENDSESSION, тобто половина сенсу цього файлу
// зникла б мовчки. Воно ніколи не показується (ShowWindow не кличемо).
package main

import (
	"log"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmDestroy           = 0x0002
	wmQueryEndSession   = 0x0011
	wmEndSession        = 0x0016
	wmWTSSessionChange  = 0x02B1
	wtsSessionLock      = 0x7
	wtsSessionUnlock    = 0x8
	notifyThisSession   = 0x0
	cwUseDefault        = ^uintptr(0x7FFFFFFF) // CW_USEDEFAULT = 0x80000000
	styleOverlapped     = 0x00000000
	idiApplicationClass = "oo-screen-agent-session"
)

// sessionWatch — стан сесії для кадрового циклу. Обидва поля читає цикл, пише
// віконна горутина; atomic саме тому, що це різні горутини, і мʼютекс тут дав
// би ілюзію, ніби стан можна читати «під замком» довше за один рядок.
type sessionWatch struct {
	locked atomic.Bool
	// unlocked зводиться при WTS_SESSION_UNLOCK і ЗНІМАЄТЬСЯ споживачем
	// (takeUnlocked): це одноразовий «спробуй просто зараз», а не рівень.
	unlocked atomic.Bool

	// probed — «заблоковано» поставив НАШ опит (inputDesktopLocked на старті), а
	// не WTS. OpenInputDesktop відмовляє і на UAC/secure desktop, де локу нема —
	// отже, не буде й WTS-unlock, і без переопиту агент вважав би сесію
	// заблокованою до першого справжнього lock/unlock: хабу session-locked
	// (нода не в /nodes), reacquire не робиться. Тому, поки probed, стіл
	// переопитуємо раз на sessionReprobe.
	probed    atomic.Bool
	lastProbe atomic.Int64 // UnixNano останнього опиту
	probe     func() bool  // inputDesktopLocked; у тестах — підміна

	// hwnd — наше приховане вікно; пишеться до ready, тож після watchSession
	// читається без синхронізації. Тести шлють у нього повідомлення напряму,
	// а не шукають за класом (на ПК може жити бойовий агент з тим самим).
	hwnd uintptr
}

// sessionReprobe — як часто переопитувати вхідний стіл, поки «заблоковано»
// відоме лише з опиту. Дешево (один OpenInputDesktop), а UAC-запит живе секунди.
const sessionReprobe = 2 * time.Second

// Locked — чи заблокована зараз консольна сесія.
func (w *sessionWatch) Locked() bool {
	if w == nil {
		return false
	}
	if w.probed.Load() {
		w.reprobe(time.Now())
	}
	return w.locked.Load()
}

// reprobe — стіл, заблокований лише за опитом, відкрився: знімаємо «locked»
// так само, як це зробив би WTS-unlock (включно з «спробуй просто зараз»).
func (w *sessionWatch) reprobe(now time.Time) {
	last := w.lastProbe.Load()
	if now.UnixNano()-last < int64(sessionReprobe) || !w.lastProbe.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	if w.probe == nil || w.probe() {
		return
	}
	if w.probed.CompareAndSwap(true, false) {
		w.locked.Store(false)
		w.unlocked.Store(true)
		log.Printf("oo-agent: вхідний стіл знову доступний без WTS-unlock (UAC/secure desktop) — reacquire (A-39)")
	}
}

// takeUnlocked повертає true рівно один раз на кожен unlock.
func (w *sessionWatch) takeUnlocked() bool { return w != nil && w.unlocked.Swap(false) }

// watchSession піднімає приховане вікно з власною горутиною і повертає стан.
// onEnd кличеться при logoff/shutdown (може бути кілька разів — гасіння ctx
// ідемпотентне). nil, якщо вікно підняти не вдалось: агент тоді працює рівно
// як до A-39, без сигналів сесії.
func watchSession(onEnd func()) *sessionWatch {
	w := &sessionWatch{probe: inputDesktopLocked}
	ready := make(chan bool, 1)
	go func() {
		// Вікно НАЗАВЖДИ привʼязане до потоку, що його створив: цикл повідомлень
		// мусить крутитись на тому самому потоці, інакше GetMessage не побачить
		// нічого.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		w.pump(onEnd, ready)
	}()
	if !<-ready {
		return nil
	}
	// WTS шле lock/unlock лише в МОМЕНТ події. Агент, що стартував уже на
	// заблокованому ПК (перезапуск завдання, оновлення 16.09.2026 на Maria),
	// вважав сесію відкритою і не казав хабу session-locked. Тож початковий
	// стан питаємо самі; далі його веде WTS, як і раніше.
	if w.probe() {
		w.locked.Store(true)
		w.probed.Store(true)
		w.lastProbe.Store(time.Now().UnixNano())
		log.Printf("oo-agent: старт на заблокованій сесії — reacquire призупинено (A-39)")
	}
	return w
}

// inputDesktopLocked — чи зараз на екрані НЕ робочий стіл користувача. На
// екрані блокування / UAC вхідний стіл — Winlogon, і процес користувача його не
// відкриє (ACCESS_DENIED). Будь-яка відмова = «заблоковано»: хибне
// «заблоковано» (UAC) зніме переопит (reprobe), хибне «відкрито» коштує 8с чекання.
func inputDesktopLocked() bool {
	h, _, _ := procOpenInputDesktop.Call(0, 0, desktopReadObjects)
	if h == 0 {
		return true
	}
	procCloseDesktop.Call(h)
	return false
}

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	wtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")

	procRegisterClassExW  = user32.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32.NewProc("CreateWindowExW")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessageW  = user32.NewProc("DispatchMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procWTSRegisterNotify = wtsapi32.NewProc("WTSRegisterSessionNotification")
	procWTSUnregNotify    = wtsapi32.NewProc("WTSUnRegisterSessionNotification")
	procOpenInputDesktop  = user32.NewProc("OpenInputDesktop")
	procCloseDesktop      = user32.NewProc("CloseDesktop")
)

const desktopReadObjects = 0x0001

type wndClassExW struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

type msgW struct {
	hwnd    windows.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

// pump реєструє клас, створює вікно й крутить цикл повідомлень до WM_QUIT.
// У ready кладе, чи вдалось піднятись — ДО входу в цикл, бо сам цикл не
// повертається до кінця життя агента.
func (w *sessionWatch) pump(onEnd func(), ready chan<- bool) {
	cls := windows.StringToUTF16Ptr(idiApplicationClass)
	proc := windows.NewCallback(func(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
		switch msg {
		case wmQueryEndSession:
			// Лише «не заперечую»: це ЗАПИТ, і людина ще може скасувати вихід
			// («програми заважають вимкненню» -> «Скасувати»). Погасити ctx тут
			// означало б прибрати ПК з OO до наступного старту задачі, хоча
			// сесія живе далі. Гасимо на WM_ENDSESSION, коли вихід вирішено.
			return 1
		case wmEndSession:
			// wParam=0 — вихід скасовано (нами чи іншою програмою): живемо далі.
			if wParam != 0 && onEnd != nil {
				onEnd()
			}
			return 0
		case wmWTSSessionChange:
			switch wParam {
			case wtsSessionLock:
				// Справжній лок: знімати його тепер буде WTS-unlock, не опит.
				w.probed.Store(false)
				w.locked.Store(true)
				log.Printf("oo-agent: сесію заблоковано — reacquire призупинено (A-39)")
			case wtsSessionUnlock:
				w.probed.Store(false)
				w.locked.Store(false)
				w.unlocked.Store(true)
				log.Printf("oo-agent: сесію розблоковано — reacquire негайно (A-39)")
			}
			return 0
		case wmDestroy:
			procPostQuitMessage.Call(0)
			return 0
		}
		r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
		return r
	})

	wc := wndClassExW{
		size:      uint32(unsafe.Sizeof(wndClassExW{})),
		wndProc:   proc,
		className: cls,
	}
	if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		log.Printf("oo-agent: RegisterClassEx для сесійних подій: %v — працюю без них", err)
		ready <- false
		return
	}
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(cls)), styleOverlapped,
		cwUseDefault, cwUseDefault, 0, 0, 0, 0, 0, 0)
	if hwnd == 0 {
		log.Printf("oo-agent: CreateWindowEx для сесійних подій: %v — працюю без них", err)
		ready <- false
		return
	}
	// Без реєстрації WTS вікно все одно ловить ENDSESSION, тож невдача тут не
	// привід відмовлятись від усього: втрачаємо лише lock/unlock.
	if ok, _, werr := procWTSRegisterNotify.Call(hwnd, notifyThisSession); ok == 0 {
		log.Printf("oo-agent: WTSRegisterSessionNotification: %v — lock/unlock не буде", werr)
	} else {
		defer procWTSUnregNotify.Call(hwnd)
	}

	w.hwnd = hwnd
	ready <- true

	var m msgW
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = помилка
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}
