package cursorproto

import (
	"sync"
	"testing"
	"time"
)

type memSink struct {
	mu       sync.Mutex
	got      [][]byte
	buffered uint64
}

func (m *memSink) Send(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got = append(m.got, append([]byte(nil), b...))
	return nil
}
func (m *memSink) BufferedAmount() uint64 { m.mu.Lock(); defer m.mu.Unlock(); return m.buffered }
func (m *memSink) n() int                 { m.mu.Lock(); defer m.mu.Unlock(); return len(m.got) }

func colorShape(fill byte) RawShape {
	d := make([]byte, 4*4*4)
	for i := range d {
		d[i] = fill
	}
	return RawShape{Type: DXGIColor, W: 4, H: 4, Pitch: 16, HotX: 1, HotY: 2, Data: d}
}

func TestPublisherShapeThenCoalescedPositions(t *testing.T) {
	p := NewPublisher()
	clock := time.Unix(100, 0)
	p.now = func() time.Time { return clock }
	s := &memSink{}
	p.SetSink(s)

	pulls := 0
	sh := func() (RawShape, bool) { pulls++; return colorShape(9), true }
	smp := Sample{Visible: true, X: 10, Y: 20, FrameW: 1920, FrameH: 1080, ShapeSeq: 1}
	p.Observe(smp, sh)
	if s.n() != 2 || Kind(s.got[0]) != KindShape || Kind(s.got[1]) != KindPos {
		t.Fatalf("want shape then pos, got %d msgs", s.n())
	}
	pos, _ := DecodePos(s.got[1])
	shape, _ := DecodeShape(s.got[0])
	if pos.X != 11 || pos.Y != 22 || !pos.Visible || pos.ShapeID != shape.ID || pos.FrameW != 1920 {
		t.Fatalf("pos %+v (hotspot must be added)", pos)
	}

	// Same seq, moved inside the coalesce window: nothing new, no re-pull.
	smp.X = 50
	p.Observe(smp, sh)
	if s.n() != 2 || pulls != 1 {
		t.Fatalf("coalescing broken: %d msgs, %d shape pulls", s.n(), pulls)
	}
	clock = clock.Add(CoalesceInterval)
	p.Flush()
	if s.n() != 3 {
		t.Fatalf("trailing position not flushed: %d", s.n())
	}
	clock = clock.Add(time.Second)
	p.Observe(smp, sh)
	if s.n() != 3 {
		t.Fatal("unchanged position resent")
	}
	// Congested: held back, delivered once drained.
	s.buffered = 1 << 20
	smp.Y = 99
	p.Observe(smp, sh)
	if s.n() != 3 {
		t.Fatal("position sent into a congested channel")
	}
	s.buffered = 0
	p.Flush()
	if s.n() != 4 {
		t.Fatal("held position never delivered")
	}
}

func TestPublisherShapeCacheAndResendOnNewSink(t *testing.T) {
	p := NewPublisher()
	s := &memSink{}
	p.SetSink(s)
	a, b := colorShape(1), colorShape(2)
	cur := a
	sh := func() (RawShape, bool) { return cur, true }
	smp := Sample{Visible: true, FrameW: 100, FrameH: 100, ShapeSeq: 1}
	p.Observe(smp, sh)
	idA := p.ShapeID()
	cur, smp.ShapeSeq = b, 2
	p.Observe(smp, sh)
	cur, smp.ShapeSeq = a, 3
	p.Observe(smp, sh)
	if p.ShapeID() != idA || len(p.cache) != 2 {
		t.Fatalf("cache: id %x vs %x, %d entries", p.ShapeID(), idA, len(p.cache))
	}
	shapes := 0
	for _, m := range s.got {
		if Kind(m) == KindShape {
			shapes++
		}
	}
	if shapes != 3 {
		t.Fatalf("every shape change must go on the wire, got %d", shapes)
	}
	s2 := &memSink{}
	p.SetSink(s2)
	if s2.n() == 0 || Kind(s2.got[0]) != KindShape {
		t.Fatal("new sink did not receive the current shape")
	}
	p.ClearSink(s2)
	p.Observe(Sample{Visible: true, X: 7, ShapeSeq: 3}, sh)
	if s2.n() != 2 {
		t.Fatal("sent after ClearSink")
	}
}

func TestPublisherHiddenWithoutShape(t *testing.T) {
	p := NewPublisher()
	s := &memSink{}
	p.SetSink(s)
	p.Observe(Sample{Visible: true, FrameW: 10, FrameH: 10}, func() (RawShape, bool) { return RawShape{}, false })
	if s.n() != 1 {
		t.Fatalf("got %d", s.n())
	}
	pos, _ := DecodePos(s.got[0])
	if pos.Visible {
		t.Fatal("visible without any shape")
	}
}

func TestPublisherRunStops(t *testing.T) {
	p := NewPublisher()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { p.Run(stop); close(done) }()
	p.SetSink(&memSink{})
	p.Observe(Sample{}, func() (RawShape, bool) { return RawShape{}, false })
	close(stop)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}
