package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/internal/control"
)

func ctlBytes(t *testing.T, m control.Msg) []byte {
	var b bytes.Buffer
	if err := control.Write(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestLockedAgentLeavesReadyNodes — агент на заблокованому ПК прибирає ноду
// з /nodes, розблокування повертає. Негативний контроль — сміття в каналі
// стан не міняє.
func TestLockedAgentLeavesReadyNodes(t *testing.T) {
	prevReg := reg
	reg = newRegistry()
	t.Cleanup(func() { reg = prevReg })
	ns := reg.getOrCreate("lock-ready")
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{} // сентинел, як у readyNode
	ns.mu.Unlock()
	inReady := func() bool {
		for _, n := range reg.readyNodes() {
			if n == ns.nodeID {
				return true
			}
		}
		return false
	}
	if !inReady() {
		t.Fatalf("нода з агентом мусить бути в /nodes до будь-якого сигналу")
	}
	handleAgentCtl(ns, []byte("resume"), time.Now())
	handleAgentCtl(ns, []byte("{битий"), time.Now())
	if !inReady() {
		t.Fatalf("сміття в каналі прибрало ноду з /nodes")
	}
	handleAgentCtl(ns, ctlBytes(t, control.FallbackReason(1, "session-locked")), time.Now())
	if inReady() {
		t.Fatalf("агент сказав session-locked, а нода досі в /nodes")
	}
	if got := reg.unavailableNodes()["lock-ready"]; got != "session-locked" {
		t.Fatalf("причина не дійшла в /nodes: %q", got)
	}
	handleAgentCtl(ns, ctlBytes(t, control.FallbackReason(2, "")), time.Now())
	if !inReady() {
		t.Fatalf("агент розблокувався, а нода не повернулась у /nodes")
	}
	if reg.unavailableNodes() != nil {
		t.Fatalf("після розблокування причина лишилась у /nodes")
	}
}
