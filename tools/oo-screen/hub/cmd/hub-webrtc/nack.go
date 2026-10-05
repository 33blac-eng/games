// NACK_recovered_ratio — чи взагалі рятує ретрансмісія ЦЬОГО глядача, і що
// робити, коли вже ні.
//
// СПЕРШУ ФАКТ ПРО НАШУ ТОПОЛОГІЮ, ПОТІМ ПРАВИЛО. Зовнішнє рев'ю описало каскад
// «глядач шле NACK -> hub ретранслює його ВГОРУ -> агент теж не має пакета ->
// шле PLI». У кандидата A такої дуги НЕМА: кожна viewer-нога має власний
// ланцюг interceptor-ів pion (newAPI() викликається на КОЖЕН offer), і
// nack.ResponderInterceptor відповідає на NACK ЛОКАЛЬНО з кільцевого буфера на
// 1024 пакети. Вгору до агента з viewer-ноги йде рівно один тип RTCP — PLI.
//
// А от позитивний зворотний зв'язок у fanout лишається, просто по іншій дузі:
// пакет випав із кільця -> responder мовчить -> браузер, не дочекавшись
// ретрансмісії, шле PLI -> keyframe -> сплеск бітрейту -> втрати в СУСІДНЬОГО
// глядача -> його PLI -> ... І оскільки propagatePLI не мав ЖОДНОГО дебаунсу,
// N глядачів однієї ноди коштували агентові N запитів IDR на одну й ту саму
// втрату. Дебаунс PLI (pliGate) і є розрив цієї дуги; він тут, а не в main.go,
// саме тому, що це та сама історія.
//
// ПРАВИЛО, яке реалізує решта файлу: якщо частка задоволених NACK < 50%,
// ретрансмісія цьому глядачеві більше не допомагає — не чекаємо, доки браузер
// сам здасться, а одразу просимо keyframe і на nackFallbackHold перестаємо
// переоцінювати ногу.
//
// ЧОМУ МІРЯЄМО САМІ, А НЕ БЕРЕМО ЛІЧИЛЬНИК PION: ResponderInterceptor не віддає
// жодної статистики — на промах він просто мовчки нічого не пише
// (pkg/nack/responder_interceptor.go, resendPackets). Зате УМОВА, за якої він
// віддасть пакет, детермінована й повністю відома: rtpbuffer.Get віддає seq
// тоді й лише тоді, коли highestAdded-seq < size (internal/rtpbuffer). А
// egress-нумерація хаба суцільна — кожен живий глядач отримує КОЖЕН пакет, а
// хто не встигає, того рвуть цілком (dropViewer). Тому «лежить у буфері» ==
// «потрапляє у вікно»: це не оцінка, а перерахунок тієї самої умови.
//
// ЧОГО ЗРОБИТИ НЕ МОЖНА, і це чесна межа: вимкнути NACK для ОДНІЄЇ ноги В
// РАНТАЙМІ. Ланцюг interceptor-ів фіксується при створенні PeerConnection, а
// «nack» у відповіді SDP знімається лише перенегоціацією; рантайм-перемикача
// pion не має. Per-leg вимкнення доступне тільки на момент offer (у newAPI()
// свій registry на кожну ногу) — на живій нозі ні. Але воно й не потрібне:
// NACK, на який хаб МОЖЕ відповісти, шкоди не робить, а коштує промах рівно
// одного пошуку в мапі. Шкоду робить ОЧІКУВАННЯ глядача на ретрансмісію, якої
// не буде, — і обриваємо саме його.
package main

import (
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
)

const (
	// nackBufferSize — розмір кільця ретрансмісії pion. 1024 — дефолт
	// ResponderInterceptor, і RegisterDefaultInterceptors його не переозначає
	// (webrtc/interceptor.go передає лише loggerFactory). Пакет, старший за це
	// вікно, responder віддати НЕ може — цим і визначається «задоволено».
	nackBufferSize = 1024

	nackWindow     = 2 * time.Second // вікно, за яким рахуємо частку
	nackMinSamples = 16              // менше запитів у вікні — статистики немає, не судимо
	nackGoodRatio  = 0.5             // < 50% задоволених -> NACK більше не рятує

	// nackFallbackHold — скільки не переоцінюємо ногу після переходу на PLI.
	// 3 с із діапазону 2-5 с рев'ю: коротше — переоцінка впаде в той самий
	// сплеск, який щойно й спричинив keyframe; довше — глядач сидить на
	// keyframe-режимі вже після того, як шлях полагодився.
	nackFallbackHold = 3 * time.Second

	// pliDebnc — стеля частоти keyframe-запитів ВІД ГЛЯДАЧІВ на ноду. Потік
	// один на всіх, отже й keyframe один на всіх: N глядачів, що втратили той
	// самий пакет, мають коштувати агентові ОДИН IDR, а не N.
	// ponytail: свідомо ОКРЕМИЙ годинник від ns.lastKeyframeReq (control-канал).
	// Спільний був би точніший — keyframe є keyframe — але він гасив би IDR,
	// яким супроводжується нова ціль бітрейту, а це шлях, доведений живим
	// прогоном. Обʼєднувати лише після такого ж прогону.
	pliDebnc = 500 * time.Millisecond
)

// nackRecoverable — точна умова pion rtpbuffer.Get: пакет ще в кільці, якщо
// відстань від найновішого записаного seq менша за вікно. Арифметика uint16
// сама відкидає «seq з майбутнього» (різниця обгортається у величезне число),
// тож окремої перевірки половини діапазону не треба: window <= 1024 << 32768.
func nackRecoverable(highest, seq, window uint16) bool {
	return highest-seq < window
}

// nackWindowFor — фактичне вікно ноги. Поки вона віддала менше за розмір
// кільця, старіших пакетів у ньому просто немає (нога підключилась посеред
// потоку), і рахувати їх задоволеними означало б завищити частку.
func nackWindowFor(sent uint64) uint16 {
	if sent < nackBufferSize {
		return uint16(sent)
	}
	return nackBufferSize
}

// nackStats — підсумок ОДНОГО закритого вікна: скільки seq запитано, скільки з
// них хаб міг віддати, і чи час переходити на PLI. closed=false означає, що
// вікно ще набирається — судити й логувати нічого.
type nackStats struct {
	req, hit uint64
	ratio    float64
	closed   bool
	escalate bool
}

// onNack оновлює лічильники ноги за одним NACK і, коли вікно закрилось, судить:
// частка задоволених нижча за поріг -> ретрансмісія цій нозі більше не
// допомагає. Від мережі не залежить нічого: вхід — сам пакет, стан ноги і
// «зараз», тож усі гілки рішення тестуються без сокетів і без годинника.
func onNack(ns *nodeSession, vl *viewerLeg, n *rtcp.TransportLayerNack, now time.Time) nackStats {
	sent := atomic.LoadUint64(&vl.sent)
	if sent == 0 {
		// Нога ще нічого не віддала: буфера ретрансмісії в неї фізично немає
		// (BindLocalStream наповнює його на записі), судити нема про що.
		return nackStats{}
	}
	highest := uint16(atomic.LoadUint32(&vl.lastSeq))
	if h, ok := fecHighestSeq(n.MediaSSRC); ok {
		highest = h // FEC (fec.go) зсунув вихідні seq уперед
	}
	// Вікно — у тому ж просторі seq, що й highest: з FEC буфер responder-а
	// тримає і медіа, і FEC, тож міряємо вихідним лічильником, не vl.sent.
	window := nackWindowFor(legOutSent(vl, n.MediaSSRC))

	var req, hit uint64
	var seqs []uint16
	for i := range n.Nacks {
		n.Nacks[i].Range(func(seq uint16) bool {
			req++
			seqs = append(seqs, seq)
			if nackRecoverable(highest, seq, window) {
				hit++
			}
			return true
		})
	}
	if req == 0 {
		return nackStats{}
	}

	ns.mu.Lock()
	defer ns.mu.Unlock()
	// P1: NACK під час проби — наслідок нашого ж навантаження (probe.go); у
	// preLoss B4 він не йде, інакше невдала проба різала б ціль відео.
	if !vl.noteProbeNack(len(seqs), now) {
		vl.noteNackSeqs(seqs) // B4: втрати до ретрансмісії (legCongestion)
	}
	if vl.nackWinAt.IsZero() {
		vl.nackWinAt = now
	}
	vl.nackReq += req
	vl.nackHit += hit
	if now.Sub(vl.nackWinAt) < nackWindow {
		return nackStats{}
	}

	st := nackStats{req: vl.nackReq, hit: vl.nackHit, closed: true}
	st.ratio = float64(st.hit) / float64(st.req)
	vl.nackReq, vl.nackHit, vl.nackWinAt = 0, 0, now

	if st.req < nackMinSamples || st.ratio >= nackGoodRatio {
		return st
	}
	if now.Before(vl.pliUntil) {
		// Нога вже в PLI-режимі: агента повторно не смикаємо, дочікуємо кінця
		// витримки і аж тоді пробуємо NACK знову.
		return st
	}
	vl.pliUntil = now.Add(nackFallbackHold)
	st.escalate = true
	return st
}

// pliGate — дебаунс keyframe-запитів від глядачів, на ВСЮ ноду (див. pliDebnc).
// Кликати БЕЗ ns.mu; лок бере сам.
func pliGate(ns *nodeSession, now time.Time) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if !ns.lastPLI.IsZero() && now.Sub(ns.lastPLI) < pliDebnc {
		return false
	}
	ns.lastPLI = now
	return true
}

// logNackWindow — телеметрія закритого вікна тим самим NDJSON, що й решта хаба:
// req/hit/ratio видно в логу поруч із рядками "ctl" (ціль бітрейту), тож
// зіставити «впала частка -> поїхала ціль» можна без окремого інструменту.
func logNackWindow(node string, st nackStats) {
	if !st.closed {
		return
	}
	ndjsonf(`{"leg":"nack","node":%q,"req":%d,"hit":%d,"ratio":%.2f,"pli":%v}`+"\n",
		node, st.req, st.hit, st.ratio, st.escalate)
}
