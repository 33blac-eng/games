package textmode

import "testing"

func TestFraction(t *testing.T) {
	cases := []struct {
		area int64
		w, h int
		want float64
	}{
		{0, 100, 100, 0},
		{-5, 100, 100, 0},
		{2500, 100, 100, 0.25},
		{20000, 100, 100, 1},
		{10, 0, 100, 1},
	}
	for _, c := range cases {
		if got := Fraction(c.area, c.w, c.h); got != c.want {
			t.Errorf("Fraction(%d,%d,%d)=%v want %v", c.area, c.w, c.h, got, c.want)
		}
	}
}

func TestEntersAfterQuietFrames(t *testing.T) {
	d := New(Config{})
	for i := 1; i < DefaultMinFrames; i++ {
		if text, _ := d.Update(0.002, 0); text {
			t.Fatalf("text mode after only %d frames", i)
		}
	}
	text, flipped := d.Update(0.002, 0)
	if !text || !flipped {
		t.Fatalf("want enter at frame %d, got text=%v flipped=%v", DefaultMinFrames, text, flipped)
	}
	if _, flipped := d.Update(0.002, 0); flipped {
		t.Fatal("flipped again while staying in text mode")
	}
}

func TestMotionBlocksAndExits(t *testing.T) {
	d := New(Config{})
	for i := 0; i < 50; i++ {
		if text, _ := d.Update(0.01, 0.5); text { // scrolling: small dirty, big move
			t.Fatal("scrolling classified as text")
		}
	}
	d = New(Config{})
	for i := 0; i < 20; i++ {
		d.Update(0.001, 0)
	}
	if !d.Text() {
		t.Fatal("expected text mode")
	}
	exited := false
	for i := 0; i < 10; i++ {
		if text, flipped := d.Update(1, 0); !text && flipped {
			exited = true
			break
		}
	}
	if !exited {
		t.Fatal("full-screen changes did not leave text mode")
	}
}

func TestHysteresis(t *testing.T) {
	d := New(Config{})
	for i := 0; i < 30; i++ {
		d.Update(0.01, 0)
	}
	if !d.Text() {
		t.Fatal("expected text mode")
	}
	// Between Enter and Exit thresholds: stay in text mode.
	for i := 0; i < 50; i++ {
		d.Update(0.10, 0)
	}
	if !d.Text() {
		t.Fatal("hysteresis band dropped text mode")
	}
}

func TestOneBigFrameResetsQuietCount(t *testing.T) {
	d := New(Config{Alpha: 1, MinFrames: 3})
	d.Update(0, 0)
	d.Update(0, 0)
	d.Update(1, 0)
	if text, _ := d.Update(0, 0); text {
		t.Fatal("quiet count not reset by a big change")
	}
	d.Update(0, 0)
	if text, _ := d.Update(0, 0); !text {
		t.Fatal("expected text after 3 quiet frames")
	}
	d.Reset()
	if d.Text() {
		t.Fatal("Reset kept text mode")
	}
}

func TestClampNaN(t *testing.T) {
	d := New(Config{Alpha: 1})
	nan := 0.0
	nan = nan / nan
	d.Update(nan, -1)
	if c, m := d.Smoothed(); c != 0 || m != 0 {
		t.Fatalf("got %v %v", c, m)
	}
}
