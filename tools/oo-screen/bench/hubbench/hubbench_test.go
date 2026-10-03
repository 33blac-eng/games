package main

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// Annex-B ключового набору: SPS, PPS, великий IDR (буде FU-A), потім P-кадр.
func keyAU() []byte {
	b := []byte{0, 0, 0, 1, 0x67, 0x4d, 0x00, 0x2a, 0xaa, 0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80, 0, 0, 0, 1, 0x65}
	for i := 0; i < 5000; i++ {
		b = append(b, byte(i%200+1))
	}
	return b
}

func pAU() []byte {
	b := []byte{0, 0, 0, 1, 0x41}
	for i := 0; i < 3000; i++ {
		b = append(b, byte(i%200+1))
	}
	return b
}

func packetize(t *testing.T, aus ...[]byte) []*rtp.Packet {
	t.Helper()
	pk := rtp.NewPacketizer(1200, 102, 1, &codecs.H264Payloader{}, rtp.NewFixedSequencer(100), 90000)
	var out []*rtp.Packet
	for _, a := range aus {
		out = append(out, pk.Packetize(a, 1500)...)
	}
	return out
}

func TestIDRTrackerCompletes(t *testing.T) {
	pkts := packetize(t, pAU(), keyAU(), pAU())
	var tr idrTracker
	doneAt := -1
	for i, p := range pkts {
		if tr.feed(p.SequenceNumber, p.Timestamp, p.Marker, p.Payload) {
			doneAt = i
		}
	}
	if doneAt < 0 {
		t.Fatal("IDR never completed")
	}
	// Завершитись мусить на marker-пакеті ключового AU, не раніше і не на P-кадрі.
	if !pkts[doneAt].Marker || pkts[doneAt].Timestamp != pkts[len(packetize(t, pAU()))].Timestamp {
		t.Fatalf("completed on wrong packet %d", doneAt)
	}
}

func TestIDRTrackerGapResets(t *testing.T) {
	pkts := packetize(t, keyAU(), pAU())
	var tr idrTracker
	for i, p := range pkts {
		if i == 2 { // губимо середину IDR
			continue
		}
		if tr.feed(p.SequenceNumber, p.Timestamp, p.Marker, p.Payload) {
			t.Fatal("IDR with a hole must not count as decodable")
		}
	}
}

func TestNalTypes(t *testing.T) {
	pkts := packetize(t, keyAU())
	ty, _ := nalTypes(pkts[0].Payload)
	// pion шле SPS+PPS разом у STAP-A.
	if len(ty) != 2 || ty[0] != 7 || ty[1] != 8 {
		t.Fatalf("first packet types %v, want STAP-A [7 8]", ty)
	}
	ty, start := nalTypes(pkts[1].Payload)
	if len(ty) != 1 || ty[0] != 5 || !start {
		t.Fatalf("second packet %v start=%v, want FU-A IDR start", ty, start)
	}
	if !isKeyStart(pkts[0].Payload) || isKeyStart(pkts[1].Payload) {
		t.Fatal("isKeyStart")
	}
}

func TestParseStatCPU(t *testing.T) {
	stat := []byte("1234 (hub (x) y) S 1 2 3 4 5 6 7 8 9 10 250 30 0 0 20 0 9 0")
	u, s, err := parseStatCPU(stat)
	if err != nil || u != 250 || s != 30 {
		t.Fatalf("got %d %d %v", u, s, err)
	}
	a := procSample{at: time.Unix(0, 0), cpuTick: 100}
	b := procSample{at: time.Unix(2, 0), cpuTick: 400}
	if got := cpuPct(a, b); got != 150 {
		t.Fatalf("cpuPct %v", got)
	}
}

func TestParseGoroutineTotal(t *testing.T) {
	if n := parseGoroutineTotal([]byte("goroutine profile: total 1234\n1 @ 0x")); n != 1234 {
		t.Fatal(n)
	}
	if n := parseGoroutineTotal([]byte("garbage")); n != -1 {
		t.Fatal(n)
	}
}

func TestPercentileAndERP(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3}
	if percentile(xs, 0) != 1 || percentile(xs, 50) != 3 || percentile(xs, 100) != 5 {
		t.Fatal("percentile")
	}
	if erpConsumeNode("n007~123") != "n007" || erpConsumeNode("plain") != "plain" {
		t.Fatal("erpConsumeNode")
	}
}
