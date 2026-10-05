package cursorproto

import (
	"testing"
	"time"
)

// F9: paused publisher sends nothing; pausing hides the overlay; resuming
// re-sends shape and the latest position.
func TestPublisherPauseResume(t *testing.T) {
	p := NewPublisher()
	clock := time.Unix(100, 0)
	p.now = func() time.Time { return clock }
	s := &memSink{}
	p.SetSink(s)
	sh := func() (RawShape, bool) { return colorShape(9), true }
	p.Observe(Sample{Visible: true, X: 10, Y: 20, FrameW: 800, FrameH: 600, ShapeSeq: 1}, sh)
	if s.n() != 2 {
		t.Fatalf("want 2 msgs, got %d", s.n())
	}
	p.SetActive(false)
	if s.n() != 3 || Kind(s.got[2]) != KindPos {
		t.Fatalf("pause must send one hidden pos, got %d", s.n())
	}
	if pos, _ := DecodePos(s.got[2]); pos.Visible {
		t.Fatal("pause pos must be hidden")
	}
	clock = clock.Add(time.Second)
	p.Observe(Sample{Visible: true, X: 30, Y: 40, FrameW: 800, FrameH: 600, ShapeSeq: 2}, func() (RawShape, bool) { return colorShape(7), true })
	p.Flush()
	p.SetActive(false) // idempotent
	if s.n() != 3 || p.Active() {
		t.Fatalf("paused publisher sent: %d msgs", s.n())
	}
	p.SetActive(true)
	if s.n() != 5 || Kind(s.got[3]) != KindShape || Kind(s.got[4]) != KindPos {
		t.Fatalf("resume must re-send shape+pos, got %d", s.n())
	}
	if pos, _ := DecodePos(s.got[4]); !pos.Visible || pos.X != 31 {
		t.Fatalf("resume pos %+v", pos)
	}
}

func TestModeRoundTrip(t *testing.T) {
	for _, on := range []bool{false, true} {
		got, ok := DecodeMode(EncodeMode(on))
		if !ok || got != on {
			t.Fatalf("mode %v -> %v %v", on, got, ok)
		}
	}
	for _, b := range [][]byte{{Magic, KindMode}, {Magic, KindMode, 2}, {Magic, KindPos, 1}, {'X', KindMode, 1}, {Magic, KindMode, 1, 0}} {
		if _, ok := DecodeMode(b); ok {
			t.Fatalf("accepted %v", b)
		}
	}
	if Validate(EncodeMode(true)) == nil {
		t.Fatal("mode must never pass the relay validator")
	}
}
