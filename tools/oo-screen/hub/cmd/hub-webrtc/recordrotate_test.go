package main

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// 15.09.2026: файл запису закривається після ОСТАННЬОГО глядача, а не разом з
// агентською ногою. Без цього сеанси різних днів зливались в один файл.
func TestRecordClosesAfterLastViewerLeaves(t *testing.T) {
	withRecordFlag(t, true)
	prev := recordIdleClose
	recordIdleClose = 20 * time.Millisecond
	t.Cleanup(func() { recordIdleClose = prev })

	ns := &nodeSession{nodeID: "rot"}
	vl := silentViewer(t, ns)
	ns.viewerCount.Store(1)
	rec := startRecording(ns.nodeID)
	if rec == nil {
		t.Fatal("startRecording з прапорцем віддав nil")
	}
	ns.rec.Store(rec)

	if !removeViewer(ns, vl) {
		t.Fatal("removeViewer не знайшов ногу")
	}
	if ns.viewerCount.Load() != 0 {
		t.Fatalf("viewerCount=%d після останнього глядача", ns.viewerCount.Load())
	}
	deadline := time.Now().Add(2 * time.Second)
	for ns.rec.Load() != nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ns.rec.Load() != nil {
		t.Fatal("файл запису не закрито після останнього глядача")
	}
}

// Глядач повернувся до спливу паузи — той самий файл лишається.
func TestRecordSurvivesQuickReconnect(t *testing.T) {
	withRecordFlag(t, true)
	prev := recordIdleClose
	recordIdleClose = 40 * time.Millisecond
	t.Cleanup(func() { recordIdleClose = prev })

	ns := &nodeSession{nodeID: "rec2"}
	vl := silentViewer(t, ns)
	ns.viewerCount.Store(1)
	rec := startRecording(ns.nodeID)
	ns.rec.Store(rec)
	removeViewer(ns, vl)
	ns.mu.Lock()
	ns.viewers[&webrtc.PeerConnection{}] = vl
	ns.viewerCount.Store(1)
	ns.mu.Unlock()
	time.Sleep(120 * time.Millisecond)
	if ns.rec.Load() != rec {
		t.Fatal("швидке перепідключення розірвало файл запису")
	}
	rec.Close()
}
