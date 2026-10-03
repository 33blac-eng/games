package tiles

import "sort"

// TileSize — default tile edge, source pixels.
const TileSize = 64

// SelectConfig tunes the text-tile selector. Zero fields take defaults.
type SelectConfig struct {
	Tile int // tile edge (default TileSize)
	// EdgeDelta — luma step between neighbours that counts as a glyph edge.
	EdgeDelta int
	// ChromaMin — max(R,G,B)-min(R,G,B) at which a pixel counts as coloured.
	ChromaMin int
	// MinEdgeFrac — minimum share of edge pixels (flat areas are skipped).
	MinEdgeFrac float64
	// MinChromaEdges — minimum number of edge pixels that touch a coloured
	// pixel: grey text has none, and 4:2:0 already carries it losslessly
	// enough in luma — those tiles are not worth bytes.
	MinChromaEdges int
	// MaxGradFrac — maximum share of "soft" steps (photo/gradient content):
	// text sits on flat backgrounds, photos do not.
	MaxGradFrac float64
}

func (c *SelectConfig) defaults() {
	if c.Tile <= 0 {
		c.Tile = TileSize
	}
	if c.EdgeDelta <= 0 {
		c.EdgeDelta = 48
	}
	if c.ChromaMin <= 0 {
		c.ChromaMin = 40
	}
	if c.MinEdgeFrac <= 0 {
		c.MinEdgeFrac = 0.01
	}
	if c.MinChromaEdges <= 0 {
		c.MinChromaEdges = 12
	}
	if c.MaxGradFrac <= 0 {
		c.MaxGradFrac = 0.20
	}
}

// Rect is a selected tile with its score (higher = more chroma text).
type Rect struct {
	X, Y, W, H int
	Score      int
}

// Image is a BGRA (DXGI_FORMAT_B8G8R8A8) buffer.
type Image struct {
	Pix    []byte
	Stride int
	W, H   int
}

func luma(p []byte, i int) int { // BT.601-ish, integer
	return (29*int(p[i]) + 150*int(p[i+1]) + 77*int(p[i+2])) >> 8
}

func chroma(p []byte, i int) int {
	b, g, r := int(p[i]), int(p[i+1]), int(p[i+2])
	mx, mn := r, r
	if g > mx {
		mx = g
	}
	if b > mx {
		mx = b
	}
	if g < mn {
		mn = g
	}
	if b < mn {
		mn = b
	}
	return mx - mn
}

func absi(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Classify scores one tile; ok reports whether it looks like coloured text.
func Classify(img Image, x0, y0, w, h int, cfg SelectConfig) (score int, ok bool) {
	cfg.defaults()
	edges, grads, chromaEdges, n := 0, 0, 0, 0
	p := img.Pix
	for y := y0; y < y0+h; y++ {
		row := y * img.Stride
		for x := x0; x < x0+w; x++ {
			i := row + x*4
			l := luma(p, i)
			n++
			// right and down neighbours inside the tile
			for _, j := range [2]int{i + 4, i + img.Stride} {
				if (j == i+4 && x+1 >= x0+w) || (j == i+img.Stride && y+1 >= y0+h) {
					continue
				}
				d := absi(l - luma(p, j))
				switch {
				case d >= cfg.EdgeDelta:
					edges++
					if chroma(p, i) >= cfg.ChromaMin || chroma(p, j) >= cfg.ChromaMin {
						chromaEdges++
					}
				case d > 2:
					grads++
				}
			}
		}
	}
	if n == 0 {
		return 0, false
	}
	steps := float64(2 * n)
	if float64(edges)/steps < cfg.MinEdgeFrac || float64(grads)/steps > cfg.MaxGradFrac ||
		chromaEdges < cfg.MinChromaEdges {
		return chromaEdges, false
	}
	return chromaEdges, true
}

// Select splits img into tiles and returns those that look like coloured
// text, best first (descending score, then raster order).
func Select(img Image, cfg SelectConfig) []Rect {
	cfg.defaults()
	if img.W <= 0 || img.H <= 0 || img.Stride < img.W*4 || len(img.Pix) < img.Stride*(img.H-1)+img.W*4 {
		return nil
	}
	var out []Rect
	for y := 0; y < img.H; y += cfg.Tile {
		h := min(cfg.Tile, img.H-y)
		for x := 0; x < img.W; x += cfg.Tile {
			w := min(cfg.Tile, img.W-x)
			if s, ok := Classify(img, x, y, w, h, cfg); ok {
				out = append(out, Rect{X: x, Y: y, W: w, H: h, Score: s})
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	return out
}
