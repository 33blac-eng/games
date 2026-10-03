package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/hub"
)

// newViewerTrack — свіжий TrackLocalStaticRTP як у setupViewerLeg (WriteRTP на
// незвʼязаний трек у pion — no-op без помилки, тож forwardToViewer безпечно
// викликати в unit-тесті без реального PeerConnection).
func newViewerTrack(t *testing.T) *webrtc.TrackLocalStaticRTP {
	t.Helper()
	trk, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: h264FmtpLine,
	}, "video", "test")
	if err != nil {
		t.Fatalf("new track: %v", err)
	}
	return trk
}

// readyNode — nodeSession з живим publisher-ом і ОДНИМ Connected глядачем (його
// нога live через recomputeBinding), готовий приймати forwardToViewers.
func readyNode(t *testing.T, id string) *nodeSession {
	t.Helper()
	ns := &nodeSession{nodeID: id}
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{} // сентинел: recomputeBinding перевіряє лише != nil
	ns.mu.Unlock()
	addReadyViewer(t, ns)
	return ns
}

func forwardN(ns *nodeSession, count int, baseSeq uint16, baseTS uint32) {
	for i := 0; i < count; i++ {
		forwardToViewers(ns, agentGen1, &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: baseSeq + uint16(i), Timestamp: baseTS + uint32(i)*3000},
			Payload: []byte{0x00, 0x01, 0x02},
		})
	}
}

// TestTwoNodesForwardIndependently — два publisher на різні ноди + два viewer:
// пересилка кожної ноди веде рахунок у СВОЄМУ egress-просторі й не чіпає іншу
// ноду. Це і є маршрутизація "кожен viewer бачить свою ноду".
func TestTwoNodesForwardIndependently(t *testing.T) {
	nsA := readyNode(t, "nodeA")
	nsB := readyNode(t, "nodeB")

	forwardN(nsA, 5, 100, 900000)
	forwardN(nsB, 3, 40000, 5000000)

	nsA.mu.Lock()
	aOut := nsA.lastOutSeq
	aHave := nsA.haveEgress
	nsA.mu.Unlock()
	nsB.mu.Lock()
	bOut := nsB.lastOutSeq
	bHave := nsB.haveEgress
	nsB.mu.Unlock()

	if !aHave || !bHave {
		t.Fatalf("egress not initialized: A=%v B=%v", aHave, bHave)
	}
	// A: перший пакет ініціалізує (out=0), далі +4 => 4. B: +2 => 2.
	if aOut != 4 {
		t.Fatalf("nodeA lastOutSeq = %d, want 4 (5 packets, monotonic from 0)", aOut)
	}
	if bOut != 2 {
		t.Fatalf("nodeB lastOutSeq = %d, want 2 (3 packets) — cross-talk with nodeA?", bOut)
	}
}

// TestRecomputeBindingFailClosedNoPublisher — глядачі готові, але publisher-а
// ноди немає (agentPC == nil): не форвардимо НІКОМУ (fail-closed). Щойно агент
// зникає — знімаються УСІ ноги, не одна.
func TestRecomputeBindingFailClosedNoPublisher(t *testing.T) {
	ns := &nodeSession{nodeID: "n"}
	v1 := addViewer(ns, newPC(t), newViewerTrack(t), "u1")
	v2 := addViewer(ns, newPC(t), newViewerTrack(t), "u2")
	t.Cleanup(func() { removeViewer(ns, v1); removeViewer(ns, v2) })
	markViewerReady(ns, v1)
	markViewerReady(ns, v2)

	live := func() (bool, bool) {
		ns.mu.Lock()
		defer ns.mu.Unlock()
		return v1.live, v2.live
	}

	ns.mu.Lock()
	ns.agentPC = nil // немає publisher-а
	ns.mu.Unlock()
	recomputeBinding(ns)
	if a, b := live(); a || b {
		t.Fatalf("live without a publisher: v1=%v v2=%v, want false/false (fail-closed)", a, b)
	}

	// Publisher зʼявився -> форвардимо обом.
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{}
	ns.mu.Unlock()
	recomputeBinding(ns)
	if a, b := live(); !a || !b {
		t.Fatalf("live after publisher appeared: v1=%v v2=%v, want true/true", a, b)
	}

	// Publisher зник -> знімаємо обох.
	ns.mu.Lock()
	ns.agentPC = nil
	ns.mu.Unlock()
	recomputeBinding(ns)
	if a, b := live(); a || b {
		t.Fatalf("still live after publisher gone: v1=%v v2=%v, want false/false", a, b)
	}
}

// TestRegistryReconnectIsolation — reconnect однієї ноди (новий запис у мапі)
// не чіпає іншу: getOrCreate тієї ж ноди повертає ТОЙ САМИЙ nodeSession, різні
// ноди — різні, а бамп generation однієї ноди не рухає generation іншої.
func TestRegistryReconnectIsolation(t *testing.T) {
	r := newRegistry()
	a1 := r.getOrCreate("A")
	b1 := r.getOrCreate("B")
	if a1 == b1 {
		t.Fatalf("different nodes share a nodeSession")
	}
	if r.getOrCreate("A") != a1 {
		t.Fatalf("getOrCreate(A) returned a new session on reconnect, want the same record")
	}
	if r.get("A") != a1 || r.get("B") != b1 {
		t.Fatalf("get() returned wrong session")
	}
	if r.get("C") != nil {
		t.Fatalf("get(C) != nil for unknown node, want nil (fail-closed lookup)")
	}
	genB0 := b1.generation
	a1.generation++ // імітуємо replace ноди A
	if b1.generation != genB0 {
		t.Fatalf("nodeB generation moved when nodeA reconnected: %d != %d", b1.generation, genB0)
	}
}

// fakeERP — мінімальний ERP consume-ендпоінт: ticket "t-<node>" -> claims з
// цим node. Дозволяє тестувати ticket-режимну маршрутизацію viewer-а на hub.
func fakeERP(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Ticket string `json:"ticket"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		node := strings.TrimPrefix(body.Ticket, "t-")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hub.TicketClaims{UserID: "u1", OrgID: "o1", NodeID: node})
	}))
}

// TestViewerTicketRoutingFailClosed — ticket-режим: viewer з тікетом на ноду З
// publisher-ом проходить node-перевірку (доходить до SDP-негоціації), а viewer
// на ноду БЕЗ publisher-а отримує 404 ДО створення PeerConnection (fail-closed
// -> Mesh-фолбек). Порожній node у тікеті -> 403.
func TestViewerTicketRoutingFailClosed(t *testing.T) {
	srv := fakeERP(t)
	defer srv.Close()

	// Вмикаємо ticket-режим на час тесту.
	prevBase, prevKey, prevReg := erpBase, hubKey, reg
	erpBase, hubKey = srv.URL, "test-key"
	reg = newRegistry()
	defer func() { erpBase, hubKey, reg = prevBase, prevKey, prevReg }()

	// Publisher існує лише для nodeA.
	nsA := reg.getOrCreate("nodeA")
	nsA.mu.Lock()
	nsA.agentPC = &webrtc.PeerConnection{}
	nsA.mu.Unlock()

	post := func(ticket string) int {
		body, _ := json.Marshal(offerReq{SDP: "invalid-sdp", Ticket: ticket})
		r := httptest.NewRequest(http.MethodPost, "/offer/viewer", strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		handleOffer("viewer")(w, r)
		return w.Code
	}

	// nodeB: немає publisher -> 404 fail-closed (до SDP).
	if code := post("t-nodeB"); code != http.StatusNotFound {
		t.Fatalf("viewer to nodeB (no publisher): got %d, want 404 fail-closed", code)
	}
	// Порожній node у тікеті -> 403.
	if code := post("t-"); code != http.StatusForbidden {
		t.Fatalf("viewer with empty node: got %d, want 403", code)
	}
	// nodeA: publisher є -> проходить node-перевірку; далі падає на невалідному
	// SDP (500), що доводить: fail-closed НЕ спрацював, дійшли до негоціації.
	if code := post("t-nodeA"); code == http.StatusNotFound || code == http.StatusForbidden {
		t.Fatalf("viewer to nodeA (publisher present) rejected by node-binding: got %d, want to pass to SDP stage", code)
	}
}
