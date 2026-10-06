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
