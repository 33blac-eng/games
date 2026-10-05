// tilesel — bench helper: run internal/tiles.Select on a PNG and print the
// selected tile rects as JSON (used by bench/quality/final.py).
package main

import (
	"encoding/json"
	"image"
	"image/draw"
	"image/png"
	"os"

	"github.com/organicoils/oo-screen/internal/tiles"
)

func main() {
	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	src, err := png.Decode(f)
	if err != nil {
		panic(err)
	}
	b := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	pix := rgba.Pix // RGBA -> BGRA, as DXGI delivers it
	for i := 0; i+3 < len(pix); i += 4 {
		pix[i], pix[i+2] = pix[i+2], pix[i]
	}
	rects := tiles.Select(tiles.Image{Pix: pix, Stride: rgba.Stride, W: b.Dx(), H: b.Dy()}, tiles.SelectConfig{})
	if rects == nil {
		rects = []tiles.Rect{}
	}
	if err := json.NewEncoder(os.Stdout).Encode(rects); err != nil {
		panic(err)
	}
}
