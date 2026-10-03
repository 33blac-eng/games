package tiles

import (
	"bytes"
	"errors"
	"image/png"
	"testing"
)

// TestEncodePNGPooledConcurrent — pooled encoder state must not leak between
// concurrent tiles: parallel encodes are byte-identical to serial ones and
// decode to the source pixels.
func TestEncodePNGPooledConcurrent(t *testing.T) {
	img := canvas(256, 256, 250, 250, 250)
	for i := 0; i < 4; i++ {
		glyphs(img, i*64+4, i*64+4, byte(40*i), 0, 200)
	}
	want := make([][]byte, 4)
	for i := range want {
		b, err := EncodePNG(img, i*64, i*64, 64, 64, png.BestSpeed)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = b
	}
	errs := make(chan error, 64)
	for g := 0; g < 64; g++ {
		go func(i int) {
			b, err := EncodePNG(img, i*64, i*64, 64, 64, png.BestSpeed)
			if err == nil && !bytes.Equal(b, want[i]) {
				err = errors.New("pooled encode differs")
			}
			errs <- err
		}(g % 4)
	}
	for g := 0; g < 64; g++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	dec, err := png.Decode(bytes.NewReader(want[1]))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := dec.At(0, 0).RGBA()
	o := 64*img.Stride + 64*4
	if byte(r>>8) != img.Pix[o+2] || byte(g>>8) != img.Pix[o+1] || byte(b>>8) != img.Pix[o] {
		t.Fatal("decoded pixel mismatch")
	}
}
