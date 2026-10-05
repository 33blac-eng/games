package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/ulpfec"
	"github.com/pion/rtcp"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

func TestFECParamsFrom(t *testing.T) {
	c := []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, PayloadType: 102},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/RED"}, PayloadType: 63},
	}
	if _, ok := fecParamsFrom(c); ok {
		t.Fatal("red without ulpfec must not enable FEC")
	}
	c = append(c, webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/ulpfec"}, PayloadType: 64})
	p, ok := fecParamsFrom(c)
	if !ok || p.RedPT != 63 || p.FecPT != 64 {
		t.Fatalf("%+v %v", p, ok)
	}
}

// Без OO_SCREEN_FEC нічого не реєструється — MediaEngine той самий.
func TestFECOffByDefault(t *testing.T) {
	if fecEnabled {
		t.Skip("OO_SCREEN_FEC set in env")
	}
	m := &webrtc.MediaEngine{}
	if err := registerFECCodecs(m); err != nil {
		t.Fatal(err)
	}
	i := &interceptor.Registry{}
	addFECInterceptor(i)
	if _, ok := fecHighestSeq(1); ok {
		t.Fatal("no FEC legs expected")
	}
}

// З прапорцем: глядач, що пропонує red+ulpfec, отримує їх у answer, і
// bindViewerFEC знаходить узгоджені PT.
func TestFECNegotiation(t *testing.T) {
	old := fecEnabled
	fecEnabled = true
	defer func() { fecEnabled = old }()

	api, err := newAPI("")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	trk, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpFor("")}, "video", "x")
	if _, err := hub.AddTrack(trk); err != nil {
		t.Fatal(err)
	}

	vm := &webrtc.MediaEngine{}
	for _, c := range []webrtc.RTPCodecParameters{
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpFor("")}, PayloadType: 102},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/red", ClockRate: 90000}, PayloadType: 116},
		{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: "video/ulpfec", ClockRate: 90000}, PayloadType: 117},
	} {
		if err := vm.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			t.Fatal(err)
		}
	}
	viewer, err := webrtc.NewAPI(webrtc.WithMediaEngine(vm)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	if _, err := viewer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	offer, _ := viewer.CreateOffer(nil)
	_ = viewer.SetLocalDescription(offer)
	if err := hub.SetRemoteDescription(offer); err != nil {
		t.Fatal(err)
	}
	bindViewerFEC(hub)
	ans, err := hub.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ans.SDP, "red/90000") || !strings.Contains(ans.SDP, "ulpfec/90000") {
		t.Fatalf("answer lacks red/ulpfec:\n%s", ans.SDP)
	}
	ssrc := uint32(hub.GetSenders()[0].GetParameters().Encodings[0].SSRC)
	v, ok := fecLegs.Load(ssrc)
	if !ok {
		t.Fatal("leg not bound")
	}
	fecLegs.Delete(ssrc)
	_ = v
}

// Рев'ю FEC: з FEC highest і буфер responder-а — у вихідному просторі seq
// (медіа + FEC), тож і вікно NACK, і знаменник preLoss рахуються там, а не за
// vl.sent (лише медіа). Інакше на молодій нозі NACK, який hub може
// задовольнити, рахувався промахом, а preLoss завищувався на частку FEC.
func TestFECNackWindowUsesOutputSpace(t *testing.T) {
	old := fecEnabled
	fecEnabled = true
	defer func() { fecEnabled = old }()
	const ssrc = 4242
	p := ulpfec.Params{HighestOut: new(atomic.Uint32), OutCount: new(atomic.Uint64)}
	fecLegs.Store(uint32(ssrc), p)
	defer fecLegs.Delete(uint32(ssrc))

	// 400 медіа + 100 FEC = 500 вихідних, seq 1..500.
	vl := &viewerLeg{sent: 400, lastSeq: 400}
	p.HighestOut.Store(500)
	p.OutCount.Store(500)
	ns := &nodeSession{nodeID: "n"}
	t0 := time.Now()
	n := &rtcp.TransportLayerNack{MediaSSRC: ssrc}
	for _, s := range seqRange(10, 20) { // ~490 назад: у буфері (500), не у 400
		n.Nacks = append(n.Nacks, rtcp.NackPair{PacketID: s})
	}
	onNack(ns, vl, n, t0)
	st := onNack(ns, vl, n, t0.Add(nackWindow))
	if !st.closed || st.hit != st.req || st.escalate {
		t.Fatalf("seq у буфері responder-а пораховано промахом: %+v", st)
	}

	// preLoss: 20 унікальних NACK на 500 вихідних = 0.04 (не 20/400 = 0.05).
	ns2 := &nodeSession{nodeID: "c"}
	vl2 := &viewerLeg{}
	p.OutCount.Store(0)
	legCongestion(ns2, vl2, ssrc, t0)
	p.OutCount.Store(500)
	vl2.sent = 400
	vl2.noteNackSeqs(seqRange(10, 20))
	if sig := legCongestion(ns2, vl2, ssrc, t0); sig.preLoss < 0.039 || sig.preLoss > 0.041 {
		t.Fatalf("preLoss %.4f, want 0.04", sig.preLoss)
	}
}
