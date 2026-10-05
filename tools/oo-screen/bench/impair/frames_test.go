package impair

import (
	"testing"
	"time"
)

// синтетичний потік: кадр = 3 пакети, IDR кожні 10 кадрів, 60 к/с.
func synth(nFrames int, lose func(frame, pkt int) bool, late func(frame int) time.Duration) []Pkt {
	var out []Pkt
	var seq uint64 = 1 << 16
	for f := 0; f < nFrames; f++ {
		send := t0.Add(time.Duration(f) * 16667 * time.Microsecond)
		for k := 0; k < 3; k++ {
			p := Pkt{Ext: seq, TS: uint32(f * tsStep), Marker: k == 2, IDR: f%10 == 0 && k == 0, Start: k == 0,
				At: send.Add(5 * time.Millisecond)}
			if late != nil {
				p.At = p.At.Add(late(f))
			}
			seq++
			if lose != nil && lose(f, k) {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

func TestFramesClean(t *testing.T) {
	fr := Frames(synth(100, nil, nil))
	s := Summarize(fr, t0, t0.Add(100*16667*time.Microsecond))
	if s.Expected != 100 || s.Complete != 100 || s.Decodable != 100 || s.Freezes != 0 {
		t.Fatalf("%+v", s)
	}
	if s.LatP95Ms > 1 {
		t.Fatalf("латентність над базою має бути ~0: %+v", s)
	}
}

func TestFramesLossBreaksChainUntilIDR(t *testing.T) {
	// губимо середній пакет кадру 13: кадри 13..19 не декодовні, 20 (IDR) — так.
	fr := Frames(synth(40, func(f, k int) bool { return f == 13 && k == 1 }, nil))
	s := Summarize(fr, t0, t0.Add(40*16667*time.Microsecond))
	if s.Complete != 39 || s.Decodable != 33 {
		t.Fatalf("%+v", s)
	}
	if s.Freezes != 0 { // 7 кадрів = 117 мс < 200
		t.Fatalf("%+v", s)
	}
}

func TestFramesWholeFramesLostAndFreeze(t *testing.T) {
	// кадри 21..35 зникли повністю (і з ними нічого до IDR 40): розрив 20 кадрів = 333 мс
	fr := Frames(synth(60, func(f, k int) bool { return f >= 21 && f <= 35 }, nil))
	s := Summarize(fr, t0, t0.Add(60*16667*time.Microsecond))
	if s.Expected != 60 || s.Complete != 45 || s.Decodable != 41 {
		t.Fatalf("%+v", s)
	}
	if s.Freezes != 1 || s.FreezeMs < 300 || s.FreezeMs > 370 {
		t.Fatalf("%+v", s)
	}
}

func TestFramesLostMarkerAndStart(t *testing.T) {
	// губимо останній пакет кадру 5 і перший кадру 7
	fr := Frames(synth(10, func(f, k int) bool { return (f == 5 && k == 2) || (f == 7 && k == 0) }, nil))
	if fr[5].Complete || fr[7].Complete || !fr[6].Complete {
		t.Fatalf("%+v", fr[5:8])
	}
}

func TestFramesLateRetransmitAddsLatency(t *testing.T) {
	fr := Frames(synth(30, nil, func(f int) time.Duration {
		if f == 12 {
			return 100 * time.Millisecond
		}
		return 0
	}))
	s := Summarize(fr, t0, t0.Add(30*16667*time.Microsecond))
	if s.Decodable != 30 || s.LatP95Ms < 50 {
		t.Fatalf("%+v", s)
	}
}

func TestIsIDRPayload(t *testing.T) {
	if !IsIDRPayload([]byte{0x65, 0}) || IsIDRPayload([]byte{0x41, 0}) {
		t.Fatal("single")
	}
	if !IsIDRPayload([]byte{0x7c, 0x85}) || IsIDRPayload([]byte{0x7c, 0x81}) {
		t.Fatal("fu-a")
	}
	stap := []byte{0x78, 0, 2, 0x67, 0, 0, 2, 0x68, 0, 0, 2, 0x65, 0}
	if !IsIDRPayload(stap) {
		t.Fatal("stap-a")
	}
}
