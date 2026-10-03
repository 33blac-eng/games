// soak_probe — довгоживучий recvonly WebRTC viewer для 8-годинного soak-тесту
// hub-webrtc (кандидат A). На відміну від bench/vtest/main.go (який гасне
// після перших 30 RTP-пакетів), цей клієнт тримає з'єднання ВЕСЬ soak і
// періодично скидає монотонний лічильник отриманих RTP-пакетів у JSON-файл
// стану, який читає bench/soak.py для обчислення frames_delta між семплами.
//
// Автентифікація: T1 static-token режим hub-webrtc (без OO_SCREEN_ERP_BASE) —
// POST {"sdp":..., "token": token} на /offer/viewer, ticket не потрібен.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type stateFile struct {
	Count     uint64 `json:"count"`
	UpdatedAt string `json:"updated_at"`
	Connected bool   `json:"connected"`
}

func writeState(path string, count uint64, connected bool) {
	st := stateFile{Count: count, UpdatedAt: time.Now().UTC().Format(time.RFC3339), Connected: connected}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

func main() {
	signal := flag.String("signal", "http://127.0.0.1:4470/offer/viewer", "POST offer URL")
	token := flag.String("token", "t1-dev-token", "T1 static token")
	state := flag.String("state", "bench/soak_viewer_state.json", "path to JSON state file, rewritten periodically")
	writeEvery := flag.Duration("write-every", 5*time.Second, "how often to flush the counter to the state file")
	loss := flag.Float64("loss", 0, "частка втрат (0..1) на нозі hub->viewer; >0 піднімає локальне UDP-реле і переписує кандидата у відповіді")
	lossAfter := flag.Duration("loss-after", 0, "почати дропати через стільки часу після старту реле")
	lossFor := flag.Duration("loss-for", 0, "дропати стільки часу, потім чисто (0 = до кінця)")
	delay := flag.Duration("delay", 0, "додана однобічна затримка на нозі hub->viewer; >0 піднімає те саме локальне UDP-реле, що й -loss")
	delayAfter := flag.Duration("delay-after", 0, "почати затримувати через стільки часу після старту реле")
	delayFor := flag.Duration("delay-for", 0, "затримувати стільки часу, потім чисто (0 = до кінця)")
	delayRamp := flag.Duration("delay-ramp", 0, "наростити затримку з нуля до -delay за цей час (0 = сходинкою); саме РАМПА і є bufferbloat")
	delayJitter := flag.Duration("delay-jitter", 0, "коливання затримки ±ця амплітуда (синусоїда) — рівно те, чим нормальна мережа відрізняється від loopback")
	delayJitterPeriod := flag.Duration("delay-jitter-period", 4*time.Second, "період коливання -delay-jitter")
	flag.Parse()

	var count uint64
	// Розрив між КАДРАМИ в приймача — рівно те, що міряє сторож 3.0 с у
	// браузері. maxGapMs — за весь прогін, gapWindowMs — за інтервал між
	// друками (щоб стартовий розігрів не ховав спокійну ділянку).
	var frames, maxGapMs, gapWindowMs uint64
	connected := int32(0)

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
	// БЕЗ цього реєстру pion не генерує ЖОДНОГО Receiver Report, а RR — єдине
	// джерело втрат і RTT для контролера бітрейту в хабі. Браузер шле RR завжди;
	// проба без реєстру мовчала, і хаб за 32 с потоку отримав рівно 0 семплів
	// (перевірено цим самим стендом). rtcpTap друкує РІВНО ті поля, які хаб
	// потім читає, — щоб «сигнал живий» був числом, а не здогадкою.
	ir := &interceptor.Registry{}
	// Кран ПЕРШИМ: interceptor.Chain обгортає в порядку додавання, тож перший
	// сидить упритул до транспорту й бачить і те, що пишуть решта interceptor-ів
	// (саме там народжуються RR), і все вхідне RTCP до їхньої обробки.
	tap := &rtcpTap{}
	ir.Add(tapFactory{tap})
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		fmt.Println("interceptors:", err)
		os.Exit(2)
	}
	// B6: вікно SRTP/SRTCP replay — 1024, як у libwebrtc. Дефолт pion 64:
	// NACK-ретрансмісія, що приходить через >64 пакети (800 пак/с x RTT +
	// опит NACK), мовчки відкидається SRTP-шаром — проба бачила б втрату,
	// якої браузер не має (bench/RESULTS-network.md, «Пастки»).
	se := webrtc.SettingEngine{}
	se.SetSRTPReplayProtectionWindow(1024)
	se.SetSRTCPReplayProtectionWindow(1024)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fmt.Println("pc:", err)
		os.Exit(2)
	}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		fmt.Println("transceiver:", err)
		os.Exit(2)
	}
	pc.OnTrack(func(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
		fmt.Println("soak_probe: OnTrack fired — hub published the track")
		atomic.StoreInt32(&connected, 1)
		// ОБОВʼЯЗКОВИЙ read-loop вхідного RTCP. Без нього receiver-interceptor
		// не бачить жодного Sender Report, а отже кладе в RR LastSenderReport=0
		// — і RTT у хабі не виводиться нізвідки. Хаб про цю ж пастку пише у
		// себе (main.go: "інакше interceptors мертві"), проба її не мала.
		go func() {
			buf := make([]byte, 1500)
			for {
				if _, _, err := r.Read(buf); err != nil {
					return
				}
			}
		}()
		// ponytail: межа кадру = зміна мітки RTP. Усі фрагменти одного H.264 AU
		// несуть однакову мітку, тож цього досить; marker-біт нічого не додав би.
		var lastTS uint32
		var lastAt time.Time
		for {
			p, _, err := t.ReadRTP()
			if err != nil {
				fmt.Println("soak_probe: ReadRTP ended:", err)
				atomic.StoreInt32(&connected, 0)
				return
			}
			atomic.AddUint64(&count, 1)
			if p.Timestamp == lastTS && !lastAt.IsZero() {
				continue
			}
			now := time.Now()
			if !lastAt.IsZero() {
				g := uint64(now.Sub(lastAt).Milliseconds())
				if g > atomic.LoadUint64(&maxGapMs) {
					atomic.StoreUint64(&maxGapMs, g)
				}
				if g > atomic.LoadUint64(&gapWindowMs) {
					atomic.StoreUint64(&gapWindowMs, g)
				}
			}
			lastTS, lastAt = p.Timestamp, now
			atomic.AddUint64(&frames, 1)
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Println("soak_probe PC:", s)
		if s != webrtc.PeerConnectionStateConnected {
			atomic.StoreInt32(&connected, 0)
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		fmt.Println("offer:", err)
		os.Exit(2)
	}
	gc := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		fmt.Println("setLocal:", err)
		os.Exit(2)
	}
	<-gc

	offerSDP := pc.LocalDescription().SDP
	var imp *relay
	if *loss > 0 || *delay > 0 || *delayJitter > 0 {
		imp, err = startRelay(impairment{
			loss: *loss, lossAfter: *lossAfter, lossFor: *lossFor,
			delay: *delay, delayAfter: *delayAfter, delayFor: *delayFor, delayRamp: *delayRamp,
			delayJitter: *delayJitter, delayJitterPeriod: *delayJitterPeriod,
		})
		if err != nil {
			fmt.Println("relay:", err)
			os.Exit(2)
		}
		if offerSDP, err = imp.rewrite(offerSDP, imp.up); err != nil {
			fmt.Println("relay offer:", err)
			os.Exit(2)
		}
	}

	body, _ := json.Marshal(map[string]string{"sdp": offerSDP, "token": *token})
	httpResp, err := http.Post(*signal, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Println("POST:", err)
		os.Exit(2)
	}
	rb, _ := io.ReadAll(httpResp.Body)
	httpResp.Body.Close()
	if httpResp.StatusCode != 200 {
		fmt.Printf("soak_probe: offer rejected %d: %s\n", httpResp.StatusCode, string(rb))
		os.Exit(1)
	}
	var ans struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(rb, &ans); err != nil {
		fmt.Println("bad answer:", err, string(rb))
		os.Exit(2)
	}
	sdp := ans.SDP
	if imp != nil {
		if err := imp.learnHub(sdp); err != nil {
			fmt.Println("relay answer:", err)
			os.Exit(2)
		}
		if sdp, err = imp.rewrite(sdp, imp.down); err != nil {
			fmt.Println("relay answer:", err)
			os.Exit(2)
		}
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}); err != nil {
		fmt.Println("setRemote:", err)
		os.Exit(2)
	}

	fmt.Println("soak_probe: connected, holding session and counting RTP packets forever (state:", *state, ")")
	ticker := time.NewTicker(*writeEvery)
	defer ticker.Stop()
	lastStats := time.Now()
	for range ticker.C {
		c := atomic.LoadUint64(&count)
		conn := atomic.LoadInt32(&connected) == 1
		writeState(*state, c, conn)
		// Ті самі числа, з яких приймач будує свій RR, — тобто рівно те, що
		// бачить контролер бітрейту в хабі. Без них «чому ціль не піднялась»
		// лишається здогадкою.
		if time.Since(lastStats) >= 5*time.Second {
			lastStats = time.Now()
			fmt.Printf("gaps: frames=%d maxgap=%dms maxgap_window=%dms\n",
				atomic.LoadUint64(&frames), atomic.LoadUint64(&maxGapMs),
				atomic.SwapUint64(&gapWindowMs, 0))
			printInboundStats(pc)
		}
	}
}

// printInboundStats друкує втрати й jitter вхідного потоку так, як їх рахує
// сам приймач. Jitter у pion — секунди; RR возить такти 90 кГц, тому даємо і
// їх — але вже ЛИШЕ для діагностики: контролер бітрейту за jitter більше не
// керує (177a3e56), рішення приймається за втратами.
func printInboundStats(pc *webrtc.PeerConnection) {
	for _, s := range pc.GetStats() {
		in, ok := s.(webrtc.InboundRTPStreamStats)
		if !ok || in.Kind != "video" {
			continue
		}
		fmt.Printf("stats: received=%d lost=%d nacks=%d jitter=%.1fms (%.0f ticks@90k)\n",
			in.PacketsReceived, in.PacketsLost, in.NACKCount, in.Jitter*1000, in.Jitter*90000)
	}
}

// --- імпейрмент ноги hub->viewer -------------------------------------------
//
// Навіщо власне реле, а не clumsy/netem: clumsy — це WinDivert-драйвер (на цій
// машині не встановлений, і ставити драйвер заради бенчу зайве), а
// bench/netem.sh за власною шапкою «RUNS ON THE VPS ONLY» — тобто на бойовій
// машині, чого локальний прогін робити не має права. Реле дає ті самі
// СПРАВЖНІ дірки в потоці: пакет реально не доходить, приймач сам бачить
// розрив у sequence number і сам рахує FractionLost у своєму RR. Нічого не
// підроблюємо — просто не доставляємо.
//
// ponytail: дроп рівномірний, без Gilbert-Elliott; щоб довести «hub шле
// bitrate_target і повертає ціль назад», цього досить. Пачкова модель
// вставляється на місці, у drop().

// relay — UDP-реле viewer <-> hub із керованим дропом у бік viewer-а.
//
// ДВА сокети, і це принципово. З одним (тільки перед hub-ом) ICE обходить
// реле: hub і так шле STUN-перевірки на КАНДИДАТІВ З OFFER-а, тобто прямо на
// сокет viewer-а, той бачить peer-reflexive кандидата з реальною адресою hub-а,
// валідує цю пару й номінує ЇЇ — медіа йде повз реле (перевірено: forwarded=0
// при 20920 отриманих пакетах). Тому підміняємо кандидатів у ОБОХ описах: hub
// знає лише адресу up, viewer — лише адресу down, і прямої пари просто нема з
// чого скласти.
type relay struct {
	up, down  *net.UDPConn // up дивиться на hub, down — на viewer
	hub       atomic.Pointer[net.UDPAddr]
	viewer    atomic.Pointer[net.UDPAddr]
	loss      float64
	dropFrom  time.Time // з якого моменту дропати
	dropUntil time.Time // до якого (нульовий = до кінця)
	mu        sync.Mutex
	rnd       *rand.Rand
	dropped   uint64
	forwarded uint64

	// --- затримка, ТОЙ САМИЙ зразок, що й втрати: своє вікно й свій лічильник ---
	delay        time.Duration
	delayRamp    time.Duration
	delayJitter  time.Duration
	jitterPeriod time.Duration
	delayFrom    time.Time
	delayUntil   time.Time
	q            chan delayedPkt
	qdropped     uint64
}

// delayedPkt — пакет, який чекає свого часу доставки в лінії затримки.
type delayedPkt struct {
	at   time.Time
	data []byte
}

// impairment — керовані вади ноги hub->viewer. У втрат і затримки КОЖНОЇ своє
// вікно, бо головний сценарій — саме затримка БЕЗ втрат.
type impairment struct {
	loss                           float64
	lossAfter, lossFor             time.Duration
	delay, delayAfter, delayFor    time.Duration
	delayRamp                      time.Duration
	delayJitter, delayJitterPeriod time.Duration
}

// curDelay — скільки затримки діє ЗАРАЗ. Рампа й є bufferbloat: черга на шляху
// наливається поступово, і контролер ловить саме РІСТ приросту, а не рівень.
// Поза вікном — нуль (черга розсмокталась).
func (r *relay) curDelay(now time.Time) time.Duration {
	if (r.delay <= 0 && r.delayJitter <= 0) || now.Before(r.delayFrom) {
		return 0
	}
	if !r.delayUntil.IsZero() && now.After(r.delayUntil) {
		return 0
	}
	d := r.delay
	if r.delayRamp > 0 {
		if el := now.Sub(r.delayFrom); el < r.delayRamp {
			d = time.Duration(float64(r.delay) * float64(el) / float64(r.delayRamp))
		}
	}
	// Коливання — СИНУСОЇДА, а не шум на пакет: справжня черга гойдається цілком,
	// і пакети лишаються в порядку. Порядок тримається, поки |d(jit)/dt| < 1,
	// тобто поки 2*pi*амплітуда < період — для 30 мс / 4 с це з великим запасом.
	if r.delayJitter > 0 && r.jitterPeriod > 0 {
		ph := 2 * math.Pi * float64(now.UnixNano()%int64(r.jitterPeriod)) / float64(r.jitterPeriod)
		d += time.Duration(float64(r.delayJitter) * math.Sin(ph))
		if d < 0 {
			d = 0
		}
	}
	return d
}

// deliver — єдиний писар лінії затримки, тому порядок збережено, поки затримка
// не спадає. ponytail: на спаді (кінець вікна) хвіст черги вийде після свіжих
// пакетів — рівно так поводиться справжня черга, коли розсмоктується.
func (r *relay) deliver() {
	for p := range r.q {
		if w := time.Until(p.at); w > 0 {
			time.Sleep(w)
		}
		if dst := r.viewer.Load(); dst != nil {
			_, _ = r.down.WriteToUDP(p.data, dst)
		}
	}
}

// isMedia — RTP/SRTP і RTCP/SRTCP за RFC 7983 (перший байт 128..191). STUN і
// DTLS у діапазон не потрапляють і вад не отримують: інакше ми б ламали ICE й
// рукостискання, а не моделювали мережу. RTCP входить СВІДОМО — Sender Report
// їде тим самим шляхом, і саме його затримка й дає ріст RTT у приймача.
func isMedia(p []byte) bool { return len(p) > 0 && p[0] >= 128 && p[0] <= 191 }

// dropping — чи діє імпейрмент саме зараз.
func (r *relay) dropping(now time.Time) bool {
	if now.Before(r.dropFrom) {
		return false
	}
	return r.dropUntil.IsZero() || !now.After(r.dropUntil)
}

// drop — чи не доставляти цей пакет. Дропаємо ЛИШЕ медіа (RTP/SRTP: перший
// байт 128..191 за RFC 7983). STUN і DTLS проходять завжди — інакше ми б
// зламали ICE-перевірки й рукостискання, а не змоделювали втрати в потоці.
func (r *relay) drop(p []byte, now time.Time) bool {
	if !isMedia(p) || !r.dropping(now) {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rnd.Float64() < r.loss
}

// startRelay піднімає обидва сокети й насоси. Кликати ДО відправки offer-а:
// адреса up має потрапити в offer, інакше hub її не дізнається.
func startRelay(im impairment) (*relay, error) {
	// Слухаємо на ВСІХ інтерфейсах, а в SDP підміняємо лише ПОРТ, лишаючи IP
	// як був. Інакше сокет pion-а, прибитий до IP інтерфейсу, не докинув би до
	// 127.0.0.1 (Windows такий пакет із чужою source-адресою не доставляє).
	any := &net.UDPAddr{IP: net.IPv4zero}
	up, err := net.ListenUDP("udp4", any)
	if err != nil {
		return nil, fmt.Errorf("listen up: %w", err)
	}
	down, err := net.ListenUDP("udp4", any)
	if err != nil {
		up.Close()
		return nil, fmt.Errorf("listen down: %w", err)
	}
	now := time.Now()
	r := &relay{up: up, down: down, loss: im.loss, dropFrom: now.Add(im.lossAfter),
		rnd:   rand.New(rand.NewSource(now.UnixNano())),
		delay: im.delay, delayRamp: im.delayRamp, delayFrom: now.Add(im.delayAfter),
		delayJitter: im.delayJitter, jitterPeriod: im.delayJitterPeriod,
		// ponytail: 8192 пакетів ~ кілька секунд потоку 60 к/с; переповнення
		// рахуємо ОКРЕМО (qdropped), щоб «затримка» ніколи не перетворилась на
		// «втрати» нишком — це знищило б увесь сенс сценарію без втрат.
		q: make(chan delayedPkt, 8192)}
	if im.lossFor > 0 {
		r.dropUntil = r.dropFrom.Add(im.lossFor)
	}
	if im.delayFor > 0 {
		r.delayUntil = r.delayFrom.Add(im.delayFor)
	}

	go r.pump(down, up, &r.viewer, &r.hub, false) // viewer -> hub: чисто, RR та ICE мають доходити
	go r.pump(up, down, &r.hub, &r.viewer, true)  // hub -> viewer: тут і дропаємо/затримуємо
	go r.deliver()
	go func() {
		for t := range time.Tick(2 * time.Second) {
			r.mu.Lock()
			d, f, qd := r.dropped, r.forwarded, r.qdropped
			r.mu.Unlock()
			fmt.Printf("relay: dropping=%v dropped=%d forwarded=%d delay=%v queued=%d qdropped=%d\n",
				r.dropping(t), d, f, r.curDelay(t).Round(time.Millisecond), len(r.q), qd)
		}
	}()
	fmt.Printf("relay: up=%s down=%s loss=%.3f from +%s for %s | delay=%v from +%s for %s ramp=%s jitter=±%v/%v\n",
		up.LocalAddr(), down.LocalAddr(), im.loss, im.lossAfter, im.lossFor,
		im.delay, im.delayAfter, im.delayFor, im.delayRamp, im.delayJitter, im.delayJitterPeriod)
	return r, nil
}

// pump переливає з in у out, запамʼятовуючи адресу відправника (from) і
// відправляючи на останню відому адресу другого боку (to).
func (r *relay) pump(in, out *net.UDPConn, from, to *atomic.Pointer[net.UDPAddr], lossy bool) {
	buf := make([]byte, 2048)
	for {
		n, src, err := in.ReadFromUDP(buf)
		if err != nil {
			return
		}
		now := time.Now()
		from.Store(src)
		dst := to.Load()
		if dst == nil {
			continue // другий бік ще не озвався — слати нікуди
		}
		if lossy && r.drop(buf[:n], now) {
			r.mu.Lock()
			r.dropped++
			r.mu.Unlock()
			continue
		}
		if lossy {
			r.mu.Lock()
			r.forwarded++
			r.mu.Unlock()
			if d := r.curDelay(now); d > 0 && isMedia(buf[:n]) {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				select {
				case r.q <- delayedPkt{now.Add(d), cp}:
				default:
					r.mu.Lock()
					r.qdropped++
					r.mu.Unlock()
				}
				continue
			}
		}
		_, _ = out.WriteToUDP(buf[:n], dst)
	}
}

// learnHub дістає з answer-а адресу, на яку реле шле все від viewer-а.
func (r *relay) learnHub(sdp string) error {
	f := firstIPv4HostCandidate(sdp)
	if f == nil {
		return fmt.Errorf("в answer немає IPv4 UDP host-кандидата")
	}
	port, err := strconv.Atoi(f[5])
	if err != nil {
		return fmt.Errorf("порт кандидата %q: %w", f[5], err)
	}
	addr := &net.UDPAddr{IP: net.ParseIP(f[4]), Port: port}
	if addr.IP == nil {
		return fmt.Errorf("адреса кандидата %q не парситься", f[4])
	}
	r.hub.Store(addr)
	fmt.Printf("relay: hub candidate = %s\n", addr)
	return nil
}

// rewrite лишає в описі РІВНО ОДНОГО кандидата — сокет реле. Решта прямих
// шляхів викидається: вони й були тим, чим ICE обходив імпейрмент.
func (r *relay) rewrite(sdp string, sock *net.UDPConn) (string, error) {
	f := firstIPv4HostCandidate(sdp)
	if f == nil {
		return "", fmt.Errorf("немає IPv4 UDP host-кандидата, нема що переписувати")
	}
	f[5] = strconv.Itoa(sock.LocalAddr().(*net.UDPAddr).Port) // IP лишається, міняємо порт
	cand := strings.Join(f, " ")

	lines := strings.Split(sdp, "\r\n")
	out := make([]string, 0, len(lines)+1)
	inserted := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "a=candidate:") {
			continue
		}
		if ln == "a=end-of-candidates" && !inserted {
			out, inserted = append(out, cand), true
		}
		out = append(out, ln)
	}
	if !inserted {
		// Без маркера end-of-candidates: кандидат має лишитись у медіа-секції,
		// а вона тут одна — тож кінець SDP і є її кінцем.
		if n := len(out); n > 0 && out[n-1] == "" {
			out = append(out[:n-1], cand, "")
		} else {
			out = append(out, cand)
		}
	}
	return strings.Join(out, "\r\n"), nil
}

// firstIPv4HostCandidate — поля першого IPv4 UDP host-кандидата опису.
func firstIPv4HostCandidate(sdp string) []string {
	for _, ln := range strings.Split(sdp, "\r\n") {
		if !strings.HasPrefix(ln, "a=candidate:") || !strings.Contains(ln, "typ host") {
			continue
		}
		f := strings.Fields(ln)
		if len(f) < 8 || !strings.EqualFold(f[2], "udp") || strings.Contains(f[4], ":") {
			continue // не UDP або IPv6 — реле тримаємо на IPv4
		}
		return f
	}
	return nil
}

// --- RTCP-кран: що САМЕ проба кладе у Receiver Report ---
//
// Питання, заради якого кран існує: чи стоїть у RR НЕНУЛЬОВИЙ LastSenderReport.
// Хаб виводить RTT рівно з нього (hub/cmd/hub-webrtc/rtt.go: RTT = зараз − LSR −
// DLSR), і при LSR == 0 семпла немає взагалі, тобто "rttExcessMs" у ctl-рядку
// назавжди нуль, а захист від bufferbloat існує лише на папері.
//
// LSR приймач може заповнити ТІЛЬКИ якщо реально отримав Sender Report, тож
// ненульовий LSR — це одночасно й доказ, що sender-report interceptor у хабі
// (RegisterDefaultInterceptors) справді шле SR. Одне вимірювання, два факти.
type rtcpTap struct {
	interceptor.NoOp
	srIn atomic.Uint64
}

func (t *rtcpTap) BindRTCPReader(r interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(b []byte, a interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attr, err := r.Read(b, a)
		if err != nil {
			return n, attr, err
		}
		if pkts, e := rtcp.Unmarshal(b[:n]); e == nil {
			for _, pk := range pkts {
				if sr, ok := pk.(*rtcp.SenderReport); ok {
					t.srIn.Add(1)
					fmt.Printf("SR in: ntp=%#016x rtp=%d pkts=%d octets=%d (total SR=%d)\n",
						sr.NTPTime, sr.RTPTime, sr.PacketCount, sr.OctetCount, t.srIn.Load())
				}
			}
		}
		return n, attr, err
	})
}

func (t *rtcpTap) BindRTCPWriter(w interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, a interceptor.Attributes) (int, error) {
		now := time.Now()
		for _, pk := range pkts {
			rr, ok := pk.(*rtcp.ReceiverReport)
			if !ok {
				continue
			}
			for _, rep := range rr.Reports {
				rtt := "НЕМАЄ (LSR=0)"
				if rep.LastSenderReport != 0 {
					// Та сама арифметика, що в хабі, тим самим годинником:
					// число з цього рядка має збігатися з тим, з якого хаб
					// рахує rttExcessMs.
					ticks := ntpMiddle32(now) - rep.LastSenderReport - rep.Delay
					rtt = (time.Duration(ticks) * time.Second / 65536).String()
				}
				fmt.Printf("RR out: LSR=%#08x DLSR=%.1fms fracLost=%d/256 jitter=%d srIn=%d rtt=%s\n",
					rep.LastSenderReport, float64(rep.Delay)*1000/65536,
					rep.FractionLost, rep.Jitter, t.srIn.Load(), rtt)
			}
		}
		return w.Write(pkts, a)
	})
}

// tapFactory віддає ОДИН і той самий кран, щоб лічильник SR був наскрізний.
type tapFactory struct{ t *rtcpTap }

func (f tapFactory) NewInterceptor(string) (interceptor.Interceptor, error) { return f.t, nil }

// ntpMiddle32 — свідома КОПІЯ шести рядків із hub/cmd/hub-webrtc/rtt.go: обидва
// файли — package main у різних бінарях, імпортувати нізвідки. Копія тут і
// потрібна: якби проба рахувала інакше, збіг чисел нічого б не доводив.
func ntpMiddle32(t time.Time) uint32 {
	const epochOffset = 2_208_988_800
	ns := uint64(t.Unix()+epochOffset)*uint64(time.Second) + uint64(t.Nanosecond())
	sec := ns / uint64(time.Second)
	frac := ((ns % uint64(time.Second)) << 32) / uint64(time.Second)
	return uint32(sec<<16) | uint32(frac>>16)
}
