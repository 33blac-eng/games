// tiledup — bench helper: replay a sequence of tile episodes (one PNG per
// episode, in order, same screen geometry) through internal/tiles twice —
// plain Build (every episode resends every selected tile) and BuildDedup
// (one shared Held: unchanged tiles go as a TypeKeep entry) — and print the
// wire bytes of both as JSON (used by bench/quality/tiles_dedup.py).
package main

import (
	"encoding/json"
	"image"
	"image/draw"
	"image/png"
	"os"

	"github.com/organicoils/oo-screen/internal/tiles"
)

type episode struct {
	Selected  int `json:"selected"`
	FullSent  int `json:"full_sent"`
	FullBytes int `json:"full_bytes"`
	DedupSent int `json:"dedup_sent"`
	DedupKept int `json:"dedup_kept"`
	DedupB    int `json:"dedup_bytes"`
}

func load(path string) tiles.Image {
	f, err := os.Open(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
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
	return tiles.Image{Pix: pix, Stride: rgba.Stride, W: b.Dx(), H: b.Dy()}
}

func main() {
	held := &tiles.Held{}
	out := []episode{}
	ok := func([]byte) bool { return true }
	for i, p := range os.Args[1:] {
		img := load(p)
		ep := uint32(i + 1)
		full := tiles.Build(img, ep, ep, tiles.SelectConfig{}, tiles.DefaultEpisodeBytes, ok)
		dd := tiles.BuildDedup(img, ep, ep, tiles.SelectConfig{}, tiles.DefaultEpisodeBytes, held, ok)
		out = append(out, episode{Selected: full.Selected, FullSent: full.Sent, FullBytes: full.Bytes,
			DedupSent: dd.Sent, DedupKept: dd.Kept, DedupB: dd.Bytes})
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		panic(err)
	}
}
