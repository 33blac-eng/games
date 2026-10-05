//go:build windows

package capture

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Нерухомий екран: AcquireNextFrame віддає лише WAIT_TIMEOUT, тобто сесія, що
// стартувала на такому екрані, не отримує ПЕРШОГО кадру взагалі — глядач падає
// на first-frame-timeout, так і не побачивши картинки. GDIFrame бере поточний
// робочий стіл незалежно від того, змінювався він чи ні.
//
// Тест шукає момент СПРАВЖНЬОЇ нерухомості (дуплікація двічі поспіль віддала
// таймаут) і вимагає від GDIFrame кадру саме там. Другий таймаут поспіль — це і
// є негативний контроль у самому тесті: у цю ж мить DXGI-шлях порожній.
func TestGDIFrameOnStaticScreen(t *testing.T) {
	const (
		idleProbe = 300 * time.Millisecond
		attempts  = 40
		wantIdle  = 2
	)

	c, err := New(0)
	if err != nil {
		if errors.Is(err, ErrNotAvailable) {
			t.Skipf("Desktop Duplication тут недоступний: %v", err)
		}
		t.Fatalf("New(0): %v", err)
	}
	defer c.Close()

	w, h := c.Size()
	idle := 0
	for i := 0; i < attempts && idle < wantIdle; i++ {
		if !timedOut(t, c, idleProbe) {
			continue // екран рухається — ще не той момент
		}
		if !timedOut(t, c, idleProbe) {
			continue // рухнувся між пробами: нерухомості не спіймали
		}
		idle++

		start := time.Now()
		f, err := c.GDIFrame()
		if err != nil {
			t.Fatalf("нерухомий екран (%d поспіль таймаутів по %v): GDIFrame: %v",
				idle, idleProbe, err)
		}
		if f.Width != w || f.Height != h {
			t.Fatalf("GDIFrame віддав %dx%d; дуплікація каже %dx%d", f.Width, f.Height, w, h)
		}
		if len(f.Y) == 0 {
			t.Fatalf("GDIFrame віддав кадр без люма-площини (readback увімкнено)")
		}
		if !anyNonZero(f.Y) {
			t.Fatalf("GDIFrame віддав порожню (суцільно нульову) люму — це не робочий стіл")
		}
		t.Logf("нерухомий екран: GDIFrame віддав %dx%d за %v",
			f.Width, f.Height, time.Since(start).Round(time.Millisecond))
	}

	if idle < wantIdle {
		t.Skipf("екран не завмер двічі поспіль за %d спроб по %v — перевіряти нічого",
			attempts, idleProbe)
	}
}

func timedOut(t *testing.T, c *Capturer, d time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, err := c.NextFrame(ctx)
	if err == nil {
		return false
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NextFrame: %v", err)
	}
	return true
}

func anyNonZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}
