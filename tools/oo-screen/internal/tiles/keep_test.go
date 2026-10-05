package tiles

import (
	"bytes"
	"errors"
	"testing"
)

// keepGolden — Keep(9, 4, 1920, 1080, [{64,128,64,32}]); the same bytes are
// asserted by the player test
// (total-erp-app/resources/js/remote/__tests__/text-tiles.test.mjs).
var keepGolden = []byte{0x4f, 0x54, 0x01, 0x04, 0x09, 0, 0, 0, 0x04, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0, 0x80, 0x07, 0x38, 0x04, 0, 0, 0, 0, 0x08, 0, 0, 0,
	0x40, 0, 0x80, 0, 0x40, 0, 0x20, 0}

func TestKeepMessage(t *testing.T) {
	b, err := Keep(9, 4, 1920, 1080, []Rect{{X: 64, Y: 128, W: 64, H: 32}})
	if err != nil || !bytes.Equal(b, keepGolden) {
		t.Fatalf("keep wire % x %v", b, err)
	}
	m, err := Decode(b)
	if err != nil || m.Type != TypeKeep || m.Epoch != 9 || m.Frame != 4 || m.SrcW != 1920 {
		t.Fatalf("decode %+v %v", m, err)
	}
	if r := KeepRects(m.Payload); len(r) != 1 || r[0] != (Rect{X: 64, Y: 128, W: 64, H: 32}) {
		t.Fatalf("rects %+v", r)
	}
	empty, err := Keep(1, 0, 8, 8, nil)
	if m, err2 := Decode(empty); err != nil || err2 != nil || len(m.Payload) != 0 {
		t.Fatalf("empty keep %v %v", err, err2)
	}
	mut := func(f func(b []byte)) []byte { c := append([]byte(nil), keepGolden...); f(c); return c }
	for name, b := range map[string][]byte{
		"outside": mut(func(b []byte) { b[32] = 0xF0; b[33] = 0x07 }),
		"zero-w":  mut(func(b []byte) { b[36] = 0 }),
		"format":  mut(func(b []byte) { b[24] = FormatPNG }),
		"hdr-x":   mut(func(b []byte) { b[12] = 1 }),
		"no-src":  mut(func(b []byte) { b[20], b[21] = 0, 0 }),
		"partial": append(mut(func(b []byte) { b[28] = 9 }), 0),
	} {
		if _, err := Decode(b); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if m, _ := Decode(Restamp(keepGolden, 77)); m.Epoch != 77 || keepGolden[4] != 9 {
		t.Fatal("restamp")
	}
}

func TestBuildDedup(t *testing.T) {
	img := canvas(512, 128, 255, 255, 255)
	for x := 0; x < 512; x += 64 {
		for y := 0; y < 128; y += 64 {
			glyphs(img, x, y, 200, 0, byte(x/4))
		}
	}
	var h Held
	collect := func(epoch uint32, ok func(int) bool) ([]uint8, Stats) {
		var types []uint8
		i := 0
		st := BuildDedup(img, epoch, 1, SelectConfig{}, 1<<30, &h, func(m []byte) bool {
			if !ok(i) {
				return false
			}
			i++
			d, err := Decode(m)
			if err != nil || d.Epoch != epoch {
				t.Fatalf("%v %+v", err, d)
			}
			types = append(types, d.Type)
			return true
		})
		return types, st
	}
	all := func(int) bool { return true }
	ty, st := collect(1, all)
	if len(ty) == 0 || ty[0] != TypeKeep || st.Sent != 16 || st.Kept != 0 || h.Len() != 16 {
		t.Fatalf("first %+v", st)
	}
	if _, st = collect(2, all); st.Sent != 0 || st.Kept != 16 {
		t.Fatalf("second %+v", st)
	}
	// one tile changes; abort right after the keep: held = the 15 kept
	img.Pix[70*img.Stride+70*4] ^= 0xFF
	if _, st = collect(3, func(i int) bool { return i < 1 }); !st.Aborted || st.Kept != 15 || h.Len() != 15 {
		t.Fatalf("aborted %+v held %d", st, h.Len())
	}
	if _, st = collect(4, all); st.Sent != 1 || st.Kept != 15 || h.Len() != 16 {
		t.Fatalf("resend changed %+v", st)
	}
	// keep not delivered: held unchanged
	if _, st = collect(5, func(int) bool { return false }); h.Len() != 16 || st.Kept != 0 {
		t.Fatal("held changed by an undelivered keep")
	}
	// geometry change: nothing kept
	img = canvas(256, 128, 255, 255, 255)
	glyphs(img, 0, 0, 200, 0, 0)
	if _, st = collect(6, all); st.Kept != 0 || st.Sent == 0 {
		t.Fatalf("geometry %+v", st)
	}
}
