package main

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/cursorproto"
	"github.com/pion/webrtc/v4"
)

type fakeSink struct {
	mu       sync.Mutex
	got      [][]byte
	buffered uint64
	state    webrtc.DataChannelState
	fail     bool
}

func newSink() *fakeSink { return &fakeSink{state: webrtc.DataChannelStateOpen} }

func (f *fakeSink) Send(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("boom")
	}
	f.got = append(f.got, append([]byte(nil), b...))
	return nil
}
func (f *fakeSink) BufferedAmount() uint64 { f.mu.Lock(); defer f.mu.Unlock(); return f.buffered }
func (f *fakeSink) ReadyState() webrtc.DataChannelState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}
func (f *fakeSink) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) }

func testShape(t *testing.T) []byte {
	t.Helper()
	s, err := cursorproto.BuildShape(make([]byte, 16*16*4), 16, 16, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cursorproto.EncodeShape(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testPos(x int32) []byte {
	return cursorproto.EncodePos(cursorproto.Pos{Visible: true, ShapeID: 1, X: x, Y: 2, FrameW: 100, FrameH: 100})
}

func TestRelayFansOutAndValidates(t *testing.T) {
	r := newDCRelay(cursorRelayConfig())
	a, b := newSink(), newSink()
	r.addViewer(a)
	r.addViewer(b)
	now := time.Now()
	if n := r.publish(testPos(1), now); n != 2 {
		t.Fatalf("delivered to %d, want 2", n)
	}
	// Junk and oversize never reach anybody.
	if n := r.publish([]byte("hello"), now); n != 0 {
		t.Fatal("junk forwarded")
	}
	if n := r.publish(make([]byte, cursorproto.MaxMessage+1), now); n != 0 {
		t.Fatal("oversize forwarded")
	}
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("counts %d %d", a.count(), b.count())
	}
}

func TestRelayViewerCap(t *testing.T) {
	cfg := cursorRelayConfig()
	cfg.maxViewers = 2
	r := newDCRelay(cfg)
	if !r.addViewer(newSink()) || !r.addViewer(newSink()) {
		t.Fatal("under cap refused")
	}
	if r.addViewer(newSink()) {
		t.Fatal("over cap accepted")
	}
}

func TestRelayRateCap(t *testing.T) {
	r := newDCRelay(cursorRelayConfig())
	s := newSink()
	r.addViewer(s)
	now := time.Now()
	for i := 0; i < 1000; i++ {
		r.publish(testPos(int32(i)), now)
	}
	if c := s.count(); c > cursorRelayConfig().burst+1 {
		t.Fatalf("rate cap let %d through at one instant", c)
	}
}

func TestRelayReplaysStickyToLateViewer(t *testing.T) {
	r := newDCRelay(cursorRelayConfig())
	now := time.Now()
	shape := testShape(t)
	r.publish(shape, now)
	r.publish(testPos(5), now)
	r.publish(testPos(6), now)
	late := newSink()
	r.addViewer(late)
	if late.count() != 2 {
		t.Fatalf("replayed %d, want shape+pos", late.count())
	}
	if !bytes.Equal(late.got[0], shape) {
		t.Fatal("shape not replayed first")
	}
	p, _ := cursorproto.DecodePos(late.got[1])
	if p.X != 6 {
		t.Fatalf("stale pos replayed: %d", p.X)
	}
	r.reset()
	again := newSink()
	r.addViewer(again)
	if again.count() != 0 {
		t.Fatal("reset kept sticky state")
	}
}

func TestRelayBackpressureSkipsPositionsNotShapes(t *testing.T) {
	r := newDCRelay(cursorRelayConfig())
	s := newSink()
	r.addViewer(s)
	s.buffered = 32 << 10
	now := time.Now()
	r.publish(testPos(1), now)
	if s.count() != 0 {
		t.Fatal("position sent to congested viewer")
	}
	r.publish(testShape(t), now)
	if s.count() != 1 {
		t.Fatal("shape skipped for congested viewer")
	}
	s.buffered = 2 << 20
	r.publish(testShape(t), now)
	if s.count() != 1 {
		t.Fatal("sent above hard limit")
	}
}

func TestRelayDropsDeadViewers(t *testing.T) {
	r := newDCRelay(cursorRelayConfig())
	dead, closed := newSink(), newSink()
	dead.fail = true
	closed.state = webrtc.DataChannelStateClosed
	r.addViewer(dead)
	r.addViewer(closed)
	r.publish(testPos(1), time.Now())
	if r.viewerCount() != 0 {
		t.Fatalf("dead viewers kept: %d", r.viewerCount())
	}
}

// signalPair connects offerer and answerer in-process.
func signalPair(t *testing.T, off, ans *webrtc.PeerConnection) {
	t.Helper()
	o, err := off.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	g := webrtc.GatheringCompletePromise(off)
	if err := off.SetLocalDescription(o); err != nil {
		t.Fatal(err)
	}
	<-g
	if err := ans.SetRemoteDescription(*off.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	a, err := ans.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	g2 := webrtc.GatheringCompletePromise(ans)
	if err := ans.SetLocalDescription(a); err != nil {
		t.Fatal(err)
	}
	<-g2
	if err := off.SetRemoteDescription(*ans.LocalDescription()); err != nil {
		t.Fatal(err)
	}
}

func newCursorPC(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// End to end over real SCTP: agent channel "oosc-cursor" -> hub relay ->
// viewer channel opened by the browser, with a late viewer receiving the
// replayed shape first.
func TestCursorRelayEndToEnd(t *testing.T) {
	ns := &nodeSession{nodeID: "n-cursor"}
	t.Cleanup(func() { forgetRelays(ns) })

	// Agent leg.
	agent, hubA := newCursorPC(t), newCursorPC(t)
	adc, err := agent.CreateDataChannel(cursorproto.ChannelLabel, nil)
	if err != nil {
		t.Fatal(err)
	}
	hubA.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == cursorproto.ChannelLabel {
			attachAgentRelay(ns, dc, cursorRelayConfig())
		}
	})
	agentOpen := make(chan struct{})
	adc.OnOpen(func() { close(agentOpen) })
	signalPair(t, agent, hubA)
	select {
	case <-agentOpen:
	case <-time.After(10 * time.Second):
		t.Fatal("agent channel did not open")
	}
	shape := testShape(t)
	if err := adc.Send(shape); err != nil {
		t.Fatal(err)
	}
	// Let the shape land in the relay before the viewer joins.
	r := relayFor(ns, cursorRelayConfig())
	if !waitFor(5*time.Second, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.sticky[cursorproto.KindShape] != nil
	}) {
		t.Fatal("shape never reached relay")
	}

	// Viewer leg: browser is offerer and opens the channel.
	browser, hubV := newCursorPC(t), newCursorPC(t)
	bdc, err := browser.CreateDataChannel(cursorproto.ChannelLabel, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 16)
	bdc.OnMessage(func(m webrtc.DataChannelMessage) { got <- append([]byte(nil), m.Data...) })
	hubV.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == cursorproto.ChannelLabel {
			viewerRelayHandler(ns, hubV, dc, cursorRelayConfig())
		}
	})
	signalPair(t, browser, hubV)

	recv := func() []byte {
		select {
		case b := <-got:
			return b
		case <-time.After(10 * time.Second):
			t.Fatal("viewer received nothing")
			return nil
		}
	}
	if b := recv(); !bytes.Equal(b, shape) {
		t.Fatalf("first message is not the replayed shape: kind %d", cursorproto.Kind(b))
	}
	if err := adc.Send(testPos(42)); err != nil {
		t.Fatal(err)
	}
	p, err := cursorproto.DecodePos(recv())
	if err != nil || p.X != 42 {
		t.Fatalf("pos: %+v %v", p, err)
	}
}

// One viewer leg opening many 'oosc-cursor' channels must not take every
// viewer slot of the node: the relay accepts one channel per owner.
func TestRelayOneChannelPerViewerLeg(t *testing.T) {
	cfg := cursorRelayConfig()
	cfg.maxViewers = 3
	r := newDCRelay(cfg)
	legA, legB := new(int), new(int)
	a1, a2 := newSink(), newSink()
	if !r.addViewerOwned(legA, a1) {
		t.Fatal("first channel of leg A refused")
	}
	if r.addViewerOwned(legA, a2) || r.addViewerOwned(legA, newSink()) {
		t.Fatal("second channel of the same leg accepted")
	}
	if !r.addViewerOwned(legB, newSink()) {
		t.Fatal("leg B starved by leg A's extra channels")
	}
	r.removeViewer(a2) // never admitted: must not free leg A's slot
	if r.addViewerOwned(legA, newSink()) {
		t.Fatal("removing a rejected channel freed the leg's slot")
	}
	r.removeViewer(a1)
	if !r.addViewerOwned(legA, a2) {
		t.Fatal("leg A cannot reopen after its channel closed")
	}
	if n := r.viewerCount(); n != 2 {
		t.Fatalf("viewers = %d, want 2", n)
	}
}
