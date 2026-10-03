package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

// На нерухомому екрані DXGI не віддає кадрів узагалі, а сторож у браузері рве
// сесію після 3000 мс без кадру. Тест тримає три межі keepalive: чекали довше
// порогу — пересилаємо останній кадр; дочекались кадру — ні; на паузі — ніколи.
func TestShouldKeepalive(t *testing.T) {
	cases := []struct {
		name     string
		waitErr  error
		paused   bool
		haveLast bool
		want     bool
	}{
		{"чекали довше порогу — пересилаємо останній", context.DeadlineExceeded, false, true, true},
		{"кадр прийшов раніше порогу — не пересилаємо", nil, false, true, false},
		{"на паузі не пересилаємо (гейтинг важливіший)", context.DeadlineExceeded, true, true, false},
		{"на паузі не пересилаємо навіть без кадру", context.DeadlineExceeded, true, false, false},
		{"першого кадру ще не було — пересилати нема чого", context.DeadlineExceeded, false, false, false},
		{"агента зупиняють — не пересилаємо", context.Canceled, false, true, false},
		{"чужа помилка захоплення — не пересилаємо", io.ErrUnexpectedEOF, false, true, false},
		{"обгорнутий дедлайн усе одно дедлайн", fmt.Errorf("capture: %w", context.DeadlineExceeded), false, true, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldKeepalive(c.waitErr, c.paused, c.haveLast)
			if got != c.want {
				t.Fatalf("shouldKeepalive(%v, paused=%v, haveLast=%v) = %v; хочемо %v",
					c.waitErr, c.paused, c.haveLast, got, c.want)
			}
		})
	}
}

// Поріг має лишатись помітно меншим за сторож 3000 мс у web/desktop-oo.js:
// саме з цього запасу випливає, що два поспіль загублені keepalive ще не
// валять сесію. Якщо хтось підніме keepaliveAfter — має спершу побачити це.
func TestKeepaliveAfterLeavesRoomForWatchdog(t *testing.T) {
	const browserWatchdog = 3000 * time.Millisecond

	if keepaliveAfter*3 > browserWatchdog {
		t.Fatalf("keepaliveAfter=%v: два пропущені keepalive (%v) вже перевищують сторож %v",
			keepaliveAfter, keepaliveAfter*3, browserWatchdog)
	}
	if keepaliveAfter <= 0 {
		t.Fatalf("keepaliveAfter=%v — дедлайн очікування має бути додатним", keepaliveAfter)
	}
}

// PTS keepalive-кадру має відповідати РЕАЛЬНОМУ простою: Duration у
// webrtcTransport.send — це різниця PTS сусідніх AU, і саме вона крутить
// RTP-годинник у pion. Якщо лічильник рухати на +1, RTP відставатиме від
// стінного годинника майже на цілий keepaliveAfter щоразу.
func TestKeepaliveAdvancesPTSByRealIdleTime(t *testing.T) {
	for _, fps := range []int{15, 30, 60} {
		frameInterval := time.Second / time.Duration(fps)

		var captureSeq uint64
		ptsOf := func() time.Duration {
			return time.Duration(captureSeq) * time.Second / time.Duration(fps)
		}

		before := ptsOf()
		captureSeq += seqAdvance(keepaliveAfter, frameInterval) // те саме, що робить цикл
		got := ptsOf() - before

		if got != keepaliveAfter {
			t.Fatalf("fps=%d: PTS зсунувся на %v; хочемо %v (реальний простій)",
				fps, got, keepaliveAfter)
		}
	}
}

// PTS ЗВИЧАЙНОГО кадру має так само йти за стінним годинником. На нерухомому
// екрані кадр приходить через сотні мілісекунд, і зсув на один кадровий
// інтервал лишає RTP-годинник позаду стінного: приймач рахує цю різницю як
// jitter, а hub на jitter скручує бітрейт до підлоги при нульових втратах
// (виміряно: перемальовка 20 Гц -> jitter 20–24 мс; рух 66 Гц -> 0.2–1.0 мс).
func TestRealFramePTSFollowsWallClock(t *testing.T) {
	for _, fps := range []int{15, 30, 60} {
		frameInterval := time.Second / time.Duration(fps)

		for _, waited := range []time.Duration{
			700 * time.Millisecond, // екран стоїть, кадр ледь устиг до keepalive
			250 * time.Millisecond, // рідка перемальовка
			50 * time.Millisecond,  // 20 Гц — саме той режим, де мірявся jitter
			5 * time.Millisecond,   // швидше за 1/fps: PTS усе одно мусить рости
		} {
			var captureSeq uint64
			ptsOf := func() time.Duration {
				return time.Duration(captureSeq) * time.Second / time.Duration(fps)
			}

			before := ptsOf()
			captureSeq += seqAdvance(waited, frameInterval) // те саме, що робить цикл
			got := ptsOf() - before

			if got <= 0 {
				t.Fatalf("fps=%d чекали %v: PTS не зріс (%v) — різниця PTS нульова, send підставить абсолютний PTS",
					fps, waited, got)
			}
			// Похибка в межах одного кадрового інтервалу — це квантування
			// сітки PTS, а не відставання: воно не накопичується.
			want := waited
			if want < frameInterval {
				want = frameInterval
			}
			if d := got - want; d > frameInterval || d < -frameInterval {
				t.Fatalf("fps=%d чекали %v: PTS зсунувся на %v (хочемо %v ±%v) — RTP розійдеться зі стінним годинником на %v, приймач побачить це як jitter",
					fps, waited, got, want, frameInterval, want-got)
			}
		}
	}
}

// Сесія, що стартувала на ВЖЕ нерухомому екрані: жодного кадру не було,
// keepalive повторювати нема чого, і глядач падає на first-frame-timeout.
// Єдиний вихід — пересоздати дуплікацію (свіжа віддає поточний робочий стіл).
func TestShouldRearm(t *testing.T) {
	cases := []struct {
		name     string
		waitErr  error
		paused   bool
		haveLast bool
		want     bool
	}{
		{"нерухомий екран і жодного кадру — беремо через GDI", context.DeadlineExceeded, false, false, true},
		{"кадр уже був — keepalive впорається сам", context.DeadlineExceeded, false, true, false},
		{"кадр прийшов вчасно — хапати нема чого", nil, false, false, false},
		{"на паузі не хапаємо: глядача нема", context.DeadlineExceeded, true, false, false},
		{"агента зупиняють — не хапаємо", context.Canceled, false, false, false},
		{"чужа помилка захоплення — її лікує звичайний retry", io.ErrUnexpectedEOF, false, false, false},
		{"обгорнутий дедлайн усе одно дедлайн", fmt.Errorf("capture: %w", context.DeadlineExceeded), false, false, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRearm(c.waitErr, c.paused, c.haveLast); got != c.want {
				t.Fatalf("shouldRearm(%v, paused=%v, haveLast=%v) = %v; хочемо %v",
					c.waitErr, c.paused, c.haveLast, got, c.want)
			}
		})
	}
}

// Дужки в shouldKeepalive легко зламати так, що дедлайн почне «протікати» у
// гілку помилки. Перевіряємо, що звичайна помилка захоплення не видає себе
// за дедлайн навіть коли решта умов keepalive виконана.
func TestShouldKeepaliveIgnoresNonDeadlineErrors(t *testing.T) {
	for _, err := range []error{
		errors.New("capture: output 0: device removed"),
		context.Canceled,
		io.EOF,
	} {
		if shouldKeepalive(err, false, true) {
			t.Fatalf("shouldKeepalive(%v) = true; помилка захоплення не дедлайн", err)
		}
	}
}

// Три межі admission: вільний транспорт пропускає все; зайнятий ріже; але
// зайнятий НЕ має права мовчати довше за admissionFloor — саме там гине
// keepalive-кадр, а з ним і сесія.
func TestShouldAdmit(t *testing.T) {
	cases := []struct {
		name          string
		queued        int64
		sinceAdmitted time.Duration
		want          bool
	}{
		{"транспорт вільний — пускаємо", 0, 0, true},
		{"транспорт вільний, пауза була довга — все одно пускаємо", 0, 5 * time.Second, true},
		{"транспорт зайнятий, пауза коротка — ріжемо", 1, 10 * time.Millisecond, false},
		{"транспорт зайнятий, пауза майже на межі — ще ріжемо", 1, admissionFloor - time.Millisecond, false},
		{"транспорт зайнятий, пауза на межі — пускаємо", 1, admissionFloor, true},
		{"транспорт зайнятий, пауза за межею — пускаємо", 1, admissionFloor + time.Second, true},
		{"черга глибока — межа однаково діє", 8, admissionFloor, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldAdmit(c.queued, c.sinceAdmitted); got != c.want {
				t.Fatalf("shouldAdmit(queued=%d, since=%v) = %v; хочемо %v",
					c.queued, c.sinceAdmitted, got, c.want)
			}
		})
	}
}

// Насичений транспорт: queued не падає до нуля НІКОЛИ. Екран при цьому
// нерухомий, тож кожен прохід циклу коштує рівно keepaliveAfter — NextFrame
// вичікує дедлайн наново після кожного continue. Модель цих двох умов і є той
// випадок, у якому admission-дроп викидав keepalive-кадр: без межі кадрів не
// виходить ЖОДНОГО, і сторож у браузері рве сесію безповоротно.
func TestAdmissionKeepsPauseUnderWatchdog(t *testing.T) {
	const browserWatchdog = 3000 * time.Millisecond
	const transportBusy = 1 // відправка попереднього кадру ще не завершилась

	total := 30 * time.Second // на порядок довше за сторож
	now := time.Time{}
	lastAdmitAt := now

	var worst time.Duration
	var admits, drops int
	for i := 0; i < int(total/keepaliveAfter); i++ {
		now = now.Add(keepaliveAfter) // дедлайн вичерпано -> keepalive-кадр
		if !shouldAdmit(transportBusy, now.Sub(lastAdmitAt)) {
			drops++
			continue
		}
		if gap := now.Sub(lastAdmitAt); gap > worst {
			worst = gap
		}
		lastAdmitAt = now
		admits++
	}

	if admits == 0 {
		t.Fatalf("за %v насиченого транспорту admission не пропустив ЖОДНОГО кадру — сторож %v рве сесію",
			total, browserWatchdog)
	}
	if drops == 0 {
		t.Fatal("жодного дропу не сталось — тест не перевіряє admission, а лише сам себе")
	}
	if worst >= browserWatchdog {
		t.Fatalf("найдовша пауза між пропущеними кадрами %v >= сторож %v (admits=%d drops=%d)",
			worst, browserWatchdog, admits, drops)
	}
}

// Межа має лишати запас на саму відправку: кадр, який admission пропустив,
// мусить ще встигнути дійти до браузера. Якщо хтось підніме admissionFloor до
// сторожа впритул — запасу не лишиться, і межа стане декорацією.
func TestAdmissionFloorLeavesRoomForDelivery(t *testing.T) {
	const browserWatchdog = 3000 * time.Millisecond

	if admissionFloor >= browserWatchdog {
		t.Fatalf("admissionFloor=%v >= сторож %v: примусовий кадр приходить уже після смерті сесії",
			admissionFloor, browserWatchdog)
	}
	if admissionFloor < keepaliveAfter {
		t.Fatalf("admissionFloor=%v < keepaliveAfter=%v: межа спрацьовує раніше за keepalive, admission вимкнено",
			admissionFloor, keepaliveAfter)
	}
}
