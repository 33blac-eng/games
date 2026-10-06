// loadgen — скільки глядачів на ОДНУ ноду тримає ця машина, і що впирається
// першим. Цифра «~50 сесій на VPS» досі приходила з чужої критики й ніколи не
// мірялась; це інструмент, яким її можна перевірити, а не ще одна оцінка.
//
// ЧОМУ НЕ ФЕРМА БРАУЗЕРІВ. Питання стоїть про ХАБ: syscall на пакет, SRTP,
// GC, локи, копіювання на кожного глядача. Браузер до цієї відповіді додає
// лише декодер H.264 — найдорожчу й найменш релевантну частину. Тому глядач
// тут — pion recvonly, який НЕ ДЕКОДУЄ, а лише читає RTP і рахує розриви seq.
//
// ПУБЛІКАТОР ТЕЖ СИНТЕТИЧНИЙ, і це навмисно: oo-agent знімає екран через DXGI,
// тобто його темп залежить від того, що відбувається на екрані. Для питання
// «скільки глядачів» джерело має бути МЕТРОНОМОМ, інакше не відрізниш просідання
// хаба від просідання захоплення. Тут це рівний потік -pps пакетів по -size байт.
//
// ЩО ЧЕСНО МІРЯЄТЬСЯ. Клієнт живе на тій самій машині, що й хаб, і сам платить
// за SRTP на КОЖНУ ногу. Тому число «глядачів до першого дропу» — це стеля
// ПАРИ хаб+клієнт на одному боксі, тобто НИЖНЯ оцінка стелі самого хаба. Щоб
// не сплутати, хто впав, звіт друкує CPU обох процесів окремо (-cpu), і при
// розборі дивитись треба саме на цей рядок.
//
// ЗАПУСК (T1 static-token, без ERP):
//
//	hub-webrtc.exe                       # в іншому вікні
//	loadgen -viewers 50 -dur 20s
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const h264FmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64002a"

var (
	hubURL    = flag.String("hub", "http://127.0.0.1:4470", "база URL хаба")
	authToken = flag.String("token", "t1-dev-token", "OO_SCREEN_T1_TOKEN хаба")
	nodeID    = flag.String("node", "", "node_id публікатора (порожній = T1-фолбек хаба)")
	viewers   = flag.Int("viewers", 10, "скільки глядачів на цю ноду")
	dur       = flag.Duration("dur", 20*time.Second, "вікно вимірювання після того, як усі підключились")
	pps       = flag.Int("pps", 830, "темп публікатора, пакетів/с (830 x 1200Б ~ 8 Мбіт/с)")
	size      = flag.Int("size", 1100, "розмір payload, байт")
	connGap   = flag.Duration("gap", 20*time.Millisecond, "пауза між підключеннями глядачів")
)

// offerBody — тіло /offer/* хаба (див. offerReq у hub-webrtc).
type offerBody struct {
	SDP     string `json:"sdp"`
	Token   string `json:"token"`
	Node    string `json:"node,omitempty"`
	Bitrate uint64 `json:"bitrate,omitempty"`
}

func newAPI() (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	// Той самий PT 102 і той самий fmtp, що й у хаба, інакше нема на чому
	// зійтись; nack у feedback — щоб ланцюг interceptor-ів був як у браузера.
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: h264FmtpLine,
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
	// Лише UDP4: хаб збирає і v4, і v6, але для пари на localhost вистачає
	// однієї сімʼї, а кожна зайва — ще сокет і ще горутина на КОЖНУ з N ніг.
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(se)), nil
}

// negotiate робить offer, віддає його хабу і ставить відповідь.
func negotiate(pc *webrtc.PeerConnection, path string, body offerBody) error {
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
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	// Таймаут обовʼязковий: обробник /offer/* хаба блокується на
	// GatheringCompletePromise, і коли в нього закінчуються UDP-порти
	// (SetEphemeralUDPPortRange), відповідь просто не приходить. Без таймауту
	// вимірювач замість числа дає зависання.
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Post(*hubURL+path, "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s -> %s: %s", path, resp.Status, bytes.TrimSpace(raw))
	}
	var ans struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return err
	}
	return pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP})
}

// viewer — одна recvonly-нога. Лічильники атомарні: їх читає головна горутина,
// поки читач ще крутиться.
type viewer struct {
	id   int
	pc   *webrtc.PeerConnection
	pkts uint64 // прийнято RTP-пакетів
	lost uint64 // сума розривів seq (скільки пакетів не прийшло)
	dead uint64 // 1 = потік обірвався до кінця вимірювання

	connected chan struct{}
	once      sync.Once
}

func (v *viewer) read(tr *webrtc.TrackRemote) {
	var last uint16
	started := false
	for {
		pkt, _, err := tr.ReadRTP()
		if err != nil {
			atomic.StoreUint64(&v.dead, 1)
			return
		}
		atomic.AddUint64(&v.pkts, 1)
		if started {
			// Розрив = скільки seq пропущено. Ретрансмісія (той самий seq
			// пізніше) дасть diff з половини діапазону — це не втрата, а
			// дублікат, і в lost вона не рахується.
			if diff := pkt.SequenceNumber - last; diff > 1 && diff < 1<<15 {
				atomic.AddUint64(&v.lost, uint64(diff-1))
			}
		}
		if !started || pkt.SequenceNumber-last < 1<<15 {
			last = pkt.SequenceNumber
			started = true
		}
	}
}

func startViewer(id int) (*viewer, error) {
	api, err := newAPI()
	if err != nil {
		return nil, err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	v := &viewer{id: id, pc: pc, connected: make(chan struct{})}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	}); err != nil {
		_ = pc.Close()
		return nil, err
	}
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { go v.read(tr) })
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			v.once.Do(func() { close(v.connected) })
		}
	})
	if err := negotiate(pc, "/offer/viewer", offerBody{Token: *authToken}); err != nil {
		_ = pc.Close()
		return nil, err
	}
	return v, nil
}

// startPublisher піднімає агентську ногу і повертає трек, у який лити RTP.
func startPublisher() (*webrtc.PeerConnection, *webrtc.TrackLocalStaticRTP, error) {
	api, err := newAPI()
	if err != nil {
		return nil, nil, err
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, err
	}
	trk, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: h264FmtpLine,
	}, "video", "loadgen")
	if err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	sender, err := pc.AddTrack(trk)
	if err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	// RTCP-цикл агентської ноги обовʼязковий: без нього ланцюг interceptor-ів
	// не крутиться, і PLI від хаба нікуди не приходить.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
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
	bps := uint64(*pps) * uint64(*size+12) * 8
	if err := negotiate(pc, "/offer/agent", offerBody{Token: *authToken, Node: *nodeID, Bitrate: bps}); err != nil {
		_ = pc.Close()
		return nil, nil, err
	}
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		_ = pc.Close()
		return nil, nil, fmt.Errorf("публікатор не підключився за 10с")
	}
	return pc, trk, nil
}

// publish ллє рівний потік. Тікер грубіший за міжпакетний інтервал навмисно: на
// Windows таймер має роздільність ~15 мс, тож просити 1.2 мс безглуздо — темп
// тримається дозуванням «скільки пакетів мало б піти на цей момент».
func publish(trk *webrtc.TrackLocalStaticRTP, sent *uint64, stop <-chan struct{}) {
	payload := make([]byte, *size)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	start := time.Now()
	var seq uint16
	var n uint64
	tsStep := uint32(90000 / *pps)
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			due := uint64(now.Sub(start).Seconds() * float64(*pps))
			for n < due {
				pkt := &rtp.Packet{
					Header: rtp.Header{
						Version:        2,
						PayloadType:    102,
						SequenceNumber: seq,
						Timestamp:      uint32(n) * tsStep,
						SSRC:           0x5EED,
					},
					Payload: payload,
				}
				if err := trk.WriteRTP(pkt); err != nil {
					return
				}
				seq++
				n++
				atomic.StoreUint64(sent, n)
			}
		}
	}
}

func main() {
	flag.Parse()
	if *pps <= 0 || *size <= 0 || *viewers <= 0 {
		fmt.Fprintln(os.Stderr, "-pps, -size і -viewers мають бути > 0")
		os.Exit(2)
	}

	pubPC, trk, err := startPublisher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "публікатор: %v\n", err)
		os.Exit(1)
	}
	defer pubPC.Close()

	stop := make(chan struct{})
	var published uint64
	go publish(trk, &published, stop)

	// Підключення глядачів по одному з паузою: залп із 50 offer одночасно
	// міряв би ICE-сходження, а не пропускну здатність fanout-у.
	legs := make([]*viewer, 0, *viewers)
	var failed int
	for i := 1; i <= *viewers; i++ {
		v, err := startViewer(i)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "глядач %d не підключився: %v\n", i, err)
			continue
		}
		legs = append(legs, v)
		time.Sleep(*connGap)
	}
	defer func() {
		for _, v := range legs {
			_ = v.pc.Close()
		}
	}()

	// Чекаємо Connected на всіх, хто взагалі отримав answer. Дедлайн —
	// АБСОЛЮТНИЙ момент, а не один time.After на весь цикл: канал від
	// time.After віддає значення РІВНО ОДИН раз, тож спільний на всі ноги він
	// після першого ж таймауту блокував би вимірювач назавжди — саме так
	// перший прогін на 100 глядачів і завис замість того, щоб дати число.
	deadline := time.Now().Add(20 * time.Second)
	var notConnected int
	for _, v := range legs {
		select {
		case <-v.connected:
		case <-time.After(time.Until(deadline)):
			notConnected++
		}
	}
	time.Sleep(500 * time.Millisecond) // хай устоїться перший потік

	// Вікно вимірювання: рахуємо ДЕЛЬТИ, щоб пакети, пропущені під час
	// підключення сусідів, не сідали в підсумок як «втрати».
	base := make([]uint64, len(legs))
	baseLost := make([]uint64, len(legs))
	for i, v := range legs {
		base[i] = atomic.LoadUint64(&v.pkts)
		baseLost[i] = atomic.LoadUint64(&v.lost)
	}
	basePub := atomic.LoadUint64(&published)
	t0 := time.Now()

	time.Sleep(*dur)

	elapsed := time.Since(t0)
	pubDelta := atomic.LoadUint64(&published) - basePub
	close(stop)

	type row struct {
		id         int
		got, lost  uint64
		dead       bool
		deliveryPc float64
	}
	rows := make([]row, 0, len(legs))
	var dead, lossy int
	for i, v := range legs {
		r := row{
			id:   v.id,
			got:  atomic.LoadUint64(&v.pkts) - base[i],
			lost: atomic.LoadUint64(&v.lost) - baseLost[i],
			dead: atomic.LoadUint64(&v.dead) == 1,
		}
		if pubDelta > 0 {
			r.deliveryPc = float64(r.got) / float64(pubDelta) * 100
		}
		if r.dead {
			dead++
		}
		if r.lost > 0 || r.deliveryPc < 99.5 {
			lossy++
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].deliveryPc < rows[b].deliveryPc })

	fmt.Printf("\n=== loadgen: %d глядач(ів) запитано, %d підключено, %d без answer, %d не дійшли до Connected ===\n",
		*viewers, len(legs), failed, notConnected)
	fmt.Printf("вікно %.1fс, публікатор віддав %d пакетів (%.0f пак/с, ~%.1f Мбіт/с на джерелі)\n",
		elapsed.Seconds(), pubDelta, float64(pubDelta)/elapsed.Seconds(),
		float64(pubDelta)*float64(*size+12)*8/elapsed.Seconds()/1e6)
	fmt.Printf("ніг обірвалось: %d; ніг із втратами: %d\n", dead, lossy)

	fmt.Println("найгірші 5 ніг (доставка %, втрачено, обірвана):")
	for i, r := range rows {
		if i >= 5 {
			break
		}
		fmt.Printf("  #%-3d %6.2f%%  lost=%-7d dead=%v\n", r.id, r.deliveryPc, r.lost, r.dead)
	}
	if n := len(rows); n > 0 {
		fmt.Printf("медіана доставки: %.2f%%, найкраща: %.2f%%\n",
			rows[n/2].deliveryPc, rows[n-1].deliveryPc)
	}
}
