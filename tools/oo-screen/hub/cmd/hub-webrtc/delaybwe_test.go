package main

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// linkSim — пакети по 1200 Б з темпом sendBps через вузьке місце capBps
// (FIFO без дропів). Повертає моменти відправки й приходу.
func linkSim(t0 time.Time, n int, sendBps, capBps float64) (send, arr []time.Time) {
	const size = 1200
	gap := time.Duration(float64(size*8) / sendBps * float64(time.Second))
	svc := time.Duration(float64(size*8) / capBps * float64(time.Second))
	var free time.Time
	for i := 0; i < n; i++ {
		s := t0.Add(time.Duration(i) * gap)
		start := s
		if free.After(start) {
			start = free
		}
		free = start.Add(svc)
		send = append(send, s)
		arr = append(arr, free.Add(20*time.Millisecond))
	}
	return
}

func TestTrendlineNoOveruseBelowCapacity(t *testing.T) {
	tr := newTrendline()
	s, a := linkSim(time.Unix(1000, 0), 3000, 1.5e6, 2e6)
	for i := range s {
		if tr.Add(s[i], a[i]); tr.Fired() {
			t.Fatalf("OVERUSE на %d пакеті при 1.5 Мбіт/с через 2 Мбіт/с", i)
		}
	}
}

func TestTrendlineOveruseAboveCapacity(t *testing.T) {
	tr := newTrendline()
	s, a := linkSim(time.Unix(1000, 0), 3000, 4e6, 2e6)
	for i := range s {
		if tr.Add(s[i], a[i]); tr.Fired() {
			// Черга росте на 1 мс за 2 мс: детектор мусить побачити за ~0.5 с.
			if d := s[i].Sub(s[0]); d > 600*time.Millisecond {
				t.Fatalf("OVERUSE запізно: %v", d)
			}
			return
		}
	}
	t.Fatal("OVERUSE не спрацював при 4 Мбіт/с через 2 Мбіт/с")
}

// fbFor — TWCC-фідбек на seq [base, base+len(arr)) (усі отримані), з
// приходами arr; ReferenceTime ставиться за першим приходом.
func fbFor(base uint16, arr []time.Time) *rtcp.TransportLayerCC {
	ref := arr[0].Truncate(64 * time.Millisecond)
	fb := &rtcp.TransportLayerCC{
		BaseSequenceNumber: base,
		PacketStatusCount:  uint16(len(arr)),
		ReferenceTime:      uint32(ref.Sub(time.Unix(0, 0)) / (64 * time.Millisecond)),
		PacketChunks: []rtcp.PacketStatusChunk{&rtcp.RunLengthChunk{
			PacketStatusSymbol: rtcp.TypeTCCPacketReceivedSmallDelta, RunLength: uint16(len(arr)),
		}},
	}
	prev := ref
	for _, x := range arr {
		fb.RecvDeltas = append(fb.RecvDeltas, &rtcp.RecvDelta{
			Type: rtcp.TypeTCCPacketReceivedSmallDelta, Delta: int64(x.Sub(prev) / time.Microsecond),
		})
		prev = x
	}
	return fb
}

// Наскрізь: stamp -> фідбек -> OVERUSE + acked ≈ стелі + sent ≈ темпу.
func TestTwccLegFeedbackOveruseAndRates(t *testing.T) {
	tl := newTwccLeg(nil)
	tl.extID = 5
	s, a := linkSim(time.Unix(1000, 0), 1000, 4e6, 2e6)
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: 1}, Payload: make([]byte, 1188)}
	for i := range s {
		out := tl.stamp(pkt, s[i])
		if out == pkt || len(pkt.Header.Extensions) != 0 {
			t.Fatal("stamp мусить повертати КОПІЮ, оригінал (спільний для ніг) не чіпати")
		}
		if b := out.Header.GetExtension(5); len(b) != 2 || uint16(b[0])<<8|uint16(b[1]) != uint16(i) {
			t.Fatalf("seq розширення: %v, хотіли %d", b, i)
		}
	}
	over := false
	var acked, sent uint64
	for i := 0; i+50 <= len(a); i += 50 {
		var o bool
		acked, sent, _, o = tl.onFeedback(fbFor(uint16(i), a[i:i+50]))
		over = over || o
	}
	if !over {
		t.Fatal("OVERUSE не спрацював")
	}
	if acked < 1.8e6 || acked > 2.2e6 {
		t.Fatalf("acked %d, очікували ~2 Мбіт/с (стеля)", acked)
	}
	if sent < 3.4e6 {
		t.Fatalf("sent %d, очікували ~4 Мбіт/с", sent)
	}
}

func TestStampWithoutExtensionPassesThrough(t *testing.T) {
	tl := newTwccLeg(nil) // sender nil — розширення не узгоджено
	pkt := &rtp.Packet{Header: rtp.Header{Version: 2}}
	if tl.stamp(pkt, time.Now()) != pkt {
		t.Fatal("без узгодженого transport-cc пакет мусить іти як є")
	}
}

func TestWithDelayCutsToAckedAndHolds(t *testing.T) {
	now := time.Unix(2000, 0)
	c := newBitrateCtl(8_000_000)
	c.lastSent = now.Add(-10 * time.Second)
	// OVERUSE + канал впирається (acked 1.9 < 0.9×7.2): ціль = 0.85×acked.
	c2, send := c.withDelay(true, 1_900_000, 7_200_000, now.Add(-time.Second), now)
	if !send || c2.target != uint64(0.85*1_900_000) || c2.reason != "delay" {
		t.Fatalf("зріз: send=%v ціль %d причина %q", send, c2.target, c2.reason)
	}
	// Енкодер ще шле старе (sent ≫ ціль) — глибше не ріжемо.
	c3, send := c2.withDelay(true, 1_000_000, 4_000_000, now.Add(500*time.Millisecond), now.Add(time.Second))
	if send || c3.target != c2.target {
		t.Fatalf("каскад під час запізнення енкодера: send=%v ціль %d", send, c3.target)
	}
	// Підйом тримається delayUpHold.
	if !c3.delayHeld(now.Add(2*time.Second)) || c3.delayHeld(now.Add(delayUpHold+2*time.Second)) {
		t.Fatal("delayHeld поза вікном delayUpHold")
	}
	c4 := c3
	c4.goodSince = now.Add(-time.Minute)
	if _, up := c4.stepSig(0, 0, congSignals{}, now.Add(1500*time.Millisecond)); up {
		t.Fatal("підйом під час епізоду затримки")
	}
}

func TestWithDelayIgnoresIDRBurstAndProbe(t *testing.T) {
	now := time.Unix(2000, 0)
	c := newBitrateCtl(8_000_000)
	c.target = 1_500_000
	// OVERUSE від сплеску IDR, а доставлено все відправлене: ні зрізу, ні утримання.
	c2, send := c.withDelay(true, 1_480_000, 1_500_000, now.Add(-time.Second), now)
	if send || c2.delayHeld(now) {
		t.Fatalf("IDR-сплеск нижче стелі різав/тримав: send=%v held=%v", send, c2.delayHeld(now))
	}
	// Затор від власної проби — ціль відео не чіпаємо.
	c.probeMuteUntil = now.Add(time.Second)
	if _, send := c.withDelay(true, 1_000_000, 3_000_000, now.Add(-time.Second), now); send {
		t.Fatal("зріз відео за чергу від проби")
	}
}

func TestDelayBWEDefaultOff(t *testing.T) {
	if delayBWEEnabled {
		t.Skip("OO_SCREEN_DELAYBWE=1 у середовищі")
	}
	for _, fb := range videoFeedback() {
		if fb.Type == "transport-cc" {
			t.Fatal("transport-cc оголошено без прапорця — Chrome перестав би слати REMB")
		}
	}
	// Нульовий delayOverAt — нічого не тримає.
	if (bitrateCtl{}).delayHeld(time.Now()) {
		t.Fatal("delayHeld без жодного OVERUSE")
	}
}
