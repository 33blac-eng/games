package main

import (
	"testing"

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
