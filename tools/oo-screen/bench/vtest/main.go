// vtest — мінімальний recvonly WebRTC viewer для діагностики node-binding hub-а.
// Підключається до /offer/viewer з ticket, звітує, чи приходить трек і RTP.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
)

func main() {
	signal := flag.String("signal", "", "POST offer URL")
	ticket := flag.String("ticket", "", "one-time ticket")
	flag.Parse()

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64002a",
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}},
		}, PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		fmt.Println("codec:", err)
		os.Exit(2)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fmt.Println("pc:", err)
		os.Exit(2)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		fmt.Println("transceiver:", err)
		os.Exit(2)
	}
	gotRTP := make(chan int, 1)
	pc.OnTrack(func(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		fmt.Println("VIEWER: OnTrack fired — hub published the track")
		n := 0
		for {
			_, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			n++
			if n == 30 {
				select {
				case gotRTP <- n:
				default:
				}
				return
			}
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) { fmt.Println("VIEWER PC:", s) })

	offer, _ := pc.CreateOffer(nil)
	gc := webrtc.GatheringCompletePromise(pc)
	pc.SetLocalDescription(offer)
	<-gc
	body, _ := json.Marshal(map[string]string{"sdp": pc.LocalDescription().SDP, "ticket": *ticket, "token": *ticket})
	resp, err := http.Post(*signal, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Println("POST:", err)
		os.Exit(2)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Printf("VIEWER: offer rejected %d: %s\n", resp.StatusCode, string(rb))
		os.Exit(1)
	}
	var ans struct {
		SDP string `json:"sdp"`
	}
	json.Unmarshal(rb, &ans)
	pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP})

	select {
	case n := <-gotRTP:
		fmt.Printf("VIEWER: RECEIVED %d RTP packets — VIDEO FLOWS ✓\n", n)
	case <-time.After(10 * time.Second):
		fmt.Println("VIEWER: connected but NO RTP in 10s — track not published (node mismatch?) ✗")
		os.Exit(1)
	}
}
