package tiles

import (
	"image/png"
	"path/filepath"
	"strings"
	"testing"
)

// Мікробенчі текстових тайлів на bench/corpus (1080p і 1440p): вибір тайлів
// (Select), PNG одного тайла і цілий епізод (Build із бюджетом агента).
// Sub-бенч на кожен файл корпусу; без корпусу — Skip.

func benchCorpus(b *testing.B) []string {
	b.Helper()
	files, _ := filepath.Glob("../../bench/corpus/*.png")
	if len(files) == 0 {
		b.Skip("no corpus")
	}
	return files
}

func benchName(f string) string { return strings.TrimSuffix(filepath.Base(f), ".png") }

func BenchmarkSelect(b *testing.B) {
	for _, f := range benchCorpus(b) {
		img := loadBGRA(b, f)
		b.Run(benchName(f), func(b *testing.B) {
			b.SetBytes(int64(img.W * img.H * 4))
			b.ReportAllocs()
			n := 0
			for b.Loop() {
				n = len(Select(img, SelectConfig{}))
			}
			b.ReportMetric(float64(n), "tiles")
		})
	}
}

// PNG одного 64×64 тайла (BestSpeed, як у Build) — перший вибраний тайл кадру.
func BenchmarkEncodePNGTile(b *testing.B) {
	for _, f := range benchCorpus(b) {
		img := loadBGRA(b, f)
		rects := Select(img, SelectConfig{})
		if len(rects) == 0 {
			continue
		}
		r := rects[0]
		b.Run(benchName(f), func(b *testing.B) {
			b.ReportAllocs()
			var n int
			for b.Loop() {
				p, err := EncodePNG(img, r.X, r.Y, r.W, r.H, png.BestSpeed)
				if err != nil {
					b.Fatal(err)
				}
				n = len(p)
			}
			b.ReportMetric(float64(n), "B/tile")
		})
	}
}

// Повний епізод із бюджетом агента (DefaultEpisodeBytes) — так його будує
// агент на статичному екрані.
func BenchmarkBuildEpisode(b *testing.B) {
	for _, f := range benchCorpus(b) {
		img := loadBGRA(b, f)
		b.Run(benchName(f), func(b *testing.B) {
			b.ReportAllocs()
			var st Stats
			for b.Loop() {
				st = Build(img, 1, 1, SelectConfig{}, DefaultEpisodeBytes, func([]byte) bool { return true })
			}
			b.ReportMetric(float64(st.Sent), "sent")
			b.ReportMetric(float64(st.Bytes), "B/episode")
		})
	}
}
