package main

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// viewerOf — нога ноди за її PeerConnection.
func viewerOf(t *testing.T, ns *nodeSession, pc *webrtc.PeerConnection) *viewerLeg {
	t.Helper()
	ns.mu.Lock()
	defer ns.mu.Unlock()
	vl := ns.viewers[pc]
	if vl == nil {
		t.Fatalf("нога для цієї PeerConnection не зареєстрована в ноді")
	}
	return vl
}

// TestSecondViewerJoinsWithoutEvictingFirst — Ф1 на місці колишнього
// "viewer-replace": другий глядач ДОДАЄТЬСЯ до першого, а не вибиває його
// (раніше нова нога закривала попередню PeerConnection). Інваріант старого
// фікса гонки при цьому лишається: нова нога не стає live, поки не Connected —
// публікацію вмикає ЛИШЕ recomputeBinding, тож трек ніколи не йде на
// PeerConnection, яка ще не підключилась.
func TestSecondViewerJoinsWithoutEvictingFirst(t *testing.T) {
	ns := &nodeSession{nodeID: "node-race"}

	pc1, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pc1: %v", err)
	}
	defer pc1.Close()

	if _, err := setupViewerLeg(ns, pc1, nil, "", ""); err != nil {
		t.Fatalf("setupViewerLeg(pc1): %v", err)
	}

	// Перший глядач уже Connected, publisher є — саме стан, у якому раніше
	// друга нога вибивала першу.
	ns.mu.Lock()
	ns.agentPC = pc1 // будь-який ненульовий publisher
	ns.mu.Unlock()
	vl1 := viewerOf(t, ns, pc1)
	markViewerReady(ns, vl1)
	recomputeBinding(ns)

	pc2, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pc2: %v", err)
	}
	defer pc2.Close()

	if _, err := setupViewerLeg(ns, pc2, nil, "", ""); err != nil {
		t.Fatalf("setupViewerLeg(pc2): %v", err)
	}
	vl2 := viewerOf(t, ns, pc2)

	if n := viewerCount(ns); n != 2 {
		t.Fatalf("ns.viewers = %d після приєднання другого глядача, want 2 (другий не має заміщати першого)", n)
	}

	ns.mu.Lock()
	live1, ready2, live2 := vl1.live, vl2.ready, vl2.live
	ns.mu.Unlock()

	if !live1 {
		t.Fatalf("перший глядач перестав бути live після приєднання другого")
	}
	if ready2 {
		t.Fatalf("vl2.ready = true одразу після додавання, want false (pc2 ще не Connected)")
	}
	if live2 {
		t.Fatalf("vl2.live = true одразу після додавання, want false (не можна публікувати на непідключену pc2)")
	}

	// Стару ногу НЕ рвемо: даємо event loop-у pion той самий час, за який
	// попередня реалізація встигала її закрити.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if pc1.ConnectionState() == webrtc.PeerConnectionStateClosed {
			t.Fatalf("pc1 закрито приєднанням другого глядача — це і є той самий eviction, який Ф1 прибирає")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
