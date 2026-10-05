package cursorproto

import "testing"

// Мікробенчі протоколу курсора: позиція йде на кожен рух (до ~125/с), форма —
// рідко, але 32×32 RGBA або PNG до 256×256. Кешується BuildShape (PNG + ID).

var sinkPos Pos

func BenchmarkEncodePos(b *testing.B) {
	p := Pos{Visible: true, ShapeID: 0xdeadbeef, X: 1919, Y: 1079, FrameW: 1920, FrameH: 1080}
	b.ReportAllocs()
	for b.Loop() {
		_ = EncodePos(p)
	}
}

func BenchmarkDecodePos(b *testing.B) {
	m := EncodePos(Pos{Visible: true, ShapeID: 7, X: 100, Y: 200, FrameW: 1920, FrameH: 1080})
	b.ReportAllocs()
	for b.Loop() {
		p, err := DecodePos(m)
		if err != nil {
			b.Fatal(err)
		}
		sinkPos = p
	}
}

func benchShape(b *testing.B) Shape {
	b.Helper()
	px := rgba(32, 32, 0x80)
	return Shape{ID: ShapeID(32, 32, 3, 4, px), Format: FormatRGBA, W: 32, H: 32, HotX: 3, HotY: 4, Data: px}
}

func BenchmarkEncodeShapeRGBA32(b *testing.B) {
	s := benchShape(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := EncodeShape(s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeShapeRGBA32(b *testing.B) {
	m, err := EncodeShape(benchShape(b))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := DecodeShape(m); err != nil {
			b.Fatal(err)
		}
	}
}

// BuildShape — повний шлях агента на зміні форми (crop + ID + кодування).
func BenchmarkBuildShape32(b *testing.B) {
	px := rgba(32, 32, 0x80)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := BuildShape(px, 32, 32, 3, 4); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidatePos(b *testing.B) {
	m := EncodePos(Pos{Visible: true, X: 1, FrameW: 1920, FrameH: 1080})
	b.ReportAllocs()
	for b.Loop() {
		if err := Validate(m); err != nil {
			b.Fatal(err)
		}
	}
}
