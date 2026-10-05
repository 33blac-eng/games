package input

// security_test.go — безпековий аудит інʼєкції вводу (SECURITY-AUDIT.md, SecIxx).

import (
	"errors"
	"math"
	"testing"
	"unsafe"
)

// SecI01: ворожі події з дроту відкидаються ДО SendInput.
func TestSecI01HostileEventsRejected(t *testing.T) {
	bad := []string{
		`{"v":2,"type":"mouse_move","x":0.5,"y":0.5}`,   // чужа версія
		`{"v":1,"type":"mouse_move","x":1.5,"y":0.5}`,   // поза 0..1
		`{"v":1,"type":"mouse_move","x":-0.1,"y":0.5}`,  // відʼємна
		`{"v":1,"type":"mouse_move","x":0.5}`,           // лише x
		`{"v":1,"type":"mouse_button","button":"evil"}`, // невідома кнопка
		`{"v":1,"type":"mouse_wheel","wheel_x":0,"wheel_y":0}`,
		`{"v":1,"type":"key"}`,                        // ні scancode, ні unicode
		`{"v":1,"type":"key","unicode":1114112}`,      // не code point
		`{"v":1,"type":"exec","cmd":"calc"}`,          // невідомий тип
		`{"v":1,"type":"mouse_move","x":1e309,"y":0}`, // Inf у JSON
		`not json`,
	}
	for _, m := range bad {
		if _, err := ParseEvent([]byte(m)); err == nil {
			t.Errorf("прийнято: %s", m)
		}
	}
}

// SecI02: NaN/Inf у Go-структурі (обхід JSON) теж ловиться Validate.
func TestSecI02NaNInfRejected(t *testing.T) {
	nan := math.NaN()
	if (Event{V: Version, Kind: KindMouseMove, X: &nan, Y: &nan}).Validate() == nil {
		t.Fatal("NaN координати прийнято")
	}
	if (Event{V: Version, Kind: KindMouseWheel, WheelY: math.Inf(1)}).Validate() == nil {
		t.Fatal("Inf колесо прийнято")
	}
}

// SecI03: координати завжди затиснуті в межі захопленої поверхні.
func TestSecI03CoordinatesClamped(t *testing.T) {
	b := Bounds{Left: -1920, Top: 0, Width: 1920, Height: 1080}
	for _, v := range []float64{0, 1, 0.9999999} {
		x, y := b.pixel(v, v)
		if x < b.Left || x >= b.Left+b.Width || y < b.Top || y >= b.Top+b.Height {
			t.Fatalf("pixel(%v)=(%d,%d) поза поверхнею", v, x, y)
		}
	}
}

// SecI04 (SEC #37): комбінації клавіш лишаються pass-through (призначення —
// «повний контроль»), але (а) OO_AGENT_INPUT_BLOCK_KEYS блокує обрані scancode,
// (б) усе затиснуте відпускається ReleaseAll при закритті сесії/каналу.
func TestSecI04HeldKeysReleasedAndBlocklist(t *testing.T) {
	if _, err := ParseEvent([]byte(`{"v":1,"type":"key","scancode":57435,"down":true}`)); err != nil {
		t.Fatalf("Win-клавішу відкинуто протоколом (%v) — pass-through за дизайном", err)
	}
	var h heldState
	down := func(ev Event) Event { ev.V, ev.Down = Version, true; return ev }
	h.track(down(Event{Kind: KindKey, Scancode: 0x1D}))   // Ctrl
	h.track(down(Event{Kind: KindKey, Scancode: 0xE05B})) // Win
	h.track(down(Event{Kind: KindKey, Unicode: 'a'}))     // символ
	h.track(down(Event{Kind: KindMouseButton, Button: ButtonLeft}))
	h.track(down(Event{Kind: KindMouseButton, Button: ButtonX2}))
	h.track(Event{V: Version, Kind: KindMouseButton, Button: ButtonX2}) // відпущено
	ups := h.releaseInputs()
	if len(ups) != 4 {
		t.Fatalf("releaseInputs=%d подій, want 4", len(ups))
	}
	var keyUps, leftUp int
	for _, in := range ups {
		switch in.typ {
		case inputKeyboard:
			ki := (*rawKeybdInput)(unsafe.Pointer(&in.mi))
			if ki.flags&keyUp == 0 {
				t.Fatalf("клавіша без keyUp: %+v", ki)
			}
			if ki.scan == 0x5B && ki.flags&keyExtended == 0 {
				t.Fatal("Win відпущено без extended")
			}
			keyUps++
		case inputMouse:
			if in.mi.flags != mouseLeftUp {
				t.Fatalf("кнопка: flags=%#x", in.mi.flags)
			}
			leftUp++
		}
	}
	if keyUps != 3 || leftUp != 1 || len(h.releaseInputs()) != 0 {
		t.Fatalf("keyUps=%d leftUp=%d, повторний release не порожній", keyUps, leftUp)
	}

	// ReleaseAll через Injector: шле рівно відпускання і очищає стан.
	var sent []rawInput
	prev := sendRaw
	sendRaw = func(in []rawInput) error { sent = append(sent, in...); return nil }
	t.Cleanup(func() { sendRaw = prev })
	inj := &Injector{}
	inj.held.track(down(Event{Kind: KindKey, Scancode: 0x38}))
	if err := inj.ReleaseAll(); err != nil || len(sent) != 1 {
		t.Fatalf("ReleaseAll err=%v sent=%d", err, len(sent))
	}
	if k, b := inj.Held(); k != 0 || b != 0 || inj.ReleaseAll() != nil || len(sent) != 1 {
		t.Fatal("після ReleaseAll щось лишилось затиснутим")
	}

	// Блок-лист.
	bl, err := ParseBlockedKeys("0xE05B, 0xE05C")
	if err != nil {
		t.Fatal(err)
	}
	inj.SetBlockedKeys(bl)
	if err := inj.Inject(down(Event{Kind: KindKey, Scancode: 0xE05B})); !errors.Is(err, ErrKeyBlocked) {
		t.Fatalf("Win не заблоковано: %v", err)
	}
	if inj.isBlocked(down(Event{Kind: KindKey, Scancode: 0x5B})) {
		t.Fatal("не-extended 0x5B ([) заблоковано помилково")
	}
	if _, err := ParseBlockedKeys("0xE05B,zz"); err == nil {
		t.Fatal("сміття в блок-листі прийнято")
	}
}
