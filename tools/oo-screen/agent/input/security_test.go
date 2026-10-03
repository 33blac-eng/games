package input

// security_test.go — безпековий аудит інʼєкції вводу (SECURITY-AUDIT.md, SecIxx).

import (
	"math"
	"testing"
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

// SecI04 (KnownFAIL, документовано): протокол дозволяє будь-які scancode —
// зокрема Win (0xE05B) — тобто Win+R і подібні комбінації інʼєктуються без
// фільтра. Це відповідає призначенню («повний контроль»), але на рівні агента
// немає ні allow-list, ні автоматичного відпускання затиснутих клавіш при
// розриві сесії.
func TestSecI04AnyKeyComboAcceptedKnownFAIL(t *testing.T) {
	if _, err := ParseEvent([]byte(`{"v":1,"type":"key","scancode":57435,"down":true}`)); err != nil {
		t.Fatalf("Win-клавішу відкинуто (%v) — зʼявився фільтр: переверніть тест", err)
	}
}
