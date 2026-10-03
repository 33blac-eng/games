package refine

import (
	"testing"
	"time"
)

func TestIdleThenTwoFramesThenStop(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := New(Config{MinGap: 33 * time.Millisecond})
	if _, ok := s.Due(t0); ok {
		t.Fatal("due before any motion")
	}
	if w := s.Wait(t0, time.Second); w != time.Second {
		t.Fatalf("unarmed wait %v", w)
	}
	s.Motion(t0)
	if w := s.Wait(t0, time.Second); w != DefaultIdle {
		t.Fatalf("armed wait %v", w)
	}
	if _, ok := s.Due(t0.Add(199 * time.Millisecond)); ok {
		t.Fatal("due before idle")
	}
	now := t0.Add(200 * time.Millisecond)
	qp, ok := s.Due(now)
	if !ok || qp != 22 {
		t.Fatalf("first refine %d %v", qp, ok)
	}
	s.Sent(now, 1000, 12_000_000)
	if _, ok := s.Due(now.Add(10 * time.Millisecond)); ok {
		t.Fatal("min gap ignored")
	}
	now = now.Add(33 * time.Millisecond)
	qp, ok = s.Due(now)
	if !ok || qp != 18 {
		t.Fatalf("second refine %d %v", qp, ok)
	}
	s.Sent(now, 1000, 12_000_000)
	if _, ok := s.Due(now.Add(time.Hour)); ok {
		t.Fatal("third refine")
	}
	if w := s.Wait(now, time.Second); w != time.Second {
		t.Fatalf("done wait %v", w)
	}
	if !s.NeedRestore() || s.NeedRestore() {
		t.Fatal("restore must be reported exactly once")
	}
}

func TestMotionAborts(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := New(Config{})
	s.Motion(t0)
	now := t0.Add(DefaultIdle)
	if _, ok := s.Due(now); !ok {
		t.Fatal("not due")
	}
	s.Sent(now, 100, 0)
	s.Motion(now.Add(time.Millisecond)) // рух: refine переривається
	if s.Refining() {
		t.Fatal("still refining after motion")
	}
	if _, ok := s.Due(now.Add(50 * time.Millisecond)); ok {
		t.Fatal("refine without fresh idle")
	}
	if !s.NeedRestore() {
		t.Fatal("encoder settings not restored after abort")
	}
	if qp, ok := s.Due(now.Add(time.Millisecond + DefaultIdle)); !ok || qp != 22 {
		t.Fatal("refine sequence did not restart from first QP")
	}
}

func TestBudgetUnderPeak(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := New(Config{})
	s.Motion(t0)
	now := t0.Add(DefaultIdle)
	// 150 КБ на 12 Мбіт/с = 100 мс на піку.
	s.Sent(now, 150_000, 12_000_000)
	if _, ok := s.Due(now.Add(99 * time.Millisecond)); ok {
		t.Fatal("budget exceeded peak")
	}
	if _, ok := s.Due(now.Add(100 * time.Millisecond)); !ok {
		t.Fatal("not due after budget window")
	}
}

func TestPostponeAndDisarm(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := New(Config{})
	s.Motion(t0)
	now := t0.Add(DefaultIdle)
	s.Postpone(now, 30*time.Millisecond)
	if _, ok := s.Due(now); ok {
		t.Fatal("postpone ignored")
	}
	if w := s.Wait(now, time.Second); w != 30*time.Millisecond {
		t.Fatalf("wait %v", w)
	}
	s.Disarm()
	if _, ok := s.Due(now.Add(time.Hour)); ok {
		t.Fatal("due after disarm")
	}
	if w := s.Wait(now.Add(time.Hour), time.Second); w != time.Second {
		t.Fatal("wait after disarm")
	}
}
