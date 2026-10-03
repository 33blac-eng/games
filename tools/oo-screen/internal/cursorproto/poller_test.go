package cursorproto

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func newPolled(t *testing.T) (*Poller, *memSink, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(100, 0)}
	pub := NewPublisher()
	pub.now = clk.now
	sink := &memSink{}
	pub.SetSink(sink)
	pub.ObserveShape(1, func() (RawShape, bool) { return colorShape(0x80), true })
	return &Poller{Pub: pub}, sink, clk
}

func lastPos(t *testing.T, m *memSink) Pos {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.got) - 1; i >= 0; i-- {
		if Kind(m.got[i]) == KindPos {
			p, err := DecodePos(m.got[i])
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatal("no position sent")
	return Pos{}
}

func posCount(m *memSink) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range m.got {
		if Kind(b) == KindPos {
			n++
		}
	}
	return n
}

var geo = Geometry{Left: 1920, Top: 0, W: 1920, H: 1080, FrameW: 1920, FrameH: 1080}

func TestPollerSendsOnlyOnChange(t *testing.T) {
	p, sink, clk := newPolled(t)
	if !p.Step(Reading{Showing: true, X: 2000, Y: 50}, true, geo, true) {
		t.Fatal("poller should own positions")
	}
	got := lastPos(t, sink)
	if !got.Visible || got.X != 80 || got.Y != 50 || got.FrameW != 1920 {
		t.Fatalf("pos %+v", got)
	}
	n := posCount(sink)
	for i := 0; i < 50; i++ { // pointer at rest: nothing new on the wire
		clk.t = clk.t.Add(PollInterval)
		p.Step(Reading{Showing: true, X: 2000, Y: 50}, true, geo, true)
	}
	if posCount(sink) != n {
		t.Fatalf("resting pointer sent %d extra positions", posCount(sink)-n)
	}
}

func TestPollerCoalescesAndDeliversFinal(t *testing.T) {
	p, sink, clk := newPolled(t)
	p.Step(Reading{Showing: true, X: 1920, Y: 0}, true, geo, true)
	n0 := posCount(sink)
	// 1 ms polling (faster than the 8 ms coalescing): moves in between are
	// folded, at most one message per CoalesceInterval.
	for i := 1; i <= 40; i++ {
		clk.t = clk.t.Add(time.Millisecond)
		p.Step(Reading{Showing: true, X: 1920 + i, Y: i}, true, geo, true)
	}
	sent := posCount(sink) - n0
	if sent > 40/int(CoalesceInterval/time.Millisecond)+1 || sent < 3 {
		t.Fatalf("%d positions for 40 ms of motion", sent)
	}
	// pointer stops; the next tick after the interval flushes the last move
	clk.t = clk.t.Add(CoalesceInterval)
	p.Step(Reading{Showing: true, X: 1960, Y: 40}, true, geo, true)
	if got := lastPos(t, sink); got.X != 40 || got.Y != 40 {
		t.Fatalf("final position %+v", got)
	}
}

func TestPollerOtherMonitorHides(t *testing.T) {
	p, sink, clk := newPolled(t)
	p.Step(Reading{Showing: true, X: 100, Y: 100}, true, geo, true) // left of output
	if lastPos(t, sink).Visible {
		t.Fatal("pointer on another monitor must be hidden")
	}
	n := posCount(sink)
	for i := 0; i < 10; i++ { // moving on the other monitor is not news
		clk.t = clk.t.Add(PollInterval)
		p.Step(Reading{Showing: true, X: 100 + i, Y: 100}, true, geo, true)
	}
	if posCount(sink) != n {
		t.Fatal("moves on another monitor were sent")
	}
	clk.t = clk.t.Add(PollInterval)
	p.Step(Reading{Showing: false, X: 2000, Y: 10}, true, geo, true)
	if lastPos(t, sink).Visible {
		t.Fatal("hidden cursor shown")
	}
}

func TestPollerYieldsWithoutGeometry(t *testing.T) {
	p, _, _ := newPolled(t)
	if p.Step(Reading{}, false, geo, true) {
		t.Fatal("no reading: must yield to the frame loop")
	}
	if p.Step(Reading{Showing: true}, true, Geometry{}, false) {
		t.Fatal("no geometry: must yield")
	}
	rot := Geometry{W: 1080, H: 1920, FrameW: 1920, FrameH: 1080}
	if p.Step(Reading{Showing: true}, true, rot, true) {
		t.Fatal("rect != frame (rotated): must yield")
	}
}

func TestLateShapeShowsPolledPointer(t *testing.T) {
	clk := &fakeClock{t: time.Unix(100, 0)}
	pub := NewPublisher()
	pub.now = clk.now
	sink := &memSink{}
	pub.SetSink(sink)
	p := &Poller{Pub: pub}
	p.Step(Reading{Showing: true, X: 2000, Y: 10}, true, geo, true)
	if lastPos(t, sink).Visible {
		t.Fatal("visible without a shape")
	}
	clk.t = clk.t.Add(CoalesceInterval)
	pub.ObserveShape(7, func() (RawShape, bool) { return colorShape(0x40), true })
	got := lastPos(t, sink)
	if !got.Visible || got.X != 80 || got.ShapeID == 0 {
		t.Fatalf("after first shape: %+v", got)
	}
}
