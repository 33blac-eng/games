package tiles

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/png"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakePNG — PNG signature + IHDR(w,h), padded with zeros to n bytes (n ≥ 33).
func fakePNG(w, h uint16, n int) []byte {
	b := make([]byte, n)
	copy(b, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13, 'I', 'H', 'D', 'R',
		0, 0, byte(w >> 8), byte(w), 0, 0, byte(h >> 8), byte(h), 8, 6, 0, 0, 0})
	return b
}

func TestRoundTrip(t *testing.T) {
	in := &Msg{Type: TypeTile, Epoch: 7, Frame: 99, X: 64, Y: 128, W: 64, H: 32,
		SrcW: 1920, SrcH: 1080, Format: FormatPNG, Payload: fakePNG(64, 32, 33)}
	b, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	// golden — the same bytes are asserted by the player test
	// (total-erp-app/resources/js/remote/__tests__/text-tiles.test.mjs).
	golden := []byte{0x4f, 0x54, 0x01, 0x01, 0x07, 0, 0, 0, 0x63, 0, 0, 0, 0x40, 0, 0x80, 0,
		0x40, 0, 0x20, 0, 0x80, 0x07, 0x38, 0x04, 0x01, 0, 0, 0, 0x21, 0, 0, 0}
	golden = append(golden, fakePNG(64, 32, 33)...)
	if !bytes.Equal(b, golden) {
		t.Fatalf("wire changed: % x", b)
	}
	if len(b) != HeaderSize+33 || string(b[:2]) != "OT" {
		t.Fatalf("bad encoding % x", b)
	}
	out, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if out.Epoch != 7 || out.Frame != 99 || out.X != 64 || out.Y != 128 || out.W != 64 || out.H != 32 ||
		out.SrcW != 1920 || out.SrcH != 1080 || !bytes.Equal(out.Payload, in.Payload) {
		t.Fatalf("mismatch %+v", out)
	}
	inv, err := Decode(Invalidate(8, 100))
	if err != nil || inv.Type != TypeInvalidate || inv.Epoch != 8 || inv.Frame != 100 || inv.Payload != nil {
		t.Fatalf("invalidate %+v %v", inv, err)
	}
}

func TestDecodeRejects(t *testing.T) {
	good, _ := Encode(&Msg{Type: TypeTile, Epoch: 1, W: 4, H: 4, SrcW: 8, SrcH: 8, Format: FormatPNG, Payload: fakePNG(4, 4, 33)})
	mut := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	cases := map[string]struct {
		b   []byte
		err error
	}{
		"short":     {good[:10], ErrShort},
		"magic":     {mut(func(b []byte) []byte { b[0] = 'X'; return b }), ErrMagic},
		"version":   {mut(func(b []byte) []byte { b[2] = 9; return b }), ErrMagic},
		"len":       {mut(func(b []byte) []byte { b[28] = 5; return b }), ErrLength},
		"truncated": {good[:len(good)-1], ErrLength},
		"type":      {mut(func(b []byte) []byte { b[3] = 3; return b }), ErrInvalid},
		"reserved":  {mut(func(b []byte) []byte { b[26] = 1; return b }), ErrInvalid},
		"outside":   {mut(func(b []byte) []byte { b[12] = 6; return b }), ErrInvalid},
		"zero-w":    {mut(func(b []byte) []byte { b[16] = 0; return b }), ErrInvalid},
		"format":    {mut(func(b []byte) []byte { b[24] = 7; return b }), ErrInvalid},
		"huge":      {make([]byte, MaxMessage+1), ErrTooLarge},
	}
	for name, c := range cases {
		if _, err := Decode(c.b); !errors.Is(err, c.err) {
			t.Errorf("%s: got %v want %v", name, err, c.err)
		}
	}
	if _, err := Encode(&Msg{Type: TypeTile, W: 4, H: 4, SrcW: 8, SrcH: 8, Format: FormatPNG,
		Payload: make([]byte, MaxPayload+1)}); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize encode: %v", err)
	}
	if _, err := Encode(&Msg{Type: TypeTile, W: 300, H: 4, SrcW: 1000, SrcH: 8, Format: FormatPNG,
		Payload: fakePNG(300, 4, 33)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("oversize tile: %v", err)
	}
	// max-size message still decodes
	big, err := Encode(&Msg{Type: TypeTile, W: 4, H: 4, SrcW: 8, SrcH: 8, Format: FormatPNG, Payload: fakePNG(4, 4, MaxPayload)})
	if err != nil || len(big) != MaxMessage {
		t.Fatalf("max encode: %v %d", err, len(big))
	}
	if _, err := Decode(big); err != nil {
		t.Fatalf("max decode: %v", err)
	}
}

// canvas returns a w x h BGRA image filled with (r,g,b).
func canvas(w, h int, r, g, b byte) Image {
	img := Image{Pix: make([]byte, w*h*4), Stride: w * 4, W: w, H: h}
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = b, g, r, 255
	}
	return img
}

func set(img Image, x, y int, r, g, b byte) {
	i := y*img.Stride + x*4
	img.Pix[i], img.Pix[i+1], img.Pix[i+2] = b, g, r
}

// glyphs draws 1-px "strokes" (fake text) into the tile at (x0,y0).
func glyphs(img Image, x0, y0 int, r, g, b byte) {
	for row := 4; row < 60; row += 12 {
		for x := x0 + 2; x < x0+60; x++ {
			if (x/3)%2 == 0 {
				for dy := 0; dy < 7; dy++ {
					if dy == 0 || dy == 6 || x%5 == 0 {
						set(img, x, y0+row+dy, r, g, b)
					}
				}
			}
		}
	}
}

func TestSelect(t *testing.T) {
	img := canvas(256, 64, 255, 255, 255)
	glyphs(img, 0, 0, 0, 0, 0)     // tile 0: black text — luma only, skip
	glyphs(img, 64, 0, 220, 0, 0)  // tile 1: red text — select
	glyphs(img, 128, 0, 0, 0, 230) // tile 2: blue text — select
	rnd := rand.New(rand.NewSource(1))
	for y := 0; y < 64; y++ { // tile 3: noisy gradient photo — skip
		for x := 192; x < 256; x++ {
			v := byte(x*2 + y + rnd.Intn(12))
			set(img, x, y, v, byte(int(v)/2+y), 255-v)
		}
	}
	got := Select(img, SelectConfig{})
	if len(got) != 2 {
		t.Fatalf("selected %+v", got)
	}
	xs := map[int]bool{got[0].X: true, got[1].X: true}
	if !xs[64] || !xs[128] {
		t.Fatalf("wrong tiles %+v", got)
	}
	if got[0].Score < got[1].Score {
		t.Fatal("not sorted by score")
	}
	// flat screen: nothing
	if s := Select(canvas(200, 100, 30, 90, 200), SelectConfig{}); len(s) != 0 {
		t.Fatalf("flat: %+v", s)
	}
	// edge tiles clipped to the image
	odd := canvas(100, 70, 255, 255, 255)
	glyphs(odd, 36, 6, 0, 160, 0)
	for _, r := range Select(odd, SelectConfig{}) {
		if r.X+r.W > 100 || r.Y+r.H > 70 {
			t.Fatalf("rect outside: %+v", r)
		}
	}
	// garbage geometry does not panic
	if Select(Image{Pix: make([]byte, 10), Stride: 4, W: 4, H: 4}, SelectConfig{}) != nil {
		t.Fatal("short buffer accepted")
	}
}

func TestBuildBudgetAndAbort(t *testing.T) {
	img := canvas(512, 128, 255, 255, 255)
	for x := 0; x < 512; x += 64 {
		for y := 0; y < 128; y += 64 {
			glyphs(img, x, y, 200, 0, byte(x/4))
		}
	}
	var msgs [][]byte
	st := Build(img, 3, 5, SelectConfig{}, 1<<30, func(m []byte) bool { msgs = append(msgs, m); return true })
	if st.Selected != 16 || st.Sent != 16 || st.Capped {
		t.Fatalf("stats %+v", st)
	}
	for _, m := range msgs {
		d, err := Decode(m)
		if err != nil || d.Epoch != 3 || d.Frame != 5 || d.SrcW != 512 {
			t.Fatalf("decode %v %+v", err, d)
		}
		pi, err := png.Decode(bytes.NewReader(d.Payload))
		if err != nil {
			t.Fatal(err)
		}
		// lossless: compare a pixel
		r, g, b, _ := pi.At(10, 4).RGBA()
		i := (int(d.Y)+4)*img.Stride + (int(d.X)+10)*4
		if byte(r>>8) != img.Pix[i+2] || byte(g>>8) != img.Pix[i+1] || byte(b>>8) != img.Pix[i] {
			t.Fatal("PNG not lossless")
		}
	}
	capB := st.Bytes / 2
	st2 := Build(img, 3, 5, SelectConfig{}, capB, func([]byte) bool { return true })
	if !st2.Capped || st2.Bytes > capB || st2.Sent == 0 {
		t.Fatalf("cap %+v", st2)
	}
	n := 0
	st3 := Build(img, 3, 5, SelectConfig{}, 1<<30, func([]byte) bool { n++; return n < 3 })
	if !st3.Aborted || st3.Sent != 2 {
		t.Fatalf("abort %+v", st3)
	}
}

func TestEpisodes(t *testing.T) {
	var e Episodes
	e.MinInterval = time.Second
	t0 := time.Unix(1000, 0)
	if inv := e.Motion(); inv != nil {
		t.Fatal("invalidate before anything sent")
	}
	ep, fr, ok := e.Start(t0)
	if !ok || fr != 1 {
		t.Fatalf("start %v %v", ok, fr)
	}
	if _, _, ok := e.Start(t0.Add(5 * time.Second)); ok {
		t.Fatal("second start in same epoch")
	}
	inv := e.Motion()
	m, err := Decode(inv)
	if err != nil || m.Epoch != ep+1 || e.Current(ep) {
		t.Fatalf("invalidate %+v %v", m, err)
	}
	if _, _, ok := e.Start(t0.Add(500 * time.Millisecond)); ok {
		t.Fatal("rate limit ignored")
	}
	if _, _, ok := e.Start(t0.Add(1500 * time.Millisecond)); !ok {
		t.Fatal("start after interval")
	}
	r, _ := Decode(e.Reset())
	if _, _, ok := e.Start(t0.Add(1600 * time.Millisecond)); !ok || r.Epoch != ep+2 {
		t.Fatal("reset should allow immediate start")
	}
}

// TestCorpusBytes measures the tile payload on bench/corpus (run with -v).
func TestCorpusBytes(t *testing.T) {
	files, _ := filepath.Glob("../../bench/corpus/*.png")
	if len(files) == 0 {
		t.Skip("no corpus")
	}
	total := 0
	for _, f := range files {
		img := loadBGRA(t, f)
		t0 := time.Now()
		st := Build(img, 1, 1, SelectConfig{}, 1<<30, func([]byte) bool { return true })
		dt := time.Since(t0)
		capped := Build(img, 1, 1, SelectConfig{}, DefaultEpisodeBytes, func([]byte) bool { return true })
		total += st.Bytes
		t.Logf("%-24s tiles %4d/%4d  bytes %8d (%.0f KiB)  2MB-capped sent %d  build %v",
			filepath.Base(f), st.Selected, ((img.W+63)/64)*((img.H+63)/64), st.Bytes, float64(st.Bytes)/1024, capped.Sent, dt.Round(time.Millisecond))
	}
	t.Logf("corpus mean %.0f KiB per static screen", float64(total)/float64(len(files))/1024)
}

func loadBGRA(t testing.TB, f string) Image {
	fh, err := os.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	src, err := png.Decode(fh)
	if err != nil {
		t.Fatal(err)
	}
	b := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	for i := 0; i < len(rgba.Pix); i += 4 {
		rgba.Pix[i], rgba.Pix[i+2] = rgba.Pix[i+2], rgba.Pix[i]
	}
	return Image{Pix: rgba.Pix, Stride: rgba.Stride, W: b.Dx(), H: b.Dy()}
}

func TestStillMessage(t *testing.T) {
	m, err := Decode(Still(9, 4))
	if err != nil || m.Type != TypeStill || m.Epoch != 9 || m.Frame != 4 || m.Payload != nil {
		t.Fatalf("still %+v %v", m, err)
	}
	if _, err := Encode(&Msg{Type: TypeStill, Format: FormatPNG}); err == nil {
		t.Fatal("still with format accepted")
	}
	if _, err := Encode(&Msg{Type: TypeStill, Payload: []byte{1}}); err == nil {
		t.Fatal("still with payload accepted")
	}

	var e Episodes
	if e.Still() != nil {
		t.Fatal("still announced without an episode")
	}
	ep, fr, _ := e.Start(time.Unix(1, 0))
	s, err := Decode(e.Still())
	if err != nil || s.Epoch != ep || s.Frame != fr {
		t.Fatalf("episode still %+v %v", s, err)
	}
	e.Motion()
	if e.Still() != nil {
		t.Fatal("still announced after motion ended the episode")
	}
}

func TestSelectExcludesCursor(t *testing.T) {
	img := canvas(256, 64, 255, 255, 255)
	glyphs(img, 64, 0, 220, 0, 0)
	glyphs(img, 128, 0, 0, 0, 230)
	// pointer at (150,10): its rect overlaps tile x=128 only
	ex := CursorExclude(150, 10)
	got := Select(img, SelectConfig{Exclude: []Rect{ex}})
	if len(got) != 1 || got[0].X != 64 {
		t.Fatalf("got %+v", got)
	}
	// degenerate exclude rect is ignored
	if n := len(Select(img, SelectConfig{Exclude: []Rect{{X: 0, Y: 0}}})); n != 2 {
		t.Fatalf("empty exclude dropped tiles: %d", n)
	}
	// Build honours it as well
	var xs []int
	Build(img, 1, 1, SelectConfig{Exclude: []Rect{ex}}, 1<<30, func(b []byte) bool {
		m, _ := Decode(b)
		xs = append(xs, int(m.X))
		return true
	})
	if len(xs) != 1 || xs[0] != 64 {
		t.Fatalf("build sent %v", xs)
	}
}

// TestDecodeRejectsPNGBomb — a 45-byte message (32-byte header + 13 bytes) or
// a 61-byte one with a full IHDR claiming 65535x65535 must not pass; nor a
// tile whose source canvas would be gigantic.
func TestDecodeRejectsPNGBomb(t *testing.T) {
	hdr := func(w, h, srcW, srcH uint16, payload []byte) []byte {
		b := make([]byte, HeaderSize+len(payload))
		b[0], b[1], b[2], b[3] = 'O', 'T', Version, TypeTile
		b[16], b[17], b[18], b[19] = byte(w), byte(w>>8), byte(h), byte(h>>8)
		b[20], b[21], b[22], b[23] = byte(srcW), byte(srcW>>8), byte(srcH), byte(srcH>>8)
		b[24] = FormatPNG
		b[28] = byte(len(payload))
		copy(b[HeaderSize:], payload)
		return b
	}
	bomb45 := hdr(64, 64, 1920, 1080, fakePNG(0xFFFF, 0xFFFF, 29)[:13])
	if len(bomb45) != 45 {
		t.Fatalf("len %d", len(bomb45))
	}
	cases := map[string][]byte{
		"bomb45":        bomb45,
		"ihdr-mismatch": hdr(64, 64, 1920, 1080, fakePNG(0xFFFF, 0xFFFF, 33)),
		"not-png":       hdr(64, 64, 1920, 1080, make([]byte, 33)),
		"huge-src":      hdr(64, 64, 0xFFFF, 0xFFFF, fakePNG(64, 64, 33)),
		"src-over":      hdr(64, 64, MaxSrcSide+1, 1080, fakePNG(64, 64, 33)),
	}
	for name, b := range cases {
		if _, err := Decode(b); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := Decode(hdr(64, 64, MaxSrcSide, MaxSrcSide, fakePNG(64, 64, 33))); err != nil {
		t.Errorf("max src rejected: %v", err)
	}
}
