package tiles

import (
	"bytes"
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

// EncodePNG encodes the w x h tile at (x,y) of a BGRA image as an opaque
// RGB PNG (Go's encoder writes colour type 2 for fully opaque NRGBA).
func EncodePNG(img Image, x, y, w, h int, level png.CompressionLevel) ([]byte, error) {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for r := 0; r < h; r++ {
		src := img.Pix[(y+r)*img.Stride+x*4:]
		d := dst.Pix[r*dst.Stride:]
		for c := 0; c < w; c++ {
			s := c * 4
			d[s], d[s+1], d[s+2], d[s+3] = src[s+2], src[s+1], src[s], 0xFF
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: level}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Stats of one built episode.
type Stats struct {
	Selected int // tiles the selector picked
	Sent     int // tiles emitted
	Bytes    int // total bytes emitted (headers included)
	Capped   bool
	Aborted  bool
}

// Build selects text tiles of img, encodes each as PNG and hands every wire
// message to emit, best tiles first, until maxBytes would be exceeded or emit
// returns false (epoch went stale / channel gone).
func Build(img Image, epoch, frame uint32, cfg SelectConfig, maxBytes int,
	emit func(msg []byte) bool) Stats {
	var st Stats
	if img.W > 0xFFFF || img.H > 0xFFFF {
		return st
	}
	rects := Select(img, cfg)
	st.Selected = len(rects)
	for _, r := range rects {
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
