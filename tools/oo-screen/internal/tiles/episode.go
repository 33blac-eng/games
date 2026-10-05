package tiles

import (
	"bytes"
	"hash/maphash"
	"image"
	"image/png"
	"sync"
	"time"
)

// Defaults for the per-episode budget and rate.
const (
	// DefaultEpisodeBytes — cap of tile payload per static episode (2 MB).
	DefaultEpisodeBytes = 2 << 20
	// DefaultMinInterval — episodes no more often than this: a screen that
	// changes every second must not turn into a 2 MB/s side stream.
	DefaultMinInterval = 2 * time.Second
)

// pngBufferPool reuses png.Encoder's internal state (zlib writer, row
// buffers, ~1.2 MB) across tiles: without it every tile paid for a fresh
// flate compressor, ~90 % of an episode's CPU. sync.Pool makes it safe for
// concurrent encoders.
type pngBufferPool struct{ p sync.Pool }

func (b *pngBufferPool) Get() *png.EncoderBuffer {
	eb, _ := b.p.Get().(*png.EncoderBuffer)
	return eb
}

func (b *pngBufferPool) Put(eb *png.EncoderBuffer) { b.p.Put(eb) }

var (
	pngPool = &pngBufferPool{}
	// tileScratch — the NRGBA conversion buffer and the output buffer, reused
	// per call; only the returned PNG bytes are freshly allocated.
	tileScratch = sync.Pool{New: func() any { return new(tileBufs) }}
)

type tileBufs struct {
	pix []byte
	out bytes.Buffer
}

// EncodePNG encodes the w x h tile at (x,y) of a BGRA image as an opaque
// RGB PNG (Go's encoder writes colour type 2 for fully opaque NRGBA).
// Safe for concurrent use.
func EncodePNG(img Image, x, y, w, h int, level png.CompressionLevel) ([]byte, error) {
	tb := tileScratch.Get().(*tileBufs)
	defer tileScratch.Put(tb)
	n := w * h * 4
	if cap(tb.pix) < n {
		tb.pix = make([]byte, n)
	}
	dst := &image.NRGBA{Pix: tb.pix[:n], Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	for r := 0; r < h; r++ {
		src := img.Pix[(y+r)*img.Stride+x*4:]
		d := dst.Pix[r*dst.Stride:]
		for c := 0; c < w; c++ {
			s := c * 4
			d[s], d[s+1], d[s+2], d[s+3] = src[s+2], src[s+1], src[s], 0xFF
		}
	}
	tb.out.Reset()
	enc := png.Encoder{CompressionLevel: level, BufferPool: pngPool}
	if err := enc.Encode(&tb.out, dst); err != nil {
		return nil, err
	}
	return bytes.Clone(tb.out.Bytes()), nil
}

// Stats of one built episode.
type Stats struct {
	Selected int // tiles the selector picked
	Sent     int // tiles emitted
	Kept     int // tiles re-validated by TypeKeep (not resent)
	Bytes    int // total bytes emitted (headers included)
	Capped   bool
	Aborted  bool
}

// Build selects text tiles of img, encodes each as PNG and hands every wire
// message to emit, best tiles first, until maxBytes would be exceeded or emit
// returns false (epoch went stale / channel gone).
func Build(img Image, epoch, frame uint32, cfg SelectConfig, maxBytes int,
	emit func(msg []byte) bool) Stats {
	return BuildDedup(img, epoch, frame, cfg, maxBytes, nil, emit)
}

// TileKey — tile rect as a map key.
type TileKey struct{ X, Y, W, H uint16 }

func keyOf(r Rect) TileKey { return TileKey{uint16(r.X), uint16(r.Y), uint16(r.W), uint16(r.H)} }

// Held mirrors what the viewer side retains: tile rect -> pixel hash, for one
// source geometry. The agent owns one per channel; Reset when the channel (and
// so every viewer's store) is new. Safe for concurrent use.
type Held struct {
	mu   sync.Mutex
	gen  uint64
	w, h int
	m    map[TileKey]uint64
}

// Reset forgets everything (new channel / viewer set).
func (h *Held) Reset() {
	h.mu.Lock()
	h.gen++
	h.m, h.w, h.h = nil, 0, 0
	h.mu.Unlock()
}

// Len — tiles currently believed held.
func (h *Held) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.m)
}

var hashSeed = maphash.MakeSeed()

// tileHash — hash of the tile's BGRA pixels (position is the map key).
func tileHash(img Image, r Rect) uint64 {
	var mh maphash.Hash
	mh.SetSeed(hashSeed)
	for y := r.Y; y < r.Y+r.H; y++ {
		o := y*img.Stride + r.X*4
		_, _ = mh.Write(img.Pix[o : o+r.W*4])
	}
	return mh.Sum64()
}

// BuildDedup is Build with cross-episode dedup. held == nil: plain Build (no
// TypeKeep). Otherwise the selected tiles whose pixels equal what the viewer
// already holds are announced in ONE TypeKeep message emitted first; only the
// rest is encoded and sent. held is updated to exactly what the viewer holds
// afterwards (kept + successfully emitted).
func BuildDedup(img Image, epoch, frame uint32, cfg SelectConfig, maxBytes int, held *Held,
	emit func(msg []byte) bool) Stats {
	var st Stats
	if img.W > 0xFFFF || img.H > 0xFFFF || img.W > MaxSrcSide || img.H > MaxSrcSide {
		return st
	}
	rects := Select(img, cfg)
	st.Selected = len(rects)
	var hashes []uint64
	var gen uint64
	if held != nil {
		hashes = make([]uint64, len(rects))
		for i, r := range rects {
			hashes[i] = tileHash(img, r)
		}
		held.mu.Lock()
		gen = held.gen
		prev := held.m
		if held.w != img.W || held.h != img.H {
			prev = nil
		}
		var keep []Rect
		next := make(map[TileKey]uint64, len(rects))
		rest := rects[:0:0]
		restH := hashes[:0:0]
		for i, r := range rects {
			k := keyOf(r)
			if hv, ok := prev[k]; ok && hv == hashes[i] && len(keep) < MaxPayload/KeepRectSize {
				keep = append(keep, r)
				next[k] = hv
				continue
			}
			rest = append(rest, r)
			restH = append(restH, hashes[i])
		}
		held.mu.Unlock()
		msg, err := Keep(epoch, frame, img.W, img.H, keep)
		if err != nil {
			return st
		}
		if !emit(msg) {
			st.Aborted = true
			return st
		}
		st.Bytes += len(msg)
		st.Kept = len(keep)
		held.mu.Lock()
		if held.gen == gen {
			held.m, held.w, held.h = next, img.W, img.H
		}
		held.mu.Unlock()
		rects, hashes = rest, restH
	}
	for i, r := range rects {
		payload, err := EncodePNG(img, r.X, r.Y, r.W, r.H, png.BestSpeed)
		if err != nil || len(payload) > MaxPayload {
			continue
		}
		msg, err := Encode(&Msg{
			Type: TypeTile, Epoch: epoch, Frame: frame,
			X: uint16(r.X), Y: uint16(r.Y), W: uint16(r.W), H: uint16(r.H),
			SrcW: uint16(img.W), SrcH: uint16(img.H), Format: FormatPNG, Payload: payload,
		})
		if err != nil {
			continue
		}
		if st.Bytes+len(msg) > maxBytes {
			st.Capped = true
			break
		}
		if !emit(msg) {
			st.Aborted = true
			break
		}
		st.Sent++
		st.Bytes += len(msg)
		if held != nil {
			held.mu.Lock()
			if held.gen == gen {
				held.m[keyOf(r)] = hashes[i]
			}
			held.mu.Unlock()
		}
	}
	return st
}

// Episodes is the agent-side state: the current epoch, whether tiles of this
// epoch are (being) sent, and the episode rate limit. Safe for concurrent
// use: the capture loop calls Motion/Start, the encoding goroutine Current.
type Episodes struct {
	MinInterval time.Duration

	mu        sync.Mutex
	epoch     uint32
	frame     uint32 // content generation: bumps with every changed frame
	started   bool   // an episode was started in this epoch
	lastStart time.Time
}

// Motion — a changed (non-noop) frame is about to be encoded. Returns the
// invalidate message to send BEFORE that frame, or nil if nothing of the
// current epoch has been started (nothing to invalidate).
func (e *Episodes) Motion() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.frame++
	if !e.started {
		return nil
	}
	e.epoch++
	e.started = false
	return Invalidate(e.epoch, e.frame)
}

// Start — the screen is static and refined. ok=false: already started in
// this epoch, or the previous episode is younger than MinInterval.
func (e *Episodes) Start(now time.Time) (epoch, frame uint32, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	gap := e.MinInterval
	if gap <= 0 {
		gap = DefaultMinInterval
	}
	if e.started || (!e.lastStart.IsZero() && now.Sub(e.lastStart) < gap) {
		return 0, 0, false
	}
	e.started, e.lastStart = true, now
	return e.epoch, e.frame, true
}

// Still — a still repeat (keepalive) of the current picture is about to be
// sent. Returns the TypeStill announcement when an episode of the current
// epoch was started (tiles may be on screen), nil otherwise.
func (e *Episodes) Still() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return nil
	}
	return Still(e.epoch, e.frame)
}

// Current reports whether epoch is still the live one.
func (e *Episodes) Current(epoch uint32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.epoch == epoch
}

// Reset — the channel/viewer set changed (new connection): forget the
// "already started" mark so a fresh viewer gets tiles of a still screen,
// and bump the epoch so nothing in flight survives.
func (e *Episodes) Reset() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.epoch++
	e.started = false
	e.lastStart = time.Time{}
	return Invalidate(e.epoch, e.frame)
}
