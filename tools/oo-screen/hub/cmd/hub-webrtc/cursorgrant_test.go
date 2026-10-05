package main

import (
	"sync"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/cursorproto"
	"github.com/pion/webrtc/v4"
)

// F9: the hub grants the cursor layer to the agent only while EVERY viewer
// leg of the node has an open 'oosc-cursor' channel and recording is off.
func TestCursorGrantNegotiation(t *testing.T) {
	prevRec := recordEnabled.Load()
	recordEnabled.Store(false)
	t.Cleanup(func() { recordEnabled.Store(prevRec) })

	ns := &nodeSession{nodeID: "n-grant"}
	t.Cleanup(func() { forgetRelays(ns) })
	r := relayFor(ns, cursorRelayConfig())
	agent := newSink()
	r.setAgent(agent)

	last := func() (bool, int) {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		if len(agent.got) == 0 {
			return false, 0
		}
		on, ok := cursorproto.DecodeMode(agent.got[len(agent.got)-1])
		if !ok {
			t.Fatalf("agent got a non-mode message")
		}
		return on, len(agent.got)
	}
	setViewers := func(pcs ...*webrtc.PeerConnection) {
		ns.mu.Lock()
		ns.viewers = map[*webrtc.PeerConnection]*viewerLeg{}
		for _, pc := range pcs {
			ns.viewers[pc] = &viewerLeg{pc: pc}
		}
		ns.mu.Unlock()
	}

	refreshCursorGrant(ns)
	if on, n := last(); on || n != 1 {
		t.Fatalf("no viewers: want one explicit revoke, got on=%v n=%d", on, n)
	}
	refreshCursorGrant(ns)
	if _, n := last(); n != 1 {
		t.Fatal("unchanged grant must not be re-sent")
	}

	pc1, pc2 := &webrtc.PeerConnection{}, &webrtc.PeerConnection{}
	setViewers(pc1)
	refreshCursorGrant(ns)
	if on, _ := last(); on {
		t.Fatal("viewer without cursor channel: must stay revoked")
	}
	s1 := newSink()
	r.addViewerOwned(pc1, s1)
	refreshCursorGrant(ns)
	if on, _ := last(); !on {
		t.Fatal("all viewers capable: want grant")
	}

	// A second viewer without cursorLayer joins: revoke.
	setViewers(pc1, pc2)
	refreshCursorGrant(ns)
	if on, _ := last(); on {
		t.Fatal("legacy viewer joined: must revoke")
	}
	s2 := newSink()
	r.addViewerOwned(pc2, s2)
	refreshCursorGrant(ns)
	if on, _ := last(); !on {
		t.Fatal("both capable: want grant")
	}

	// A channel that is no longer open does not count.
	s2.mu.Lock()
	s2.state = webrtc.DataChannelStateClosing
	s2.mu.Unlock()
	refreshCursorGrant(ns)
	if on, _ := last(); on {
		t.Fatal("closing channel must revoke")
	}
	s2.mu.Lock()
	s2.state = webrtc.DataChannelStateOpen
	s2.mu.Unlock()
	refreshCursorGrant(ns)

	// Recording keeps the drawn cursor in the MKV.
	recordEnabled.Store(true)
	refreshCursorGrant(ns)
	if on, _ := last(); on {
		t.Fatal("recording on: must revoke")
	}
	recordEnabled.Store(false)
	refreshCursorGrant(ns)
	if on, _ := last(); !on {
		t.Fatal("recording off again: want grant")
	}

	// New agent channel: it starts revoked and must be told again.
	agent2 := newSink()
	r.setAgent(agent2)
	refreshCursorGrant(ns)
	if agent2.count() != 1 {
		t.Fatalf("new agent channel got %d grants", agent2.count())
	}
	// Agent gone: nothing to send to, no panic.
	r.agentGone(agent2)
	refreshCursorGrant(ns)
	if agent2.count() != 1 {
		t.Fatal("sent to a gone agent")
	}
}

// No relay (agent without -cursor-layer): refresh is a no-op.
func TestCursorGrantNoRelay(t *testing.T) {
	refreshCursorGrant(&nodeSession{nodeID: "n-none"})
}

// slowAgent widens the window between the state update and Send so that
// unserialized grants would reach the agent out of order.
type slowAgent struct{ *fakeSink }

func (s slowAgent) Send(b []byte) error {
	time.Sleep(50 * time.Microsecond)
	return s.fakeSink.Send(b)
}

// Concurrent refreshCursorGrant (ticker, viewer open/close) must leave the
// last Mode the agent received equal to what the hub recorded; otherwise the
// 1 s ticker never re-sends and the agent can stay on the layer forever.
func TestCursorGrantConcurrentOrder(t *testing.T) {
	prevRec := recordEnabled.Load()
	recordEnabled.Store(false)
	t.Cleanup(func() { recordEnabled.Store(prevRec) })

	for iter := 0; iter < 50; iter++ {
		ns := &nodeSession{nodeID: "n-grant-race"}
		r := relayFor(ns, cursorRelayConfig())
		agent := slowAgent{newSink()}
		r.setAgent(agent)
		pc1, pc2 := &webrtc.PeerConnection{}, &webrtc.PeerConnection{}
		r.addViewerOwned(pc1, newSink())

		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 20; i++ {
					ns.mu.Lock()
					ns.viewers = map[*webrtc.PeerConnection]*viewerLeg{pc1: {pc: pc1}}
					if (g+i)%2 == 0 {
						ns.viewers[pc2] = &viewerLeg{pc: pc2} // layer-less viewer
					}
					ns.mu.Unlock()
					refreshCursorGrant(ns)
				}
			}(g)
		}
		wg.Wait()

		agent.mu.Lock()
		sent, ok := cursorproto.DecodeMode(agent.got[len(agent.got)-1])
		agent.mu.Unlock()
		r.mu.Lock()
		rec := r.granted
		r.mu.Unlock()
		forgetRelays(ns)
		if !ok || sent != rec {
			t.Fatalf("iter %d: agent last got on=%v, hub recorded granted=%v", iter, sent, rec)
		}
	}
}
