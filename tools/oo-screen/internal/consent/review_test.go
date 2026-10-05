package consent

import (
	"testing"
	"time"
)

// Ревʼю S3 #1: згода глядача A не покриває глядача B, що приєднався пізніше.
func TestSecondViewerRequiresNewConsent(t *testing.T) {
	g, ui, s, sig := setup(AlwaysAsk)
	ui.answer <- true
	sig(true)
	waitFor(t, func() bool { return g.Allowed() })
	g.ViewerJoin() // hub: ще одна нога; resume на 1->2 hub не шле
	if g.Allowed() || !s.paused.Load() {
		t.Fatal("new viewer must close the gate until a fresh answer")
	}
	waitFor(t, func() bool { return ui.asks.Load() == 2 })
	ui.answer <- true
	waitFor(t, func() bool { return g.Allowed() && !s.paused.Load() })
}

func TestSecondViewerDeniedStaysClosed(t *testing.T) {
	g, ui, s, sig := setup(AlwaysAsk)
	ui.answer <- true
	sig(true)
	waitFor(t, func() bool { return g.Allowed() })
	ui.answer <- false
	g.ViewerJoin()
	waitFor(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.pending == nil && ui.asks.Load() == 2
	})
	if g.Allowed() || !s.paused.Load() {
		t.Fatal("deny for second viewer must keep everything paused")
	}
}

func TestViewerJoinUnattendedNoPrompt(t *testing.T) {
	g, ui, _, sig := setup(Unattended)
	sig(true)
	waitFor(t, func() bool { return g.Allowed() })
	g.ViewerJoin()
	if !g.Allowed() || ui.asks.Load() != 0 {
		t.Fatal("unattended: join must not prompt or revoke")
	}
	var nilGate *Gate
	nilGate.ViewerJoin() // policy off — no-op, no panic
}

// Ревʼю S3 #2: цикли join/leave під час відкритого запиту не плодять вікон.
func TestResumePauseCyclesSingleDialog(t *testing.T) {
	g, ui, s, sig := setup(AlwaysAsk)
	for i := 0; i < 100; i++ {
		sig(true)
		sig(false)
	}
	time.Sleep(20 * time.Millisecond)
	if n := ui.asks.Load(); n != 1 {
		t.Fatalf("asks=%d after 100 resume/pause cycles, want 1", n)
	}
	sig(true) // глядач повернувся — той самий діалог
	ui.answer <- true
	waitFor(t, func() bool { return g.Allowed() && !s.paused.Load() })
	if n := ui.asks.Load(); n != 1 {
		t.Fatalf("asks=%d, want 1", n)
	}
}
