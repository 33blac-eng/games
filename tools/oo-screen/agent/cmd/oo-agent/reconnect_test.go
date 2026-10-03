//go:build windows

package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/control"
)

// grace у тестах короткий, а очікування — з кратним запасом (×6), щоб зелений
// результат не залежав від планувальника. Порівнюємо ФАКТ спрацювання, а не
// його час: міряти тут мілісекунди означало б міряти шум.
const testGrace = 30 * time.Millisecond

// recorder ловить причини, з якими decider вирішив перепідключатись.
type recorder struct {
	ch chan string
}

func newRecorder() *recorder { return &recorder{ch: make(chan string, 8)} }

func (r *recorder) fire(reason string) { r.ch <- reason }

// got чекає рішення до timeout; "" = за цей час рішення не було.
func (r *recorder) got(timeout time.Duration) string {
	select {
	case s := <-r.ch:
		return s
	case <-time.After(timeout):
		return ""
	}
}

// TestReconnectDeciderStates — правило рішення про реконект за станом
// PeerConnection. Те саме, що в глядача (desktop-oo-webrtc.js
// createDisconnectGrace), тільки на боці агента.
func TestReconnectDeciderStates(t *testing.T) {
	t.Run("failed → одразу", func(t *testing.T) {
		r := newRecorder()
		d := newReconnectDecider(testGrace, r.fire)
		d.note("failed")
		if got := r.got(testGrace / 3); got != "pc-failed" {
			t.Fatalf("failed мав вирішити НЕГАЙНО, дістали %q", got)
		}
	})

	t.Run("closed → одразу", func(t *testing.T) {
		r := newRecorder()
		d := newReconnectDecider(testGrace, r.fire)
		d.note("closed")
		if got := r.got(testGrace / 3); got != "pc-closed" {
			t.Fatalf("closed мав вирішити НЕГАЙНО, дістали %q", got)
		}
	})

	t.Run("disconnected → лише після витримки", func(t *testing.T) {
		r := newRecorder()
		d := newReconnectDecider(testGrace, r.fire)
		d.note("disconnected")
		if !d.pending() {
			t.Fatal("після disconnected мала піти витримка")
		}
		// До витримки рішення бути НЕ повинно: стан транзієнтний.
		select {
		case s := <-r.ch:
			t.Fatalf("вирішив ДО витримки: %q", s)
		case <-time.After(testGrace / 3):
		}
		if got := r.got(6 * testGrace); got != "pc-disconnected-grace" {
			t.Fatalf("після витримки мав вирішити, дістали %q", got)
		}
	})

	t.Run("connected скасовує витримку", func(t *testing.T) {
		r := newRecorder()
		d := newReconnectDecider(testGrace, r.fire)
		d.note("disconnected")
		d.note("connected")
		if d.pending() {
			t.Fatal("connected мав погасити витримку")
		}
		if got := r.got(6 * testGrace); got != "" {
			t.Fatalf("сесія відновилась сама, а він усе одно вирішив: %q", got)
		}
	})

	t.Run("повторний disconnected не перезапускає витримку", func(t *testing.T) {
		r := newRecorder()
		d := newReconnectDecider(testGrace, r.fire)
		d.note("disconnected")
		time.Sleep(testGrace / 2)
		d.note("disconnected") // мигтіння стану не має відсувати рішення назавжди
		if got := r.got(6 * testGrace); got != "pc-disconnected-grace" {
			t.Fatalf("витримку перезапустили замість дочекатись, дістали %q", got)
		}
	})

	t.Run("рішення одноразове", func(t *testing.T) {
		// Наше ж close() транспорту дає "closed"; воно не має рахуватись новою
		// причиною — інакше реконект перезапускав би сам себе.
		r := newRecorder()
		d := newReconnectDecider(testGrace, r.fire)
		d.note("failed")
		d.note("closed")
		d.note("disconnected")
		if got := r.got(testGrace / 3); got != "pc-failed" {
			t.Fatalf("перше рішення = %q", got)
		}
		if got := r.got(6 * testGrace); got != "" {
			t.Fatalf("вирішив удруге: %q", got)
		}
	})
}

// TestBackoffGrowsAndCaps — витримка росте, але має стелю: лежачий hub не
// має отримати молотарку з парку ПК.
func TestBackoffGrowsAndCaps(t *testing.T) {
	d := reconnectBackoffMin
	prev := d
	for i := 0; i < 3; i++ {
		d = nextBackoff(d)
		if d <= prev {
			t.Fatalf("крок %d: витримка не виросла (%s -> %s)", i, prev, d)
		}
		prev = d
	}
	// Досить кроків, щоб гарантовано впертись у стелю (30с від 1с — 5 подвоєнь).
	for i := 0; i < 20; i++ {
		d = nextBackoff(d)
	}
	if d != reconnectBackoffMax {
		t.Fatalf("витримка не впирається у стелю: %s (стеля %s)", d, reconnectBackoffMax)
	}
	if got := nextBackoff(reconnectBackoffMax); got != reconnectBackoffMax {
		t.Fatalf("стеля має бути нерухома, дістали %s", got)
	}
}

// TestJitterBackoffStaysInBand — розкид ±20% і ніколи не нуль/відʼємний:
// нульова витримка — це та сама молотарка, лише без назви.
func TestJitterBackoffStaysInBand(t *testing.T) {
	const base = 8 * time.Second
	lo, hi := base-base/5, base+base/5
	for i := 0; i < 500; i++ {
		got := jitterBackoff(base)
		if got < lo || got > hi {
			t.Fatalf("розкид вийшов за смугу: %s (треба %s..%s)", got, lo, hi)
		}
	}
	// Дві сотні викликів мусять дати різні значення — інакше розкиду нема,
	// і парк усе одно піде на hub строєм.
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		seen[jitterBackoff(base)] = true
	}
	if len(seen) < 2 {
		t.Fatal("jitterBackoff повертає одне й те саме — розкиду немає")
	}
}

// ---------------------------------------------------------------------------
// Сторож живості хаба. Час тут КЕРОВАНИЙ: жодного сну, жодного time.Now() —
// усе рішення береться від переданого моменту, тому тест міряє ПРАВИЛО, а не
// планувальник.

// t0 — довільна фіксована точка відліку. Конкретне значення не має значення,
// значення мають лише різниці.
var t0 = time.Date(2026, 8, 30, 8, 41, 51, 0, time.UTC)

func newTestLiveness() *hubLiveness {
	return &hubLiveness{timeout: control.HeartbeatTimeout}
}

// (а) Хаб мовчить довше за поріг -> агент рве і перепідключається.
//
// Це і є аварія 30.08: о 08:41:51 агент підключився, о 08:44 хаб
// перезапустили, і далі — вісім хвилин мовчання при знятому публікаторі.
func TestHubLivenessSilentHubTearsDown(t *testing.T) {
	l := newTestLiveness()
	l.beat(t0) // перший удар нової сесії: сторож озброєно

	if l.silent(t0.Add(control.HeartbeatTimeout - time.Millisecond)) {
		t.Fatalf("вирішив рвати ДО порогу %s", control.HeartbeatTimeout)
	}
	if !l.silent(t0.Add(control.HeartbeatTimeout)) {
		t.Fatalf("хаб мовчить %s — сторож мав вирішити рвати", control.HeartbeatTimeout)
	}
	// Восьма хвилина мовчання не сміє дати ДРУГЕ рішення: реконект уже пішов,
	// і наступне має право визріти лише в новій сесії.
	if l.silent(t0.Add(8 * time.Minute)) {
		t.Fatal("вирішив удруге, не побачивши жодного удару нової сесії")
	}
}

// (б) 🔴 НАЙВАЖЛИВІШИЙ. Глядача немає, агент на паузі й не шле НІЧОГО, хаб
// ЖИВИЙ і б'є рівно — рвати не можна. Хибне спрацювання тут рвало б з'єднання
// кожні HeartbeatTimeout на КОЖНОМУ простоюючому ПК парку, тобто лікування
// вийшло б гіршим за хворобу.
func TestHubLivenessIdleAgentWithLiveHubNeverTearsDown(t *testing.T) {
	l := newTestLiveness()

	// Дві години простою. Пульс хаба — кожні HeartbeatInterval; сторожа
	// питаємо кожні 50 мс, бо саме з таким кроком обертається кадровий цикл на
	// паузі (main.go: gatePaused -> time.After(50ms)).
	const idle = 2 * time.Hour
	for elapsed := time.Duration(0); elapsed < idle; elapsed += control.HeartbeatInterval {
		l.beat(t0.Add(elapsed))
		for step := time.Duration(0); step < control.HeartbeatInterval; step += 50 * time.Millisecond {
			if now := t0.Add(elapsed + step); l.silent(now) {
				t.Fatalf("порвав ЖИВЕ з'єднання на простої через %s", elapsed+step)
			}
		}
	}
}

// Старий хаб пульсу не шле. Сторож мусить лишатись РОЗЗБРОЄНИМ: інакше новий
// агент рвав би справну сесію рівно за розкладом.
func TestHubLivenessDisarmedUntilFirstMessage(t *testing.T) {
	l := newTestLiveness()
	if l.silent(t0.Add(time.Hour)) {
		t.Fatal("вирішив рвати, не побачивши від хаба ЖОДНОГО повідомлення")
	}
}

// Будь-яке повідомлення каналу — ознака життя, не лише heartbeat.
func TestNoteHubMessageArmsOnAnyMessage(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"heartbeat", mustJSON(t, control.Heartbeat(1))},
		{"bitrate_target", mustJSON(t, control.BitrateTarget(2, 3_000_000))},
		{"текстовий resume старого протоколу", []byte("resume")},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newTestLiveness()
			noteHubMessage(l, t0, c.data)
			if l.silent(t0.Add(control.HeartbeatTimeout - time.Millisecond)) {
				t.Fatal("озброївся, але вирішив рвати ДО порогу")
			}
			if !l.silent(t0.Add(control.HeartbeatTimeout)) {
				t.Fatalf("%s не озброїв сторожа", c.name)
			}
		})
	}
}

// Явний shutdown від хаба — рішення БЕЗ очікування порогу: чекати 25 с того,
// про що вже сказали прямим текстом, нема сенсу.
func TestNoteHubMessageShutdownDecidesAtOnce(t *testing.T) {
	l := newTestLiveness()
	noteHubMessage(l, t0, mustJSON(t, control.Shutdown(1, "publisher lost: failed")))
	if !l.silent(t0) {
		t.Fatal("після shutdown від хаба сторож мав вирішити НЕГАЙНО")
	}
}

// N обґрунтоване, а не взяте з голови — обидві межі тут ВИКОНУВАНІ.
func TestHubSilenceTimeoutBounds(t *testing.T) {
	// Низ: канал надійний (SCTP ordered/reliable), тож удар не губиться, а
	// затримується ретрансмісією. Менше трьох ударів запасу — і сторож рватиме
	// живе на брижах мережі.
	if control.HeartbeatTimeout < 3*control.HeartbeatInterval {
		t.Fatalf("поріг %s — менше трьох ударів по %s: рватиме живе на затримках",
			control.HeartbeatTimeout, control.HeartbeatInterval)
	}
	// Верх: перезапуск хаба лікується за ДЕСЯТКИ секунд, не хвилини. Виявлення
	// плюс рукостискання нового дзвінка (waitConnected, 10 с) мусить лишатись
	// помітно меншим за хвилину — і меншим за ті ~45 с, за які 30.08
	// поверталися агенти, чий розрив PeerConnection таки побачив.
	const handshake = 10 * time.Second
	if control.HeartbeatTimeout+handshake >= 45*time.Second {
		t.Fatalf("поріг %s + рукостискання %s = %s: сторож став найповільнішою ланкою",
			control.HeartbeatTimeout, handshake, control.HeartbeatTimeout+handshake)
	}
}

func mustJSON(t *testing.T, m control.Msg) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %s: %v", m.Type, err)
	}
	return b
}

// Реконект з ІНШОЇ причини (помилка відправки, стан PeerConnection) не сміє
// лишати сторожа озброєним зі старим ударом: інакше він вирішить рвати щойно
// підняту сесію, не давши їй і одного heartbeat.
func TestHubLivenessDisarmOnNewSession(t *testing.T) {
	l := newTestLiveness()
	l.beat(t0)
	l.disarm() // саме це робить dialWebRTC на початку кожного дзвінка
	if l.silent(t0.Add(time.Hour)) {
		t.Fatal("вирішив рвати НОВУ сесію за старим ударом попередньої")
	}
}
