// B1 (TZ-GENERAL §5): кеш GOP має бути AU-орієнтованим. x264 zerolatency
// кладе IDR у 4 слайси (FU-A кожен), а з nal-hrd — ще й SEI між PPS та IDR.
// Старий детектор «фронту ключового пакета» перезапускав кеш на кожному
// слайсі, і нова нога отримувала IDR без SPS/PPS, вважаючись primed.
package main

import (
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// auBuilder — потік пакетів агента з наростаючими seq.
type auBuilder struct{ seq uint16 }

func (b *auBuilder) pkt(ts uint32, payload []byte) *rtp.Packet {
	b.seq++
	return &rtp.Packet{Header: rtp.Header{SequenceNumber: b.seq, Timestamp: ts}, Payload: payload}
}

// fuA — слайс типу nalType у трьох FU-A-фрагментах: старт, середина, кінець.
func fuA(b *auBuilder, ts uint32, nalType byte) []*rtp.Packet {
	ind := byte(0x60 | 28)
	return []*rtp.Packet{
		b.pkt(ts, []byte{ind, 0x80 | nalType, 0xAA, 0xBB}),
		b.pkt(ts, []byte{ind, nalType, 0xCC}),
		b.pkt(ts, []byte{ind, 0x40 | nalType, 0xDD}),
	}
}

// idrAU — повний ключовий AU: [SEI перед SPS?] STAP-A(SPS,PPS) [SEI?] і
// slices слайсів IDR.
func idrAU(b *auBuilder, ts uint32, slices int, seiBefore, seiMid bool) []*rtp.Packet {
	var out []*rtp.Packet
	if seiBefore {
		out = append(out, b.pkt(ts, []byte{0x06, 0x05, 0x01, 0x80}))
	}
	out = append(out, b.pkt(ts, []byte{0x78, 0, 2, 0x67, 0x42, 0, 2, 0x68, 0xCE}))
	if seiMid {
		out = append(out, b.pkt(ts, []byte{0x06, 0x01, 0x01, 0x80}))
	}
	for i := 0; i < slices; i++ {
		out = append(out, fuA(b, ts, 5)...)
	}
	return out
}

// pAU — P-кадр із slices слайсів (тип 1).
func pAU(b *auBuilder, ts uint32, slices int) []*rtp.Packet {
	var out []*rtp.Packet
	for i := 0; i < slices; i++ {
		out = append(out, fuA(b, ts, 1)...)
	}
	return out
}

func newPrimeLeg(ns *nodeSession) *viewerLeg {
	vl := &viewerLeg{
		pc:   &webrtc.PeerConnection{},
		out:  make(chan *rtp.Packet, viewerQueueDepth+gopMaxPackets),
		done: make(chan struct{}),
	}
	ns.mu.Lock()
	ns.viewers[vl.pc] = vl
	ns.mu.Unlock()
	markViewerReady(ns, vl)
	recomputeBinding(ns)
	return vl
}

func TestGopMultiSliceSEIPrimes(t *testing.T) {
	quietNDJSON(t, nil)
	for _, c := range []struct {
		name              string
		slices            int
		seiBefore, seiMid bool
	}{
		{"1 слайс", 1, false, false},
		{"4 слайси", 4, false, false},
		{"4 слайси + SEI між PPS та IDR", 4, false, true},
		{"4 слайси + SEI перед SPS", 4, true, false},
		{"1 слайс + SEI", 1, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ns := &nodeSession{nodeID: "au"}
			ns.agentPC = &webrtc.PeerConnection{}
			quietViewer(ns)
			b := &auBuilder{}
			// Старий GOP, щоб перезапуск кешу справді відбувся.
			for _, p := range idrAU(b, 1000, c.slices, c.seiBefore, c.seiMid) {
				forwardToViewers(ns, agentGen1, p)
			}
			for _, p := range pAU(b, 2500, c.slices) {
				forwardToViewers(ns, agentGen1, p)
			}
			// Новий IDR, потім два P-кадри.
			idr := idrAU(b, 4000, c.slices, c.seiBefore, c.seiMid)
			want := len(idr)
			for _, p := range idr {
				forwardToViewers(ns, agentGen1, p)
			}
			for _, ts := range []uint32{5500, 7000} {
				for _, p := range pAU(b, ts, c.slices) {
					forwardToViewers(ns, agentGen1, p)
					want++
				}
			}
			ns.mu.Lock()
			startTS := ns.gop.startTS
			ns.mu.Unlock()
			vl := newPrimeLeg(ns)
			if len(vl.out) != want {
				t.Fatalf("нова нога отримала %d пакетів, want %d (весь AU IDR + хвіст)", len(vl.out), want)
			}
			if !viewerPrimed(ns, vl) {
				t.Fatalf("кеш самодостатній, а primed=false")
			}
			first := <-vl.out
			if first.Timestamp != startTS {
				t.Fatalf("перший пакет кешу не з останнього IDR-AU")
			}
			hasSPS := false
			h264NALTypes(first.Payload, func(t byte) { hasSPS = hasSPS || t == 7 })
			if !c.seiBefore && !hasSPS {
				t.Fatalf("кеш починається не з SPS/PPS: тип %d", first.Payload[0]&0x1F)
			}
			if c.seiBefore && first.Payload[0]&0x1F != 6 {
				t.Fatalf("SEI, що стояв перед SPS у ключовому AU, загубився")
			}
		})
	}
}

// Кеш, що починається з IDR без SPS/PPS, не самодостатній: нога НЕ primed і
// отже попросить keyframe.
func TestGopIDRWithoutParamSetsNotPrimed(t *testing.T) {
	quietNDJSON(t, nil)
	ns := &nodeSession{nodeID: "au-nosps"}
	ns.agentPC = &webrtc.PeerConnection{}
	quietViewer(ns)
	b := &auBuilder{}
	for i := 0; i < 4; i++ {
		for _, p := range fuA(b, 1000, 5) {
			forwardToViewers(ns, agentGen1, p)
		}
	}
	for _, p := range pAU(b, 2500, 4) {
		forwardToViewers(ns, agentGen1, p)
	}
	vl := newPrimeLeg(ns)
	if len(vl.out) != 0 || viewerPrimed(ns, vl) {
		t.Fatalf("IDR без SPS/PPS віддано як готовий кеш: %d пакетів, primed=%v", len(vl.out), viewerPrimed(ns, vl))
	}
}
