package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/hub"
	"github.com/organicoils/oo-screen/internal/multimon"
)

func withMultimon(t *testing.T, on bool) {
	t.Helper()
	prev := multimonEnabled
	multimonEnabled = on
	t.Cleanup(func() { multimonEnabled = prev })
}

// TestMultimonAgentAuthUsesNodeToken — потік монітора автентифікується
// токеном СВОЄЇ ноди (на ПК лише він), а не чужої; без прапорця "X#m1" —
// звичайний node_id, що вимагає власного токена.
func TestMultimonAgentAuthUsesNodeToken(t *testing.T) {
	const master = "separate-master-0123456789abcdef"
	t.Setenv("OO_SCREEN_AGENT_AUTH", "strict")
	t.Setenv("OO_SCREEN_LEGACY_AGENT_TOKEN", "")
	t.Setenv("OO_SCREEN_AGENT_SECRET", master)
	tokA := hub.NodeToken(master, "nodeA")

	withMultimon(t, false)
	if agentAuthorized(agentAuthNode("nodeA#m1"), tokA) {
		t.Fatal("фіча вимкнена, а токен nodeA відчинив nodeA#m1")
	}
	withMultimon(t, true)
	if !agentAuthorized(agentAuthNode("nodeA#m1"), tokA) {
		t.Fatal("токен nodeA не відчинив власний потік nodeA#m1")
	}
	if agentAuthorized(agentAuthNode("nodeB#m1"), tokA) {
		t.Fatal("токен nodeA відчинив потік ЧУЖОЇ ноди nodeB#m1")
	}
	if agentAuthNode("nodeA#m0") != "nodeA#m0" || agentAuthNode("nodeA#m16") != "nodeA#m16" {
		t.Fatal("некоректний індекс монітора не сміє зводитись до базової ноди")
	}
}

func postViewerMonitor(ticket string, monitor int) int {
	body, _ := json.Marshal(offerReq{SDP: "invalid-sdp", Ticket: ticket, Monitor: monitor})
	r := httptest.NewRequest(http.MethodPost, "/offer/viewer", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleOffer("viewer")(w, r)
	// Невалідний SDP навмисно: допущений глядач падає ДАЛІ, на негоціації.
	// Відмови маршрутизації розрізняємо за тілом, а не лише за кодом.
	if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "bad monitor") {
		return badMonitor
	}
	return w.Code
}

// badMonitor — псевдокод «відхилено саме як некоректний монітор».
const badMonitor = -400

// TestMultimonEndToEndRouting — два реальні agent-ноги (монітор 0 під nodeA,
// монітор 1 під nodeA#m1) через POST /offer/agent: /control бачить обидва
// потоки, глядач з квитком на nodeA допускається до монітора 1, монітор без
// publisher-а — 404 fail-closed, квиток на іншу ноду монітор nodeA не відчиняє.
func TestMultimonEndToEndRouting(t *testing.T) {
	withTicketMode(t)
	withMultimon(t, true)

	dialAgentLeg(t, "nodeA")
	dialAgentLeg(t, "nodeA#m1")
	for _, id := range []string{"nodeA", "nodeA#m1"} {
		ns := reg.get(id)
		if ns == nil || !waitFor(15*time.Second, ns.hasAgent) {
			t.Fatalf("%s: agent-нога не піднялась", id)
		}
	}

	w := postControl(controlReq{offerReq: offerReq{Ticket: "t-nodeA"}})
	var got controlResp
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || fmt.Sprint(got.Streams) != "[0 1]" {
		t.Fatalf("/control: %d streams=%v, want [0 1]", w.Code, got.Streams)
	}

	if c := postViewerMonitor("t-nodeA", 1); c == http.StatusNotFound || c == badMonitor || c == http.StatusForbidden {
		t.Fatalf("монітор 1: %d — глядач не допущений до живого потоку", c)
	}
	if c := postViewerMonitor("t-nodeA", 2); c != http.StatusNotFound {
		t.Fatalf("монітор 2 без publisher-а: %d, want 404", c)
	}
	if c := postViewerMonitor("t-nodeB", 1); c != http.StatusNotFound {
		t.Fatalf("квиток nodeB, монітор 1: %d, want 404 (nodeB#m1 нема)", c)
	}
	if c := postViewerMonitor("t-nodeA", -1); c != badMonitor {
		t.Fatalf("monitor=-1: %d, want 400", c)
	}

	// Рев'ю F6: з живим nodeA#m1 select_output основного потоку заблоковано —
	// інакше він переїхав би на монітор, який уже захоплює дитина.
	one := 1
	if w := postControl(controlReq{offerReq: offerReq{Ticket: "t-nodeA"}, Output: &one}); w.Code != http.StatusConflict {
		t.Fatalf("select_output при живих потоках моніторів: %d, want 409", w.Code)
	}

	// Вимкнений прапорець: monitor>0 — 400, monitor 0 — старий шлях.
	multimonEnabled = false
	if c := postViewerMonitor("t-nodeA", 1); c != badMonitor {
		t.Fatalf("фіча вимкнена, monitor=1: %d, want bad monitor", c)
	}
	if c := postViewerMonitor("t-nodeA", 0); c == http.StatusNotFound || c == badMonitor {
		t.Fatalf("фіча вимкнена, monitor=0: %d — старий шлях зламано", c)
	}
	w = postControl(controlReq{offerReq: offerReq{Ticket: "t-nodeA"}})
	got = controlResp{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Streams) != 0 {
		t.Fatalf("фіча вимкнена, а streams=%v", got.Streams)
	}
}

// TestMultimonRevokeCoversMonitorStreams — revoke node=X знімає і X#m<i>.
func TestMultimonRevokeCoversMonitorStreams(t *testing.T) {
	withTicketMode(t)
	nodeWithAgent("nodeA", nil, 0)
	nodeWithAgent("nodeA#m2", nil, 0)
	nodeWithAgent("nodeAB#m1", nil, 0)
	got := monitorStreamSessions("nodeA")
	if len(got) != 1 || got[0].nodeID != "nodeA#m2" {
		t.Fatalf("got %d sessions, want тільки nodeA#m2", len(got))
	}
}

// TestMultimonTracksForwardIndependently — потоки моніторів однієї ноди мають
// окремий egress: пакети монітора 1 не зсувають seq монітора 0.
func TestMultimonTracksForwardIndependently(t *testing.T) {
	m0 := readyNode(t, "pc")
	m1 := readyNode(t, "pc#m1")
	forwardN(m0, 10, 100, 0)
	forwardN(m1, 4, 9000, 0)
	m0.mu.Lock()
	a := m0.lastOutSeq
	m0.mu.Unlock()
	m1.mu.Lock()
	b := m1.lastOutSeq
	m1.mu.Unlock()
	if a != 9 || b != 3 {
		t.Fatalf("lastOutSeq m0=%d m1=%d, want 9/3", a, b)
	}
}

// BenchmarkMultimonForward — ціна роздачі одного «кадрового такту» на N
// потоків моніторів (по одному глядачу на потік): ns/op = один пакет у КОЖЕН
// із N потоків. Кожен трек — свій fanout, тож очікувано лінійно від N.
func BenchmarkMultimonForward(b *testing.B) {
	benchQuietLog(b)
	prevOut := ndjsonOut
	ndjsonOut = io.Discard
	b.Cleanup(func() { ndjsonOut = prevOut })
	stream := benchStream()
	for _, n := range []int{1, 2, 3} {
		b.Run(fmt.Sprintf("monitors=%d", n), func(b *testing.B) {
			nodes := make([]*nodeSession, n)
			var legs []*viewerLeg
			for m := range nodes {
				ns := &nodeSession{nodeID: multimon.NodeID("pc", m), startBps: 8_000_000}
				ns.agentPC = &webrtc.PeerConnection{}
				ns.viewers = make(map[*webrtc.PeerConnection]*viewerLeg)
				vl := &viewerLeg{ready: true, out: make(chan *rtp.Packet, viewerQueueDepth+gopMaxPackets), done: make(chan struct{})}
				ns.viewers[&webrtc.PeerConnection{}] = vl
				legs = append(legs, vl)
				recomputeBinding(ns)
				nodes[m] = ns
			}
			drain := func() {
				for _, vl := range legs {
					for len(vl.out) > 0 {
						<-vl.out
					}
				}
			}
			drain()
			b.ReportAllocs()
			i := 0
			seq, ts := uint16(0), uint32(0)
			for b.Loop() {
				src := stream[i%len(stream)]
				if i%(benchPktsPerSec/benchFPS) == 0 {
					ts += 90000 / benchFPS
				}
				for _, ns := range nodes {
					pkt := &rtp.Packet{Header: src.Header, Payload: src.Payload}
					pkt.SequenceNumber = seq
					pkt.Timestamp = ts
					forwardToViewers(ns, agentGen1, pkt)
				}
				seq++
				i++
				if i%(viewerQueueDepth/2) == 0 {
					b.StopTimer()
					drain()
					b.StartTimer()
				}
			}
			b.StopTimer()
			for _, vl := range legs {
				if !vl.live || vl.discarding {
					b.Fatalf("leg not forwarding: live=%v discarding=%v", vl.live, vl.discarding)
				}
			}
		})
	}
}

// TestMultimonHasMonitorStreams — блокування select_output вмикається лише
// фічею і лише живим publisher-ом X#m<i> саме цієї ноди.
func TestMultimonHasMonitorStreams(t *testing.T) {
	withTicketMode(t)
	withMultimon(t, true)
	nodeWithAgent("solo", nil, 0)
	nodeWithAgent("soloX#m1", nil, 0)
	if hasMonitorStreams("solo") {
		t.Fatal("solo без solo#m<i> вважається multimon")
	}
	nodeWithAgent("solo#m3", nil, 0)
	if !hasMonitorStreams("solo") {
		t.Fatal("solo#m3 живий, а блокування нема")
	}
	multimonEnabled = false
	if hasMonitorStreams("solo") {
		t.Fatal("фіча вимкнена, а select_output блокується")
	}
}
