package p2p_test

// N6 інтеграційний тест: два pion-піри (агент і «браузер») на loopback,
// сигналізація через справжній HTTP Broker з хуками авторизації/аудиту, як у
// хаба. «Симульований NAT» для відкату — агентові заборонено всі локальні
// адреси (SetIPFilter → false), тож прямої пари бути не може: так поводиться
// симетричний NAT без STUN/TURN.

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/hub"
	"github.com/organicoils/oo-screen/internal/p2p"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type fakeConsent struct{ ok atomic.Bool }

func (c *fakeConsent) Allowed() bool { return c.ok.Load() }

func loopbackSE(blocked bool) *webrtc.SettingEngine {
	se := &webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIPFilter(func(ip net.IP) bool { return !blocked && ip.IsLoopback() })
	se.SetICETimeouts(time.Second, 2*time.Second, 200*time.Millisecond)
	return se
}

type rig struct {
	t       *testing.T
	srv     *httptest.Server
	broker  *p2p.Broker
	audit   string
	viewers atomic.Int32
	consent *fakeConsent
	blocked atomic.Bool

	// paused — агент «завис/упав»: не полить взагалі.
	paused atomic.Bool
	// forget — агент «перезапустився»: реєстр ніг порожній, ноги не названі.
	forget      atomic.Bool
	consentPoll time.Duration
	sessionTTL  time.Duration
	reg         p2p.AgentLegs

	mu   sync.Mutex
	legs []*p2p.AgentLeg
	got  [][]byte
}

const node = "node-1"

func newRig(t *testing.T, opts ...func(*rig)) *rig {
	r := &rig{t: t, consent: &fakeConsent{}}
	for _, o := range opts {
		o(r)
	}
	r.consent.ok.Store(true)
	r.audit = filepath.Join(t.TempDir(), "audit.jsonl")
	al, err := hub.OpenAuditLog(r.audit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = al.Close() })
	cfg := p2p.DefaultConfig()
	cfg.Enabled = true
	cfg.ConnectTimeout = 3 * time.Second
	cfg.SignalTimeout = 5 * time.Second
	r.broker = p2p.NewBroker(cfg, p2p.Hooks{
		Authorize: func(_ *http.Request, ticket string) (p2p.Grant, int, int, string) {
			switch ticket {
			case "t-control":
				return p2p.Grant{User: "u1", Node: node, Grant: "control", Claims: "claims-u1"}, int(r.viewers.Load()), 0, ""
			case "t-view":
				return p2p.Grant{User: "u2", Node: node, Grant: "view"}, int(r.viewers.Load()), 0, ""
			}
			return p2p.Grant{}, 0, http.StatusForbidden, "ticket consume failed"
		},
		AgentAuth: func(req *http.Request, n string) bool {
			return n == node && req.Header.Get("Authorization") == "Bearer agent-"+node
		},
		AuditStart: func(g p2p.Grant, id string) func(string) {
			s := al.StartSession(hub.AuditRecord{Node: g.Node, User: g.User, Grant: g.Grant, Session: id[:8], Detail: "transport=p2p"})
			return s.End
		},
	})
	r.broker.PollWait = 300 * time.Millisecond
	if r.sessionTTL > 0 {
		r.broker.SessionTTL = r.sessionTTL
	}
	mux := http.NewServeMux()
	r.broker.Register(mux, nil)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		r.mu.Lock()
		for _, l := range r.legs {
			_ = l.Close()
		}
		r.mu.Unlock()
	})
	go r.agentLoop(stop, cfg)
	return r
}

func (r *rig) agentReq(method, path string, body any) {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	req, _ := http.NewRequest(method, r.srv.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer agent-"+node)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// agentLoop — те, що робить oo-agent: poll → NewAgentLeg → answer/result.
func (r *rig) agentLoop(stop chan struct{}, cfg p2p.Config) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		if r.paused.Load() {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		active := r.reg.Active()
		if r.forget.Load() {
			active = ""
		}
		req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/p2p/poll?node="+node+"&active="+active, nil)
		req.Header.Set("Authorization", "Bearer agent-"+node)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		var m p2p.AgentMsg
		if resp.StatusCode == http.StatusOK {
			_ = json.NewDecoder(resp.Body).Decode(&m)
		}
		resp.Body.Close()
		if r.reg.Handle(m) || m.Type != "offer" {
			continue
		}
		o := *m.Offer
		track, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "oo")
		leg, sdp, err := p2p.NewAgentLeg(o, p2p.AgentLegOptions{
			Config: cfg, SettingEngine: loopbackSE(r.blocked.Load()), Consent: r.consent,
			Tracks: []webrtc.TrackLocal{track}, ConsentPoll: r.consentPoll,
			OnInput: func(b []byte) {
				r.mu.Lock()
				r.got = append(r.got, b)
				r.mu.Unlock()
			},
			OnState: func(state, pair, reason string) {
				r.agentReq(http.MethodPost, "/p2p/result", p2p.ResultReq{ID: o.ID, Node: node, State: state, Pair: pair, Reason: reason})
			},
		})
		if err != nil {
			reason := "error"
			if err == p2p.ErrNoConsent {
				reason = "consent"
			}
			r.agentReq(http.MethodPost, "/p2p/answer", p2p.AgentAnswerReq{ID: o.ID, Node: node, Error: reason})
			continue
		}
		r.mu.Lock()
		r.legs = append(r.legs, leg)
		r.mu.Unlock()
		r.reg.Add(o.ID, leg)
		r.agentReq(http.MethodPost, "/p2p/answer", p2p.AgentAnswerReq{ID: o.ID, Node: node, SDP: sdp})
		go func() {
			for i := 0; i < 100; i++ {
				_ = track.WriteSample(media.Sample{Data: []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, byte(i)}, Duration: 33 * time.Millisecond})
				time.Sleep(30 * time.Millisecond)
			}
		}()
	}
}

type viewer struct {
	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
	dcOpen   chan struct{}
	track    chan struct{}
	state    chan string
	status   int
	resp     p2p.ViewerOfferResp
	fellBack chan string
}

func (r *rig) viewer(ticket string) *viewer {
	t := r.t
	api := webrtc.NewAPI(webrtc.WithSettingEngine(*loopbackSE(false)))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	v := &viewer{pc: pc, dcOpen: make(chan struct{}), track: make(chan struct{}, 1), state: make(chan string, 4), fellBack: make(chan string, 1)}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	v.dc, _ = pc.CreateDataChannel(p2p.InputLabel, nil)
	v.dc.OnOpen(func() { close(v.dcOpen) })
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if _, _, err := tr.ReadRTP(); err == nil {
			select {
			case v.track <- struct{}{}:
			default:
			}
		}
	})
	cfg := p2p.DefaultConfig()
	cfg.ConnectTimeout = 3 * time.Second
	p2p.Monitor(pc, cfg, nil, func(state, pair, reason string) {
		if state == p2p.StateFallback {
			v.fellBack <- reason
		}
	})
	off, _ := pc.CreateOffer(nil)
	done := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(off)
	<-done
	body, _ := json.Marshal(p2p.ViewerOfferReq{Ticket: ticket, SDP: pc.LocalDescription().SDP})
	resp, err := http.Post(r.srv.URL+"/p2p/offer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	v.status = resp.StatusCode
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		_ = json.NewDecoder(resp.Body).Decode(&v.resp)
	}
	if resp.StatusCode == http.StatusOK {
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: v.resp.SDP}); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func (r *rig) auditEvents() string {
	b, _ := os.ReadFile(r.audit)
	if _, _, err := hub.VerifyAuditFile(r.audit); err != nil {
		r.t.Fatalf("audit chain broken: %v", err)
	}
	return string(b)
}

func TestP2PDirectControl(t *testing.T) {
	r := newRig(t)
	v := r.viewer("t-control")
	if v.status != http.StatusOK {
		t.Fatalf("status %d %+v", v.status, v.resp)
	}
	select {
	case <-v.dcOpen:
	case <-time.After(8 * time.Second):
		t.Fatal("data channel did not open")
	}
	select {
	case <-v.track:
	case <-time.After(8 * time.Second):
		t.Fatal("no media over the direct leg")
	}
	_ = v.dc.SendText(`{"t":"mm","x":1}`)
	waitFor(t, "input delivered", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.got) == 1 })
	waitFor(t, "direct counted", func() bool { return r.broker.Metrics().Snapshot().Direct == 1 })
	s := r.broker.Metrics().Snapshot()
	if s.Pairs["host/host"] != 1 || s.Relayed != 0 || s.DirectShare() != 1 {
		t.Fatalf("metrics %+v", s)
	}
	if p := p2p.SelectedPair(v.pc); p == nil || p.Local.Typ != webrtc.ICECandidateTypeHost {
		t.Fatalf("viewer pair %v", p)
	}
	// Другий глядач на ту саму ноду — лише через хаб.
	if v2 := r.viewer("t-view"); v2.status != http.StatusConflict || v2.resp.Reason != "multi-viewer" {
		t.Fatalf("second viewer: %d %+v", v2.status, v2.resp)
	}
	// Відкликання (S2) закриває пряму сесію й пише session_end.
	if n := r.broker.RevokeUser("u1"); n != 1 {
		t.Fatalf("revoked %d", n)
	}
	// Агент забирає "close" poll-ом і сам рве пряму ногу (хаб медіа не бачить).
	waitFor(t, "agent leg closed by revoke", func() bool { c, _ := r.legs[0].Closed(); return c })
	if _, ok := r.broker.RedeemRelayTicket(v.resp.RelayTicket); ok {
		t.Fatal("relay ticket survived revoke")
	}
	a := r.auditEvents()
	if !strings.Contains(a, `"session_start"`) || !strings.Contains(a, `transport=p2p`) || !strings.Contains(a, `"grant":"control"`) || !strings.Contains(a, `revoked`) {
		t.Fatalf("audit:\n%s", a)
	}
	var buf bytes.Buffer
	_ = r.broker.Metrics().WriteProm(&buf)
	if !strings.Contains(buf.String(), `oo_hub_p2p_candidate_pair_total{pair="host/host"} 1`) {
		t.Fatalf("prom:\n%s", buf.String())
	}
}

func TestP2PViewOnlyInputDropped(t *testing.T) {
	r := newRig(t)
	v := r.viewer("t-view")
	if v.status != http.StatusOK {
		t.Fatalf("status %d", v.status)
	}
	select {
	case <-v.dcOpen:
	case <-time.After(8 * time.Second):
		t.Fatal("dc")
	}
	for i := 0; i < 5; i++ {
		_ = v.dc.SendText(`{"t":"kd","k":"a"}`)
	}
	waitFor(t, "drops", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.legs) == 0 {
			return false
		}
		_, d := r.legs[0].InputCounts()
		return d == 5
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) != 0 {
		t.Fatalf("view-only input reached agent: %d", len(r.got))
	}
}

func TestP2PConsentWithdrawnDropsInput(t *testing.T) {
	// Watcher згоди фактично вимкнено, щоб перевірити саме гейт на подію.
	r := newRig(t, func(r *rig) { r.consentPoll = time.Hour })
	v := r.viewer("t-control")
	<-v.dcOpen
	r.consent.ok.Store(false) // «Завершити» на ПК
	_ = v.dc.SendText(`{"t":"kd"}`)
	waitFor(t, "drop", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, d := r.legs[0].InputCounts()
		return d == 1
	})
}

func TestP2PNoConsentFallsBack(t *testing.T) {
	r := newRig(t)
	r.consent.ok.Store(false)
	v := r.viewer("t-control")
	if v.status != http.StatusConflict || v.resp.Fallback != "relay" || v.resp.Reason != "consent" {
		t.Fatalf("got %d %+v", v.status, v.resp)
	}
	if r.broker.Active() != 0 {
		t.Fatal("session left open")
	}
	// ERP-квиток спожито /p2p/offer — relay-повтор іде з relay-квитком, один раз.
	if !strings.HasPrefix(v.resp.RelayTicket, p2p.RelayTicketPrefix) {
		t.Fatalf("no relay ticket: %+v", v.resp)
	}
	if g, ok := r.broker.RedeemRelayTicket(v.resp.RelayTicket); !ok || g.Claims != "claims-u1" || g.Node != node {
		t.Fatalf("redeem: %v %+v", ok, g)
	}
	if _, ok := r.broker.RedeemRelayTicket(v.resp.RelayTicket); ok {
		t.Fatal("relay ticket reused")
	}
	if a := r.auditEvents(); !strings.Contains(a, "fallback:consent") {
		t.Fatalf("audit:\n%s", a)
	}
}

func TestP2PNATBlockedFallsBackToRelay(t *testing.T) {
	r := newRig(t)
	r.blocked.Store(true)
	v := r.viewer("t-control")
	if v.status != http.StatusOK {
		t.Fatalf("signalling should succeed: %d", v.status)
	}
	if _, ok := r.broker.RedeemRelayTicket(v.resp.RelayTicket); ok {
		t.Fatal("relay ticket redeemable while direct leg is alive")
	}
	select {
	case reason := <-v.fellBack:
		if reason != "connect-timeout" && reason != "ice-failed" {
			t.Fatalf("reason %q", reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("viewer did not fall back")
	}
	waitFor(t, "relay counted", func() bool { return r.broker.Metrics().Snapshot().Relayed == 1 })
	waitFor(t, "node freed", func() bool { return r.broker.Active() == 0 })
	if s := r.broker.Metrics().Snapshot(); s.Direct != 0 || s.DirectShare() != 0 {
		t.Fatalf("%+v", s)
	}
	if g, ok := r.broker.RedeemRelayTicket(v.resp.RelayTicket); !ok || g.User != "u1" {
		t.Fatalf("relay ticket after ICE fallback: %v %+v", ok, g)
	}
}

func TestP2PAuthAndMultiViewer(t *testing.T) {
	r := newRig(t)
	if v := r.viewer("forged"); v.status != http.StatusForbidden {
		t.Fatalf("bad ticket: %d", v.status)
	}
	if r.broker.Metrics().Snapshot().Attempts != 0 {
		t.Fatal("unauthorized request counted")
	}
	r.viewers.Store(1) // уже є глядач через хаб — fan-out лишається
	if v := r.viewer("t-control"); v.status != http.StatusConflict || v.resp.Reason != "multi-viewer" {
		t.Fatalf("%d %+v", v.status, v.resp)
	}
	// Агент чужої ноди / без токена не бачить poll.
	resp, _ := http.Get(r.srv.URL + "/p2p/poll?node=" + node)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("poll without auth: %d", resp.StatusCode)
	}
}

func TestP2PDisabledRegistersNothing(t *testing.T) {
	b := p2p.NewBroker(p2p.DefaultConfig(), p2p.Hooks{})
	mux := http.NewServeMux()
	b.Register(mux, nil)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/p2p/offer", "application/json", strings.NewReader("{}"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled p2p route answered %d", resp.StatusCode)
	}
	c := p2p.ConfigFromEnv(func(k string) string {
		return map[string]string{"OO_SCREEN_P2P": "1", "OO_SCREEN_P2P_STUN": "stun:a:3478, stun:b:3478", "OO_SCREEN_P2P_TURN_URL": "turn:t", "OO_SCREEN_P2P_TURN_USER": "u", "OO_SCREEN_P2P_TURN_PASS": "p"}[k]
	})
	if !c.Enabled || len(c.ICEServers()) != 3 {
		t.Fatalf("%+v", c)
	}
}

func directUp(t *testing.T, r *rig) *viewer {
	t.Helper()
	v := r.viewer("t-control")
	if v.status != http.StatusOK {
		t.Fatalf("status %d %+v", v.status, v.resp)
	}
	select {
	case <-v.track:
	case <-time.After(8 * time.Second):
		t.Fatal("no media over the direct leg")
	}
	return v
}

func TestP2PConsentWithdrawnStopsVideo(t *testing.T) {
	r := newRig(t, func(r *rig) { r.consentPoll = 50 * time.Millisecond })
	v := directUp(t, r)
	r.consent.ok.Store(false) // «Завершити» на ПК
	waitFor(t, "leg closed", func() bool { c, why := r.legs[0].Closed(); return c && why == "consent-withdrawn" })
	if s := r.legs[0].PC.ConnectionState(); s != webrtc.PeerConnectionStateClosed {
		t.Fatalf("agent pc %v", s)
	}
	waitFor(t, "viewer pc down", func() bool { return v.pc.ICEConnectionState() != webrtc.ICEConnectionStateConnected })
	waitFor(t, "hub freed node", func() bool { return r.broker.Active() == 0 })
	if a := r.auditEvents(); !strings.Contains(a, "consent-withdrawn") {
		t.Fatalf("audit:\n%s", a)
	}
}

func TestP2PRevokeWhileAgentNotPolling(t *testing.T) {
	r := newRig(t)
	directUp(t, r)
	r.paused.Store(true)
	time.Sleep(400 * time.Millisecond) // поточний poll відпрацював
	if n := r.broker.RevokeUser("u1"); n != 1 {
		t.Fatalf("revoked %d", n)
	}
	if c, _ := r.legs[0].Closed(); c {
		t.Fatal("closed without poll?")
	}
	r.paused.Store(false) // "close" не загубився, поки агент мовчав
	waitFor(t, "agent leg closed after resume", func() bool { c, _ := r.legs[0].Closed(); return c })
}

func TestP2PAgentLostFreesNode(t *testing.T) {
	r := newRig(t, func(r *rig) { r.sessionTTL = 700 * time.Millisecond })
	directUp(t, r)
	r.paused.Store(true) // агент завис: ні poll, ні result
	waitFor(t, "ttl reaped", func() bool { return r.broker.Active() == 0 })
	if a := r.auditEvents(); !strings.Contains(a, "agent-lost") {
		t.Fatalf("audit:\n%s", a)
	}
	r.paused.Store(false)
	// Стару ногу агент таки рве (хаб назвав її в "close"), нода знову вільна.
	waitFor(t, "stale leg closed", func() bool { c, _ := r.legs[0].Closed(); return c })
	if v := r.viewer("t-control"); v.status != http.StatusOK {
		t.Fatalf("node still blocked: %d %+v", v.status, v.resp)
	}
}

func TestP2PAgentRestartFreesNode(t *testing.T) {
	r := newRig(t)
	directUp(t, r)
	r.forget.Store(true) // перезапуск: poll з порожнім active
	waitFor(t, "agent-lost on restart", func() bool { return r.broker.Active() == 0 })
	if a := r.auditEvents(); !strings.Contains(a, "agent-lost") {
		t.Fatalf("audit:\n%s", a)
	}
}
