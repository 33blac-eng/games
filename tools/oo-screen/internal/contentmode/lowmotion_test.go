package contentmode

import (
	"math"
	"testing"
	"time"
)

func TestCapperFullMotionLiftsImmediately(t *testing.T) {
	c := NewCapper(CapConfig{})
	t0 := time.Unix(0, 0)
	for i := 0; i < 30; i++ { // ~1 с набору
		c.Update(Text, 0.005, t0.Add(time.Duration(i)*33*time.Millisecond))
	}
	if c.Frac() != DefaultLowFrac {
		t.Fatalf("after 1s typing frac=%v", c.Frac())
	}
	if got := c.Bps(8_000_000); got != 2_000_000 {
		t.Fatalf("bps=%d", got)
	}
	if f := c.Update(Text, 0.6, t0.Add(1100*time.Millisecond)); f != 1 {
		t.Fatalf("scroll frame must lift cap, frac=%v", f)
	}
}

func TestCapperHoldBeforeLowering(t *testing.T) {
	c := NewCapper(CapConfig{})
	t0 := time.Unix(0, 0)
	if f := c.Update(Normal, 0.01, t0); f != 1 {
		t.Fatalf("first small frame lowered at once: %v", f)
	}
	if f := c.Update(Normal, 0.01, t0.Add(400*time.Millisecond)); f != 1 {
		t.Fatalf("lowered before hold: %v", f)
	}
	if f := c.Update(Normal, 0.01, t0.Add(500*time.Millisecond)); f != DefaultLowFrac {
		t.Fatalf("not lowered after hold: %v", f)
	}
	// великий кадр рве серію: знову чекаємо Hold
	c.Update(Normal, 0.5, t0.Add(600*time.Millisecond))
	if f := c.Update(Normal, 0.01, t0.Add(700*time.Millisecond)); f != 1 {
		t.Fatalf("hold not restarted: %v", f)
	}
}

func TestCapperVideoCorner(t *testing.T) {
	c := NewCapper(CapConfig{})
	t0 := time.Unix(0, 0)
	c.Update(Text, 0.01, t0)
	c.Update(Text, 0.01, t0.Add(time.Second))
	if c.Frac() != DefaultLowFrac {
		t.Fatal(c.Frac())
	}
	if f := c.Update(Video, 0.11, t0.Add(1100*time.Millisecond)); f != DefaultVideoFrac {
		t.Fatalf("raise to video frac must be immediate: %v", f)
	}
}

func TestCapperMinAndNaN(t *testing.T) {
	c := NewCapper(CapConfig{Hold: time.Millisecond})
	t0 := time.Unix(0, 0)
	c.Update(Text, 0, t0)
	c.Update(Text, 0, t0.Add(time.Second))
	if got := c.Bps(2_000_000); got != DefaultMinCapBps {
		t.Fatalf("min not applied: %d", got)
	}
	if got := c.Bps(800_000); got != 800_000 {
		t.Fatalf("cap above target: %d", got)
	}
	if f := c.Update(Text, math.NaN(), t0.Add(2*time.Second)); f != 1 {
		t.Fatalf("NaN area must fail open: %v", f)
	}
	c.Reset()
	if c.Frac() != 1 {
		t.Fatal("reset")
	}
}
