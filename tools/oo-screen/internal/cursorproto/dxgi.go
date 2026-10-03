package cursorproto

import (
	"bytes"
	"image"
	"image/png"
	"time"
)

// DXGI_OUTDUPL_POINTER_SHAPE_TYPE values.
const (
	DXGIMonochrome  = 1
	DXGIColor       = 2
	DXGIMaskedColor = 4
)

// FromDXGI converts a raw DXGI pointer shape into straight-alpha RGBA.
// For monochrome shapes h is the DXGI height (AND mask + XOR mask, i.e. twice
// the visible height); the returned height is the visible one.
//
// Screen-inverting pixels (mono AND=1,XOR=1; masked-colour XOR pixels) cannot
// be expressed in RGBA; they are drawn opaque black (the usual approximation,
// e.g. for the I-beam). ok=false on malformed input.
func FromDXGI(kind, w, h, pitch int, data []byte) (rgba []byte, outH int, ok bool) {
	if w <= 0 || h <= 0 || pitch <= 0 {
		return nil, 0, false
	}
	switch kind {
	case DXGIMonochrome:
		vh := h / 2
		if vh <= 0 || pitch*h > len(data) || (w+7)/8 > pitch {
			return nil, 0, false
		}
		out := make([]byte, w*vh*4)
		for y := 0; y < vh; y++ {
			for x := 0; x < w; x++ {
				bit := byte(0x80 >> uint(x%8))
				and := data[y*pitch+x/8]&bit != 0
				xor := data[(y+vh)*pitch+x/8]&bit != 0
				o := (y*w + x) * 4
				switch {
				case !and && !xor: // black
					out[o+3] = 255
				case !and && xor: // white
					out[o], out[o+1], out[o+2], out[o+3] = 255, 255, 255, 255
				case and && !xor: // transparent
				default: // invert -> black
					out[o+3] = 255
				}
			}
		}
		return out, vh, true
	case DXGIColor, DXGIMaskedColor:
		if pitch < w*4 || pitch*(h-1)+w*4 > len(data) {
			return nil, 0, false
		}
		out := make([]byte, w*h*4)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				i := y*pitch + x*4 // BGRA
				o := (y*w + x) * 4
				b, g, r, a := data[i], data[i+1], data[i+2], data[i+3]
				if kind == DXGIMaskedColor {
					if a == 0 { // opaque colour
						out[o], out[o+1], out[o+2], out[o+3] = r, g, b, 255
					} else if r|g|b != 0 { // XOR with screen -> approximate black
						out[o+3] = 255
					}
					continue
				}
				out[o], out[o+1], out[o+2], out[o+3] = r, g, b, a
			}
		}
		return out, h, true
	}
	return nil, 0, false
}

// Crop trims a shape to at most MaxDim x MaxDim keeping the hotspot inside.
func Crop(rgba []byte, w, h, hx, hy int) ([]byte, int, int, int, int) {
	if w <= MaxDim && h <= MaxDim {
		return rgba, w, h, hx, hy
	}
	nw, nh := min(w, MaxDim), min(h, MaxDim)
	out := make([]byte, nw*nh*4)
	for y := 0; y < nh; y++ {
		copy(out[y*nw*4:(y+1)*nw*4], rgba[y*w*4:y*w*4+nw*4])
	}
	return out, nw, nh, min(hx, nw-1), min(hy, nh-1)
}

// BuildShape turns RGBA into a wire-ready Shape: PNG-encoded (cursors are
// mostly transparent and compress well) and guaranteed to fit MaxMessage.
// If the PNG would not fit, raw RGBA is used when that fits; otherwise the
// image is halved until it does (hotspot scaled with it). The ID always
// hashes the ORIGINAL (cropped) pixels, so it is stable per pointer.
func BuildShape(rgba []byte, w, h, hx, hy int) (Shape, error) {
	rgba, w, h, hx, hy = Crop(rgba, w, h, hx, hy)
	id := ShapeID(w, h, hx, hy, rgba)
	for {
		img := &image.NRGBA{Pix: rgba, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return Shape{}, err
		}
		s := Shape{ID: id, Format: FormatPNG, W: w, H: h, HotX: hx, HotY: hy, Data: buf.Bytes()}
		if ShapeHeader+buf.Len() <= MaxMessage {
			return s, nil
		}
		if ShapeHeader+len(rgba) <= MaxMessage {
			s.Format, s.Data = FormatRGBA, rgba
			return s, nil
		}
		nw, nh := max(1, w/2), max(1, h/2)
		out := make([]byte, nw*nh*4)
		for y := 0; y < nh; y++ {
			for x := 0; x < nw; x++ {
				src := (2*y*w + 2*x) * 4
				copy(out[(y*nw+x)*4:(y*nw+x)*4+4], rgba[src:src+4])
			}
		}
		rgba, w, h, hx, hy = out, nw, nh, min(hx/2, nw-1), min(hy/2, nh-1)
	}
}

// Coalescer decides which position samples go on the wire: only changes, and
// no more often than CoalesceInterval. A suppressed change is remembered and
// flushed by the next Due call once the interval has passed, so the final
// resting position is always delivered.
type Coalescer struct {
	last    Pos
	sent    bool
	pending bool
	cur     Pos
	lastAt  time.Time
}

// Offer records the latest sample.
func (c *Coalescer) Offer(p Pos) {
	c.cur = p
	c.pending = !c.sent || p != c.last
}

// Due returns the sample to send now, if any.
func (c *Coalescer) Due(now time.Time) (Pos, bool) {
	if !c.pending {
		return Pos{}, false
	}
	if c.sent && now.Sub(c.lastAt) < CoalesceInterval {
		return Pos{}, false
	}
	c.last, c.sent, c.pending, c.lastAt = c.cur, true, false, now
	return c.last, true
}

// Reset forgets what was sent (new channel / reconnect): the next Offer is
// always sent.
func (c *Coalescer) Reset() { *c = Coalescer{} }
