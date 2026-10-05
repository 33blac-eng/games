// B3 (TZ-GENERAL §5): кеш GOP ноди, на яку ніхто не дивиться (або без
// агента), не тримає пам'ять і не віддає застарілий хвіст.
package main

import (
	"runtime"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// TestGopFreedWhenLastViewerLeaves — останній глядач пішов: кеш порожній
// одразу (і новий глядач не отримає давній хвіст, а попросить IDR).
func TestGopFreedWhenLastViewerLeaves(t *testing.T) {
	quietNDJSON(t, nil)
	ns := &nodeSession{nodeID: "b3"}
	ns.agentPC = &webrtc.PeerConnection{}
	vl := newPrimeLeg(ns)
	b := &auBuilder{}
	for _, p := range idrAU(b, 1000, 4, false, true) {
		forwardToViewers(ns, agentGen1, p)
	}
	ns.mu.Lock()
	n := len(ns.gop.replay())
	ns.mu.Unlock()
	if n == 0 {
		t.Fatal("тест не наповнив кеш")
	}
	removeViewer(ns, vl)
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if ns.gop.pkts != nil || ns.gop.bytes != 0 {
		t.Fatalf("нода без глядачів тримає %d пакетів кешу", len(ns.gop.pkts))
	}
}

// TestGopResetReleasesMemory — B3: reset() відпускає payload-и (купа після GC
// повертається), а не лише обнуляє довжину зрізу.
func TestGopResetReleasesMemory(t *testing.T) {
	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	const nodes = 10
	caches := make([]gopCache, nodes)
	base := heap()
	for n := range caches {
		g := &caches[n]
		g.note(&rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x78, 0, 2, 0x67, 0x42, 0, 2, 0x68, 0xCE, 0, 2, 0x65, 0x88}})
		for i := 1; i < 800; i++ { // ≈ 1 МБ на ноду
			p := make([]byte, 1200)
			p[0] = 0x21
			g.note(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i) * 300}, Payload: p})
		}
	}
	full := heap()
	if full-base < nodes*800*1000 {
		t.Fatalf("кеші не наповнились: +%d байт", full-base)
	}
	for n := range caches {
		caches[n].reset()
	}
	after := heap()
	if after > base+256<<10 {
		t.Fatalf("після reset() купа %d КБ, на старті %d КБ (наповнено %d КБ) — payload-и не відпущено",
			after>>10, base>>10, full>>10)
	}
	runtime.KeepAlive(caches)
}
