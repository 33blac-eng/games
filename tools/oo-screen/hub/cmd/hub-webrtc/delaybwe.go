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

	"github.com/organicoils/oo-screen/internal/bwe"
)

var delayBWEEnabled = flagOn(os.Getenv("OO_SCREEN_DELAYBWE"))

// Сам детектор (Trendline, AckedRate, розбір TWCC, рішення DelayCut) з N6
// живе в internal/bwe: той самий код крутить агент на прямій нозі. Тут —
// лише проводка viewer-ноги хаба (stamp копії пакета, проба, ns.bitrate).
const (
	delayUpHold      = bwe.DelayUpHold
	delayExtRecheck  = 500 * time.Millisecond // як часто питати SDP про id розширення
	probeDelayAbort  = bwe.ProbeDelayAbort
	delayKFBelow     = bwe.DelayKFBelow
	delayCutDebounce = bwe.DelayCutDebounce
)

// newTrendline — детектор градієнта затримки (internal/bwe).
func newTrendline() bwe.Trendline { return bwe.NewTrendline() }

// twccLeg — transport-cc стан ОДНІЄЇ viewer-ноги. Пише pump (stamp), читає
// RTCP-цикл (onFeedback). Облік відправленого і детектор — bwe.TWCC (свій
// мʼютекс); тут лише id розширення і стеження за пробою.
type twccLeg struct {
	mu     sync.Mutex
	sender *webrtc.RTPSender
	extID  uint8
	extAt  time.Time
	core   *bwe.TWCC
	// probeFor/probeMin — проба, за якою стежимо, і мінімум накопиченої
	// затримки від її старту (probeQueued).
	probeFor *legProbe
	probeMin float64
}

func newTwccLeg(sender *webrtc.RTPSender) *twccLeg {
	return &twccLeg{sender: sender, core: bwe.NewTWCC()}
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
	id := t.extIDLocked(now)
	t.mu.Unlock()
	if id == 0 {
		return pkt
	}
	cp := *pkt
	cp.Header.Extensions = append([]rtp.Extension(nil), pkt.Header.Extensions...)
	// Розмір із розширенням: 2 байти seq + (за першого розширення) заголовок
	// one-byte блоку. Точність тут — та сама, що й раніше (MarshalSize після
	// SetExtension), бо seq має фіксовану довжину.
	if err := cp.Header.SetExtension(id, []byte{0, 0}); err != nil {
		return pkt
	}
	seq := t.core.NextSeq(now, cp.MarshalSize())
	_ = cp.Header.SetExtension(id, []byte{byte(seq >> 8), byte(seq)})
	return &cp
}

// onFeedback — розбір TWCC-фідбеку (bwe.TWCC.OnFeedback).
func (t *twccLeg) onFeedback(fb *rtcp.TransportLayerCC) (acked, sent uint64, winStart time.Time, newOver bool) {
	f := t.core.OnFeedback(fb)
	return f.Acked, f.Sent, f.WinStart, f.Over
}

// probeQueued — чи виросла черга під час проби p (накопичена затримка
// детектора, мс) більше ніж на probeDelayAbort від свого мінімуму від старту.
func (t *twccLeg) probeQueued(p *legProbe, now time.Time) bool {
	acc, ok := t.core.QueueAcc()
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Before(p.start) || !ok {
		return false
	}
	if t.probeFor != p {
		t.probeFor, t.probeMin = p, acc
		return false
	}
	if acc < t.probeMin {
		t.probeMin = acc
	}
	return acc-t.probeMin > probeDelayAbort
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
	if over && bwe.Limited(acked, sent) {
		c.delayOverAt = now
	}
	if now.Before(c.probeMuteUntil) || c.probing {
		// Це наша проба налила чергу — ціль відео не чіпаємо (див. probe.go).
		return c, false
	}
	// Рішення — спільне з прямою ногою агента (internal/bwe.DelayCut).
	next, congested, cut := bwe.DelayCut(bwe.DelayIn{
		Over: over, Acked: acked, Sent: sent, WinStart: winStart, Now: now,
		Target: c.target, LastSent: c.lastSent, LastWasDelay: c.reason == "delay",
		Held: c.delayHeld(now), Floor: minBitrateBps,
	})
	if congested {
		c.congAt = now
	}
	if !cut {
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
