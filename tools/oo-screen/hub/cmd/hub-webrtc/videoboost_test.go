package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/control"
)

func TestVideoBoost(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	clean := now.Add(-10 * time.Second)
	base := bitrateCtl{target: 4_000_000, startBps: 8_000_000, goodSince: clean, lastSent: now.Add(-10 * time.Second)}
	cases := []struct {
		name string
		c    bitrateCtl
		want uint64 // 0 = no change
	}{
		{"step x1.5", base, 6_000_000},
		{"clamped to ceiling", func() bitrateCtl { c := base; c.target = 7_000_000; return c }(), 8_000_000},
		{"at ceiling", func() bitrateCtl { c := base; c.target = 8_000_000; return c }(), 0},
		{"remb limits", func() bitrateCtl { c := base; c.remb = 5_000_000; return c }(), 5_000_000},
		{"cutFrom limits", func() bitrateCtl { c := base; c.cutFrom = 4_500_000; return c }(), 4_500_000},
		{"not clean", func() bitrateCtl { c := base; c.goodSince = time.Time{}; return c }(), 0},
		{"debounce", func() bitrateCtl { c := base; c.lastSent = now.Add(-time.Second); return c }(), 0},
		{"uninitialised", bitrateCtl{}, 0},
		{"remb below target", func() bitrateCtl { c := base; c.remb = 3_000_000; return c }(), 0},
	}
	for _, c := range cases {
		got, ok := c.c.videoBoost(now)
		if c.want == 0 {
			if ok || got.target != c.c.target {
				t.Errorf("%s: unexpected boost to %d", c.name, got.target)
			}
			continue
		}
		if !ok || got.target != c.want || got.reason != "video" || !got.lastSent.Equal(now) {
			t.Errorf("%s: got %d ok=%v reason=%q want %d", c.name, got.target, ok, got.reason, c.want)
		}
	}
}

func TestHandleAgentCtlContentMode(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	ns := &nodeSession{nodeID: "v", startBps: 8_000_000}
	ns.bitrate = bitrateCtl{target: 4_000_000, startBps: 8_000_000, goodSince: now.Add(-time.Minute)}
	b, _ := json.Marshal(control.ContentMode(1, "video"))
	handleAgentCtl(ns, b, now)
	if !ns.videoMode || ns.bitrate.target != 6_000_000 {
		t.Fatalf("video: mode=%v target=%d", ns.videoMode, ns.bitrate.target)
	}
	// Повтор у вікні дебаунсу — без кроку; RR-шлях теж дебаунситься.
	tryVideoBoost(ns, now.Add(time.Second))
	if ns.bitrate.target != 6_000_000 {
		t.Fatalf("debounce broken: %d", ns.bitrate.target)
	}
	tryVideoBoost(ns, now.Add(4*time.Second))
	if ns.bitrate.target != 8_000_000 {
		t.Fatalf("second step want ceiling, got %d", ns.bitrate.target)
	}
	b, _ = json.Marshal(control.ContentMode(2, "normal"))
	handleAgentCtl(ns, b, now.Add(5*time.Second))
	if ns.videoMode || ns.bitrate.target != 8_000_000 {
		t.Fatalf("normal: mode=%v target=%d (exit must not cut)", ns.videoMode, ns.bitrate.target)
	}
	ns.bitrate.target = 4_000_000
	tryVideoBoost(ns, now.Add(time.Minute))
	if ns.bitrate.target != 4_000_000 {
		t.Fatal("boost outside video mode")
	}
	// Сміття/інша версія/інший тип — ігнор.
	for _, raw := range []string{"resume", `{"v":9,"type":"content_mode","mode":"video"}`, `{"v":1,"type":"heartbeat"}`} {
		handleAgentCtl(ns, []byte(raw), now)
		if ns.videoMode {
			t.Fatalf("%q switched video on", raw)
		}
	}
}
