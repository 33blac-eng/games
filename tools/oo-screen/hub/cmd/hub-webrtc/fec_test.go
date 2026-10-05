package main

import (
	"strings"
	"testing"

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
