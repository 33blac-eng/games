package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMultimonChildrenSingleMonitorUnchanged(t *testing.T) {
	if got := multimonChildren(1, 0, 3); got != nil {
		t.Fatalf("один монітор: діти %v, want жодної", got)
	}
	if got := multimonChildren(0, 0, 3); got != nil {
		t.Fatalf("енумерація впала (0): %v", got)
	}
	if got := multimonChildren(4, 0, 1); got != nil {
		t.Fatalf("max=1: %v", got)
	}
}

func TestMultimonChildrenSelection(t *testing.T) {
	cases := []struct {
		n, active, max int
		want           string
	}{
		{2, 0, 3, "[1]"},
		{3, 0, 3, "[1 2]"},
		{4, 0, 3, "[1 2]"}, // стеля: основний + 2
		{4, 0, 8, "[1 2 3]"},
		{3, 1, 3, "[2]"}, // основний на 1 — 0 зарезервований, дитина лише 2
		{40, 0, 99, "[1 2 3 4 5 6 7 8 9 10 11 12 13 14 15]"},
	}
	for _, c := range cases {
		if got := fmt.Sprint(multimonChildren(c.n, c.active, c.max)); got != c.want {
			t.Errorf("n=%d active=%d max=%d: %s, want %s", c.n, c.active, c.max, got, c.want)
		}
	}
}

func TestMultimonChildArgs(t *testing.T) {
	parent := []string{
		"-transport", "webrtc", "-hub=http://h/offer/agent", "-node", "pc-7",
		"-output", "0", "-token=SECRET", "-token-file", `C:\t.txt`, "-audio", "-input=true",
		"--cursor-layer", "-multimon", "-multimon-max=4", "-fps", "30", "-log", `C:\oo\agent.log`,
	}
	bools := map[string]bool{"audio": true, "input": true, "cursor-layer": true, "multimon": true}
	got := multimonChildArgs(parent, 2, "pc-7", `C:\oo\agent.log`, func(n string) bool { return bools[n] })
	s := strings.Join(got, " ")
	for _, bad := range []string{"SECRET", `C:\t.txt`, "-audio", "-input", "cursor-layer", "-multimon ", "multimon-max", "-node pc-7", "-output 0", `-log C:\oo\agent.log`} {
		if strings.Contains(s+" ", bad) {
			t.Errorf("дитина успадкувала %q: %s", bad, s)
		}
	}
	for _, want := range []string{"-transport webrtc", "-hub=http://h/offer/agent", "-fps 30", "-multimon-child=2", "-output=2", "-node=pc-7#m2", `-log=C:\oo\agent.m2.log`} {
		if !strings.Contains(s, want) {
			t.Errorf("немає %q: %s", want, s)
		}
	}
}

func TestChildLogPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\oo\agent.log`: `C:\oo\agent.m1.log`,
		`C:\o.o\agent`:    `C:\o.o\agent.m1`,
		"a.log":           "a.m1.log",
	} {
		if got := childLogPath(in, 1); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestMultimonChildEnv(t *testing.T) {
	env := multimonChildEnv([]string{"PATH=x", "OO_AGENT_TOKEN=old", "OO_SCREEN_AUDIO=1", "oo_screen_input=1", "OO_SCREEN_MULTIMON=1"}, "tok")
	s := strings.Join(env, ";")
	if s != "PATH=x;OO_AGENT_TOKEN=tok;OO_SCREEN_AUDIO=0;OO_SCREEN_INPUT=0" {
		t.Fatalf("env: %s", s)
	}
}

// TestSuperviseChildRestartsAndStops — дитина, що падає, перезапускається;
// скасований ctx зупиняє нагляд.
func TestSuperviseChildRestartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var starts atomic.Int32
	done := make(chan struct{})
	go func() {
		superviseChild(ctx, 1, func() (func() error, error) {
			if starts.Add(1) == 1 {
				return nil, errors.New("boom")
			}
			return func() error { <-ctx.Done(); return ctx.Err() }, nil
		})
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for starts.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if starts.Load() != 2 {
		t.Fatalf("перезапусків %d, want 2", starts.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("нагляд не зупинився після cancel")
	}
}

func TestWatchParentStdin(t *testing.T) {
	var stopped atomic.Bool
	watchParentStdin(strings.NewReader("x"), func() { stopped.Store(true) })
	if !stopped.Load() {
		t.Fatal("EOF батька не зупинив дитину")
	}
}
