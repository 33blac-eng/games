package main

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// R5: нода, з якої пішли агент і глядачі, зникає з реєстру після grace, а не
// живе вічно (churnsoak: реєстр ріс з кожним новим node_id).
func TestReapIdleNodeAfterGrace(t *testing.T) {
	r := newRegistry()
	ns := r.getOrCreate("n1")
	relayFor(ns, relayConfig{label: "reap-test"})
	t.Cleanup(func() { forgetRelays(ns) })

	rp := newReaper()
	now := time.Now()
	grace := time.Minute
	if n := rp.sweep(r, now, grace); n != 0 {
		t.Fatalf("перший прохід лише помічає простій, зняв %d", n)
	}
	if n := rp.sweep(r, now.Add(30*time.Second), grace); n != 0 {
		t.Fatalf("до grace зняв %d", n)
	}
	if n := rp.sweep(r, now.Add(61*time.Second), grace); n != 1 {
		t.Fatalf("після grace мав зняти 1, зняв %d", n)
	}
	if r.get("n1") != nil {
		t.Fatal("нода лишилась у реєстрі")
	}
	if _, ok := relays.Load(relayKey{ns, "reap-test"}); ok {
		t.Fatal("ретранслятор знятої ноди лишився в relays")
	}
	if len(rp.idleSince) != 0 {
		t.Fatalf("жнець сам тримає %d записів", len(rp.idleSince))
	}
}

// Нода з агентом або глядачем не чіпається; свіжий offer (touched) відкладає
// прибирання — агент між answer і Connected виглядає простою.
func TestReapKeepsBusyAndFreshlyTouched(t *testing.T) {
	r := newRegistry()
	busy := r.getOrCreate("busy")
	busy.mu.Lock()
	busy.viewers = map[*webrtc.PeerConnection]*viewerLeg{nil: {}}
	busy.mu.Unlock()
	fresh := r.getOrCreate("fresh")

	rp := newReaper()
	t0 := time.Now()
	grace := time.Minute
	rp.sweep(r, t0, grace)
	// offer до "fresh" за мить до кінця grace
	fresh.touched.Store(t0.Add(50 * time.Second).UnixNano())
	if n := rp.sweep(r, t0.Add(70*time.Second), grace); n != 0 {
		t.Fatalf("зняв %d, а мав 0", n)
	}
	if r.get("busy") == nil || r.get("fresh") == nil {
		t.Fatal("зайняту або щойно торкнуту ноду знято")
	}
	if n := rp.sweep(r, t0.Add(111*time.Second), grace); n != 1 || r.get("fresh") != nil {
		t.Fatalf("fresh мав зникнути після grace від touched (зняв %d)", n)
	}
	if r.get("busy") == nil {
		t.Fatal("ноду з глядачем знято")
	}
	// Повернення до справ скидає лічильник простою.
	busy.mu.Lock()
	busy.viewers = nil
	busy.mu.Unlock()
	if n := rp.sweep(r, t0.Add(112*time.Second), grace); n != 0 {
		t.Fatalf("щойно звільнена нода знята одразу (%d)", n)
	}
}
