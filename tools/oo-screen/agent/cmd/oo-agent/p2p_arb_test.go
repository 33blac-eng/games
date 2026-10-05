package main

import "testing"

// Енкодер спільний: ціль — мінімум живих ніг і хаба, а не «хто останній».
func TestP2PBitrateArbitration(t *testing.T) {
	a := &p2pAgent{legBps: map[string]uint64{}}
	var got []uint64
	a.onBitrate = func(b uint64) { got = append(got, b) }
	last := func() uint64 { return got[len(got)-1] }

	a.applyBitrate("lossy", 2_000_000, true)
	a.applyBitrate("healthy", 4_000_000, true) // здорова нога не піднімає
	if last() != 2_000_000 {
		t.Fatalf("healthy leg raised target: %v", got)
	}
	a.setHubTarget(1_500_000) // зріз хаба для relay не перебивається
	a.applyBitrate("healthy", 4_200_000, true)
	if last() != 1_500_000 {
		t.Fatalf("hub cut overridden: %v", got)
	}
	a.setHubTarget(0)
	if last() != 2_000_000 {
		t.Fatalf("after hub reset: %v", got)
	}
	a.applyBitrate("lossy", 1_000_000, false) // unchanged — ігнор
	a.forgetLeg("lossy")
	if last() != 4_200_000 {
		t.Fatalf("after lossy leg gone: %v", got)
	}
	n := len(got)
	a.applyBitrate("healthy", 4_200_000, true)
	if len(got) != n {
		t.Fatalf("duplicate target re-applied: %v", got)
	}
}

// Relay-глядачів нема (gate хаба "pause") — ціль хаба не тримає енкодер;
// зʼявився relay-глядач — знову в мінімумі.
func TestP2PBitrateHubIdle(t *testing.T) {
	a := &p2pAgent{legBps: map[string]uint64{}}
	var got []uint64
	a.onBitrate = func(b uint64) { got = append(got, b) }
	last := func() uint64 { return got[len(got)-1] }

	a.applyBitrate("leg", 4_000_000, true)
	a.setHubTarget(1_000_000)
	if last() != 1_000_000 {
		t.Fatalf("hub min: %v", got)
	}
	a.setHubViewers(false)
	if last() != 4_000_000 {
		t.Fatalf("idle hub still constrains: %v", got)
	}
	a.setHubTarget(800_000) // ціль від хаба без глядачів — ігнор у мінімумі
	if last() != 4_000_000 {
		t.Fatalf("idle hub target applied: %v", got)
	}
	n := len(got)
	a.setHubViewers(false) // повтор — без змін
	if len(got) != n {
		t.Fatalf("repeat pause re-applied: %v", got)
	}
	a.setHubViewers(true)
	if last() != 800_000 {
		t.Fatalf("relay viewer back, hub not restored: %v", got)
	}
}
