package refine

import (
	"testing"
	"time"
)

// Пер-кадрова ціна refine у кадровому циклі агента: Motion на зміненому кадрі
// плюс Wait/Due на кожному тіку (так їх кличе main.go).
func BenchmarkPerFrame(b *testing.B) {
	s := New(Config{MinGap: 33 * time.Millisecond})
	now := time.Unix(0, 0)
	b.ReportAllocs()
	for b.Loop() {
		now = now.Add(33 * time.Millisecond)
		s.Motion(now)
		_ = s.Wait(now, time.Second)
		_, _ = s.Due(now)
	}
}

// Простій: кадрів немає, цикл крутить лише Wait/Due/Sent (кроки уточнення).
func BenchmarkIdleRefineCycle(b *testing.B) {
	now := time.Unix(0, 0)
	b.ReportAllocs()
	for b.Loop() {
		s := New(Config{MinGap: 33 * time.Millisecond})
		s.Motion(now)
		for !s.Complete() {
			now = now.Add(s.Wait(now, time.Second))
			if qp, ok := s.Due(now); ok {
				_ = qp
				s.Sent(now, 100_000, 12_000_000)
			}
		}
	}
}
