package h264

import (
	"encoding/hex"
	"math/rand"
	"testing"
)

// Мікробенчі гарячих шляхів агента/хаба: розбір SPS, перепис VUI-кольору і
// розрізання Annex-B на 1080p IDR-AU (~200 КБ). Корпусу .h264 у репо немає,
// тож AU синтетичний: справжній SPS libx264 (main1080) + PPS + IDR-слайс із
// псевдовипадкових ненульових байтів (стартових кодів усередині немає).

func benchSPS(b *testing.B) []byte {
	b.Helper()
	sps, err := hex.DecodeString(spsVectors[0].hex) // main 1920x1080
	if err != nil {
		b.Fatal(err)
	}
	return sps
}

func benchIDRAU(b *testing.B, size int) []byte {
	b.Helper()
	rng := rand.New(rand.NewSource(1))
	au := []byte{0, 0, 0, 1, 0x09, 0xF0} // AUD
	au = append(au, 0, 0, 0, 1)
	au = append(au, benchSPS(b)...)
	au = append(au, 0, 0, 0, 1, 0x68, 0xEE, 0x3C, 0x80) // PPS
	au = append(au, 0, 0, 0, 1, 0x65)                   // IDR
	for len(au) < size {
		au = append(au, byte(rng.Intn(255)+1))
	}
	return au
}

func BenchmarkParseSPS(b *testing.B) {
	sps := benchSPS(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseSPS(sps); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRewriteSPSColourBT709(b *testing.B) {
	sps := benchSPS(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := RewriteSPSColourBT709(sps); err != nil {
			b.Fatal(err)
		}
	}
}

// Перепис SPS усередині цілого IDR-AU (так його кличе агент на кожному IDR).
func BenchmarkRewriteAnnexBSPSColourBT709_IDR200K(b *testing.B) {
	au := benchIDRAU(b, 200<<10)
	b.SetBytes(int64(len(au)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := RewriteAnnexBSPSColourBT709(au); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSplitNALs_IDR200K(b *testing.B) {
	au := benchIDRAU(b, 200<<10)
	b.SetBytes(int64(len(au)))
	b.ReportAllocs()
	for b.Loop() {
		if n := len(SplitNALs(au)); n != 4 {
			b.Fatalf("NALs = %d, want 4", n)
		}
	}
}

func BenchmarkSplitAUs_IDR200K(b *testing.B) {
	au := benchIDRAU(b, 200<<10)
	b.SetBytes(int64(len(au)))
	b.ReportAllocs()
	for b.Loop() {
		if n := len(SplitAUs(au)); n != 1 {
			b.Fatalf("AUs = %d, want 1", n)
		}
	}
}
