package cursorproto

import (
	"log"
	"sync"
	"time"
)

// PosBufferLimit — above this many bytes queued in the channel a position is
// held back (the next one is absolute anyway). Shapes always go.
const PosBufferLimit = 8 << 10

// shapeCacheMax — how many encoded shapes the publisher keeps.
const shapeCacheMax = 32

// Sink is the part of a DataChannel the publisher needs.
type Sink interface {
	Send([]byte) error
	BufferedAmount() uint64
}

// RawShape is a DXGI pointer shape as delivered (see FromDXGI).
type RawShape struct {
	Type       int // DXGIMonochrome | DXGIColor | DXGIMaskedColor
	W, H       int // H counts both masks for monochrome
	Pitch      int
	HotX, HotY int
	Data       []byte
}

// Sample is the pointer state of one captured frame.
type Sample struct {
	Visible        bool
	X, Y           int // top-left of the pointer image (DXGI PointerPosition)
	FrameW, FrameH int
	ShapeSeq       uint32 // bumps whenever DXGI has a new shape
}

// Publisher turns per-frame pointer samples into wire messages: the shape
// when it changes (PNG, cached by content hash so arrow <-> I-beam flips do
// not re-encode), the hotspot position only on change and at most every
// CoalesceInterval. Call Run (or Flush on a ticker) so a position suppressed
// by coalescing is still delivered.
type Publisher struct {
	mu   sync.Mutex
	sink Sink
	co   Coalescer

	haveSeq bool
	lastSeq uint32

	shapeID   uint32
	shapeMsg  []byte
	shapeSent bool
	hotX      int
	hotY      int

	cache map[uint32][]byte // raw-shape hash -> encoded message
	ids   map[uint32]uint32 // raw-shape hash -> wire id
	order []uint32

	now func() time.Time
}

// NewPublisher returns an idle publisher (no sink yet).
func NewPublisher() *Publisher {
	return &Publisher{cache: map[uint32][]byte{}, ids: map[uint32]uint32{}, now: time.Now}
}

// SetSink attaches a freshly opened channel: what was "already sent" is
// forgotten, the current shape and position go again.
func (p *Publisher) SetSink(s Sink) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sink = s
	p.shapeSent = false
	p.co.Reset()
	p.flushLocked()
}

// ClearSink detaches s (if it is still the current sink).
func (p *Publisher) ClearSink(s Sink) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sink == s {
		p.sink = nil
	}
}

// Observe is called for EVERY captured frame, NoChange ones included: that is
// where DXGI reports pointer moves. shape is pulled only when ShapeSeq changed.
func (p *Publisher) Observe(s Sample, shape func() (RawShape, bool)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.haveSeq || s.ShapeSeq != p.lastSeq {
		if raw, ok := shape(); ok {
			p.setShapeLocked(raw)
			p.lastSeq, p.haveSeq = s.ShapeSeq, true
		}
	}
	p.co.Offer(Pos{
		Visible: s.Visible && p.shapeID != 0,
		ShapeID: p.shapeID,
		X:       int32(s.X + p.hotX),
		Y:       int32(s.Y + p.hotY),
		FrameW:  clampU16(s.FrameW),
		FrameH:  clampU16(s.FrameH),
	})
	p.flushLocked()
}

// ShapeID is the id of the current shape (0 = none).
func (p *Publisher) ShapeID() uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shapeID
}

func clampU16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > 0xffff {
		return 0xffff
	}
	return uint16(v)
}

func (p *Publisher) setShapeLocked(raw RawShape) {
	key := ShapeID(raw.W, raw.H, raw.HotX, raw.HotY, raw.Data) ^ uint32(raw.Type)<<24
	if msg, ok := p.cache[key]; ok {
		p.useShapeLocked(p.ids[key], msg, raw)
		return
	}
	rgba, h, ok := FromDXGI(raw.Type, raw.W, raw.H, raw.Pitch, raw.Data)
	if !ok {
		log.Printf("cursor: unusable DXGI shape (type=%d %dx%d)", raw.Type, raw.W, raw.H)
		return
	}
	hx, hy := min(max(raw.HotX, 0), raw.W-1), min(max(raw.HotY, 0), h-1)
	s, err := BuildShape(rgba, raw.W, h, hx, hy)
	if err != nil {
		log.Printf("cursor: shape: %v", err)
		return
	}
	msg, err := EncodeShape(s)
	if err != nil {
		log.Printf("cursor: shape: %v", err)
		return
	}
	if len(p.order) >= shapeCacheMax {
		old := p.order[0]
		p.order = p.order[1:]
		delete(p.cache, old)
		delete(p.ids, old)
	}
	p.cache[key], p.ids[key] = msg, s.ID
	p.order = append(p.order, key)
	p.useShapeLocked(s.ID, msg, raw)
}

func (p *Publisher) useShapeLocked(id uint32, msg []byte, raw RawShape) {
	if id != p.shapeID {
		p.shapeSent = false
	}
	p.shapeID, p.shapeMsg = id, msg
	// The wire carries the hotspot; DXGI gives the top-left corner.
	p.hotX, p.hotY = raw.HotX, raw.HotY
}

// Flush sends whatever is due.
func (p *Publisher) Flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushLocked()
}

func (p *Publisher) flushLocked() {
	if p.sink == nil {
		return
	}
	if !p.shapeSent && p.shapeMsg != nil {
		if err := p.sink.Send(p.shapeMsg); err != nil {
			return
		}
		p.shapeSent = true
	}
	if p.sink.BufferedAmount() > PosBufferLimit {
		return // position stays pending for the next tick
	}
	if pos, ok := p.co.Due(p.now()); ok {
		_ = p.sink.Send(EncodePos(pos))
	}
}

// Run flushes every CoalesceInterval until stop is closed.
func (p *Publisher) Run(stop <-chan struct{}) {
	t := time.NewTicker(CoalesceInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.Flush()
		}
	}
}
