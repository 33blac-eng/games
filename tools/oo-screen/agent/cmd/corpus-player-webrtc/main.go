// corpus-player-webrtc — кандидат A агент. Читає записаний Annex-B корпус,
// зациклює з пейсингом 60fps, шле H.264 через WebRTC (TrackLocalStaticSample)
// на hub-webrtc (:4470, POST /offer/agent). Pion сам пакетизує AU в RTP.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/organicoils/oo-screen/internal/h264"
)

const (
	frameDur     = 16667 * time.Microsecond // 60fps
	h264FmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64002a"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var token = envOr("OO_SCREEN_T1_TOKEN", "t1-dev-token")

// hubURL — конфігурований через env, щоб можна було бити у VPS-hub, а не лише
// localhost (потрібно для матричного ранера через мережу з netem).
var hubURL = envOr("OO_SCREEN_HUB_URL", "http://127.0.0.1:4470/offer/agent")

func corpusPath() string {
	return filepath.Join("bench", "corpus", "corpus-1080p60.h264")
}

func loadAUs() []h264.AU {
	data, err := os.ReadFile(corpusPath())
	if err != nil {
		log.Fatalf("read corpus: %v", err)
	}
	aus := h264.SplitAUs(data)
	if len(aus) == 0 {
		log.Fatalf("corpus produced 0 AUs")
	}
	log.Printf("corpus loaded: %d AUs", len(aus))
	return aus
}

func newAPI() (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: h264FmtpLine,
			// Без RTCPFeedback interceptor-ланцюг не будує NACK generator/responder
			// для PT 102 — див. коментар у hub-webrtc/main.go newAPI().
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "nack"},
				{Type: "nack", Parameter: "pli"},
			},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}

	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})

	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(se)), nil
}

type offerReq struct {
	SDP   string `json:"sdp"`
	Token string `json:"token"`
}
type answerResp struct {
	SDP string `json:"sdp"`
}

func negotiate(pc *webrtc.PeerConnection) error {
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	<-gatherComplete

	body, _ := json.Marshal(offerReq{SDP: pc.LocalDescription().SDP, Token: token})
	resp, err := http.Post(hubURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errStatus(resp.StatusCode)
	}
	var ans answerResp
	if err := json.NewDecoder(resp.Body).Decode(&ans); err != nil {
		return err
	}
	return pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP})
}

type errStatus int

func (e errStatus) Error() string { return "hub responded with non-200 status" }

func main() {
	aus := loadAUs()

	api, err := newAPI()
	if err != nil {
		log.Fatalf("new api: %v", err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Fatalf("new pc: %v", err)
	}

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: h264FmtpLine,
	}, "video", "oo-screen-agent")
	if err != nil {
		log.Fatalf("new track: %v", err)
	}

	sender, err := pc.AddTrack(track)
	if err != nil {
		log.Fatalf("add track: %v", err)
	}

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Printf("agent ICE: %s", s)
	})

	// RTCP read-loop власного sender-а: PLI логуємо (ігноруємо в T1 — корпус
	// має IDR/2с), NACK-пакети пропускаємо через interceptor-chain (nack
	// responder) до ретрансміту.
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := sender.Read(buf)
			if err != nil {
				return
			}
			pkts, err := rtcp.Unmarshal(buf[:n])
			if err != nil {
				continue
			}
			for _, p := range pkts {
				if _, ok := p.(*rtcp.PictureLossIndication); ok {
					log.Printf("PLI received from hub (ignored in T1, corpus has IDR/2s)")
				}
			}
		}
	}()

	if err := negotiate(pc); err != nil {
		log.Fatalf("negotiate: %v", err)
	}
	log.Printf("negotiated with hub, waiting for PeerConnectionStateConnected...")

	// negotiate() чекає лише SetRemoteDescription — не ICE/DTLS. Без цього
	// перші AU (SPS/PPS/IDR) писались у WriteSample одразу після SDP,
	// губились до встановлення зʼєднання (finding 11).
	if err := waitConnected(pc, 10*time.Second); err != nil {
		log.Fatalf("wait connected: %v", err)
	}
	log.Printf("connected to hub, streaming...")

	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()

	i := 0
	for range ticker.C {
		au := aus[i%len(aus)]
		if err := track.WriteSample(media.Sample{Data: au.Data, Duration: frameDur}); err != nil {
			// НЕ просуваємо AU на помилці (finding 12): просування створює
			// дірку в стрімі (пропущений AU) поверх уже втраченого сімпла.
			// Короткий бек-офф + retry того ж AU на наступному тіку.
			log.Printf("write sample failed, will retry same AU %d: %v", i, err)
			continue
		}
		i++
	}
}

// waitConnected блокується до PeerConnectionStateConnected або таймауту.
func waitConnected(pc *webrtc.PeerConnection, timeout time.Duration) error {
	done := make(chan struct{})
	var closeOnce sync.Once
	// Хендлер реєструємо ПЕРЕД повторною перевіркою стану — інакше є вікно,
	// де стан встигає стати Connected між первинним читанням і підпискою.
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("agent PC state: %s", s)
		if s == webrtc.PeerConnectionStateConnected {
			closeOnce.Do(func() { close(done) })
		}
	})
	if pc.ConnectionState() == webrtc.PeerConnectionStateConnected {
		closeOnce.Do(func() { close(done) })
	}
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %s waiting for PeerConnectionStateConnected (state=%s)", timeout, pc.ConnectionState())
	}
}
