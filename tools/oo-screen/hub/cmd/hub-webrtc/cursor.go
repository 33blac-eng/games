// Шар курсора (cursor layer): агент шле форму й позицію вказівника окремим
// DataChannel-ом (internal/cursorproto), а не вмальовує його в кадр. Хаб тут —
// лише ретранслятор agent -> усі глядачі ноди.
//
// 🔴 ВМИКАЄТЬСЯ НЕ ХАБОМ. Канал на agent-нозі з'являється лише в агента з
// -cursor-layer, на viewer-нозі — лише коли плеєр із config.cursorLayer
// відкрив канал 'oosc-cursor' (спільний диспетчер OnDataChannel viewer-ноги). Без обох хаб поводиться бітово
// так само, як до цього файла: обробника немає, каналу немає, SDP той самий.
//
// РЕТРАНСЛЯТОР УЗАГАЛЬНЕНИЙ (dcRelay): мітка, валідатор і «липкі» повідомлення
// — параметри, а не код. Паралельно заводиться ще один канал agent->viewers
// ('tiles'); йому досить свого dcRelay з іншою міткою, без правок тут.
//
// ЗАСУВКИ (всі на боці хаба):
//   - розмір/формат: кожне повідомлення проходить validate (для курсора —
//     cursorproto.Validate: ≤64 KiB, форма ≤256x256) — інакше не йде нікуди;
//   - частота від агента: rate.Limiter (агент коалесить до ≤125/с; стеля 2x);
//   - глядачів на канал: maxViewers (дзеркало maxViewersPerNode);
//   - F9, ПЕРЕГОВОРИ (лише для курсора): хаб шле агенту KindMode=1 тільки
//     коли КОЖЕН глядач ноги (ns.viewers) має відкритий 'oosc-cursor' і
//     OO_SCREEN_RECORD вимкнено; інакше KindMode=0, і агент вмальовує
//     вказівник у кадр як без прапорця. Перерахунок — на зміну глядачів
//     ноги/каналів і раз на секунду (запис міг увімкнутись перезавантаженням).
//   - повільний глядач: поки в його каналі буферизовано > maxBuffered, НЕлипкі
//     повідомлення (позиції) йому пропускаються — наступна позиція все одно
//     абсолютна. Липкі (форма) йдуть завжди, поки не перевищено hardBuffered.
package main

import (
	"log"
	"sync"
	"time"

	"github.com/organicoils/oo-screen/internal/cursorproto"
	"github.com/pion/webrtc/v4"
	"golang.org/x/time/rate"
)

// relaySink — те, що від *webrtc.DataChannel потрібно ретранслятору
// (інтерфейс заради тестів без SCTP).
type relaySink interface {
	Send([]byte) error
	BufferedAmount() uint64
	ReadyState() webrtc.DataChannelState
}

type relayConfig struct {
	label     string
	validate  func([]byte) error
	stickyKey func([]byte) (byte, bool) // повідомлення, яке переграється новому глядачу
	// essential — повідомлення, яке НЕ пропускається повільному глядачу
	// (форма курсора); решта (позиції) під тиском буфера пропускаються.
	essential  func([]byte) bool
	maxViewers int
	ratePerSec float64
	burst      int
	// maxBuffered — понад це НЕлипкі повідомлення глядачу пропускаються.
	maxBuffered uint64
	// hardBuffered — понад це глядачу не шлеться нічого.
	hardBuffered uint64
}

type dcRelay struct {
	cfg     relayConfig
	lim     *rate.Limiter
	mu      sync.Mutex
	viewers map[relaySink]any // sink -> власник (viewer-нога)
	// owners — один канал на власника: інакше одна viewer-нога, відкривши
	// N каналів 'oosc-cursor', зайняла б усі maxViewers місць ноди (і
	// множила б трафік ретрансляції на себе).
	owners map[any]relaySink
	sticky map[byte][]byte
	order  []byte // порядок липких ключів (форма раніше за позицію)
	// agent — поточний агентський канал (nil — немає). Закриття старого
	// каналу після відкриття нового не чіпає стан нового.
	agent   any
	dropped uint64

	// agentTx — куди слати дозвіл агенту (F9); granted/grantSent — що
	// агент від нас уже чув на цьому каналі.
	agentTx   interface{ Send([]byte) error }
	granted   bool
	grantSent bool
	// grantMu серіалізує весь перерахунок «обчислити бажане → Send →
	// записати стан»: інакше два конкурентні refreshCursorGrant можуть
	// доставити агенту Mode у зворотному порядку від записаного стану
	// (хаб думає granted=false, агент лишився на шарі — курсора нема).
	// Порядок блокувань: grantMu → ns.mu / r.mu.
	grantMu sync.Mutex
}

func newDCRelay(cfg relayConfig) *dcRelay {
	return &dcRelay{
		cfg:     cfg,
		lim:     rate.NewLimiter(rate.Limit(cfg.ratePerSec), cfg.burst),
		viewers: map[relaySink]any{},
		owners:  map[any]relaySink{},
		sticky:  map[byte][]byte{},
	}
}

// reset забуває липкий стан (новий агентський канал: старі форма/позиція
// можуть бути вже неправдою).
func (r *dcRelay) reset() {
	r.mu.Lock()
	r.sticky = map[byte][]byte{}
	r.order = nil
	r.mu.Unlock()
}

// setAgent — новий агентський канал: липкий стан скидається.
func (r *dcRelay) setAgent(a any) {
	r.mu.Lock()
	r.agent = a
	r.agentTx, _ = a.(interface{ Send([]byte) error })
	r.granted, r.grantSent = false, false
	r.sticky = map[byte][]byte{}
	r.order = nil
	r.mu.Unlock()
}

// agentGone — агентський канал a закрився: липкі форма/позиція більше не
// правда (агента немає), тож пізній глядач не має їх отримати. Якщо вже
// відкрито новіший канал — нічого не робимо.
func (r *dcRelay) agentGone(a any) {
	r.mu.Lock()
	if a == nil || r.agent == a {
		r.agent = nil
		r.agentTx = nil
		r.sticky = map[byte][]byte{}
		r.order = nil
	}
	r.mu.Unlock()
}

// publish валідує повідомлення агента й розсилає всім глядачам. Повертає,
// скільком глядачам його віддано.
func (r *dcRelay) publish(msg []byte, now time.Time) int {
	if r.cfg.validate != nil && r.cfg.validate(msg) != nil {
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
		return 0
	}
	if !r.lim.AllowN(now, 1) {
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
		return 0
	}
	cp := append([]byte(nil), msg...)
	isEssential := r.cfg.essential != nil && r.cfg.essential(cp)
	r.mu.Lock()
	if r.cfg.stickyKey != nil {
		if k, ok := r.cfg.stickyKey(cp); ok {
			if _, had := r.sticky[k]; !had {
				r.order = append(r.order, k)
			}
			r.sticky[k] = cp
		}
	}
	sinks := make([]relaySink, 0, len(r.viewers))
	for s := range r.viewers {
		sinks = append(sinks, s)
	}
	r.mu.Unlock()
	n := 0
	for _, s := range sinks {
		if r.sendTo(s, cp, isEssential) {
			n++
		}
	}
	return n
}

func (r *dcRelay) sendTo(s relaySink, msg []byte, essential bool) bool {
	if s.ReadyState() != webrtc.DataChannelStateOpen {
		if st := s.ReadyState(); st == webrtc.DataChannelStateClosed || st == webrtc.DataChannelStateClosing {
			r.removeViewer(s)
		}
		return false
	}
	buf := s.BufferedAmount()
	if buf > r.cfg.hardBuffered || (!essential && buf > r.cfg.maxBuffered) {
		return false
	}
	if err := s.Send(msg); err != nil {
		r.removeViewer(s)
		return false
	}
	return true
}

// addViewer приймає глядача (false — стелю вичерпано) і одразу віддає йому
// липкий стан, щоб курсор з'явився без чекання на наступну зміну форми.
func (r *dcRelay) addViewer(s relaySink) bool { return r.addViewerOwned(s, s) }

// addViewerOwned — addViewer з власником: другий канал того самого власника
// (поки перший живий) не приймається.
func (r *dcRelay) addViewerOwned(owner any, s relaySink) bool {
	r.mu.Lock()
	if prev, ok := r.owners[owner]; ok && prev != s {
		r.mu.Unlock()
		return false
	}
	if _, ok := r.viewers[s]; !ok && len(r.viewers) >= r.cfg.maxViewers {
		r.mu.Unlock()
		return false
	}
	r.viewers[s] = owner
	r.owners[owner] = s
	replay := make([][]byte, 0, len(r.order))
	for _, k := range r.order {
		replay = append(replay, r.sticky[k])
	}
	r.mu.Unlock()
	for _, m := range replay {
		r.sendTo(s, m, true)
	}
	return true
}

func (r *dcRelay) removeViewer(s relaySink) {
	r.mu.Lock()
	if owner, ok := r.viewers[s]; ok {
		if r.owners[owner] == s {
			delete(r.owners, owner)
		}
		delete(r.viewers, s)
	}
	r.mu.Unlock()
}

// grant шле агенту дозвіл/відкликання шару, якщо він змінився. Повертає,
// чи було відправлено. Викликач тримає r.grantMu, тож Send іде в тому ж
// порядку, що й записи granted.
func (r *dcRelay) grant(on bool) bool {
	r.mu.Lock()
	tx := r.agentTx
	if tx == nil || (r.grantSent && r.granted == on) {
		r.mu.Unlock()
		return false
	}
	r.granted, r.grantSent = on, true
	r.mu.Unlock()
	if err := tx.Send(cursorproto.EncodeMode(on)); err != nil {
		r.mu.Lock()
		r.grantSent = false // наступний перерахунок спробує ще
		r.mu.Unlock()
		return false
	}
	return true
}

// coversAll — у кожного з owners є живий канал у ретрансляторі. Порожній
// список — false (нікому малювати курсор — нема чого й вимикати).
func (r *dcRelay) coversAll(owners []any) bool {
	if len(owners) == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range owners {
		s, ok := r.owners[o]
		if !ok || s.ReadyState() != webrtc.DataChannelStateOpen {
			return false
		}
	}
	return true
}

// cursorGrantWanted — правило F9: шар лише коли сесія не пишеться в MKV і
// всі глядачі ноги вміють шар.
func cursorGrantWanted(ns *nodeSession, r *dcRelay) bool {
	if recordEnabled.Load() {
		return false
	}
	ns.mu.Lock()
	owners := make([]any, 0, len(ns.viewers))
	for pc := range ns.viewers {
		owners = append(owners, pc)
	}
	ns.mu.Unlock()
	return r.coversAll(owners)
}

// refreshCursorGrant перераховує дозвіл для ноди (без ns.mu на вході).
// Ретранслятора ще немає — агент без -cursor-layer, нема кому казати.
func refreshCursorGrant(ns *nodeSession) {
	v, ok := relays.Load(relayKey{ns, cursorproto.ChannelLabel})
	if !ok {
		return
	}
	r := v.(*dcRelay)
	r.grantMu.Lock()
	defer r.grantMu.Unlock()
	// Бажане рахується під grantMu: рішення, обчислене до чужого Send,
	// не може бути відправлене після нього.
	if on := cursorGrantWanted(ns, r); r.grant(on) {
		log.Printf("relay %s: cursor layer grant=%v [node=%s]", cursorproto.ChannelLabel, on, ns.nodeID)
	}
}

func (r *dcRelay) viewerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.viewers)
}

// relays — ретранслятори по (нода, мітка). Окремий реєстр, а не поле в
// nodeSession: нові канали не мусять чіпати цю структуру.
var relays sync.Map // relayKey -> *dcRelay

type relayKey struct {
	ns    *nodeSession
	label string
}

func relayFor(ns *nodeSession, cfg relayConfig) *dcRelay {
	k := relayKey{ns, cfg.label}
	if v, ok := relays.Load(k); ok {
		return v.(*dcRelay)
	}
	v, _ := relays.LoadOrStore(k, newDCRelay(cfg))
	return v.(*dcRelay)
}

// agentRelaysGone — агентська нога ноди впала (PC Failed/Closed): липкий стан
// усіх ретрансляторів ноди забувається, навіть якщо OnClose каналу не прийшов.
func agentRelaysGone(ns *nodeSession) {
	relays.Range(func(k, v any) bool {
		if k.(relayKey).ns == ns {
			v.(*dcRelay).agentGone(nil)
		}
		return true
	})
}

// forgetRelays прибирає всі ретранслятори ноди (нода зникла з реєстру).
func forgetRelays(ns *nodeSession) {
	relays.Range(func(k, _ any) bool {
		if k.(relayKey).ns == ns {
			relays.Delete(k)
		}
		return true
	})
}

// attachAgentRelay — канал агента з міткою cfg.label: кожне повідомлення йде
// в ретранслятор ноди.
func attachAgentRelay(ns *nodeSession, dc *webrtc.DataChannel, cfg relayConfig) {
	r := relayFor(ns, cfg)
	r.setAgent(dc)
	if cfg.label != cursorproto.ChannelLabel {
		dc.OnClose(func() { r.agentGone(dc) })
	} else {
		// F9: агент стартує з вказівником у кадрі; перший дозвіл — щойно
		// канал відкрито, далі — на події й раз на секунду. (pion тримає
		// ОДИН обробник OnClose — тому agentGone теж тут.)
		stop := make(chan struct{})
		var once sync.Once
		dc.OnClose(func() {
			r.agentGone(dc)
			once.Do(func() { close(stop) })
		})
		start := func() {
			refreshCursorGrant(ns)
			go func() {
				t := time.NewTicker(time.Second)
				defer t.Stop()
				for {
					select {
					case <-stop:
						return
					case <-t.C:
						refreshCursorGrant(ns)
					}
				}
			}()
		}
		var startOnce sync.Once
		dc.OnOpen(func() { startOnce.Do(start) })
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			startOnce.Do(start)
		}
	}
	log.Printf("relay %s: agent channel open [node=%s]", cfg.label, ns.nodeID)
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		r.publish(m.Data, time.Now())
	})
}

// viewerRelayHandler підписує канал глядача (відкритий браузером, ловиться
// спільним диспетчером OnDataChannel viewer-ноги) на ретранслятор ноди.
// owner — viewer-нога (її PeerConnection): один канал на ногу.
func viewerRelayHandler(ns *nodeSession, owner any, dc *webrtc.DataChannel, cfg relayConfig) {
	r := relayFor(ns, cfg)
	open := func() {
		if !r.addViewerOwned(owner, dc) {
			log.Printf("relay %s: стеля глядачів або дубль каналу [node=%s] — канал закрито", cfg.label, ns.nodeID)
			_ = dc.Close()
		}
		refreshCursorGrant(ns)
	}
	dc.OnOpen(open)
	dc.OnClose(func() { r.removeViewer(dc); refreshCursorGrant(ns) })
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		open()
	}
}

// cursorRelayConfig — параметри каналу курсора.
func cursorRelayConfig() relayConfig {
	return relayConfig{
		label:    cursorproto.ChannelLabel,
		validate: cursorproto.Validate,
		stickyKey: func(b []byte) (byte, bool) {
			k := cursorproto.Kind(b)
			return k, k == cursorproto.KindShape || k == cursorproto.KindPos
		},
		essential:    func(b []byte) bool { return cursorproto.Kind(b) == cursorproto.KindShape },
		maxViewers:   maxViewersPerNode,
		ratePerSec:   250, // агент: ≤125 позицій/с + рідкі форми
		burst:        50,
		maxBuffered:  16 << 10,
		hardBuffered: 1 << 20,
	}
}
