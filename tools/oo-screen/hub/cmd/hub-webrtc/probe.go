// P1 (TZ-GENERAL, N3/N4, B4/B5): ПРОБА СМУГИ ДУБЛІКАТАМИ + ПЕЙСИНГ ПАКЕТІВ НОГИ.
//
// Проблема, заміряна в bench/RESULTS-network.md: щоб дізнатися, що канал знову
// ширший, контролер мусив ПІДНЯТИ ВІДЕО (FASTUP) — і на сталій стелі кожна така
// проба = кадри в переповнену чергу = фриз. Без FASTUP відновлення після зняття
// стелі займало хвилину.
//
// ПРОБА (OO_SCREEN_PROBE=1, дефолт ВИМКНЕНО). Хаб не кодує, але може сам
// навантажити ногу глядача: pump на probeDur дописує ДУБЛІКАТИ щойно
// відправлених пакетів (той самий SSRC/seq — рівно як NACK-ретрансмісія pion),
// доводячи швидкість ноги до rate. Seq не зсуваються, тож не треба ні
// переписувати нумерацію, ні перекладати NACK; приймач (libsrtp/pion srtp)
// дубль відкидає replay-перевіркою, а якщо оригінал загубився — дубль його
// заміняє. Якщо за пробу + grace глядач майже не NACK-ав — канал витримав rate,
// і ціль одразу стає probeAccept*rate (а не +5%/10 с). Якщо NACK-ів більше за
// probeNackMax — проба обривається одразу (NACK приходить на масштабі RTT), а
// ціль ВІДЕО не змінюється взагалі: ціна невдалої проби — ~RTT+100 мс
// переповненої черги, яку NACK відновлює.
//
// Чому probeAccept 0.85 і probeDur 700 мс: черга вузького місця 100 мс при
// стелі cap переповнюється за probeDur, лише якщо (rate-cap)*probeDur >
// 0.1*cap, тобто cap < rate/(1+0.1/0.7) = 0.875*rate. Отже успішна проба
// гарантує cap >= ~0.875*rate, і ціль 0.85*rate — нижче за стелю. Це
// тримається, лише якщо нога реально пронесла rate: тому OK — тільки коли
// кожна нога записала ≥ probeFull (95%) від rate*probeDur, інакше
// inconclusive і ціль не змінюється (probeVerdict).
//
// ПЕЙСИНГ (OO_SCREEN_PACE=1, дефолт ВИМКНЕНО). IDR — найбільший кадр, і pion
// пише його пакети одним сплеском: при стелі 8M IDR ~250 КБ проти черги 100 мс
// (= 100 КБ) — це дропи й PLI раз на GOP. Pump ноги розтягує сплеск
// токен-бакетом на paceMul*ціль: черга вузького місця росте не більше ніж на
// S*(paceMul-1)/paceMul (S/3 при 1.5), тобто IDR проходить без дропів. Ціна —
// IDR доходить на S/(paceMul*ціль) пізніше, але через вузьке місце він однаково
// не пройшов би швидше.
package main

import (
	"math"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

var (
	probeEnabled = flagOn(os.Getenv("OO_SCREEN_PROBE"))
	paceEnabled  = flagOn(os.Getenv("OO_SCREEN_PACE"))
	paceMul      = envFloat("OO_SCREEN_PACE_MUL", 1.5)
)

const (
	probeDur     = 700 * time.Millisecond
	probeGrace   = 250 * time.Millisecond // + RTT ноги: NACK на останні пакети проби
	probeAccept  = 0.85                   // ціль після успішної проби = rate × це
	probeMaxOver = 1.1                    // rate до стелі × це: щоб 0.85*rate дійшло до ≥90% стелі
	probeStopAt  = 0.9                    // ціль ≥ стеля × це — не пробуємо
	probeClean   = time.Second            // скільки «чисто» до проби
	probeGapOK   = time.Second            // після успіху — наступна проба
	probeGapFail = 2 * time.Second        // після невдачі (× 2 за кожну наступну)
	probeGapMax  = 8 * time.Second        // ≤ 8 с, інакше N4 (≤15 с) не вміщається
	probeNackMax = 2                      // унікальних NACK за пробу — ще «чисто» (рівномірна 1% втрата)
	probeMute    = 1500 * time.Millisecond
	probeTick    = 5 * time.Millisecond
	probeFull    = 0.95 // нога мусила реально пронести ≥ стільки від rate
	probeRing    = 64   // скільки останніх пакетів ноги тримаємо для дублів

	paceBurst      = 20 * time.Millisecond // місткість бакета в часі
	paceMinBytes   = 6000                  // ...але не менше за ~4 пакети
	paceBacklog    = 256                   // черга ноги довша — не пейсимо (H-12, priming)
	paceWarmup     = 2 * time.Second       // перші секунди ноги — кеш GOP, без пейсингу
	paceRefreshInt = 50 * time.Millisecond
)

// flagOn — значення env-прапорця P1: увімкнено лише рівно "1".
func flagOn(v string) bool { return v == "1" }

// probeMul — у скільки разів проба вища за ціль. ×2 — лише одразу після
// успішної проби (канал щойно довів запас — розгін після зняття стелі);
// перша проба після зрізу ×1.5 (заміряно: ×2 над стелею 4M = 80-170 NACK,
// бо обрив проби доходить лише через RTT + опит NACK 100 мс), далі менші.
func probeMul(fails int, lastOK bool) float64 {
	switch {
	case lastOK:
		return 2.0
	case fails == 0:
		return 1.5
	case fails == 1:
		return 1.25
	default:
		return 1.15
	}
}

// probeCeil — стеля для проби: стартовий бітрейт агента і REMB.
func (c bitrateCtl) probeCeil() uint64 {
	ceil := c.startBps
	if c.remb > 0 && c.remb < ceil {
		ceil = c.remb
	}
	return ceil
}

// probeDue — ЧИСТА: чи час почати пробу і на яку швидкість (біт/с).
func (c bitrateCtl) probeDue(now time.Time) (bitrateCtl, uint64, bool) {
	if !c.probeOn || c.probing || c.startBps == 0 {
		return c, 0, false
	}
	ceil := c.probeCeil()
	if float64(c.target) >= float64(ceil)*probeStopAt {
		return c, 0, false
	}
	if c.cleanSince.IsZero() || now.Sub(c.cleanSince) < probeClean {
		return c, 0, false
	}
	if now.Before(c.probeNextAt) {
		return c, 0, false
	}
	rate := uint64(float64(c.target) * probeMul(c.probeFails, c.probeLastOK))
	if lim := uint64(float64(ceil) * probeMaxOver); rate > lim {
		rate = lim
	}
	if float64(rate) < float64(c.target)*1.05 {
		return c, 0, false
	}
	c.probing, c.probeBps = true, rate
	c.probeMuteUntil = now.Add(probeDur + probeMute)
	return c, rate, true
}

// probeOutcome — підсумок проби по всіх ногах.
type probeOutcome int

const (
	probeOK probeOutcome = iota
	probeFailed
	probeInconclusive // дублів майже не було (статичний екран) — нічого не знаємо
)

// probeDone — ЧИСТА: застосувати результат проби. send=true — ціль змінилась.
func (c bitrateCtl) probeDone(res probeOutcome, now time.Time) (bitrateCtl, bool) {
	if !c.probing {
		return c, false
	}
	c.probing = false
	rate := c.probeBps
	c.probeBps = 0
	switch res {
	case probeInconclusive:
		c.probeNextAt = now.Add(probeGapFail)
		return c, false
	case probeFailed:
		c.probeLastOK = false
		c.probeFails++
		gap := probeGapFail
		for i := 1; i < c.probeFails && gap < probeGapMax; i++ {
			gap *= 2
		}
		if gap > probeGapMax {
			gap = probeGapMax
		}
		c.probeNextAt = now.Add(gap)
		c.reason = "probe_fail"
		return c, false
	}
	c.probeFails = 0
	c.probeLastOK = true
	c.probeNextAt = now.Add(probeGapOK)
	next := uint64(float64(rate) * probeAccept)
	if ceil := c.probeCeil(); next > ceil {
		next = ceil
	}
	if next <= c.target {
		return c, false
	}
	c.target, c.lastSent = next, now
	c.reason = "probe"
	// Канал довів, що ширший: старі рівні затору більше нічого не кажуть.
	c.cutFrom, c.probeLvl, c.failedProbes, c.failLvl = 0, 0, 0, 0
	c.goodSince = now
	c.lastUpAt, c.probeFrom = time.Time{}, 0
	return c, true
}

// legProbe — проба на ОДНІЙ нозі; виконує pump, NACK-и рахує onNack.
type legProbe struct {
	start, end time.Time
	bps        uint64
	evalUntil  time.Time // NACK до цього моменту — проти проби
	sentBytes  atomic.Uint64
	padBytes   atomic.Uint64
	nacks      atomic.Int32
	aborted    atomic.Bool
}

// setProbe — виставити/зняти пробу ноги й розбудити її pump.
func (vl *viewerLeg) setProbe(p *legProbe) {
	vl.probe.Store(p)
	select {
	case vl.probeKick <- struct{}{}:
	default: // nil-канал (нога без pump-а) або побудка вже чекає
	}
}

// noteProbeNack — NACK під час проби/grace. true — NACK віднесено до проби
// (у preLoss B4 його не рахуємо: це наш власний тиск, а не затор відео).
func (vl *viewerLeg) noteProbeNack(n int, now time.Time) bool {
	p := vl.probe.Load()
	if p == nil || now.Before(p.start) || now.After(p.evalUntil) {
		return false
	}
	if p.nacks.Add(int32(n)) > probeNackMax {
		p.aborted.Store(true)
	}
	return true
}

// padOwed — ЧИСТА: скільки байтів дописати, щоб нога вийшла на bps від start.
func padOwed(bps uint64, start, now time.Time, sent uint64) int64 {
	want := float64(bps) / 8 * now.Sub(start).Seconds()
	return int64(want) - int64(sent)
}

// pacer — токен-бакет пейсингу ноги. Нульове значення = без обмеження.
type pacer struct {
	rate   float64 // байт/с; 0 — вимкнено
	tokens float64
	last   time.Time
}

func (p *pacer) setRate(bps uint64) {
	p.rate = float64(bps) / 8 * paceMul
}

func (p *pacer) capacity() float64 {
	return math.Max(p.rate*paceBurst.Seconds(), paceMinBytes)
}

// wait — ЧИСТА щодо годинника: скільки чекати перед пакетом size байтів у now.
// Списує токени (бакет може піти в мінус на час очікування).
func (p *pacer) wait(size int, now time.Time) time.Duration {
	if p.rate <= 0 {
		return 0
	}
	if p.last.IsZero() {
		p.tokens = p.capacity()
	} else {
		p.tokens += now.Sub(p.last).Seconds() * p.rate
		if c := p.capacity(); p.tokens > c {
			p.tokens = c
		}
	}
	p.last = now
	p.tokens -= float64(size)
	if p.tokens >= 0 {
		return 0
	}
	return time.Duration(-p.tokens / p.rate * float64(time.Second))
}

func pktSize(pkt *rtp.Packet) int {
	return pkt.MarshalSize()
}

// startProbe — запускає пробу rate на всіх живих ногах ноди і через
// probeDur+grace підбиває підсумок у контролер. Кликати БЕЗ ns.mu.
func startProbe(ns *nodeSession, rate uint64, now time.Time) {
	ns.mu.Lock()
	var legs []*viewerLeg
	var maxRTT time.Duration
	for _, vl := range ns.viewers {
		if !vl.live {
			continue
		}
		legs = append(legs, vl)
		if vl.rtt > maxRTT && vl.rtt < rttSaneMax {
			maxRTT = vl.rtt
		}
	}
	ns.mu.Unlock()
	eval := probeDur + probeGrace + maxRTT
	if len(legs) == 0 {
		finishProbe(ns, nil, now)
		return
	}
	for _, vl := range legs {
		vl.setProbe(&legProbe{start: now, end: now.Add(probeDur), bps: rate, evalUntil: now.Add(eval)})
	}
	ndjsonf(`{"leg":"probe","node":%q,"phase":"start","bps":%d}`+"\n", ns.nodeID, rate)
	time.AfterFunc(eval, func() { finishProbe(ns, legs, time.Now()) })
}

// probeVerdict — ЧИСТА: вердикт по ногах.
//
// sentBytes — УСЕ, що нога записала у вікні проби (відео + дублі): саме цей
// потік ішов через вузьке місце, тож він і є перевіреною швидкістю, хоч би
// скільки в ньому було дублів. Нога, що недовезла probeFull від rate (тікер
// стартував пізно, WriteRTP стояв, проба обірвалась), швидкість rate НЕ
// довела — вердикт inconclusive, ціль не росте (інакше 0.85*rate могло б
// перевищити реально перевірене). Ноги без пакетів (статичний екран, ring
// порожній) — теж inconclusive.
func probeVerdict(ps []*legProbe) probeOutcome {
	any := false
	for _, p := range ps {
		if p == nil {
			continue
		}
		any = true
		if p.aborted.Load() || p.nacks.Load() > probeNackMax {
			return probeFailed
		}
	}
	if !any {
		return probeInconclusive
	}
	for _, p := range ps {
		if p == nil {
			continue
		}
		want := float64(p.bps) / 8 * p.end.Sub(p.start).Seconds()
		if float64(p.sentBytes.Load()) < probeFull*want {
			return probeInconclusive
		}
	}
	return probeOK
}

func finishProbe(ns *nodeSession, legs []*viewerLeg, now time.Time) {
	ps := make([]*legProbe, 0, len(legs))
	var pad uint64
	var nacks int32
	for _, vl := range legs {
		p := vl.probe.Swap(nil)
		ps = append(ps, p)
		if p != nil {
			pad += p.padBytes.Load()
			nacks += p.nacks.Load()
		}
	}
	res := probeVerdict(ps)
	ns.mu.Lock()
	prev := ns.bitrate.target
	next, send := ns.bitrate.probeDone(res, now)
	ns.bitrate = next
	ns.mu.Unlock()
	ndjsonf(`{"leg":"probe","node":%q,"phase":"done","res":%d,"padBytes":%d,"nacks":%d,"bitrate":%d}`+"\n",
		ns.nodeID, int(res), pad, nacks, next.target)
	if send {
		sendBitrateTarget(ns, next.target, 0, 0, 0, false)
		metricsNoteBitrate(ns, prev, "probe", 0)
	}
}

// maybeProbe — після кроку контролера: чи не час пробувати. Кликати БЕЗ ns.mu.
func maybeProbe(ns *nodeSession, now time.Time) {
	ns.mu.Lock()
	next, rate, ok := ns.bitrate.probeDue(now)
	ns.bitrate = next
	ns.mu.Unlock()
	if ok {
		startProbe(ns, rate, now)
	}
}
