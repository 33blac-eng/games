package main

import (
	"testing"
	"time"
)

func TestTextModeGap(t *testing.T) {
	cases := []struct {
		textFPS, fps int
		text         bool
		want         time.Duration
	}{
		{15, 30, true, time.Second / 15},
		{15, 30, false, 0},
		{0, 30, true, 0},
		{30, 30, true, 0},
		{60, 30, true, 0},
		{15, 60, true, time.Second / 15},
	}
	for _, c := range cases {
		if got := textModeGap(c.textFPS, c.fps, c.text); got != c.want {
			t.Errorf("textModeGap(%d,%d,%v)=%v want %v", c.textFPS, c.fps, c.text, got, c.want)
		}
	}
}

func TestTextFlushWait(t *testing.T) {
	gap := time.Second / 15
	if w, ok := textFlushWait(false, gap, 0, time.Second); ok || w != time.Second {
		t.Fatalf("not pending: %v %v", w, ok)
	}
	if w, ok := textFlushWait(true, gap, 20*time.Millisecond, time.Second); !ok || w != gap-20*time.Millisecond {
		t.Fatalf("pending: %v %v", w, ok)
	}
	if w, ok := textFlushWait(true, gap, time.Second, time.Second); !ok || w != time.Millisecond {
		t.Fatalf("overdue: %v %v", w, ok)
	}
	// refine already wakes us earlier: keep it.
	if w, ok := textFlushWait(true, gap, 0, 10*time.Millisecond); ok || w != 10*time.Millisecond {
		t.Fatalf("shorter wait kept: %v %v", w, ok)
	}
}

func TestTextFlushDue(t *testing.T) {
	gap := 66 * time.Millisecond
	if !textFlushDue(true, false, true, gap, gap) {
		t.Fatal("due")
	}
	if textFlushDue(true, false, true, gap, gap-time.Millisecond) {
		t.Fatal("early")
	}
	if textFlushDue(true, true, true, gap, gap) || textFlushDue(true, false, false, gap, gap) || textFlushDue(false, false, true, gap, gap) {
		t.Fatal("paused/no frame/not pending")
	}
}

func TestTextCapApplies(t *testing.T) {
	if !textCapApplies(true, 0.001) {
		t.Fatal("glyph-sized change in text mode must be capped")
	}
	if textCapApplies(true, 0.11) || textCapApplies(false, 0.001) {
		t.Fatal("mid-size change / non-text must not be capped")
	}
}
