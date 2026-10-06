package refine

import (
	"testing"
	"time"
)

// C3: MFT ігнорує QP семпла (потік каже 26, хоча просили 22/18) — Converge
// шле ще кадри з TargetQP, доки виміряний QP не дійде до цілі.
func TestConvergeUntilMeasuredQP(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := New(Config{Converge: true, TargetQP: 16, MaxExtra: 4})
	s.Motion(t0)
	s.Coded(t0, Frame{Motion: true, QP: 30})
	now := t0.Add(DefaultIdle)
	measured := []int{26, 24, 20, 16} // що MFT справді видав на кожен refine
	var asked []int
	for i := 0; i < 10; i++ {
		qp, ok := s.Due(now)
		if !ok {
			break
		}
		asked = append(asked, qp)
		s.Sent(now, 1000, 0)
		s.Coded(now, Frame{Refine: qp, QP: measured[min(len(asked)-1, len(measured)-1)]})
		now = now.Add(time.Millisecond)
	}
	want := []int{22, 18, 16, 16}
	if len(asked) != len(want) {
		t.Fatalf("кадри %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Fatalf("кадри %v, want %v", asked, want)
		}
	}
	if !s.Complete() || s.WorstQP() != 16 {
		t.Fatalf("complete=%v worst=%d", s.Complete(), s.WorstQP())
	}
	// Без Converge — рівно два кадри, як раніше.
	s2 := New(Config{})
	s2.Motion(t0)
	n := 0
	for now := t0.Add(DefaultIdle); n < 10; now = now.Add(time.Millisecond) {
		qp, ok := s2.Due(now)
		if !ok {
			break
		}
		s2.Sent(now, 1000, 0)
		s2.Coded(now, Frame{Refine: qp, QP: 26})
		n++
	}
	if n != 2 {
		t.Fatalf("без Converge %d кадрів", n)
	}
}

// Стеля кадрів, бюджет байтів, невідомий QP і рух зупиняють збіжність.
func TestConvergeLimits(t *testing.T) {
	t0 := time.Unix(0, 0)
	run := func(cfg Config, qp int, bytes int) int {
		cfg.Converge = true
		s := New(cfg)
		s.Motion(t0)
		now := t0.Add(DefaultIdle)
		n := 0
		for ; n < 50; n++ {
			q, ok := s.Due(now)
			if !ok {
				break
			}
			s.Sent(now, bytes, 0)
			s.Coded(now, Frame{Refine: q, QP: qp})
			now = now.Add(time.Millisecond)
		}
		return n
	}
	if n := run(Config{MaxExtra: 3}, 30, 10); n != 5 {
		t.Fatalf("MaxExtra 3: %d кадрів, want 2+3", n)
	}
	if n := run(Config{MaxExtra: 10, ByteBudget: 3500}, 30, 1000); n != 4 {
		t.Fatalf("бюджет 3500 Б по 1000: %d кадрів, want 4", n)
	}
	if n := run(Config{}, 0, 10); n != 2 {
		t.Fatalf("невідомий QP: %d кадрів, want 2", n)
	}

	s := New(Config{Converge: true})
	s.Motion(t0)
	now := t0.Add(DefaultIdle)
	for i := 0; i < 3; i++ {
		q, _ := s.Due(now)
		s.Sent(now, 10, 0)
		s.Coded(now, Frame{Refine: q, QP: 30})
	}
	s.Motion(now)
	if s.EpisodeBytes() != 0 || s.Refining() {
		t.Fatal("рух не скинув епізод")
	}
	if _, ok := s.Due(now); ok {
		t.Fatal("після руху refine раніше Idle")
	}
}
