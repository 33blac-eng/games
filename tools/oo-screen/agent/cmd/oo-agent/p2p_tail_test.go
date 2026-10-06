package main

// N6 (хвіст): звук, шар курсора і локальний контролер бітрейту на прямій
// нозі — справжній p2pAgent.run проти справжнього Broker і pion-«браузера».

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/cursorproto"
	"github.com/organicoils/oo-screen/internal/opusenc"
	"github.com/organicoils/oo-screen/internal/p2p"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

type tailViewer struct {
	pc        *webrtc.PeerConnection
	cursor    *webrtc.DataChannel
	cursorMsg chan []byte
	video     chan uint32 // SSRC відео
	audio     chan string // MimeType звуку
	twccExt   atomic.Bool // відео-пакет прийшов із transport-cc seq
	resp      p2p.ViewerOfferResp
	status    int
}

// tailOffer — «браузер»: recvonly відео (+звук), канали input і cursor,
// (опційно) transport-cc з TWCC-фідбеком, як Chrome.
func (r *p2pRig) tailOffer(t *testing.T, ticket string, audio, cursor, twcc bool) *tailViewer {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	reg := &interceptor.Registry{}
	if twcc {
		if err := webrtc.ConfigureTWCCSender(me, reg); err != nil {
			t.Fatal(err)
		}
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(*p2pLoopSE()))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	v := &tailViewer{pc: pc, cursorMsg: make(chan []byte, 16), video: make(chan uint32, 1), audio: make(chan string, 1)}
	recv := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}
	_, _ = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, recv)
	if audio {
		_, _ = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, recv)
	}
	_, _ = pc.CreateDataChannel(p2p.InputLabel, nil)
	if cursor {
		v.cursor, _ = pc.CreateDataChannel(cursorproto.ChannelLabel, nil)
		v.cursor.OnMessage(func(m webrtc.DataChannelMessage) {
			select {
			case v.cursorMsg <- m.Data:
			default:
			}
		})
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			if tr.Kind() == webrtc.RTPCodecTypeAudio {
				select {
				case v.audio <- tr.Codec().MimeType:
				default:
				}
				continue
			}
			for _, id := range pkt.Header.GetExtensionIDs() {
				if len(pkt.Header.GetExtension(id)) == 2 {
					v.twccExt.Store(true)
				}
			}
			select {
			case v.video <- uint32(tr.SSRC()):
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
	if v.status != http.StatusOK {
		t.Fatalf("offer: %d %+v", v.status, v.resp)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: v.resp.SDP}); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestP2PAgentCarriesOpusAndCursor(t *testing.T) {
	fan := newCursorFan(cursorproto.NewPublisher())
	r := newP2PRig(t, true, func(a *p2pAgent) {
		a.audio = true
		a.audioCap = opusenc.CodecOpus.Capability()
		a.cursor = fan
	})
	v := r.tailOffer(t, "t-control", true, true, false)
	if !strings.Contains(v.resp.SDP, "m=audio") || !strings.Contains(strings.ToLower(v.resp.SDP), "opus/48000/2") {
		t.Fatalf("answer без Opus:\n%s", v.resp.SDP)
	}
	select {
	case m := <-v.audio:
		if !strings.EqualFold(m, webrtc.MimeTypeOpus) {
			t.Fatalf("звук %s, хотіли Opus", m)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("звук не дійшов прямою ногою")
	}
	// Канал курсора браузера підключено до fan (той самий шлях, що cursorPub).
	p2pWait(t, "cursor channel attached", func() bool { return len(fan.snapshot()) == 1 })
	msg := cursorproto.EncodePos(cursorproto.Pos{Visible: true, ShapeID: 7, X: 10, Y: 20, FrameW: 100, FrameH: 50})
	if err := fan.Send(msg); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-v.cursorMsg:
		p, err := cursorproto.DecodePos(got)
		if err != nil || p.X != 10 || p.Y != 20 || p.ShapeID != 7 {
			t.Fatalf("курсор: %+v %v", p, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("позиція курсора не дійшла прямою ногою")
	}
	// Нога закрилась — канал відʼєднано (Publisher не шле в мертвий канал).
	if r.broker.RevokeUser("u1") != 1 {
		t.Fatal("revoke")
	}
	p2pWait(t, "cursor channel detached", func() bool { return len(fan.snapshot()) == 0 })
}

func TestP2PAgentNoAudioWhenBrowserDidNotAsk(t *testing.T) {
	r := newP2PRig(t, true, func(a *p2pAgent) {
		a.audio = true
		a.audioCap = opusenc.CodecOpus.Capability()
	})
	v := r.tailOffer(t, "t-control", false, false, false)
	if strings.Contains(v.resp.SDP, "m=audio") {
		t.Fatal("answer з m=audio, якого браузер не просив")
	}
	r.agent.mu.Lock()
	defer r.agent.mu.Unlock()
	for _, e := range r.agent.entries {
		if e.audio != nil {
			t.Fatal("звукова доріжка без m=audio в offer")
		}
	}
}

func TestP2PAgentLocalBitrateController(t *testing.T) {
	var mu sync.Mutex
	var targets []uint64
	r := newP2PRig(t, true, func(a *p2pAgent) {
		a.bwe, a.delayBWE = true, true
		a.ceilBps = 4_000_000
		a.startBps = func() uint64 { return 4_000_000 }
		a.onBitrate = func(b uint64) { mu.Lock(); targets = append(targets, b); mu.Unlock() }
	})
	v := r.tailOffer(t, "t-control", false, false, true)
	if !strings.Contains(v.resp.SDP, sdp.TransportCCURI) {
		t.Fatalf("answer без transport-cc:\n%s", v.resp.SDP)
	}
	var ssrc uint32
	select {
	case ssrc = <-v.video:
	case <-time.After(8 * time.Second):
		t.Fatal("no video")
	}
	// Recorder агента штампує transport-wide seq, «браузер» шле TWCC-фідбек,
	// агент його розбирає (bwe.TWCC).
	p2pWait(t, "transport-cc seq on the wire", v.twccExt.Load)
	p2pWait(t, "TWCC feedback parsed by the agent", func() bool {
		r.agent.mu.Lock()
		defer r.agent.mu.Unlock()
		for _, e := range r.agent.entries {
			if fb, _ := e.twcc.Counts(); fb > 0 {
				return true
			}
		}
		return false
	})
	last := func() uint64 {
		mu.Lock()
		defer mu.Unlock()
		if len(targets) == 0 {
			return 0
		}
		return targets[len(targets)-1]
	}
	// RR з 25% втрат -> 0.7 × 4M (той самий крок, що в хабі).
	if err := v.pc.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverReport{SSRC: 1, Reports: []rtcp.ReceptionReport{{SSRC: ssrc, FractionLost: 64}}}}); err != nil {
		t.Fatal(err)
	}
	p2pWait(t, "loss cut", func() bool { return last() == 2_800_000 })
	// RR чужого SSRC — не наш потік, ціль не чіпаємо.
	_ = v.pc.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverReport{SSRC: 1, Reports: []rtcp.ReceptionReport{{SSRC: ssrc + 1, FractionLost: 255}}}})
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	n := len(targets)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("чужий SSRC змінив ціль: %v", targets)
	}
	// REMB нижче цілі — стеля; але DownDebounce 2 с після зрізу ще діє.
	time.Sleep(bweDownDebounceForTest)
	_ = v.pc.WriteRTCP([]rtcp.Packet{&rtcp.ReceiverEstimatedMaximumBitrate{SenderSSRC: 1, Bitrate: 1_000_000, SSRCs: []uint32{ssrc}}})
	p2pWait(t, "remb ceiling", func() bool { return last() == 1_000_000 })
}

// Обрив УЖЕ ЗЄДНАНОЇ прямої ноги (браузер закрив peer / мережа впала):
// агент звітує fallback, і relay_ticket, виданий разом з answer, стає дійсним
// рівно на один relay-повтор — плеєр іде через хаб, не в Mesh.
func TestP2PConnectedDropReleasesRelayTicket(t *testing.T) {
	r := newP2PRig(t, true)
	v := r.tailOffer(t, "t-control", false, false, false)
	select {
	case <-v.video:
	case <-time.After(8 * time.Second):
		t.Fatal("no video")
	}
	p2pWait(t, "direct reported", func() bool { return r.broker.Metrics().Snapshot().Direct == 1 })
	if _, ok := r.broker.RedeemRelayTicket(v.resp.RelayTicket); ok {
		t.Fatal("relay-квиток дійсний, поки пряма нога жива")
	}
	start := time.Now()
	_ = v.pc.Close()
	p2pWait(t, "relay ticket released after the connected leg dropped", func() bool {
		_, ok := r.broker.RedeemRelayTicket(v.resp.RelayTicket)
		return ok
	})
	t.Logf("relay_ticket дійсний через %v після обриву", time.Since(start).Round(10*time.Millisecond))
	if _, ok := r.broker.RedeemRelayTicket(v.resp.RelayTicket); ok {
		t.Fatal("relay-квиток викуплено двічі")
	}
	p2pWait(t, "agent leg gone", func() bool { return r.active.Load() == 0 })
}

// bweDownDebounceForTest — bwe.DownDebounce із запасом.
const bweDownDebounceForTest = 2100 * time.Millisecond

func TestP2PAgentBWEDefaultOff(t *testing.T) {
	r := newP2PRig(t, true)
	v := r.tailOffer(t, "t-control", false, false, true)
	if strings.Contains(v.resp.SDP, sdp.TransportCCURI) {
		t.Fatal("transport-cc узгоджено без прапорця")
	}
	r.agent.mu.Lock()
	defer r.agent.mu.Unlock()
	for _, e := range r.agent.entries {
		if e.ctl != nil || e.twcc != nil {
			t.Fatal("контролер прямої ноги без прапорця")
		}
	}
}
