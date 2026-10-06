package main

import (
	"errors"
	"testing"
	"time"
)

func TestOutputRectCache(t *testing.T) {
	calls := 0
	fail := false
	rect := func(idx int) (int, int, int, int, error) {
		calls++
		if fail {
			return 0, 0, 0, 0, errors.New("gone")
		}
		return 1920 * idx, 0, 1920, 1080, nil
	}
	var c outputRectCache
	t0 := time.Unix(10, 0)
	if _, ok := c.geometry(t0, nil, rect); ok || calls != 0 {
		t.Fatal("no frame yet: no geometry, no query")
	}
	fg := &frameGeom{out: 1, w: 1920, h: 1080}
	g, ok := c.geometry(t0, fg, rect)
	if !ok || g.Left != 1920 || g.FrameW != 1920 || calls != 1 {
		t.Fatalf("%+v %v %d", g, ok, calls)
	}
	for i := 0; i < 100; i++ { // 8 ms ticks within the TTL: cached
		c.geometry(t0.Add(time.Duration(i)*8*time.Millisecond), fg, rect)
	}
	if calls != 1 {
		t.Fatalf("re-queried %d times within TTL", calls)
	}
	c.geometry(t0, &frameGeom{out: 0, w: 1920, h: 1080}, rect) // output switched
	if calls != 2 {
		t.Fatal("output change must re-query")
	}
	fail = true
	if _, ok := c.geometry(t0.Add(2*time.Second), fg, rect); ok {
		t.Fatal("failed query must yield")
	}
}

// F9 дефолт ON + вимикачі: без прапорця шар дозволено; env
// OO_SCREEN_CURSOR_LAYER=0 вимикає; явний прапорець перекриває env.
func TestResolveCursorLayerDefaultOn(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "OO_SCREEN_CURSOR_LAYER" {
				return v
			}
			return ""
		}
	}
	cases := []struct {
		set, val bool
		env      string
		want     bool
	}{
		{false, false, "", true},
		{false, false, "1", true},
		{false, false, "0", false},
		{true, false, "", false},
		{true, true, "0", true},
	}
	for _, c := range cases {
		if got := resolveCursorLayer(c.set, c.val, env(c.env)); got != c.want {
			t.Errorf("set=%v val=%v env=%q: got %v want %v", c.set, c.val, c.env, got, c.want)
		}
	}
}
