// B2/B3 (TZ-GENERAL §5): кеш GOP мертвого агента не має пережити
// «publisher lost» — ні для нового агента (старий кадр 35-секундної давнини),
// ні як пам'ять ноди без агента.
package main

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestPublisherLostClearsGop(t *testing.T) {
	quietNDJSON(t, nil)
	srv := fakeERP(t)
	defer srv.Close()
	prevBase, prevKey, prevReg := erpBase, hubKey, reg
	erpBase, hubKey = srv.URL, "test-key"
	reg = newRegistry()
	defer func() { erpBase, hubKey, reg = prevBase, prevKey, prevReg }()

	remote := dialAgentLeg(t, "lostA")
	ns := reg.get("lostA")
	if ns == nil || !waitForD(15*time.Second, ns.hasAgent) {
		t.Fatal("агентська нога не піднялась")
	}

	// Кеш, як після кількох секунд трансляції.
	ns.mu.Lock()
	b := &auBuilder{}
	for _, p := range idrAU(b, 1000, 4, false, true) {
		ns.gop.note(p)
	}
	for i := uint32(1); i <= 50; i++ {
		for _, p := range pAU(b, 1000+i*1500, 4) {
			ns.gop.note(p)
		}
	}
	filled := len(ns.gop.replay())
	ns.mu.Unlock()
	if filled == 0 {
		t.Fatal("тест не наповнив кеш")
	}

	_ = remote.Close() // агент зник

	gopEmpty := func() bool {
		ns.mu.Lock()
		defer ns.mu.Unlock()
		return ns.agentPC == nil && ns.gop.pkts == nil && ns.gop.bytes == 0 && len(ns.gop.auPkts) == 0
	}
	if !waitForD(15*time.Second, gopEmpty) {
		t.Fatalf("після publisher lost кеш GOP досі тримає %d пакетів мертвого агента", len(ns.gop.pkts))
	}

	// Нова нога при новому агенті (до його першого IDR) нічого старого не
	// отримує і не вважається primed.
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{}
	ns.mu.Unlock()
	vl := newPrimeLeg(ns)
	if len(vl.out) != 0 || viewerPrimed(ns, vl) {
		t.Fatalf("новому глядачеві віддано %d пакетів старого агента (primed=%v)", len(vl.out), viewerPrimed(ns, vl))
	}
}
