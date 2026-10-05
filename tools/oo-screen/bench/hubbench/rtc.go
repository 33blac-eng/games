package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/maphash"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/stdnet"
	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/internal/h264"
)

const frameDur = 16667 * time.Microsecond // 60 fps

// corpus — AU корпусу + profile-level-id з його SPS (агент і глядач мусять
// оголосити той самий профіль, інакше хаб відмовить по H-18).
type corpus struct {
	aus     []h264.AU
	profile string
	mbps    float64
}

func loadCorpus(path string) (*corpus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	aus := h264.SplitAUs(data)
	if len(aus) == 0 {
		return nil, fmt.Errorf("%s: 0 AU", path)
	}
	c := &corpus{aus: aus, profile: "4d002a"}
	for _, n := range h264.SplitNALs(aus[0].Data) {
		if len(n) > 0 && n[0]&0x1F == 7 {
			if s, err := h264.ParseSPS(n); err == nil {
				c.profile = s.ProfileLevelID()
			}
			break
		}
	}
	c.mbps = float64(len(data)) * 8 / (float64(len(aus)) * frameDur.Seconds()) / 1e6
	return c, nil
}

func fmtp(profile string) string {
	return "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profile
}

func newAPI(profile string) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: fmtp(profile),
			RTCPFeedback: []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	i := &interceptor.Registry{}
	if debugPkts > 0 {
		i.Add(rawLogFactory{})
	}
	// Набір pion за замовчуванням, але без stats-interceptor (B7): вимірювач
	// GetStats не кличе, а stats на КОЖЕН прийнятий пакет кожної з N ніг бере
	// мʼютекс і time.Now() — це ціна клієнта, яка інакше осідає в «затримці
	// хаба» (клієнт на 2 ядрах приймає ту саму IDR-пачку N разів). NACK, RR/SR
	// і TWCC лишаються — їх веде й браузер, і хаб має бачити ту саму картину.
	// HUBBENCH_PION_STATS=1 — повний дефолтний набір.
	if os.Getenv("HUBBENCH_PION_STATS") == "1" {
		if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
			return nil, err
		}
	} else {
		if err := webrtc.ConfigureNack(m, i); err != nil {
			return nil, err
		}
		if err := webrtc.ConfigureRTCPReports(i); err != nil {
			return nil, err
		}
		if err := webrtc.ConfigureSimulcastExtensionHeaders(m); err != nil {
			return nil, err
		}
		if err := webrtc.ConfigureTWCCSender(m, i); err != nil {
			return nil, err
		}
	}
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetIncludeLoopbackCandidate(true)
	// HUBBENCH_RCVBUF=<байт> — SO_RCVBUF кожного UDP-сокета клієнта. Без
	// нього — дефолт ядра (rmem_default): на боксі з 208 КБ клієнт-глядач сам
	// губить IDR-пачку, і хвіст p99 стає клієнтським (ретрансмісія через NACK
	// клієнта pion ≈ 100 мс), а не хабовим. Замір у RESULTS-hub.md (B7) ішов
	// на rmem_default = 4 МБ.
	if n, _ := strconv.Atoi(os.Getenv("HUBBENCH_RCVBUF")); n > 0 {
		sn, err := stdnet.NewNet()
		if err != nil {
			return nil, err
		}
		se.SetNet(rcvbufNet{sn, n})
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(se)), nil
}

type offerBody struct {
	SDP    string `json:"sdp"`
	Token  string `json:"token,omitempty"`
	Ticket string `json:"ticket,omitempty"`
	Node   string `json:"node,omitempty"`
}

var httpc = &http.Client{Timeout: 20 * time.Second}

// negotiate: offer -> хаб -> answer. Повертає час, коли прийшла відповідь.
func negotiate(pc *webrtc.PeerConnection, url string, body offerBody) error {
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	<-gather
	body.SDP = pc.LocalDescription().SDP
	buf, _ := json.Marshal(body)
	resp, err := httpc.Post(url, "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s: %s", url, resp.Status, bytes.TrimSpace(raw))
	}
	var ans struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return err
	}
	return pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP})
}

// sendLog — коли агент віддав у сокет пакет із даним payload. Ключ — хеш
// payload: хаб переписує seq/ts, але payload не чіпає, тож це єдиний
// незмінний ідентифікатор пакета на шляху агент -> хаб -> глядач.
type sendLog struct {
	mu sync.Mutex
	m  map[uint64]int64
}

var hashSeed = maphash.MakeSeed()

func (s *sendLog) put(p []byte, t int64) {
	h := maphash.Bytes(hashSeed, p)
	s.mu.Lock()
	s.m[h] = t
	s.mu.Unlock()
}

func (s *sendLog) get(p []byte) (int64, bool) {
	h := maphash.Bytes(hashSeed, p)
	s.mu.Lock()
	t, ok := s.m[h]
	s.mu.Unlock()
	return t, ok
}

// agent — синтетичний паблішер: корпус у циклі, 60 fps, власний пакетизатор
// (щоб мати payload кожного пакета до відправки). honorPLI = поводитись як
// справжній агент: на PLI наступним кадром іде найближчий IDR корпусу.
type agent struct {
	pc       *webrtc.PeerConnection
	track    *webrtc.TrackLocalStaticRTP
	c        *corpus
	sent     *sendLog
	honorPLI bool
	pli      atomic.Int64
	stop     chan struct{}
	stopOnce sync.Once
	flog     atomic.Pointer[fileSendLog] // latwatch: лог відправки у файл
}

func (a *agent) setFileLog(l *fileSendLog) { a.flog.Store(l) }

func startAgent(api *webrtc.API, c *corpus, hub, token, node string, sent *sendLog, honorPLI bool) (*agent, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	tr, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: fmtp(c.profile),
	}, "video", "bench-"+node)
	if err != nil {
		pc.Close()
		return nil, err
	}
	sender, err := pc.AddTrack(tr)
	if err != nil {
		pc.Close()
		return nil, err
	}
	a := &agent{pc: pc, track: tr, c: c, sent: sent, honorPLI: honorPLI, stop: make(chan struct{})}
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
					a.pli.Add(1)
				}
			}
		}
	}()
	connected := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})
	if err := negotiate(pc, hub+"/offer/agent", offerBody{Token: token, Node: node}); err != nil {
		pc.Close()
		return nil, err
	}
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		pc.Close()
		return nil, fmt.Errorf("agent %s: not connected", node)
	}
	go a.run()
	return a, nil
}

func (a *agent) run() {
	pk := rtp.NewPacketizer(1200, 102, rand.Uint32(), &codecs.H264Payloader{}, rtp.NewRandomSequencer(), 90000)
	t := time.NewTicker(frameDur)
	defer t.Stop()
	i := rand.Intn(len(a.c.aus)) // ноди не в фазі одна з одною
	var lastPLI int64
	for {
		select {
		case <-a.stop:
			return
		case <-t.C:
		}
		if a.honorPLI {
			if p := a.pli.Load(); p != lastPLI {
				lastPLI = p
				for k := 0; k < len(a.c.aus); k++ {
					if a.c.aus[(i+k)%len(a.c.aus)].Keyframe {
						i += k
						break
					}
				}
			}
		}
		au := a.c.aus[i%len(a.c.aus)]
		i++
		for _, p := range pk.Packetize(au.Data, 1500) {
			if a.sent != nil {
				a.sent.put(p.Payload, time.Now().UnixNano())
			}
			if fl := a.flog.Load(); fl != nil {
				fl.put(payloadHash(p.Payload), time.Now().UnixNano())
			}
			if err := a.track.WriteRTP(p); err != nil {
				return
			}
		}
	}
}

func (a *agent) close() {
	a.stopOnce.Do(func() { close(a.stop) })
	a.pc.Close()
}

// viewer — recvonly-нога. Нічого не декодує: лічить пакети/байти/дірки seq,
// фіксує перший пакет і перший повний IDR, і (за наявності sendLog) затримку
// агент -> глядач на кожному пакеті.
type viewer struct {
	pc        *webrtc.PeerConnection
	tStart    time.Time
	tAnswer   time.Time
	tConn     atomic.Int64
	tFirstPkt atomic.Int64
	tFirstIDR atomic.Int64
	pkts      atomic.Uint64
	bytes     atomic.Uint64
	lost      atomic.Uint64
	lastPkt   atomic.Int64
	keyStarts atomic.Uint64

	latMu  sync.Mutex
	lat    []float64 // мс
	latOn  atomic.Bool
	onPkt  func(v *viewer, p *rtp.Packet, now int64) // опційний хук (watch)
	closed atomic.Bool
}

func startViewer(api *webrtc.API, hub string, body offerBody, sent *sendLog, onPkt func(*viewer, *rtp.Packet, int64)) (*viewer, error) {
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	v := &viewer{pc: pc, tStart: time.Now(), onPkt: onPkt}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		pc.Close()
		return nil, err
	}
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			v.tConn.CompareAndSwap(0, time.Now().UnixNano())
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		var idr idrTracker
		var have bool
		var last uint16
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			now := time.Now().UnixNano()
			v.tFirstPkt.CompareAndSwap(0, now)
			v.lastPkt.Store(now)
			if n := v.pkts.Add(1); debugPkts > 0 && n <= uint64(debugPkts) {
				ty, st := nalTypes(p.Payload)
				logf("dbg pkt#%d +%.1fms seq=%d ts=%d m=%v nal=%v fuStart=%v len=%d", n, ms(v.tStart, now), p.SequenceNumber, p.Timestamp, p.Marker, ty, st, len(p.Payload))
			}
			v.bytes.Add(uint64(len(p.Payload)))
			if have {
				if d := p.SequenceNumber - last; d > 1 && d < 0x8000 {
					v.lost.Add(uint64(d - 1))
				}
			}
			if !have || p.SequenceNumber-last < 0x8000 {
				last = p.SequenceNumber
			}
			have = true
			if idr.feed(p.SequenceNumber, p.Timestamp, p.Marker, p.Payload) {
				v.tFirstIDR.Store(now)
			}
			if isKeyStart(p.Payload) {
				v.keyStarts.Add(1)
			}
			if sent != nil && v.latOn.Load() {
				if t0, ok := sent.get(p.Payload); ok {
					if d := float64(now-t0) / 1e6; d >= 0 && d < 5000 {
						v.latMu.Lock()
						v.lat = append(v.lat, d)
						v.latMu.Unlock()
					}
				}
			}
			if v.onPkt != nil {
				v.onPkt(v, p, now)
			}
		}
	})
	if err := negotiate(pc, hub+"/offer/viewer", body); err != nil {
		pc.Close()
		return nil, err
	}
	v.tAnswer = time.Now()
	return v, nil
}

func (v *viewer) close() {
	if v.closed.CompareAndSwap(false, true) {
		v.pc.Close()
	}
}

func (v *viewer) takeLat() []float64 {
	v.latMu.Lock()
	defer v.latMu.Unlock()
	l := v.lat
	v.lat = nil
	return l
}

func ms(from time.Time, ns int64) float64 {
	if ns == 0 {
		return -1
	}
	return float64(ns-from.UnixNano()) / 1e6
}

func logf(format string, a ...any) { log.Printf(format, a...) }

// debugPkts — HUBBENCH_DEBUG_PKTS=N друкує перші N пакетів кожного глядача.
var debugPkts = func() int {
	var n int
	fmt.Sscan(os.Getenv("HUBBENCH_DEBUG_PKTS"), &n)
	return n
}()

// rcvbufNet — stdnet, що ставить SO_RCVBUF на кожен UDP-сокет (HUBBENCH_RCVBUF).
type rcvbufNet struct {
	*stdnet.Net
	n int
}

func (r rcvbufNet) ListenUDP(network string, a *net.UDPAddr) (transport.UDPConn, error) {
	c, err := r.Net.ListenUDP(network, a)
	if err == nil {
		_ = c.SetReadBuffer(r.n)
	}
	return c, err
}

func (r rcvbufNet) ListenPacket(network, address string) (net.PacketConn, error) {
	c, err := r.Net.ListenPacket(network, address)
	if u, ok := c.(*net.UDPConn); err == nil && ok {
		_ = u.SetReadBuffer(r.n)
	}
	return c, err
}
