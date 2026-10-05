package main

// N5 (TZ-GENERAL §4.2): глядач за суворим офісним firewall-ом — UDP заборонено
// ЦІЛКОМ, назовні відкрито лише TCP 443. Тести нижче ганяють ПОВНИЙ шлях
// агент -> хаб -> глядач у процесі:
//
//   - глядач — pion із SettingEngine.SetNetworkTypes(TCP4): жодного UDP-
//     кандидата, тобто рівно те, що лишається браузеру за таким firewall-ом;
//   - між глядачем і хабом стоїть tcpFront — заміна nginx `stream` на 443:
//     слухає «зовнішній» порт і проксіює байти на внутрішній ICE-TCP порт хаба
//     (OO_SCREEN_ICE_TCP_PORT). Хаб оголошує в answer-і САМЕ порт фронту
//     (OO_SCREEN_ICE_TCP_ADVERTISE_PORT), а не свій, — інакше глядач стукав би
//     в порт, якого firewall не пропускає. Порт фронту в тесті — довільний
//     вільний (bind 443 вимагав би root і конфліктував би з чужим nginx);
//     механіка переписування та сама.
//
// TestN5TCPOnlyViewerPlays — швидкий, крутиться завжди (гейт CI).
// TestN5TCPvsUDPUnderLoss — заміри HOL-ціни TCP при 1 % втрат; довгий, тому
// лише з OO_SCREEN_N5_BENCH=1 (числа — bench/RESULTS-network.md, «N5»).

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// ── синтетичне H.264 з міткою кадру ─────────────────────────────────────────

const (
	n5FPS      = 60
	n5GOP      = 120   // IDR раз на 2 с
	n5IDRBytes = 40000 // ~4 Мбіт/с сумарно
	n5PBytes   = 7000
)

var n5SPS = []byte{0x67, 0x42, 0xC0, 0x1F, 0x8C, 0x8D, 0x40}
var n5PPS = []byte{0x68, 0xCE, 0x3C, 0x80}

// n5Slice — NAL кадру seq: заголовок + ASCII-мітка "OON5|seq|unixnano|I/P|" +
// детермінований наповнювач. Байти мітки й наповнювача ніколи не дають
// 00 00 0x (емуляції start-code немає), тож Annex-B у samplebuilder-і
// розрізається однозначно і кадр можна звірити побайтно.
func n5Slice(seq int, sentNs int64, idr bool) []byte {
	size := n5PBytes
	kind := "P"
	hdr := byte(0x41) // non-IDR slice, nal_ref_idc=2
	if idr {
		size, hdr, kind = n5IDRBytes, 0x65, "I"
	}
	b := make([]byte, 0, size)
	b = append(b, hdr)
	b = append(b, fmt.Sprintf("OON5|%d|%d|%s|", seq, sentNs, kind)...)
	r := rand.New(rand.NewSource(int64(seq)))
	for len(b) < size {
		b = append(b, byte(0x10+r.Intn(0xEF)))
	}
	return b
}

// n5AU — Access Unit у Annex-B: для IDR — SPS+PPS+IDR, інакше лише slice.
func n5AU(seq int, sentNs int64, idr bool) []byte {
	sc := []byte{0, 0, 0, 1}
	var au []byte
	if idr {
		au = append(au, sc...)
		au = append(au, n5SPS...)
		au = append(au, sc...)
		au = append(au, n5PPS...)
	}
	au = append(au, sc...)
	return append(au, n5Slice(seq, sentNs, idr)...)
}

// n5Parse дістає з отриманого AU мітку кадру і перевіряє, що slice побайтно
// той самий, що агент закодував. ok=false — кадр битий (не декодовний).
func n5Parse(au []byte) (seq int, sentNs int64, idr, ok bool) {
	i := strings.Index(string(au), "OON5|")
	if i < 1 {
		return 0, 0, false, false
	}
	f := strings.SplitN(string(au[i:min(len(au), i+64)]), "|", 5)
	if len(f) < 5 {
		return 0, 0, false, false
	}
	idr = f[3] == "I"
	s, err1 := strconv.Atoi(f[1])
	ns, err2 := strconv.ParseInt(f[2], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false, false
	}
	want := n5Slice(s, ns, idr)
	got := au[i-1:]
	if len(got) != len(want) || string(got) != string(want) {
		return s, ns, idr, false
	}
	if idr && !strings.Contains(string(au[:i]), string(n5SPS)) {
		return s, ns, idr, false
	}
	return s, ns, idr, true
}

// ── агент, що шле семпли ───────────────────────────────────────────────────

func n5DialAgent(t *testing.T, node string) (*webrtc.TrackLocalStaticSample, *atomic.Bool) {
	t.Helper()
	remote, err := agentAPI(t).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("agent pc: %v", err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	ctl, err := remote.CreateDataChannel("oosc-ctl", nil)
	if err != nil {
		t.Fatalf("ctl: %v", err)
	}
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpLine,
	}, "video", "oo-screen")
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	sender, err := remote.AddTrack(track)
	if err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	// Як живий агент: PLI/FIR від хаба -> наступний кадр IDR. Без цього новий
	// глядач чекав би планового IDR до 2 с, і TTFF міряв би GOP, а не мережу.
	wantIDR := &atomic.Bool{}
	wantIDR.Store(true)
	// Основний шлях хаба — keyframe_request по oosc-ctl (bitrate.go
	// requestKeyframe); PLI лише фолбек.
	ctl.OnMessage(func(m webrtc.DataChannelMessage) {
		if strings.Contains(string(m.Data), "keyframe") {
			wantIDR.Store(true)
		}
	})
	go func() {
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				switch p.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					wantIDR.Store(true)
					// Основний шлях хаба — keyframe_request по oosc-ctl (bitrate.go
					// requestKeyframe); PLI лише фолбек.
					ctl.OnMessage(func(m webrtc.DataChannelMessage) {
						if strings.Contains(string(m.Data), "keyframe") {
							wantIDR.Store(true)
						}
					})
				}
			}
		}
	}()
	offer, _ := remote.CreateOffer(nil)
	g := webrtc.GatheringCompletePromise(remote)
	if err := remote.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-g
	body, _ := json.Marshal(offerReq{SDP: remote.LocalDescription().SDP, Token: token, Node: node})
	w := httptest.NewRecorder()
	handleOffer("agent")(w, httptest.NewRequest(http.MethodPost, "/offer/agent", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("/offer/agent: %d %s", w.Code, w.Body.String())
	}
	var ans answerResp
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if err := remote.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
		t.Fatalf("agent SetRemoteDescription: %v", err)
	}
	return track, wantIDR
}

// n5Pump шле AU з частотою n5FPS до stop.
func n5Pump(track *webrtc.TrackLocalStaticSample, wantIDR *atomic.Bool, stop <-chan struct{}) {
	tick := time.NewTicker(time.Second / n5FPS)
	defer tick.Stop()
	for seq := 0; ; seq++ {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		idr := seq%n5GOP == 0 || wantIDR.Swap(false)
		_ = track.WriteSample(media.Sample{Data: n5AU(seq, time.Now().UnixNano(), idr), Duration: time.Second / n5FPS})
	}
}

// ── імпейрменти ────────────────────────────────────────────────────────────

// n5Impair — модель лінка hub->глядач: однобічна затримка oneWay (вона ж
// ефективний RTT для відновлення, бо зворотний шлях у тесті миттєвий) і
// рівномірна втрата loss.
type n5Impair struct {
	oneWay time.Duration
	loss   float64
	// tcpPenalty — скільки ЗУПИНЯЄ потік TCP одна втрата сегмента. Ядро тут
	// loopback і нічого не губить, тож HOL моделюється явно: усе, що за
	// втраченим сегментом, чекає ретрансмісії. fast-retransmit ≈ 1×RTT,
	// хвостова втрата — RTO (Linux min 200 мс).
	tcpPenalty time.Duration
}

// tcpFront — заміна nginx `stream` на 443: TCP-проксі на хаб. Вниз (хаб ->
// глядач) розбирає кадри RFC 4571 (2 байти довжини + пакет), щоб втрата
// «сегмента» затримувала рівно з того пакета і ВСЕ, що за ним (head-of-line).
type tcpFront struct {
	ln       net.Listener
	conns    atomic.Int64
	bytesDn  atomic.Int64
	stalls   atomic.Int64
	imp      n5Impair
	backend  string
	rnd      *rand.Rand
	rndMu    sync.Mutex
	closing  chan struct{}
	closeOne sync.Once
}

func newTCPFront(t *testing.T, backend string, imp n5Impair) *tcpFront {
	t.Helper()
	ln, err := net.Listen("tcp4", ":0")
	if err != nil {
		t.Fatalf("front listen: %v", err)
	}
	f := &tcpFront{ln: ln, imp: imp, backend: backend, rnd: rand.New(rand.NewSource(5)), closing: make(chan struct{})}
	go f.serve()
	t.Cleanup(f.close)
	return f
}

func (f *tcpFront) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *tcpFront) close() {
	f.closeOne.Do(func() { close(f.closing); _ = f.ln.Close() })
}

func (f *tcpFront) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.conns.Add(1)
		go f.handle(c)
	}
}

func (f *tcpFront) lost() bool {
	if f.imp.loss <= 0 {
		return false
	}
	f.rndMu.Lock()
	defer f.rndMu.Unlock()
	return f.rnd.Float64() < f.imp.loss
}

func (f *tcpFront) handle(c net.Conn) {
	defer c.Close()
	b, err := net.Dial("tcp", f.backend)
	if err != nil {
		return
	}
	defer b.Close()
	go func() { _, _ = io.Copy(b, c); _ = b.Close() }()

	type item struct {
		buf []byte
		at  time.Time
	}
	q := make(chan item, 4096)
	go func() {
		defer c.Close()
		for it := range q {
			if d := time.Until(it.at); d > 0 {
				time.Sleep(d)
			}
			if _, err := c.Write(it.buf); err != nil {
				return
			}
			f.bytesDn.Add(int64(len(it.buf)))
		}
	}()
	defer close(q)
	var release time.Time
	hdr := make([]byte, 2)
	for {
		if _, err := io.ReadFull(b, hdr); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(hdr))
		buf := make([]byte, 2+n)
		copy(buf, hdr)
		if _, err := io.ReadFull(b, buf[2:]); err != nil {
			return
		}
		at := time.Now().Add(f.imp.oneWay)
		if at.Before(release) {
			at = release // TCP: суворий порядок, ніхто не обганяє
		}
		// Лише медіа (RTP/RTCP, RFC 7983: 128..191) — STUN/DTLS не губимо,
		// інакше міряли б невдале рукостискання, а не HOL на відео.
		if n > 0 && buf[2] >= 128 && buf[2] <= 191 && f.lost() {
			at = at.Add(f.imp.tcpPenalty)
			f.stalls.Add(1)
		}
		release = at
		select {
		case q <- item{buf, at}:
		case <-f.closing:
			return
		}
	}
}

// lossyUDP — PacketConn глядача з втратою і затримкою на ВХОДІ (хаб ->
// глядач). Той самий n5Impair, що й у tcpFront, тож порівняння чесне: різниця
// лише в тому, що UDP-втрата б'є по одному пакету, а не по всьому потоку.
type lossyUDP struct {
	net.PacketConn
	imp    n5Impair
	q      chan udpItem
	rnd    *rand.Rand
	drops  atomic.Int64
	closed chan struct{}
	once   sync.Once
}

type udpItem struct {
	buf  []byte
	addr net.Addr
	at   time.Time
}

func newLossyUDP(t *testing.T, imp n5Impair) *lossyUDP {
	t.Helper()
	pc, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		t.Fatalf("viewer udp: %v", err)
	}
	l := &lossyUDP{PacketConn: pc, imp: imp, q: make(chan udpItem, 8192), rnd: rand.New(rand.NewSource(5)), closed: make(chan struct{})}
	go func() {
		for {
			buf := make([]byte, 1600)
			n, a, err := pc.ReadFrom(buf)
			if err != nil {
				l.once.Do(func() { close(l.closed) })
				return
			}
			if n > 0 && buf[0] >= 128 && buf[0] <= 191 && l.rnd.Float64() < imp.loss {
				l.drops.Add(1)
				continue
			}
			select {
			case l.q <- udpItem{buf[:n], a, time.Now().Add(imp.oneWay)}:
			default: // переповнення черги = втрата, як у ядра
			}
		}
	}()
	t.Cleanup(func() { _ = pc.Close() })
	return l
}

func (l *lossyUDP) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case it := <-l.q:
		if d := time.Until(it.at); d > 0 {
			time.Sleep(d)
		}
		return copy(p, it.buf), it.addr, nil
	case <-l.closed:
		return 0, nil, net.ErrClosed
	}
}

// ── глядач ─────────────────────────────────────────────────────────────────

type n5Result struct {
	name              string
	transport         string // протокол обраної пари з боку глядача
	ttff              time.Duration
	sent, ok, bad     int
	lat               []time.Duration // затримка декодовних кадрів
	maxFreeze         time.Duration
	frontConns        int64
	tcpStalls, udpDrp int64
}

func (r n5Result) pct(q float64) time.Duration {
	if len(r.lat) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), r.lat...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[min(len(s)-1, int(q*float64(len(s))))]
}

func (r n5Result) String() string {
	return fmt.Sprintf("%-22s via=%s ttff=%v sent=%d decodable=%d (%.1f%%) bad=%d p50=%v p95=%v p99=%v maxFreeze=%v frontConns=%d tcpStalls=%d udpDrops=%d",
		r.name, r.transport, r.ttff.Round(time.Millisecond), r.sent, r.ok, 100*float64(r.ok)/float64(max(1, r.sent)), r.bad,
		r.pct(.5).Round(time.Millisecond), r.pct(.95).Round(time.Millisecond), r.pct(.99).Round(time.Millisecond),
		r.maxFreeze.Round(time.Millisecond), r.frontConns, r.tcpStalls, r.udpDrp)
}

// n5Run: хаб уже має агента в ноді agentNodeIDEnv; піднімає глядача з
// обмеженнями tcpOnly/udp і крутить dur, рахуючи кадри.
func n5Run(t *testing.T, name string, tcpOnly bool, imp n5Impair, dur time.Duration) n5Result {
	t.Helper()
	res := n5Result{name: name}

	se := webrtc.SettingEngine{}
	// Вікно SRTP replay 1024, як у libwebrtc/браузера (і як у хаба, B6). З
	// дефолтними 64 pion-глядач мовчки викидав пізні NACK-ретрансмісії, і UDP
	// при 1 % виглядав гірше, ніж він є у браузері (перший прогін: 93.6 %
	// декодовних, p95 911 мс — артефакт стенду, не мережі).
	se.SetSRTPReplayProtectionWindow(1024)
	var front *tcpFront
	var ludp *lossyUDP
	if tcpOnly {
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeTCP4})
		// Фронт стукає в хаб НЕ на 127.0.0.1, а на IP інтерфейсу, з якого
		// хаб оголошує кандидата: TCP-mux pion шукає агента за (ufrag,
		// ЛОКАЛЬНА IP з'єднання), і з'єднання на loopback не знаходить
		// жодної пари («Failed to ping without candidate pairs»). Перший
		// прогін тесту впав саме на цьому — у nginx `proxy_pass` теж мусить
		// бути IP інтерфейсу, див. deploy/DEPLOY.md, «N5».
		front = newTCPFront(t, net.JoinHostPort(n5HostIP(t), strconv.Itoa(int(iceTCPPort))), imp)
		prev := iceTCPAdvertisePort
		iceTCPAdvertisePort = uint16(front.port())
		t.Cleanup(func() { iceTCPAdvertisePort = prev })
	} else {
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
		ludp = newLossyUDP(t, imp)
		se.SetICEUDPMux(webrtc.NewICEUDPMux(nil, ludp))
	}
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	// Дефолтні інтерцептори pion (NACK, RTCP-звіти), як у браузера: без NACK
	// UDP-втрата була б незворотною і порівняння нечесним.
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		t.Fatal(err)
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("viewer pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}

	type got struct {
		seq  int
		sent int64
		idr  bool
		ok   bool
		at   time.Time
	}
	frames := make(chan got, 4096)
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		sb := samplebuilder.New(512, &codecs.H264Packet{}, 90000)
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			sb.Push(p)
			for s := sb.Pop(); s != nil; s = sb.Pop() {
				seq, ns, idr, ok := n5Parse(s.Data)
				frames <- got{seq, ns, idr, ok, time.Now()}
			}
		}
	})

	start := time.Now()
	offer, _ := pc.CreateOffer(nil)
	g := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-g
	if tcpOnly && strings.Contains(strings.ToLower(pc.LocalDescription().SDP), " udp ") {
		t.Fatalf("TCP-only глядач оголосив UDP-кандидата:\n%s", pc.LocalDescription().SDP)
	}
	body, _ := json.Marshal(offerReq{SDP: pc.LocalDescription().SDP, Token: token})
	w := httptest.NewRecorder()
	handleOffer("viewer")(w, httptest.NewRequest(http.MethodPost, "/offer/viewer", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("/offer/viewer: %d %s", w.Code, w.Body.String())
	}
	var ans answerResp
	_ = json.Unmarshal(w.Body.Bytes(), &ans)
	if tcpOnly {
		if !strings.Contains(ans.SDP, " "+strconv.Itoa(front.port())+" typ host tcptype passive") {
			t.Fatalf("answer не оголошує TCP-кандидата на порту фронту %d:\n%s", front.port(), ans.SDP)
		}
		if strings.Contains(ans.SDP, " "+strconv.Itoa(int(iceTCPPort))+" typ host tcptype passive") {
			t.Fatalf("answer оголошує ВНУТРІШНІЙ порт ICE-TCP %d — firewall його не пропустить:\n%s", iceTCPPort, ans.SDP)
		}
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
		t.Fatalf("viewer SetRemoteDescription: %v", err)
	}

	// Чекаємо перший ДЕКОДОВНИЙ кадр (TTFF), далі міряємо dur.
	deadline := time.After(25 * time.Second)
	var first *got
	for first == nil {
		select {
		case f := <-frames:
			if f.ok && f.idr { // декодер стартує лише з IDR
				first = &f
			}
		case <-deadline:
			t.Fatalf("%s: жодного декодовного кадру за 25 с (ICE=%s)", name, pc.ICEConnectionState())
		}
	}
	res.ttff = first.at.Sub(start)
	if pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
		res.transport = pair.Local.Protocol.String()
	}

	// Ланцюг декодування: P-кадр декодовний, лише якщо цілий і ВСІ кадри від
	// останнього IDR теж були цілими.
	chainOK := false
	lastSeq := -1
	var lastOkAt time.Time
	minSeq, maxSeq := -1, -1
	end := time.After(dur)
	handle := func(f got) {
		if minSeq < 0 {
			minSeq = f.seq
		}
		if f.seq > maxSeq {
			maxSeq = f.seq
		}
		if f.idr {
			chainOK = f.ok
		} else if !f.ok || f.seq != lastSeq+1 {
			chainOK = false
		}
		lastSeq = f.seq
		if f.ok && chainOK {
			res.ok++
			res.lat = append(res.lat, f.at.Sub(time.Unix(0, f.sent)))
			if !lastOkAt.IsZero() && f.at.Sub(lastOkAt) > res.maxFreeze {
				res.maxFreeze = f.at.Sub(lastOkAt)
			}
			lastOkAt = f.at
		} else {
			res.bad++
			if os.Getenv("N5DBG") != "" {
				t.Logf("bad seq=%d ok=%v chain=%v last=%d", f.seq, f.ok, chainOK, lastSeq)
			}
		}
	}
	// Перший кадр може бути P (кеш GOP відтворюється з IDR, але про всяк):
	chainOK = true
	lastSeq = first.seq
	minSeq, maxSeq = first.seq, first.seq
	res.ok, lastOkAt = 1, first.at
loop:
	for {
		select {
		case f := <-frames:
			handle(f)
		case <-end:
			break loop
		}
	}
	res.sent = maxSeq - minSeq + 1
	if front != nil {
		res.frontConns, res.tcpStalls = front.conns.Load(), front.stalls.Load()
	}
	if ludp != nil {
		res.udpDrp = ludp.drops.Load()
	}
	return res
}

// n5HostIP — перша не-loopback IPv4 (та, з якої pion оголошує host-кандидатів).
func n5HostIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	t.Skip("немає не-loopback IPv4 — pion не оголосить жодного host-кандидата")
	return ""
}

// n5Setup: свіжий реєстр, ICE-TCP на вільному порту (тестовий слухач
// закривається в Cleanup і Once скидається), агент шле кадри.
func n5Setup(t *testing.T) {
	t.Helper()
	prevReg, prevPort := reg, iceTCPPort
	reg = newRegistry()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	iceTCPPort = uint16(ln.Addr().(*net.TCPAddr).Port)
	_ = ln.Close()
	iceTCPMux.once = sync.Once{}
	iceTCPMux.apply, iceTCPMux.err, iceTCPMux.ln = nil, nil, nil
	t.Cleanup(func() {
		if iceTCPMux.ln != nil {
			_ = iceTCPMux.ln.Close()
		}
		iceTCPMux.once = sync.Once{}
		iceTCPMux.apply, iceTCPMux.err, iceTCPMux.ln = nil, nil, nil
		reg, iceTCPPort = prevReg, prevPort
	})

	track, wantIDR := n5DialAgent(t, agentNodeIDEnv)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go n5Pump(track, wantIDR, stop)
}

// TestN5TCPOnlyViewerPlays — глядач без жодного UDP доходить до хаба через
// «443-фронт» і отримує декодовні кадри.
func TestN5TCPOnlyViewerPlays(t *testing.T) {
	n5Setup(t)
	r := n5Run(t, "tcp-only clean", true, n5Impair{}, 3*time.Second)
	t.Log(r)
	if r.transport != "tcp" {
		t.Fatalf("обрана пара %q, want tcp", r.transport)
	}
	if r.frontConns == 0 {
		t.Fatal("жодного з'єднання через фронт — медіа пройшло повз «443»")
	}
	if r.ok < 2*n5FPS {
		t.Fatalf("за 3 с лише %d декодовних кадрів (%s)", r.ok, r)
	}
	if float64(r.ok) < 0.95*float64(r.sent) {
		t.Fatalf("декодовних кадрів %d із %d — чистий TCP мав би доставити все", r.ok, r.sent)
	}
}

// TestAdvertiseICETCPPortOnlyTouchesTCPListenerPort — переписування SDP.
func TestAdvertiseICETCPPortOnlyTouchesTCPListenerPort(t *testing.T) {
	prevP, prevA := iceTCPPort, iceTCPAdvertisePort
	t.Cleanup(func() { iceTCPPort, iceTCPAdvertisePort = prevP, prevA })
	sdp := "v=0\r\n" +
		"a=candidate:1 1 udp 2130706431 10.0.0.5 4443 typ host\r\n" +
		"a=candidate:2 1 tcp 1671430143 10.0.0.5 4443 typ host tcptype passive\r\n" +
		"a=candidate:3 1 tcp 1671430143 10.0.0.5 9 typ host tcptype active\r\n" +
		"a=candidate:4 1 TCP 1671430143 10.0.0.6 4443 typ host tcptype passive\r\n"
	iceTCPPort, iceTCPAdvertisePort = 4443, 0
	if got := advertiseICETCPPort(sdp); got != sdp {
		t.Fatalf("без ADVERTISE_PORT SDP змінився:\n%s", got)
	}
	iceTCPAdvertisePort = 443
	got := advertiseICETCPPort(sdp)
	want := strings.NewReplacer(
		"10.0.0.5 4443 typ host tcptype", "10.0.0.5 443 typ host tcptype",
		"10.0.0.6 4443 typ host tcptype", "10.0.0.6 443 typ host tcptype",
	).Replace(sdp)
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(got, "udp 2130706431 10.0.0.5 4443 typ host") {
		t.Fatal("UDP-кандидат переписано")
	}
}

// TestN5TCPvsUDPUnderLoss — HOL-ціна TCP: той самий лінк (RTT 40 мс, 1 %
// втрат на напрямку хаб -> глядач) по UDP+NACK і по TCP-443.
func TestN5TCPvsUDPUnderLoss(t *testing.T) {
	if os.Getenv("OO_SCREEN_N5_BENCH") != "1" {
		t.Skip("заміри N5 — лише з OO_SCREEN_N5_BENCH=1")
	}
	dur := 20 * time.Second
	if v, err := time.ParseDuration(os.Getenv("OO_SCREEN_N5_DUR")); err == nil {
		dur = v
	}
	const ow = 40 * time.Millisecond
	cases := []struct {
		name string
		tcp  bool
		imp  n5Impair
	}{
		{"udp clean", false, n5Impair{oneWay: ow}},
		{"tcp clean", true, n5Impair{oneWay: ow}},
		{"udp 1% +NACK", false, n5Impair{oneWay: ow, loss: .01}},
		{"tcp 1% fast-retx", true, n5Impair{oneWay: ow, loss: .01, tcpPenalty: ow + 10*time.Millisecond}},
		{"tcp 1% RTO 200ms", true, n5Impair{oneWay: ow, loss: .01, tcpPenalty: 200 * time.Millisecond}},
	}
	for _, c := range cases {
		t.Run(strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			n5Setup(t)
			r := n5Run(t, c.name, c.tcp, c.imp, dur)
			t.Log(r)
			fmt.Println("N5RESULT", r)
		})
	}
}
