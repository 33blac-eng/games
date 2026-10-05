// Package bwe — спільна логіка оцінки смуги для хаба (viewer-ноги) і агента
// (пряма нога N6). Тут живе те, що раніше було лише в hub-webrtc/delaybwe.go і
// bitrate.go: детектор перевантаження за градієнтом затримки (Trendline,
// libwebrtc TrendlineEstimator + OveruseDetector), доставлена швидкість
// (AckedRate), розбір transport-cc фідбеку (TWCC), зріз цілі по затримці
// (DelayCut) і константи контролера по втратах. Хаб і агент кличуть ОДИН код —
// розійтись по порогах вони не можуть за побудовою.
//
// Пакет чистий: без мережі й без глобального годинника (час — аргументом).
package bwe

import (
	"sync"
	"time"

	"github.com/pion/rtcp"
)

// Ручки детектора (історія замірів — у hub-webrtc/delaybwe.go).
const (
	TCCRing        = 1 << 12 // відправлені пакети в памʼяті (≥ 4 с на 8 Мбіт/с)
	TCCBurst       = 5 * time.Millisecond
	TrendWindow    = 20
	TrendSmoothing = 0.9
	TrendGain      = 4.0
	TrendThrInit   = 12.5
	TrendThrMin    = 6.0
	TrendThrMax    = 600.0
	TrendKUp       = 0.0087
	TrendKDown     = 0.039
	OveruseTimeMs  = 10.0
	AckedWindow    = 500 * time.Millisecond
	AckedRecent    = 200 * time.Millisecond // коротке вікно: acked = min(500 мс, 200 мс)

	DelayBeta        = 0.85                   // ціль = acked × це на OVERUSE
	DelayLimited     = 0.9                    // acked < sent × це = впираємось у канал
	DelayCutDebounce = 300 * time.Millisecond // між зрізами по затримці (+ свіже вікно)
	DelayUpHold      = 3 * time.Second        // після OVERUSE — без підйомів і проб
	ProbeDelayAbort  = 25.0                   // мс черги понад мінімум за пробу — обрив
	DelaySevere      = 0.6                    // acked < sent × це — ріжемо й без нового OVERUSE
	DelayRecentAt    = 0.85                   // acked < sent × це — довіряємо короткому вікну
	DelayKFBelow     = 0.5                    // зріз до ≤ цієї частки — разом з keyframe_request
	DelayLag         = 1.2                    // sent > ціль × це після зрізу — енкодер ще не догнав
	DelayCutMin      = 0.2                    // найглибший один крок (8 -> 1.6 Мбіт/с за раз)
)

// State — стан детектора.
type State int

const (
	Normal State = iota
	Overusing
	Underusing
)

// Trendline — ЧИСТИЙ (без годинника й мережі) оцінювач тренду затримки.
type Trendline struct {
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
	state                    State
	fired                    bool // OVERUSE щойно підтверджено (споживає TakeFired)
}

// NewTrendline — детектор у початковому стані.
func NewTrendline() Trendline { return Trendline{thr: TrendThrInit, overTime: -1} }

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// Add — один отриманий пакет у порядку відправки. Повертає поточний стан.
func (t *Trendline) Add(send, arr time.Time) State {
	if !t.haveGrp {
		t.haveGrp = true
		t.grpFirstSend, t.grpSend, t.grpArr = send, send, arr
		return t.state
	}
	if send.Before(t.grpSend) {
		return t.state // переставлений — у групу вже не годиться
	}
	if send.Sub(t.grpFirstSend) <= TCCBurst {
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

// TakeFired — чи підтверджено НОВИЙ OVERUSE з минулого виклику (споживає).
func (t *Trendline) TakeFired() bool {
	f := t.fired
	t.fired = false
	return f
}

// Fired — те саме без споживання.
func (t *Trendline) Fired() bool { return t.fired }

// State — поточний стан.
func (t *Trendline) State() State { return t.state }

// Acc — накопичена затримка черги, мс; ok=false — ще немає пари груп.
func (t *Trendline) Acc() (float64, bool) { return t.acc, t.havePrev }

func (t *Trendline) update(deltaMs, sendDeltaMs float64, arr time.Time) {
	if t.numDeltas < 1000 {
		t.numDeltas++
	}
	if t.firstArr.IsZero() {
		t.firstArr = arr
	}
	t.acc += deltaMs
	t.smooth = TrendSmoothing*t.smooth + (1-TrendSmoothing)*t.acc
	t.hist = append(t.hist, [2]float64{ms(arr.Sub(t.firstArr)), t.smooth})
	if len(t.hist) > TrendWindow {
		t.hist = t.hist[1:]
	}
	trend := t.prevTrend
	if len(t.hist) == TrendWindow {
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

func (t *Trendline) detect(trend, sendDeltaMs float64, arr time.Time) {
	if t.numDeltas < 2 {
		t.state = Normal
		return
	}
	n := float64(t.numDeltas)
	if n > 60 {
		n = 60
	}
	mt := n * trend * TrendGain
	t.mt = mt
	switch {
	case mt > t.thr:
		if t.overTime == -1 {
			t.overTime = sendDeltaMs / 2
		} else {
			t.overTime += sendDeltaMs
		}
		t.overCnt++
		if t.overTime > OveruseTimeMs && t.overCnt > 1 && trend >= t.prevTrend {
			t.overTime, t.overCnt = 0, 0
			t.state = Overusing
			t.fired = true
		}
	case mt < -t.thr:
		t.overTime, t.overCnt = -1, 0
		t.state = Underusing
	default:
		t.overTime, t.overCnt = -1, 0
		t.state = Normal
	}
	t.prevTrend = trend
	t.updateThr(mt, arr)
}

func (t *Trendline) updateThr(mt float64, arr time.Time) {
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
	k := TrendKUp
	if a < t.thr {
		k = TrendKDown
	}
	dt := ms(arr.Sub(t.thrAt))
	if dt > 100 {
		dt = 100
	}
	if dt < 0 {
		dt = 0
	}
	t.thr += k * (a - t.thr) * dt
	if t.thr < TrendThrMin {
		t.thr = TrendThrMin
	}
	if t.thr > TrendThrMax {
		t.thr = TrendThrMax
	}
	t.thrAt = arr
}

// AckedRate — доставлена швидкість за AckedWindow часу приходу.
type AckedRate struct {
	pts   []ackPt
	bytes int
}

type ackPt struct {
	at, send time.Time
	seq      uint16
	size     int
}

// Add — один доставлений пакет.
func (a *AckedRate) Add(at, send time.Time, seq uint16, size int) {
	a.pts = append(a.pts, ackPt{at, send, seq, size})
	a.bytes += size
	// Приходи в межах фідбеку монотонні; між фідбеками — майже.
	for len(a.pts) > 1 && at.Sub(a.pts[0].at) > AckedWindow {
		a.bytes -= a.pts[0].size
		a.pts = a.pts[1:]
	}
}

// Bps — 0, поки вікно не набралось хоча б наполовину.
func (a *AckedRate) Bps() uint64 {
	if len(a.pts) < 2 {
		return 0
	}
	span := a.pts[len(a.pts)-1].at.Sub(a.pts[0].at)
	if span < AckedWindow/2 {
		return 0
	}
	return uint64(float64(a.bytes-a.pts[0].size) * 8 / span.Seconds())
}

// RecentBps — доставлена швидкість за останні d часу приходу (≥ d/2 даних,
// інакше 0). Під перевантаженням це і є пропускна вузького місця «зараз».
func (a *AckedRate) RecentBps(d time.Duration) uint64 {
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

// TWCC — transport-cc стан ОДНІЄЇ ноги: що відправлено (NextSeq/Record) і що
// з цього прийшло (OnFeedback). Пише відправник, читає RTCP-цикл — свій
// мʼютекс.
type TWCC struct {
	mu     sync.Mutex
	next   uint16
	sent   [TCCRing]tccSent
	lastFb uint16
	haveFb bool
	est    Trendline
	acked  AckedRate
	overs  int
	fbs    int
}

// NewTWCC — порожній стан.
func NewTWCC() *TWCC { return &TWCC{est: NewTrendline()} }

// NextSeq — видати наступний transport-wide seq і запамʼятати відправку.
func (t *TWCC) NextSeq(at time.Time, size int) uint16 {
	t.mu.Lock()
	defer t.mu.Unlock()
	seq := t.next
	t.next++
	t.sent[int(seq)%TCCRing] = tccSent{seq: seq, ok: true, at: at, size: size}
	return seq
}

// Unreserve — відкотити щойно виданий seq (пакет зі seq так і не пішов).
func (t *TWCC) Unreserve(seq uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.next == seq+1 {
		t.next = seq
	}
	t.sent[int(seq)%TCCRing] = tccSent{}
}

// sentBpsLocked — швидкість ВІДПРАВКИ (разом із втраченими) за той самий
// відрізок seq, що й вікно acked. acked < sent = частина відправленого не
// доходить вчасно. 0 — замало даних.
func (t *TWCC) sentBpsLocked() uint64 {
	p := t.acked.pts
	if len(p) < 2 {
		return 0
	}
	first, last := p[0], p[len(p)-1]
	span := last.send.Sub(first.send)
	n := int(uint16(last.seq - first.seq))
	if span < AckedWindow/4 || n <= 0 || n >= TCCRing {
		return 0
	}
	bytes := 0
	for i := 1; i <= n; i++ {
		sq := first.seq + uint16(i)
		if s := t.sent[int(sq)%TCCRing]; s.ok && s.seq == sq {
			bytes += s.size
		}
	}
	return uint64(float64(bytes) * 8 / span.Seconds())
}

// Feedback — результат розбору одного TWCC-фідбеку.
type Feedback struct {
	Acked, Sent uint64    // біт/с
	WinStart    time.Time // відправка першого пакета вікна acked
	Over        bool      // НОВИЙ (щойно підтверджений) OVERUSE
}

// OnFeedback — розбір TWCC-фідбеку.
func (t *TWCC) OnFeedback(fb *rtcp.TransportLayerCC) Feedback {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fbs++
	var out Feedback
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
			if s := t.sent[int(seq)%TCCRing]; fresh && s.ok && s.seq == seq {
				t.est.Add(s.at, arr)
				if t.est.TakeFired() {
					out.Over = true
				}
				t.acked.Add(arr, s.at, seq, s.size)
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
	if out.Over {
		t.overs++
	}
	if len(t.acked.pts) > 0 {
		out.WinStart = t.acked.pts[0].send
	}
	out.Acked, out.Sent = t.acked.Bps(), t.sentBpsLocked()
	// Коротке вікно — лише при явному перевантаженні (acked < DelayRecentAt×sent):
	// без нього 200 мс на змінному контенті (IDR, паузи кадрів) дає випадково
	// низькі значення, і стеля 8M різалась до ~4 Мбіт/с (заміряно).
	if float64(out.Acked) < DelayRecentAt*float64(out.Sent) {
		if r := t.acked.RecentBps(AckedRecent); r > 0 && r < out.Acked {
			out.Acked = r
		}
	}
	return out
}

// QueueAcc — накопичена затримка детектора (мс); ok=false — ще немає даних.
func (t *TWCC) QueueAcc() (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.est.Acc()
}

// Counts — (фідбеків, OVERUSE).
func (t *TWCC) Counts() (feedback, overuse int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fbs, t.overs
}

// Limited — чи канал «впирається»: частина відправленого за вікно не дійшла.
func Limited(acked, sent uint64) bool {
	return acked > 0 && sent > 0 && float64(acked) < DelayLimited*float64(sent)
}

// DelayIn — вхід DelayCut: фідбек і те, що контролер знає про свою ціль.
type DelayIn struct {
	Over          bool
	Acked, Sent   uint64
	WinStart, Now time.Time
	Target        uint64    // поточна ціль
	LastSent      time.Time // остання зміна цілі (нуль — не було)
	LastWasDelay  bool      // остання зміна була зрізом по затримці
	Held          bool      // епізод затримки ще триває (DelayUpHold від OVERUSE+Limited)
	Floor         uint64    // підлога цілі
}

// DelayCut — ЧИСТЕ рішення «різати по затримці». congested — затор визнано
// (контролер ставить момент затору), cut — іти на нову ціль next.
//
// Лише OVERUSE разом із Limited відкриває епізод (викликач запамʼятовує його
// момент і з нього рахує Held); OVERUSE без Limited — сплеск IDR нижче стелі.
// Поки епізод триває і канал впирається (або severe), ціль = DelayBeta×acked,
// але не глибше за DelayCutMin×ціль за крок, лише по вікну, що ЦІЛКОМ після
// попередньої зміни, і не частіше за DelayCutDebounce.
func DelayCut(in DelayIn) (next uint64, congested, cut bool) {
	limited := Limited(in.Acked, in.Sent)
	severe := in.Acked > 0 && in.Sent > 0 && float64(in.Acked) < DelaySevere*float64(in.Sent)
	if !(in.Over && limited) && !(severe && in.Held) {
		return in.Target, false, false
	}
	// Енкодер ще не догнав попередній зріз (шле помітно більше за ціль):
	// надлишок — його запізнення, а не новий затор.
	if !in.LastSent.IsZero() && in.LastWasDelay && float64(in.Sent) > DelayLag*float64(in.Target) {
		return in.Target, true, false
	}
	next = uint64(float64(in.Acked) * DelayBeta)
	if lo := uint64(float64(in.Target) * DelayCutMin); next < lo {
		next = lo
	}
	if next < in.Floor {
		next = in.Floor
	}
	if next >= in.Target {
		return in.Target, true, false
	}
	if !in.LastSent.IsZero() && (in.Now.Sub(in.LastSent) < DelayCutDebounce || !in.WinStart.After(in.LastSent)) {
		return in.Target, true, false
	}
	return next, true, true
}
