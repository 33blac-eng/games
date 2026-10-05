package main

// N6: справжній цикл oo-agent (p2pAgent.run) проти справжнього p2p.Broker і
// pion-«браузера» на loopback.

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/p2p"
	"github.com/pion/webrtc/v4"
)

type p2pTestConsent struct{ ok atomic.Bool }

func (c *p2pTestConsent) Allowed() bool { return c.ok.Load() }

func p2pLoopSE() *webrtc.SettingEngine {
	se := &webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetICETimeouts(time.Second, 2*time.Second, 200*time.Millisecond)
	return se
}

type p2pRig struct {
	srv     *httptest.Server
	broker  *p2p.Broker
	agent   *p2pAgent
	consent *p2pTestConsent
	mu      sync.Mutex
	inputs  []string
	active  atomic.Int32
	idr     atomic.Int32
}

func newP2PRig(t *testing.T, consentOK bool) *p2pRig {
	r := &p2pRig{consent: &p2pTestConsent{}}
	r.consent.ok.Store(consentOK)
	cfg := p2p.DefaultConfig()
	cfg.Enabled = true
	cfg.ConnectTimeout = 3 * time.Second
	cfg.SignalTimeout = 4 * time.Second
	r.broker = p2p.NewBroker(cfg, p2p.Hooks{
		Authorize: func(_ *http.Request, ticket string) (p2p.Grant, int, int, string) {
			g := "view"
			if ticket == "t-control" {
				g = "control"
			}
			return p2p.Grant{User: "u1", Node: "n1", Grant: g, Claims: "c"}, 0, 0, ""
		},
		AgentAuth: func(req *http.Request, n string) bool {
			return n == "n1" && req.Header.Get("Authorization") == "Bearer tok"
		},
	})
	r.broker.PollWait = 300 * time.Millisecond
	mux := http.NewServeMux()
	r.broker.Register(mux, nil)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)

	a := newP2PAgent(cfg, func() string { return r.srv.URL }, "n1", func() string { return "tok" })
	a.se = p2pLoopSE()
	a.consent = r.consent
	a.retryMin = 50 * time.Millisecond
	a.onInput = func(b []byte) { r.mu.Lock(); r.inputs = append(r.inputs, string(b)); r.mu.Unlock() }
	a.onActive = func(n int) { r.active.Store(int32(n)) }
	a.onKeyframe = func() { r.idr.Add(1) }
	r.agent = a
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	// Кадри, як їх подає кадровий цикл main.go.
	go func() {
		for i := 0; ctx.Err() == nil; i++ {
			a.writeVideo([]byte{0, 0, 0, 1, 0x65, 0x88, 0x84, byte(i)}, 33*time.Millisecond)
			time.Sleep(30 * time.Millisecond)
		}
	}()
	return r
}

type p2pViewer struct {
	pc     *webrtc.PeerConnection
	dc     *webrtc.DataChannel
	dcOpen chan struct{}
	media  chan struct{}
	status int
	resp   p2p.ViewerOfferResp
}

func (r *p2pRig) offer(t *testing.T, ticket string) *p2pViewer {
	api := webrtc.NewAPI(webrtc.WithSettingEngine(*p2pLoopSE()))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	v := &p2pViewer{pc: pc, dcOpen: make(chan struct{}), media: make(chan struct{}, 1)}
	_, _ = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	v.dc, _ = pc.CreateDataChannel(p2p.InputLabel, nil)
	v.dc.OnOpen(func() { close(v.dcOpen) })
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if _, _, err := tr.ReadRTP(); err == nil {
			select {
			case v.media <- struct{}{}:
			default:
			}
		}
	})
	off, _ := pc.CreateOffer(nil)
	g := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(off)
	<-g
	body, _ := json.Marshal(p2p.ViewerOfferReq{Ticket: ticket, SDP: pc.LocalDescription().SDP})
	resp, err := http.Post(r.srv.URL+"/p2p/offer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	v.status = resp.StatusCode
	_ = json.NewDecoder(resp.Body).Decode(&v.resp)
	if v.status == http.StatusOK {
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: v.resp.SDP}); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

func p2pWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	dl := time.Now().Add(8 * time.Second)
	for time.Now().Before(dl) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func TestP2PAgentDirectInputRevoke(t *testing.T) {
	r := newP2PRig(t, true)
	v := r.offer(t, "t-control")
	if v.status != http.StatusOK || v.resp.RelayTicket == "" {
		t.Fatalf("offer: %d %+v", v.status, v.resp)
	}
	select {
	case <-v.media:
	case <-time.After(8 * time.Second):
		t.Fatal("no video over the direct leg")
	}
	<-v.dcOpen
	// Конверт браузера {ticket, event} — агент знімає його сам.
	_ = v.dc.SendText(`{"ticket":"t-control","event":{"v":1,"t":"mm"}}`)
	_ = v.dc.SendText(`not json`)
	p2pWait(t, "input delivered", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.inputs) == 1 })
	if r.inputs[0] != `{"v":1,"t":"mm"}` {
		t.Fatalf("input %q", r.inputs[0])
	}
	p2pWait(t, "direct reported via /p2p/result", func() bool { return r.broker.Metrics().Snapshot().Direct == 1 })
	if r.active.Load() != 1 || r.idr.Load() == 0 {
		t.Fatalf("active=%d idr=%d", r.active.Load(), r.idr.Load())
	}
	if r.broker.RevokeUser("u1") != 1 {
		t.Fatal("revoke")
	}
	p2pWait(t, "leg closed by hub close", func() bool { return r.active.Load() == 0 })
	p2pWait(t, "viewer sees the leg die", func() bool {
		s := v.pc.ConnectionState()
		return s == webrtc.PeerConnectionStateDisconnected || s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed
	})
}

func TestP2PAgentViewGrantDropsInput(t *testing.T) {
	r := newP2PRig(t, true)
	v := r.offer(t, "t-view")
	if v.status != http.StatusOK {
		t.Fatalf("offer: %d", v.status)
	}
	<-v.dcOpen
	_ = v.dc.SendText(`{"ticket":"t-view","event":{"v":1,"t":"mm"}}`)
	time.Sleep(300 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.inputs) != 0 {
		t.Fatalf("view grant delivered input: %v", r.inputs)
	}
}

func TestP2PAgentNoConsentFallsBack(t *testing.T) {
	r := newP2PRig(t, false)
	v := r.offer(t, "t-control")
	if v.status != http.StatusConflict || v.resp.Reason != "consent" || v.resp.RelayTicket == "" {
		t.Fatalf("offer: %d %+v", v.status, v.resp)
	}
}

func TestP2PAgentConsentWithdrawn(t *testing.T) {
	r := newP2PRig(t, true)
	v := r.offer(t, "t-control")
	if v.status != http.StatusOK {
		t.Fatalf("offer: %d", v.status)
	}
	p2pWait(t, "active", func() bool { return r.active.Load() == 1 })
	r.consent.ok.Store(false) // «Завершити сесію» на ПК
	p2pWait(t, "leg closed on consent withdrawal", func() bool { return r.active.Load() == 0 })
	p2pWait(t, "hub freed the node", func() bool { return r.broker.Active() == 0 })
}

func TestP2PHubBaseAndConfig(t *testing.T) {
	if b := p2pHubBase("http://h:4470/offer/agent"); b != "http://h:4470" {
		t.Fatal(b)
	}
	if b := p2pHubBase("localhost:4460"); b != "" {
		t.Fatal(b)
	}
	off := p2pConfig(false, "", func(string) string { return "" })
	if off.Enabled {
		t.Fatal("p2p must default off")
	}
	on := p2pConfig(true, "stun:a:3478, stun:b:3478", func(string) string { return "" })
	if !on.Enabled || len(on.STUN) != 2 {
		t.Fatalf("%+v", on)
	}
}
