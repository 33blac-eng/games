package consent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeUI struct {
	answer  chan bool
	asks    atomic.Int32
	shown   atomic.Bool
	showCnt atomic.Int32
	mu      sync.Mutex
	onEnd   func()
}

func newFake() *fakeUI { return &fakeUI{answer: make(chan bool, 4)} }
func (f *fakeUI) Ask(ctx context.Context, _ string) bool {
	f.asks.Add(1)
	select {
	case a := <-f.answer:
		return a
	case <-ctx.Done():
		return false
	}
}
func (f *fakeUI) ShowIndicator(onEnd func()) {
	f.mu.Lock()
	f.onEnd = onEnd
	f.mu.Unlock()
	f.shown.Store(true)
	f.showCnt.Add(1)
}
func (f *fakeUI) HideIndicator() { f.shown.Store(false) }

type sink struct{ paused atomic.Bool }

func (s *sink) gate(resume bool) { s.paused.Store(!resume) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func setup(p Policy) (*Gate, *fakeUI, *sink, func(bool)) {
	ui := newFake()
	s := &sink{}
	s.paused.Store(true)
	g := New(Config{Policy: p, UI: ui, Timeout: 200 * time.Millisecond, Cooldown: time.Hour})
	return g, ui, s, g.Wrap(s.gate)
}

func TestOffIsTransparent(t *testing.T) {
	g := New(Config{Policy: Off})
	if g != nil || !g.Allowed() || g.Required() {
		t.Fatal("Off must be nil/transparent")
	}
	called := false
	g.Wrap(func(bool) { called = true })(true)
	if !called {
		t.Fatal("wrap not transparent")
	}
}

func TestViewerCannotBypassWithoutAnswer(t *testing.T) {
	g, ui, s, sig := setup(AlwaysAsk)
	// Шторм resume від хаба/глядача — жоден не відчиняє гейт, діалог один.
	for i := 0; i < 100; i++ {
		sig(true)
	}
	time.Sleep(20 * time.Millisecond)
	if !s.paused.Load() || g.Allowed() || ui.shown.Load() {
		t.Fatal("gate opened without consent")
	}
	if n := ui.asks.Load(); n != 1 {
		t.Fatalf("asks=%d, want 1", n)
	}
	// таймаут = відмова, далі cooldown
	waitFor(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.pending == nil })
	sig(true)
	time.Sleep(20 * time.Millisecond)
	if !s.paused.Load() || g.Allowed() || ui.asks.Load() != 1 {
		t.Fatal("after timeout: must stay closed and not re-prompt during cooldown")
	}
}

func TestGrantShowsIndicatorAndEndStops(t *testing.T) {
	g, ui, s, sig := setup(AlwaysAsk)
	ui.answer <- true
	sig(true)
	waitFor(t, func() bool { return !s.paused.Load() })
	if !g.Allowed() || !ui.shown.Load() {
		t.Fatal("granted but no indicator / not allowed")
	}
	// кнопка «Завершити»
	ui.mu.Lock()
	end := ui.onEnd
	ui.mu.Unlock()
	end()
	if !s.paused.Load() || g.Allowed() || ui.shown.Load() {
		t.Fatal("end-session must pause, disallow, hide indicator")
	}
	sig(true) // хаб знову просить — cooldown
	time.Sleep(20 * time.Millisecond)
	if !s.paused.Load() || ui.asks.Load() != 1 {
		t.Fatal("resume during cooldown after End must be ignored")
	}
}

func TestDenyKeepsPaused(t *testing.T) {
	g, ui, s, sig := setup(AlwaysAsk)
	ui.answer <- false
	sig(true)
	waitFor(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.pending == nil })
	if !s.paused.Load() || g.Allowed() || ui.shown.Load() {
		t.Fatal("deny must keep paused")
	}
}

func TestViewerLeavesDuringPromptThenLateYes(t *testing.T) {
	g, ui, s, sig := setup(AlwaysAsk)
	sig(true)
	waitFor(t, func() bool { return ui.asks.Load() == 1 })
	sig(false) // глядач пішов — діалог скасовано
	ui.answer <- true
	time.Sleep(30 * time.Millisecond)
	if !s.paused.Load() || g.Allowed() {
		t.Fatal("stale yes must not open gate")
	}
}

func TestPauseHidesIndicatorAndNextViewerAsksAgain(t *testing.T) {
	g, ui, s, sig := setup(AlwaysAsk)
	ui.answer <- true
	sig(true)
	waitFor(t, func() bool { return g.Allowed() })
	sig(false)
	if ui.shown.Load() || g.Allowed() || !s.paused.Load() {
		t.Fatal("pause must revoke")
	}
	ui.answer <- true
	sig(true)
	waitFor(t, func() bool { return g.Allowed() })
	if ui.asks.Load() != 2 {
		t.Fatalf("asks=%d, want fresh prompt per session", ui.asks.Load())
	}
}

func TestResetOnReconnect(t *testing.T) {
	g, ui, s, sig := setup(Unattended)
	sig(true)
	waitFor(t, func() bool { return g.Allowed() })
	if ui.asks.Load() != 0 || !ui.shown.Load() {
		t.Fatal("unattended: no prompt, but indicator must be visible")
	}
	g.Reset()
	if g.Allowed() || !s.paused.Load() || ui.shown.Load() {
		t.Fatal("reset must revoke")
	}
}

func TestAskIfPresent(t *testing.T) {
	present := atomic.Bool{}
	ui := newFake()
	s := &sink{}
	g := New(Config{Policy: AskIfUserPresent, UI: ui, UserPresent: present.Load, Timeout: time.Second})
	sig := g.Wrap(s.gate)
	sig(true) // locked → без запиту
	waitFor(t, func() bool { return g.Allowed() })
	if ui.asks.Load() != 0 || !ui.shown.Load() {
		t.Fatal("absent user: no prompt, indicator on")
	}
	sig(false)
	present.Store(true)
	ui.answer <- true
	sig(true)
	waitFor(t, func() bool { return g.Allowed() })
	if ui.asks.Load() != 1 {
		t.Fatal("present user must be asked")
	}
}

func TestParsePolicy(t *testing.T) {
	for in, want := range map[string]Policy{"": Off, "always-ask": AlwaysAsk, "ask-if-user-logged-in": AskIfUserPresent, "unattended-allowed-by-admin": Unattended} {
		if p, err := ParsePolicy(in); err != nil || p != want {
			t.Errorf("%q -> %v %v", in, p, err)
		}
	}
	if _, err := ParsePolicy("yes-please"); err == nil {
		t.Error("unknown policy must error")
	}
}

// blockingShowUI — ShowIndicator висить, доки тест не відпустить: ловимо
// вікно між «згода є» і «індикатор видно».
type blockingShowUI struct {
	*fakeUI
	entered chan struct{}
	release chan struct{}
}

func (b *blockingShowUI) ShowIndicator(onEnd func()) {
	close(b.entered)
	<-b.release
	b.fakeUI.ShowIndicator(onEnd)
}

// Allowed() (ввід/кадри) не може стати true, поки індикатор ще не показано.
func TestAllowedOnlyAfterIndicatorShown(t *testing.T) {
	ui := &blockingShowUI{fakeUI: newFake(), entered: make(chan struct{}), release: make(chan struct{})}
	s := &sink{}
	s.paused.Store(true)
	g := New(Config{Policy: Unattended, UI: ui, Timeout: time.Second, Cooldown: time.Hour})
	sig := g.Wrap(s.gate)
	sig(true)
	select {
	case <-ui.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("ShowIndicator never called")
	}
	if g.Allowed() || !s.paused.Load() {
		t.Fatal("granted before indicator is visible")
	}
	close(ui.release)
	waitFor(t, func() bool { return g.Allowed() })
	if !ui.shown.Load() || s.paused.Load() {
		t.Fatal("after grant: indicator and frames must be on")
	}
}
