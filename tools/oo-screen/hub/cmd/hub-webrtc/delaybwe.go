package main

// N3 (TZ-GENERAL): ДЕТЕКТОР ПЕРЕВАНТАЖЕННЯ ЗА ГРАДІЄНТОМ ЗАТРИМКИ
// (OO_SCREEN_DELAYBWE=1, дефолт ВИМКНЕНО).
//
// Чому мало того, що вже є. Усі наявні входи контролера (bitrate.go) — RR раз
// на ~1 с: FractionLost, RTT з LSR/DLSR, preLoss із NACK. Під сталою стелею
// 2-4 Мбіт/с із чергою 100 мс черга наливається за десятки мілісекунд, і
// перший сигнал, який бачить контролер, — уже дропи (і PLI/фриз). Плюс
// приріст RTT із RR — один семпл на секунду, з мінімумом трьох (pushRTT).
//
// Що тут. Те саме, що робить libwebrtc (GCC, TrendlineEstimator +
// OveruseDetector) на transport-cc:
//   - pump ноги ставить кожному відео-пакету transport-wide seq (розширення
//     заголовка, узгоджене в SDP) і запамʼятовує момент і розмір відправки;
//   - глядач шле TWCC-фідбек (Chrome — сам, як тільки узгоджено transport-cc;
//     pion — ConfigureTWCCSender): момент приходу кожного пакета;
//   - пакети групуються в «сплески» по 5 мс відправки, для сусідніх груп
//     d = Δприходу − Δвідправки; накопичена d згладжується (0.9) і по вікну з
//     20 груп рахується нахил лінійної регресії — тренд ЧЕРГИ, що росте;
//   - modified_trend = min(N,60)·нахил·4 порівнюється з АДАПТИВНИМ порогом
//     (6..600 мс; росте повільно 0.0087, спадає швидко 0.039, як у libwebrtc).
//     Перевищення ≥10 мс і ≥2 групи поспіль при ненаспадному нахилі = OVERUSE;
//   - заодно — доставлена швидкість (acked) за останні 500 мс приходу.
//
// Як це вплітається в контролер (bitrateCtl.withDelay):
//   - OVERUSE ріже ціль до 0.85 × acked (GCC: beta = 0.85 від виміряної
//     пропускної), не частіше ніж раз на delayCutDebounce; IDR не замовляється;
//   - delayUpHold після останнього OVERUSE підйом (повільний/швидкий) і
//     проба (probe.go) заборонені — вони б лише знову налили ту саму чергу;
//   - OVERUSE під час проби = проба невдала, обривається ОДРАЗУ (раніше —
//     лише після NACK, коли дропи вже сталися), ціль відео не ріжеться;
//   - втрати/RTT/B4 лишаються як були: детектор — ще одна причина різати, а
//     не заміна. При вимкненому прапорці жоден рядок тут не виконується.
//
// Пастка, яку тут враховано: при прапорці codec оголошує transport-cc, і Chrome
// ПЕРЕСТАЄ слати REMB (див. newAPI) — тобто ця оцінка заміняє REMB, а не
// додається до нього. Тому дефолт OFF до перевірки в живому Chrome.

import (
	"os"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

var delayBWEEnabled = flagOn(os.Getenv("OO_SCREEN_DELAYBWE"))

const (
	tccRing        = 1 << 12 // відправлені пакети в памʼяті (≥ 4 с на 8 Мбіт/с)
	tccBurst       = 5 * time.Millisecond
	trendWindow    = 20
	trendSmoothing = 0.9
	trendGain      = 4.0
	trendThrInit   = 12.5
	trendThrMin    = 6.0
	trendThrMax    = 600.0
	trendKUp       = 0.0087
	trendKDown     = 0.039
	overuseTimeMs  = 10.0
	ackedWindow    = 500 * time.Millisecond
	ackedRecent    = 200 * time.Millisecond // коротке вікно: acked = min(500 мс, 200 мс)

	delayBeta        = 0.85                   // ціль = acked × це на OVERUSE
	delayLimited     = 0.9                    // acked < sent × це = впираємось у канал
	delayCutDebounce = 300 * time.Millisecond // між зрізами по затримці (+ свіже вікно)
	delayUpHold      = 3 * time.Second        // після OVERUSE — без підйомів і проб
	delayExtRecheck  = 500 * time.Millisecond // як часто питати SDP про id розширення
	probeDelayAbort  = 25.0                   // мс черги понад мінімум за пробу — обрив
	delaySevere      = 0.6                    // acked < sent × це — ріжемо й без нового OVERUSE
	delayRecentAt    = 0.85                   // acked < sent × це — довіряємо короткому вікну
	delayKFBelow     = 0.5                    // зріз до ≤ цієї частки — разом з keyframe_request
	delayLag         = 1.2                    // sent > ціль × це після зрізу — енкодер ще не догнав
	delayCutMin      = 0.2                    // найглибший один крок (8 -> 1.6 Мбіт/с за раз)
)

type bwState int

const (
	bwNormal bwState = iota
	bwOverusing
	bwUnderusing
)

// trendline — ЧИСТИЙ (без годинника й мережі) оцінювач тренду затримки.
type trendline struct {
	haveGrp, havePrev        bool
	grpFirstSend, grpSend    time.Time
	grpArr                   time.Time
	prevSend, prevArr        time.Time
	firstArr                 time.Time
	acc, smooth              float64
	hist                     [][2]float64
	numDeltas                int
	thr                      float64
	thrAt                    time.Time
	overTime                 float64
	overCnt                  int
	prevTrend, lastTrend, mt float64
	state                    bwState
	fired                    bool // OVERUSE щойно підтверджено (споживає onFeedback)
}

func newTrendline() trendline { return trendline{thr: trendThrInit, overTime: -1} }

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// add — один отриманий пакет у порядку відправки. Повертає поточний стан.
func (t *trendline) add(send, arr time.Time) bwState {
	if !t.haveGrp {
		t.haveGrp = true
		t.grpFirstSend, t.grpSend, t.grpArr = send, send, arr
		return t.state
	}
	if send.Before(t.grpSend) {
		return t.state // переставлений — у групу вже не годиться
	}
	if send.Sub(t.grpFirstSend) <= tccBurst {
		t.grpSend = send
		if arr.After(t.grpArr) {
			t.grpArr = arr
		}
		return t.state
	}
	// Група завершена.
	if t.havePrev {
		sd := t.grpSend.Sub(t.prevSend)
		ad := t.grpArr.Sub(t.prevArr)
		t.update(ms(ad)-ms(sd), ms(sd), t.grpArr)
	}
	t.havePrev = true
	t.prevSend, t.prevArr = t.grpSend, t.grpArr
	t.grpFirstSend, t.grpSend, t.grpArr = send, send, arr
	return t.state
}

func (t *trendline) update(deltaMs, sendDeltaMs float64, arr time.Time) {
	if t.numDeltas < 1000 {
		t.numDeltas++
	}
	if t.firstArr.IsZero() {
		t.firstArr = arr
	}
	t.acc += deltaMs
	t.smooth = trendSmoothing*t.smooth + (1-trendSmoothing)*t.acc
	t.hist = append(t.hist, [2]float64{ms(arr.Sub(t.firstArr)), t.smooth})
	if len(t.hist) > trendWindow {
		t.hist = t.hist[1:]
	}
	trend := t.prevTrend
	if len(t.hist) == trendWindow {
		if s, ok := linregSlope(t.hist); ok {
			trend = s
		}
	}
	t.lastTrend = trend
	t.detect(trend, sendDeltaMs, arr)
}

func linregSlope(pts [][2]float64) (float64, bool) {
	var sx, sy float64
	for _, p := range pts {
		sx += p[0]
		sy += p[1]
	}
	n := float64(len(pts))
	mx, my := sx/n, sy/n
	var num, den float64
	for _, p := range pts {
		num += (p[0] - mx) * (p[1] - my)
		den += (p[0] - mx) * (p[0] - mx)
	}
	if den == 0 {
		return 0, false
	}
	return num / den, true
}

func (t *trendline) detect(trend, sendDeltaMs float64, arr time.Time) {
	if t.numDeltas < 2 {
		t.state = bwNormal
		return
	}
	n := float64(t.numDeltas)
	if n > 60 {
		n = 60
	}
	mt := n * trend * trendGain
	t.mt = mt
	switch {
	case mt > t.thr:
		if t.overTime == -1 {
			t.overTime = sendDeltaMs / 2
		} else {
			t.overTime += sendDeltaMs
		}
		t.overCnt++
		if t.overTime > overuseTimeMs && t.overCnt > 1 && trend >= t.prevTrend {
			t.overTime, t.overCnt = 0, 0
			t.state = bwOverusing
			t.fired = true
		}
	case mt < -t.thr:
		t.overTime, t.overCnt = -1, 0
		t.state = bwUnderusing
	default:
		t.overTime, t.overCnt = -1, 0
		t.state = bwNormal
	}
	t.prevTrend = trend
	t.updateThr(mt, arr)
}

func (t *trendline) updateThr(mt float64, arr time.Time) {
	if t.thrAt.IsZero() {
		t.thrAt = arr
	}
	a := mt
	if a < 0 {
		a = -a
	}
	if a > t.thr+15 {
		t.thrAt = arr // викид (стрибок шляху) — поріг не тягнемо
		return
	}
	k := trendKUp
	if a < t.thr {
		k = trendKDown
	}
	dt := ms(arr.Sub(t.thrAt))
	if dt > 100 {
		dt = 100
	}
	if dt < 0 {
		dt = 0
	}
	t.thr += k * (a - t.thr) * dt
	if t.thr < trendThrMin {
		t.thr = trendThrMin
	}
	if t.thr > trendThrMax {
		t.thr = trendThrMax
	}
	t.thrAt = arr
}

// ackedRate — доставлена швидкість за ackedWindow часу приходу.
type ackedRate struct {
	pts   []ackPt
	bytes int
}

type ackPt struct {
	at, send time.Time
	seq      uint16
	size     int
}

func (a *ackedRate) add(at, send time.Time, seq uint16, size int) {
	a.pts = append(a.pts, ackPt{at, send, seq, size})
	a.bytes += size
	// Приходи в межах фідбеку монотонні; між фідбеками — майже.
	for len(a.pts) > 1 && at.Sub(a.pts[0].at) > ackedWindow {
		a.bytes -= a.pts[0].size
		a.pts = a.pts[1:]
	}
}

// bps — 0, поки вікно не набралось хоча б наполовину.
func (a *ackedRate) bps() uint64 {
	if len(a.pts) < 2 {
		return 0
	}
	span := a.pts[len(a.pts)-1].at.Sub(a.pts[0].at)
	if span < ackedWindow/2 {
		return 0
	}
	return uint64(float64(a.bytes-a.pts[0].size) * 8 / span.Seconds())
}

// recentBps — доставлена швидкість за останні d часу приходу (≥ d/2 даних,
// інакше 0). Під перевантаженням це і є пропускна вузького місця «зараз»;
// 500-мс вікно на першому OVERUSE ще бачить трафік до появи стелі.
func (a *ackedRate) recentBps(d time.Duration) uint64 {
	n := len(a.pts)
	if n < 2 {
		return 0
	}
	last := a.pts[n-1].at
	i := n - 1
	for i > 0 && last.Sub(a.pts[i-1].at) <= d {
		i--
	}
	span := last.Sub(a.pts[i].at)
	if span < d/2 {
		return 0
	}
	bytes := 0
	for _, p := range a.pts[i+1:] {
		bytes += p.size
	}
	return uint64(float64(bytes) * 8 / span.Seconds())
}

type tccSent struct {
	seq  uint16
	ok   bool
	at   time.Time
	size int
}

// twccLeg — transport-cc стан ОДНІЄЇ viewer-ноги. Пише pump (stamp), читає
// RTCP-цикл (onFeedback) — тому свій мʼютекс, не ns.mu.
type twccLeg struct {
	mu     sync.Mutex
	sender *webrtc.RTPSender
	extID  uint8
	extAt  time.Time
	next   uint16
	sent   [tccRing]tccSent
	lastFb uint16
	haveFb bool
	est    trendline
	acked  ackedRate
	overAt time.Time // останній OVERUSE
	overs  int
	// probeFor/probeMin — проба, за якою стежимо, і мінімум накопиченої
	// затримки від її старту (probeQueued).
	probeFor *legProbe
	probeMin float64
	feedback int
}

func newTwccLeg(sender *webrtc.RTPSender) *twccLeg {
	return &twccLeg{sender: sender, est: newTrendline()}
}

// extIDLocked — id узгодженого transport-cc розширення; 0 — не узгоджено.
func (t *twccLeg) extIDLocked(now time.Time) uint8 {
	if t.extID != 0 || t.sender == nil || now.Sub(t.extAt) < delayExtRecheck {
		return t.extID
	}
	t.extAt = now
	for _, h := range t.sender.GetParameters().HeaderExtensions {
		if h.URI == sdp.TransportCCURI {
			t.extID = uint8(h.ID)
		}
	}
	return t.extID
}

// stamp — копія пакета з transport-wide seq (оригінал спільний для всіх ніг,
// його не чіпаємо). Без узгодженого розширення — пакет як є.
func (t *twccLeg) stamp(pkt *rtp.Packet, now time.Time) *rtp.Packet {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.extIDLocked(now)
	if id == 0 {
		return pkt
	}
	cp := *pkt
	cp.Header.Extensions = append([]rtp.Extension(nil), pkt.Header.Extensions...)
	seq := t.next
	t.next++
	if err := cp.Header.SetExtension(id, []byte{byte(seq >> 8), byte(seq)}); err != nil {
		t.next--
		return pkt
	}
	t.sent[int(seq)%tccRing] = tccSent{seq: seq, ok: true, at: now, size: cp.MarshalSize()}
	return &cp
}

// sentBpsLocked — швидкість ВІДПРАВКИ (разом із втраченими) за той самий
// відрізок seq, що й вікно acked. acked < sent = частина відправленого не
// доходить вчасно: черга росте або дропи, тобто ми впираємось у канал. 0 —
// замало даних.
func (t *twccLeg) sentBpsLocked() uint64 {
	p := t.acked.pts
	if len(p) < 2 {
		return 0
	}
	first, last := p[0], p[len(p)-1]
	span := last.send.Sub(first.send)
	n := int(uint16(last.seq - first.seq))
	if span < ackedWindow/4 || n <= 0 || n >= tccRing {
		return 0
	}
	bytes := 0
	for i := 1; i <= n; i++ {
		sq := first.seq + uint16(i)
		if s := t.sent[int(sq)%tccRing]; s.ok && s.seq == sq {
			bytes += s.size
		}
	}
	return uint64(float64(bytes) * 8 / span.Seconds())
}

// onFeedback — розбір TWCC-фідбеку. Повертає acked і sent (біт/с) та чи
// стався НОВИЙ (щойно підтверджений) OVERUSE у цьому фідбеку.
// winStart — момент ВІДПРАВКИ першого пакета вікна acked: контролер не ріже
// вдруге по вікну, яке ще бачить трафік до попереднього зрізу.
func (t *twccLeg) onFeedback(fb *rtcp.TransportLayerCC) (acked, sent uint64, winStart time.Time, newOver bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.feedback++
	ref := time.Unix(0, 0).Add(time.Duration(fb.ReferenceTime) * 64 * time.Millisecond)
	arr := ref
	di := 0
	seq := fb.BaseSequenceNumber
	remaining := int(fb.PacketStatusCount)
	handle := func(sym uint16) {
		received := sym == rtcp.TypeTCCPacketReceivedSmallDelta || sym == rtcp.TypeTCCPacketReceivedLargeDelta
		if received && di < len(fb.RecvDeltas) {
			arr = arr.Add(time.Duration(fb.RecvDeltas[di].Delta) * time.Microsecond)
			di++
			fresh := !t.haveFb || int16(seq-t.lastFb) > 0
			if s := t.sent[int(seq)%tccRing]; fresh && s.ok && s.seq == seq {
				t.est.add(s.at, arr)
				if t.est.fired {
					t.est.fired = false
					newOver = true
				}
				t.acked.add(arr, s.at, seq, s.size)
				t.lastFb, t.haveFb = seq, true
			}
		}
		seq++
		remaining--
	}
	for _, c := range fb.PacketChunks {
		if remaining <= 0 {
			break
		}
		switch ch := c.(type) {
		case *rtcp.RunLengthChunk:
			for i := 0; i < int(ch.RunLength) && remaining > 0; i++ {
				handle(ch.PacketStatusSymbol)
			}
		case *rtcp.StatusVectorChunk:
			for _, s := range ch.SymbolList {
				if remaining <= 0 {
					break
				}
				handle(s)
			}
		}
	}
	if newOver {
		t.overs++
	}
	if len(t.acked.pts) > 0 {
		winStart = t.acked.pts[0].send
	}
	acked, sent = t.acked.bps(), t.sentBpsLocked()
	// Коротке вікно — лише при явному перевантаженні (acked < delayRecentAt×sent):
	// без нього 200 мс на змінному контенті (IDR, паузи кадрів) дає випадково
	// низькі значення, і стеля 8M різалась до ~4 Мбіт/с (заміряно).
	if float64(acked) < delayRecentAt*float64(sent) {
		if r := t.acked.recentBps(ackedRecent); r > 0 && r < acked {
			acked = r
		}
	}
	return acked, sent, winStart, newOver
}

// probeQueued — чи виросла черга під час проби p (накопичена затримка
// детектора, мс) більше ніж на probeDelayAbort від свого мінімуму від старту.
func (t *twccLeg) probeQueued(p *legProbe, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Before(p.start) || !t.est.havePrev {
		return false
	}
	if t.probeFor != p {
		t.probeFor, t.probeMin = p, t.est.acc
		return false
	}
	if t.est.acc < t.probeMin {
		t.probeMin = t.est.acc
	}
	return t.est.acc-t.probeMin > probeDelayAbort
}

// withDelay — ЧИСТА, на кожен TWCC-фідбек. Канал «впирається», коли
// acked < delayLimited×sent: частина відправленого за вікно не дійшла (черга
// росте або дропи). Лише OVERUSE РАЗОМ з цим відкриває епізод на delayUpHold
// (без підйомів і проб); OVERUSE без цього — сплеск IDR нижче стелі (черга
// налилась і за ~100 мс спала, за 500 мс доставлено все відправлене), і він
// нічого не тримає, інакше IDR раз на 2 с заморозив би підйом назавжди.
// Поки епізод триває і канал впирається, ціль = delayBeta×acked, але:
//   - не глибше за congCutMin×ціль за крок;
//   - лише по вікну, що ЦІЛКОМ після попередньої зміни цілі (winStart), і не
//     частіше за delayCutDebounce — інакше вікно з трафіком до зрізу або з
//     дропами перехідного процесу різало б удруге «за те саме».
//
// Друга умова тримає зрізання, поки черга повна й пласка (градієнт нуль, а
// дропи йдуть) — саме там чистий детектор тренду мовчить.
func (c bitrateCtl) withDelay(over bool, acked, sent uint64, winStart, now time.Time) (bitrateCtl, bool) {
	limited := acked > 0 && sent > 0 && float64(acked) < delayLimited*float64(sent)
	if over && limited {
		c.delayOverAt = now
	}
	if now.Before(c.probeMuteUntil) || c.probing {
		// Це наша проба налила чергу — ціль відео не чіпаємо (див. probe.go).
		return c, false
	}
	severe := acked > 0 && sent > 0 && float64(acked) < delaySevere*float64(sent)
	if !(over && limited) && !(severe && c.delayHeld(now)) {
		return c, false
	}
	c.congAt = now
	// Енкодер ще не догнав попередній зріз (шле помітно більше за ціль):
	// надлишок — його запізнення, а не новий затор; різати глибше — лише
	// недобір після того, як він догонить (заміряно на стенді: каскад 8 ->
	// 4 -> 2 -> 1 Мбіт/с під стелею 2M).
	if !c.lastSent.IsZero() && c.reason == "delay" && float64(sent) > delayLag*float64(c.target) {
		return c, false
	}
	next := uint64(float64(acked) * delayBeta)
	if lo := uint64(float64(c.target) * delayCutMin); next < lo {
		next = lo
	}
	if next < minBitrateBps {
		next = minBitrateBps
	}
	if next >= c.target {
		return c, false
	}
	if !c.lastSent.IsZero() && (now.Sub(c.lastSent) < delayCutDebounce || !winStart.After(c.lastSent)) {
		return c, false
	}
	c.cutFrom = c.target
	c.probeLvl = c.target
	c.upRun = 0
	c.probeLastOK = false
	c.goodSince, c.cleanSince = time.Time{}, time.Time{}
	c.lastUpAt, c.probeFrom = time.Time{}, 0
	c.target, c.lastSent = next, now
	c.reason = "delay"
	return c, true
}

// delayHeld — чи тримає детектор затримки підйом/пробу зараз.
func (c bitrateCtl) delayHeld(now time.Time) bool {
	return !c.delayOverAt.IsZero() && now.Sub(c.delayOverAt) < delayUpHold
}

// onTWCC — обгортка: фідбек ноги -> детектор -> контролер. Кликати без ns.mu.
func onTWCC(ns *nodeSession, vl *viewerLeg, fb *rtcp.TransportLayerCC, now time.Time) {
	t := vl.tcc.Load()
	if t == nil {
		return
	}
	acked, sent, winStart, over := t.onFeedback(fb)
	// Проба саме йде — OVERUSE або черга, що виросла на probeDelayAbort від
	// мінімуму за пробу, означає, що вона вже переповнює чергу: обриваємо
	// одразу, не чекаючи NACK (вердикт probeFailed). Дублі проби без
	// transport-cc seq, але черга від них затримує відео-пакети, які його мають.
	if p := vl.probe.Load(); p != nil && !p.aborted.Load() {
		if t.probeQueued(p, now) || over {
			p.aborted.Store(true)
			ndjsonf(`{"leg":"delay","node":%q,"probeAbort":true,"over":%t}`+"\n", ns.nodeID, over)
		}
	}
	ns.mu.Lock()
	if ns.bitrate.startBps == 0 {
		ns.bitrate = newBitrateCtl(ns.ceilingBps())
	}
	prev := ns.bitrate.target
	next, send := ns.bitrate.withDelay(over, acked, sent, winStart, now)
	ns.bitrate = next
	ns.mu.Unlock()
	if over || send {
		ndjsonf(`{"leg":"delay","node":%q,"acked":%d,"sent":%d,"bitrate":%d,"over":%t,"cut":%t}`+"\n",
			ns.nodeID, acked, sent, next.target, over, send)
	}
	if send {
		// Глибокий зріз (ціль ≤ delayKFBelow від попередньої): відправлене
		// вище за канал уже тоне в черзі, глядач однаково попросить PLI — IDR
		// замовляємо одразу, і він іде вже за новою, малою ціллю. На стенді
		// (драбина з перемиканням лише на IDR) це прибирає до GOP (2 с)
		// надлишку після зрізу; MFT перемикає QP і без IDR, тож там це лише
		// випереджений PLI. Дрібні зрізи IDR не тягнуть (див. sendBitrateTarget).
		kf := float64(next.target) <= delayKFBelow*float64(prev)
		sendBitrateTarget(ns, next.target, 0, 0, 0, kf)
		metricsNoteBitrate(ns, prev, "delay", 0)
	}
}

// videoFeedback — rtcp-fb відеокодека. З OO_SCREEN_DELAYBWE=1 додається
// transport-cc (Chrome тоді шле TWCC замість REMB — див. newAPI).
func videoFeedback() []webrtc.RTCPFeedback {
	fb := []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "goog-remb"},
	}
	if delayBWEEnabled {
		fb = append(fb, webrtc.RTCPFeedback{Type: "transport-cc"})
	}
	return fb
}
