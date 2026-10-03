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
	owners  map[any]relaySink
	sticky  map[byte][]byte
	order   []byte // порядок липких ключів (форма раніше за позицію)
	dropped uint64
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
	r.reset()
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
	}
	dc.OnOpen(open)
	dc.OnClose(func() { r.removeViewer(dc) })
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
