package keyframe

import (
	"testing"
	"time"
)

func TestDisabled(t *testing.T) {
	p := New(Config{})
	for i := 0; i < 1000; i++ {
		p.Coded(false)
	}
	if p.Due(time.Unix(100, 0), true) || p.EncoderGOP() != 0 {
		t.Fatal("вимкнена політика просить IDR")
	}
}

func TestIDRWaitsForStillness(t *testing.T) {
	t0 := time.Unix(0, 0)
	p := New(Config{GOPFrames: 300})
	if p.EncoderGOP() != 600 {
		t.Fatalf("EncoderGOP %d", p.EncoderGOP())
	}
	p.Coded(true)
	now := t0
	// 299 кадрів руху: GOP ще не вийшов.
	for i := 0; i < 299; i++ {
		p.Motion(now)
		p.Coded(false)
		now = now.Add(33 * time.Millisecond)
	}
	if p.Due(now.Add(time.Hour), true) {
		t.Fatal("IDR до кінця GOP")
	}
	// Ще 100 кадрів руху: GOP вийшов, але тиші нема.
	for i := 0; i < 100; i++ {
		p.Motion(now)
		p.Coded(false)
		if p.Due(now, false) {
			t.Fatal("IDR на кадрі з рухом")
		}
		now = now.Add(33 * time.Millisecond)
	}
	// Рух стих: keepalive через 200 мс — ще не тиша.
	last := now.Add(-33 * time.Millisecond)
	if p.Due(last.Add(200*time.Millisecond), true) {
		t.Fatal("IDR до Idle")
	}
	if !p.Due(last.Add(DefaultIdle), true) {
		t.Fatal("нема IDR у тиші після GOP")
	}
	// Refine-кадр (still=false) IDR не отримує.
	if p.Due(last.Add(time.Second), false) {
		t.Fatal("IDR на не-keepalive кадрі")
	}
	p.Coded(true)
	if p.Since() != 0 || p.Due(last.Add(2*time.Second), true) {
		t.Fatal("після IDR лічильник не скинуто")
	}
}

// IDR на запит глядача теж обнуляє GOP: періодичний не йде слідом.
func TestRequestedIDRResets(t *testing.T) {
	p := New(Config{GOPFrames: 10, Idle: time.Millisecond})
	for i := 0; i < 9; i++ {
		p.Coded(false)
	}
	p.Coded(true)
	p.Coded(false)
	if p.Due(time.Unix(10, 0), true) {
		t.Fatal("IDR одразу після запитаного")
	}
}

// Q-11: VFR-таймлайн — 3 с тексту на 15 fps, далі keepalive 1 кадр/с.
// GOP у кадрах (300 = 10 с × 30 fps) за 12 с не набирається ніколи; TimeGOP
// просить IDR рівно після 10 с від останнього IDR.
func TestTimeGOPVariableFrameRate(t *testing.T) {
	t0 := time.Unix(1000, 0)
	frames := New(Config{GOPFrames: 300})
	tg := NewTime(10 * time.Second)
	tg.Coded(t0, true)
	frames.Coded(true)
	var at []time.Duration
	step := func(d time.Duration) {
		now := t0.Add(d)
		if frames.Due(now, true) {
			t.Fatalf("GOP у кадрах спрацював на %v — таймлайн не VFR", d)
		}
		if tg.Due(now) {
			at = append(at, d)
			tg.Coded(now, true)
			return
		}
		tg.Coded(now, false)
		frames.Coded(false)
	}
	for d := time.Duration(0); d < 3*time.Second; d += time.Second / 15 {
		step(d)
	}
	for d := 3 * time.Second; d <= 12*time.Second; d += time.Second {
		step(d)
	}
	if len(at) != 1 || at[0] < 10*time.Second || at[0] > 11*time.Second {
		t.Fatalf("TimeGOP: IDR на %v, хочу один у [10 с, 11 с]", at)
	}
	if NewTime(0).Due(t0.Add(time.Hour)) {
		t.Fatal("NewTime(0) мусить бути вимкненим")
	}
}
