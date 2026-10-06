package main

import (
	"sync"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/cursorproto"
	"github.com/pion/webrtc/v4"
)

// F9: the agent follows only the hub's grant on the CURRENT cursor channel;
// junk and stale channels change nothing, closing the channel revokes.
func TestCursorGrantNegotiation(t *testing.T) {
	t.Cleanup(func() { cursorChan.Store(nil); applyCursorGrant(false) })
	applyCursorGrant(false)
	cur, old := &webrtc.DataChannel{}, &webrtc.DataChannel{}
	cursorChan.Store(cur)

	if cursorPub.Active() {
		t.Fatal("layer must start revoked (pointer composited)")
	}
	cursorGrantMessage(old, cursorproto.EncodeMode(true))
	cursorGrantMessage(cur, []byte{cursorproto.Magic, cursorproto.KindMode, 7})
	cursorGrantMessage(cur, cursorproto.EncodePos(cursorproto.Pos{}))
	if cursorPub.Active() {
		t.Fatal("stale channel / junk must not grant")
	}
	cursorGrantMessage(cur, cursorproto.EncodeMode(true))
	if !cursorPub.Active() {
		t.Fatal("grant ignored")
	}
	cursorChannelClosed(old) // an older channel closing: no effect
	if !cursorPub.Active() {
		t.Fatal("old channel close revoked the current grant")
	}
	cursorGrantMessage(cur, cursorproto.EncodeMode(false))
	if cursorPub.Active() {
		t.Fatal("revoke ignored")
	}
	cursorGrantMessage(cur, cursorproto.EncodeMode(true))
	cursorChannelClosed(cur)
	if cursorPub.Active() || cursorChan.Load() != nil {
		t.Fatal("closing the channel must revoke")
	}
}

// gateRec — дозвіл шару з записом застосувань (замість капчера/Publisher).
type gateRec struct {
	mu  sync.Mutex
	got []bool
}

func (g *gateRec) apply(on bool) { g.mu.Lock(); g.got = append(g.got, on); g.mu.Unlock() }
func (g *gateRec) last() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.got) > 0 && g.got[len(g.got)-1]
}

// F9 + N6: the hub cannot see direct legs, so the agent itself keeps the
// pointer in the frame while ANY live direct leg lacks an open oosc-cursor.
func TestCursorGateDirectLegs(t *testing.T) {
	rec := &gateRec{}
	g := &cursorGate{apply: rec.apply}
	g.setHub(true)
	if !rec.last() {
		t.Fatal("hub grant without direct legs must enable the layer")
	}
	g.legAdd("a")
	if rec.last() {
		t.Fatal("direct leg without cursor channel: layer must be revoked")
	}
	g.legCursor("a", true)
	if !rec.last() {
		t.Fatal("direct leg with cursor channel: want layer")
	}
	g.legAdd("b")
	if rec.last() {
		t.Fatal("second direct leg without channel: must revoke")
	}
	g.legAdd("a") // повтор не скидає відкритий канал
	g.legDrop("b")
	if !rec.last() {
		t.Fatal("layer-less leg gone: want layer back")
	}
	g.legCursor("a", false)
	if rec.last() {
		t.Fatal("cursor channel closed on a live leg: must revoke")
	}
	g.legDrop("a")
	g.legCursor("a", true) // пізня подія знятої ноги — ігнор
	g.legCursor("z", false)
	if !rec.last() || len(g.legs) != 0 {
		t.Fatalf("late events of dropped legs changed the gate: %v legs=%v", rec.last(), g.legs)
	}
	g.setHub(false)
	g.legAdd("c")
	g.legCursor("c", true)
	if rec.last() {
		t.Fatal("direct legs alone never enable the layer without the hub grant")
	}
}

// The real p2pAgent: a direct leg whose browser did not open oosc-cursor
// revokes a hub grant for as long as it lives; one that did, keeps it.
func TestP2PCursorGateFollowsDirectLeg(t *testing.T) {
	for _, withCursor := range []bool{false, true} {
		rec := &gateRec{}
		fan := newCursorFan(cursorproto.NewPublisher())
		fan.gate = &cursorGate{apply: rec.apply}
		fan.gate.setHub(true) // relay-глядачі хаба дозволили шар
		r := newP2PRig(t, true, func(a *p2pAgent) { a.cursor = fan })
		v := r.tailOffer(t, "t-control", false, withCursor, false)
		select {
		case <-v.video:
		case <-time.After(8 * time.Second):
			t.Fatal("no video on the direct leg")
		}
		if withCursor {
			p2pWait(t, "cursor channel open -> layer granted", rec.last)
		} else if rec.last() {
			t.Fatal("direct leg without cursor channel: pointer must stay in the frame")
		}
		if r.broker.RevokeUser("u1") != 1 {
			t.Fatal("revoke")
		}
		p2pWait(t, "leg gone -> hub grant applies again", func() bool {
			fan.gate.mu.Lock()
			defer fan.gate.mu.Unlock()
			return len(fan.gate.legs) == 0 && rec.last()
		})
	}
}
