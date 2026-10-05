package main

import (
	"testing"
	"time"
)

func TestGopReplayBudget(t *testing.T) {
	old, oldMax := gopReplaySpan, gopReplayMaxBytes
	t.Cleanup(func() { gopReplaySpan, gopReplayMaxBytes = old, oldMax })
	gopReplaySpan, gopReplayMaxBytes = 2*time.Second, 3<<20

	if got := gopReplayBudget(8_000_000); got != 2_500_000 {
		t.Fatalf("8M x 2s x 1.25 = %d, want 2500000", got)
	}
	if got := gopReplayBudget(100_000_000); got != 3<<20 {
		t.Fatalf("hard cap: %d", got)
	}
	if got := gopReplayBudget(500_000); got != gopMinBytes {
		t.Fatalf("floor: %d", got)
	}
	if got := gopReplayBudget(0); got != 3<<20 {
		t.Fatalf("unknown bitrate: %d", got)
	}
	gopReplayMaxBytes = 1 << 40
	if got := gopReplayBudget(0); got != gopMaxBytes {
		t.Fatalf("env above store cap: %d", got)
	}
	gopReplayMaxBytes = 0 // disables replay entirely
	if got := gopReplayBudget(8_000_000); got != 0 {
		t.Fatalf("zero cap: %d", got)
	}
}

// Small tail -> replayed (fast join); big self-contained tail -> tooBig, no
// packets, so the leg is not primed and the hub requests a keyframe.
func TestGopReplayForBudget(t *testing.T) {
	quietNDJSON(t, nil)
	oldSpan, oldMax, oldStore := gopReplaySpan, gopReplayMaxBytes, gopMaxSpan
	t.Cleanup(func() { gopReplaySpan, gopReplayMaxBytes, gopMaxSpan = oldSpan, oldMax, oldStore })
	gopReplaySpan, gopReplayMaxBytes, gopMaxSpan = 2*time.Second, 3<<20, 12*time.Second

	var g gopCache
	g.setBitrate(8_000_000)
	n := gopStream(&g, 8_000_000, time.Second, 1200)
	if pkts, _, big := g.replayFor(); big || len(pkts) != n {
		t.Fatalf("1 s tail: big=%v pkts=%d want %d", big, len(pkts), n)
	}

	g = gopCache{}
	g.setBitrate(8_000_000)
	gopStream(&g, 8_000_000, 8*time.Second, 1200)
	pkts, b, big := g.replayFor()
	if !big || pkts != nil || b <= gopReplayBudget(8_000_000) {
		t.Fatalf("8 s tail: big=%v pkts=%d bytes=%d", big, len(pkts), b)
	}
	if g.replay() == nil {
		t.Fatal("cache itself must stay intact (only replay is refused)")
	}
}

// Triggers inside the debounce window are not lost: exactly one trailing
// request fires at the end of the window, however many triggers there were.
func TestRequestKeyframeTrailing(t *testing.T) {
	ns := &nodeSession{nodeID: "trail"}
	requestKeyframe(ns) // sends (no agent: just logs), starts window
	ns.mu.Lock()
	first := ns.lastKeyframeReq
	ns.mu.Unlock()
	for range 5 {
		requestKeyframe(ns)
	}
	ns.mu.Lock()
	if !ns.kfTrailing || !ns.lastKeyframeReq.Equal(first) {
		ns.mu.Unlock()
		t.Fatal("debounced triggers must schedule one trailing request and not send now")
	}
	ns.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ns.mu.Lock()
		sent, pending := ns.lastKeyframeReq, ns.kfTrailing
		ns.mu.Unlock()
		if !sent.Equal(first) && !pending {
			if d := sent.Sub(first); d < keyframeDebnc {
				t.Fatalf("trailing request %v after first, want >= %v", d, keyframeDebnc)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("trailing keyframe request never fired")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
