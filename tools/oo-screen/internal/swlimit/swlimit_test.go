package swlimit

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Unix(1000, 0)

var full = Scale{1, 1}

// run feeds frames of duration enc at the policy's current FPS cadence for d.
func run(p *Policy, now *time.Time, enc, d time.Duration) (changes []Decision) {
	end := now.Add(d)
	for now.Before(end) {
		if dec := p.Observe(enc, *now); dec.Changed {
			changes = append(changes, dec)
		}
		*now = now.Add(time.Second / time.Duration(p.FPS()))
	}
	return
}

func TestLadderTrimmed(t *testing.T) {
	if got := New(Config{MaxFPS: 30, Cores: 4}).c.FPSSteps; !reflect.DeepEqual(got, []int{30, 24, 20, 15, 12, 10}) {
		t.Fatalf("steps=%v", got)
	}
	if got := New(Config{MaxFPS: 25, MinFPS: 8}).c.FPSSteps; !reflect.DeepEqual(got, []int{25, 24, 20, 15, 12, 10, 8}) {
		t.Fatalf("steps=%v", got)
	}
	if got := New(Config{MaxFPS: 5}).c.FPSSteps; !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("steps=%v", got)
	}
}

func TestFastEncoderNoChange(t *testing.T) {
	p := New(Config{MaxFPS: 30, Cores: 4})
	now := t0
	if ch := run(p, &now, 5*time.Millisecond, 30*time.Second); len(ch) != 0 {
		t.Fatalf("unexpected changes %v", ch)
	}
	if p.FrameGap() != 0 {
		t.Fatal("gap must be 0 at max fps")
	}
}

func TestFPSBeforeScale(t *testing.T) {
	// 60 ms/frame on 2 cores: even 10 fps is 0.6 > 0.5, so the FPS ladder is
	// exhausted first and only then resolution goes.
	p := New(Config{MaxFPS: 30, Cores: 2})
	now := t0
	ch := run(p, &now, 60*time.Millisecond, 60*time.Second)
	if len(ch) < 6 {
		t.Fatalf("changes=%v", ch)
	}
	for i := 0; i < 5; i++ {
		if ch[i].Scale != full {
			t.Fatalf("scale changed before fps floor: %v", ch[:i+1])
		}
	}
	if ch[4].FPS != 10 || ch[5].Scale == full {
		t.Fatalf("expected fps floor then scale: %v", ch)
	}
	if p.FrameGap() != 100*time.Millisecond {
		t.Fatalf("gap=%v", p.FrameGap())
	}
}

func TestDownHoldDebounces(t *testing.T) {
	p := New(Config{MaxFPS: 30, Cores: 8})
	now := t0
	if ch := run(p, &now, 50*time.Millisecond, time.Second); len(ch) != 0 {
		t.Fatalf("1s spike stepped down: %v", ch)
	}
	if ch := run(p, &now, 2*time.Millisecond, 5*time.Second); len(ch) != 0 {
		t.Fatalf("changes %v", ch)
	}
}

func TestHysteresisNoFlapAndRecovery(t *testing.T) {
	p := New(Config{MaxFPS: 30, Cores: 4}) // High 0.7, Low 0.525
	now := t0
	// 30 ms/frame: 30fps->0.9, 24->0.72, 20->0.6 fits. Settles at 20.
	run(p, &now, 30*time.Millisecond, 60*time.Second)
	if p.FPS() != 20 {
		t.Fatalf("fps=%d", p.FPS())
	}
	// At 20 the prediction for 24 is 0.72 >= Low: must not flap up.
	if ch := run(p, &now, 30*time.Millisecond, 60*time.Second); len(ch) != 0 {
		t.Fatalf("flapped: %v", ch)
	}
	ch := run(p, &now, 5*time.Millisecond, 120*time.Second)
	if p.FPS() != 30 || len(ch) != 2 {
		t.Fatalf("not recovered fps=%d changes=%v", p.FPS(), ch)
	}
}

func TestRecoverScaleBeforeFPS(t *testing.T) {
	p := New(Config{MaxFPS: 30, Cores: 2})
	now := t0
	run(p, &now, 80*time.Millisecond, 60*time.Second)
	if p.Scale() == full || p.FPS() != 10 {
		t.Fatalf("expected floor+scale step, got %d %v", p.FPS(), p.Scale())
	}
	ch := run(p, &now, 3*time.Millisecond, 200*time.Second)
	for _, d := range ch {
		if d.FPS != 10 && d.Scale != full {
			t.Fatalf("fps rose before scale restored: %v", ch)
		}
	}
	if p.FPS() != 30 || p.Scale() != full {
		t.Fatalf("not fully recovered: %d %v", p.FPS(), p.Scale())
	}
}

func TestBudgetShareByCores(t *testing.T) {
	if budgetShare(2) >= budgetShare(4) || budgetShare(4) >= budgetShare(16) {
		t.Fatal("budget must grow with cores")
	}
}
