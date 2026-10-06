package main

import (
	"os"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// Регресія 05.10.2026: з OO_SCREEN_FEC=1 справжній offer Chrome
// (testdata/chrome_offer.sdp) давав відповідь без H.264 (415 no_h264) —
// статичні PT red/ulpfec хаба (116/117) збігались із PT rtx/H.264 у Chrome.
// T_NOFEC=1 — контрольний прогін без FEC.
func TestFECAnswerKeepsH264ForRealChromeOffer(t *testing.T) {
	offer, err := os.ReadFile("testdata/chrome_offer.sdp")
	if err != nil {
		t.Fatal(err)
	}
	old := fecEnabled
	fecEnabled = os.Getenv("T_NOFEC") == ""
	defer func() { fecEnabled = old }()
	for _, prof := range []string{"", "4d4032", "4d4028"} {
		api, err := newAPI(prof, string(offer))
		if err != nil {
			t.Fatal(err)
		}
		pc, err := api.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		tr, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: fecH264Fmtp(prof, string(offer)),
		}, "video", "oo")
		if err != nil {
			t.Fatal(err)
		}
		// Як у setupViewerLeg/handleOffer: трек до SetRemoteDescription.
		if _, err := pc.AddTrack(tr); err != nil {
			t.Fatal(err)
		}
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
			t.Fatal(err)
		}
		ans, err := pc.CreateAnswer(nil)
		if err != nil {
			t.Fatal(err)
		}
		if m := videoCodecMismatch(ans.SDP, effectiveProfile(prof)); m != nil {
			t.Errorf("profile %q: %s: %s\n%s", prof, m.Error, m.Detail, videoSection(ans.SDP))
		}
		_ = pc.Close()
	}
}

func videoSection(sdp string) string {
	var b strings.Builder
	in := false
	for _, l := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(l, "m=") {
			in = strings.HasPrefix(l, "m=video")
		}
		if in && (strings.HasPrefix(l, "m=") || strings.HasPrefix(l, "a=rtpmap") || strings.HasPrefix(l, "a=fmtp")) {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}
