package swlimit

import (
	"testing"
	"time"
)

// Пер-кадрова ціна політики автолімітів: один Observe на закодований кадр.
// Steady — швидкий енкодер, рішення не змінюються (типовий випадок).
func BenchmarkObserveSteady(b *testing.B) {
	p := New(Config{MaxFPS: 30, Cores: 4})
	now := t0
	b.ReportAllocs()
	for b.Loop() {
		now = now.Add(time.Second / 30)
		_ = p.Observe(5*time.Millisecond, now)
	}
}

// Flapping — енкодер то повільний, то швидкий: політика ходить драбиною вниз
// і вгору (рішення з Reason, тобто fmt.Sprintf на зміні).
func BenchmarkObserveLadder(b *testing.B) {
	p := New(Config{MaxFPS: 30, Cores: 2, DownHold: time.Millisecond, UpHold: time.Millisecond})
	now := t0
	i := 0
	b.ReportAllocs()
	for b.Loop() {
		now = now.Add(time.Second / 30)
		enc := 5 * time.Millisecond
		if (i/200)%2 == 0 {
			enc = 80 * time.Millisecond
		}
		i++
		_ = p.Observe(enc, now)
	}
}
