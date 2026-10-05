package textmode

import "testing"

// Пер-кадрова ціна детектора текстового режиму: Fraction двох площ і Update.
func BenchmarkPerFrame(b *testing.B) {
	d := New(Config{})
	areas := []int64{0, 4096, 1920 * 40, 0, 1920 * 1080}
	i := 0
	b.ReportAllocs()
	for b.Loop() {
		a := areas[i%len(areas)]
		i++
		_, _ = d.Update(Fraction(a, 1920, 1080), Fraction(a/4, 1920, 1080))
	}
}
