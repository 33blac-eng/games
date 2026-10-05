package contentmode

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

const fr = time.Second / 30

// feed подає n кадрів по fr, повертає останній режим і момент.
func feed(m *Machine, at time.Time, n int, changed, moved float64) (Mode, time.Time) {
	var mode Mode
	for i := 0; i < n; i++ {
		at = at.Add(fr)
		mode, _ = m.Update(changed, moved, at)
	}
	return mode, at
}

func TestVideoEnterAfterHold(t *testing.T) {
	m := New(Config{})
	mode, at := feed(m, t0, 29, 0.11, 0) // < 1 s серії
	if mode == Video {
		t.Fatalf("video too early")
	}
	mode, _ = feed(m, at, 3, 0.11, 0)
	if mode != Video {
		t.Fatalf("want video after 1 s of 11%% motion, got %v", mode)
	}
}

func TestScrollViaMoveRectsEntersVideo(t *testing.T) {
	m := New(Config{})
	if mode, _ := feed(m, t0, 40, 0.02, 0.9); mode != Video {
		t.Fatalf("scroll (move rects) want video, got %v", mode)
	}
}

func TestSmallMotionNeverVideo(t *testing.T) {
	m := New(Config{})
	if mode, _ := feed(m, t0, 300, 0.09, 0); mode == Video {
		t.Fatal("9% motion must not enter video")
	}
}

func TestSporadicLargeChangesNotVideo(t *testing.T) {
	m := New(Config{})
	at := t0
	for i := 0; i < 20; i++ { // великий кадр раз на 400 мс (перемикання вікон)
		at = at.Add(400 * time.Millisecond)
		if mode, _ := m.Update(0.8, 0, at); mode == Video {
			t.Fatalf("sporadic large changes entered video at %d", i)
		}
	}
}

func TestStreakBrokenBySmallFrame(t *testing.T) {
	m := New(Config{})
	_, at := feed(m, t0, 20, 0.5, 0)
	_, at = feed(m, at, 1, 0.01, 0) // обрив серії
	if mode, _ := feed(m, at, 20, 0.5, 0); mode == Video {
		t.Fatal("streak must restart after a small frame")
	}
}

func TestExitHysteresisAndHold(t *testing.T) {
	m := New(Config{})
	_, at := feed(m, t0, 40, 0.3, 0)
	// 5 % підтримує режим (≥ ExitArea 4 %), хоч менше за вхід.
	if mode, _ := feed(m, at, 120, 0.05, 0); mode != Video {
		t.Fatalf("5%% motion should hold video, got %v", mode)
	}
	at = at.Add(120 * fr)
	// Рух стих: дрібні кадри не підтримують, вихід через ExitHold.
	mode, at2 := feed(m, at, 40, 0.01, 0) // 1.33 s
	if mode != Video {
		t.Fatalf("exit before ExitHold: %v", mode)
	}
	mode, _ = feed(m, at2, 6, 0.01, 0)
	if mode == Video {
		t.Fatal("still video after ExitHold")
	}
}

func TestTickExitsWhenScreenStops(t *testing.T) {
	m := New(Config{})
	_, at := feed(m, t0, 40, 0.3, 0)
	if mode, _ := m.Tick(at.Add(time.Second)); mode != Video {
		t.Fatal("tick exited before hold")
	}
	mode, flipped := m.Tick(at.Add(2 * time.Second))
	if mode == Video || !flipped {
		t.Fatalf("tick want exit flip, got %v %v", mode, flipped)
	}
}

func TestPriorityVideoOverTextAndReturnToText(t *testing.T) {
	m := New(Config{})
	mode, at := feed(m, t0, 20, 0.001, 0)
	if mode != Text {
		t.Fatalf("want text after quiet typing, got %v", mode)
	}
	// 11 % відео одразу після набору: текстовий детектор (вихід > 20 %) ще
	// тримав би Text, але Video має пріоритет.
	mode, at = feed(m, at, 40, 0.11, 0)
	if mode != Video {
		t.Fatalf("want video over text, got %v", mode)
	}
	if !m.TextDetector().Text() {
		t.Log("text detector left text mode on its own (ok)")
	}
	// Відео зупинилось, знову набір: після ExitHold — Text.
	mode, _ = feed(m, at, 80, 0.001, 0)
	if mode != Text {
		t.Fatalf("want text after video stops, got %v", mode)
	}
}

func TestNoVideoIsTextDetector(t *testing.T) {
	m := New(Config{NoVideo: true})
	if mode, _ := feed(m, t0, 100, 1, 0); mode != Normal {
		t.Fatalf("NoVideo: want normal, got %v", mode)
	}
	if mode, _ := feed(m, t0.Add(10*time.Second), 20, 0, 0); mode != Text {
		t.Fatalf("NoVideo: want text, got %v", mode)
	}
}

func TestFlippedOnlyOnChange(t *testing.T) {
	m := New(Config{})
	flips := 0
	at := t0
	for i := 0; i < 200; i++ {
		at = at.Add(fr)
		if _, f := m.Update(0.5, 0, at); f {
			flips++
		}
	}
	if flips != 1 {
		t.Fatalf("flips=%d want 1", flips)
	}
}

func TestReset(t *testing.T) {
	m := New(Config{})
	feed(m, t0, 40, 0.5, 0)
	m.Reset()
	if m.Mode() != Normal {
		t.Fatal("reset")
	}
	if mode, _ := feed(m, t0.Add(time.Minute), 10, 0.5, 0); mode == Video {
		t.Fatal("reset must clear streak")
	}
}

func TestArea(t *testing.T) {
	nan := 0.0
	nan /= nan
	for _, c := range []struct{ c, m, w float64 }{{0.2, 0.3, 0.5}, {0.8, 0.8, 1}, {-1, 0, 0}, {nan, 0, 0}} {
		if g := Area(c.c, c.m); g != c.w {
			t.Errorf("Area(%v,%v)=%v want %v", c.c, c.m, g, c.w)
		}
	}
}

func TestFPS(t *testing.T) {
	hw := FPSInput{BaseFPS: 30, VideoFPS: 60, Hardware: true, EncSec: 0.004}
	cases := []struct {
		name string
		mode Mode
		in   FPSInput
		want int
	}{
		{"normal", Normal, hw, 30},
		{"text", Text, hw, 30},
		{"hw video", Video, hw, 60},
		{"hw unmeasured", Video, FPSInput{BaseFPS: 30, VideoFPS: 60, Hardware: true}, 30},
		{"hw too slow", Video, FPSInput{BaseFPS: 30, VideoFPS: 60, Hardware: true, EncSec: 0.012}, 30},
		{"hw custom load", Video, FPSInput{BaseFPS: 30, VideoFPS: 60, Hardware: true, EncSec: 0.012, MaxLoad: 0.8}, 60},
		{"sw not permitted", Video, FPSInput{BaseFPS: 30, VideoFPS: 60, EncSec: 0.001}, 30},
		{"sw permits 50", Video, FPSInput{BaseFPS: 30, VideoFPS: 60, SoftwareFPS: 50}, 50},
		{"sw permits more", Video, FPSInput{BaseFPS: 30, VideoFPS: 60, SoftwareFPS: 90}, 60},
		{"sw below base", Video, FPSInput{BaseFPS: 30, VideoFPS: 60, SoftwareFPS: 20}, 30},
		{"video fps not above base", Video, FPSInput{BaseFPS: 60, VideoFPS: 60, Hardware: true, EncSec: 0.001}, 60},
	}
	for _, c := range cases {
		if g := FPS(c.mode, c.in); g != c.want {
			t.Errorf("%s: FPS=%d want %d", c.name, g, c.want)
		}
	}
}

func TestGap(t *testing.T) {
	if Gap(0) != 0 || Gap(60) != time.Second/60 {
		t.Fatal("gap")
	}
}

func TestModeString(t *testing.T) {
	if Normal.String() != "normal" || Text.String() != "text" || Video.String() != "video" {
		t.Fatal("string")
	}
}
