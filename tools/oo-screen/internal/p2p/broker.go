package p2p

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Hooks — точки, якими Broker спирається на хаб (авторизація й аудит лишаються
// хабовими; Broker їх не дублює).
type Hooks struct {
	// Authorize споживає квиток (ERP) і повертає grant та к-сть глядачів, що
	// вже на ноді через хаб. status != 0 — відмова з цим HTTP-статусом.
	Authorize func(r *http.Request, ticket string) (g Grant, viewers int, status int, msg string)
	// AgentAuth — чи має запит право говорити від імені агента ноди node.
	AgentAuth func(r *http.Request, node string) bool
	// AuditStart пише S4 session_start для прямої сесії й повертає end(reason).
	AuditStart func(g Grant, id string) (end func(reason string))
}

type session struct {
	id       string
	g        Grant
	answer   chan answerMsg
	end      func(string)
	decided  bool // результат (direct/turn/fallback) уже враховано в метриках
	closed   bool
	answered time.Time // коли агент віддав SDP (нуль — ще ні)
	seen     time.Time // останнє підтвердження від агента (poll ?active=, result)
	voucher  string    // relay-квиток, виданий разом із 200
}

type answerMsg struct {
	sdp string
	err string
}

// RelayTicketPrefix — префікс одноразового relay-квитка, який хаб видає
// глядачеві при відмові/відкаті прямої ноги. ERP-квиток (jti) /p2p/offer уже
// спожив, тож без цього повтор на /offer/viewer отримав би 403.
const RelayTicketPrefix = "p2pfb."

// voucher — збережені claims спожитого ERP-квитка для одного relay-повтору.
type voucher struct {
	g   Grant
	sid string    // пряма сесія, до якої привʼязаний (порожньо — відмова 409)
	exp time.Time // нуль — сесія ще жива, викупити не можна
}

// nodeQ — черга агента ноди: offers (обмежена), closes (ніколи не губляться:
// дедуп за id, віддаються першими), wake будить long-poll.
type nodeQ struct {
	offers chan AgentMsg
	closes []string
	wake   chan struct{}
}

// Broker — хабова сигналізація прямої ноги.
type Broker struct {
	cfg   Config
	hooks Hooks
	m     *Metrics

	mu       sync.Mutex
	sessions map[string]*session
	byNode   map[string]string // node -> id активної прямої сесії
	queues   map[string]*nodeQ
	vouchers map[string]*voucher
	// PollWait — скільки тримати long-poll агента (тести зменшують).
	PollWait time.Duration
	// SessionTTL — скільки answered-сесія живе без підтвердження агента
	// (poll з ?active=… або /p2p/result). Після — finish("agent-lost"), нода
	// вільна. Має бути > PollWait із запасом.
	SessionTTL time.Duration
	// VoucherTTL — скільки relay-квиток дійсний після відмови/відкату.
	VoucherTTL time.Duration
	now        func() time.Time
}

// NewBroker — брокер; Metrics спільні з /metrics хаба.
func NewBroker(cfg Config, h Hooks) *Broker {
	return &Broker{cfg: cfg, hooks: h, m: NewMetrics(),
		sessions: map[string]*session{}, byNode: map[string]string{},
		queues: map[string]*nodeQ{}, vouchers: map[string]*voucher{},
		PollWait: 25 * time.Second, SessionTTL: 90 * time.Second, VoucherTTL: 60 * time.Second,
		now: time.Now}
}

// Metrics — лічильники брокера.
func (b *Broker) Metrics() *Metrics { return b.m }

// Register вішає маршрути, лише якщо N6 увімкнено. wrap — напр. rate-limit.
func (b *Broker) Register(mux *http.ServeMux, wrap func(http.HandlerFunc) http.HandlerFunc) {
	if !b.cfg.Enabled {
		return
	}
	if wrap == nil {
		wrap = func(h http.HandlerFunc) http.HandlerFunc { return h }
	}
	mux.HandleFunc("/p2p/offer", wrap(b.HandleOffer))
	mux.HandleFunc("/p2p/poll", b.HandlePoll)
	mux.HandleFunc("/p2p/answer", b.HandleAnswer)
	mux.HandleFunc("/p2p/result", b.HandleResult)
}

// queue — під b.mu.
func (b *Broker) queue(node string) *nodeQ {
	q := b.queues[node]
	if q == nil {
		q = &nodeQ{offers: make(chan AgentMsg, 8), wake: make(chan struct{}, 1)}
		b.queues[node] = q
	}
	return q
}

// pushClose — під b.mu. Close не губиться: лишається в черзі, доки агент не
// забере його poll-ом.
func (b *Broker) pushClose(node, id string) {
	q := b.queue(node)
	for _, c := range q.closes {
		if c == id {
			return
		}
	}
	if len(q.closes) >= 256 { // захист пам'яті від агента, що ніколи не полить
		q.closes = q.closes[1:]
	}
	q.closes = append(q.closes, id)
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func newID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// issueVoucher — під b.mu.
func (b *Broker) issueVoucher(g Grant, sid string, exp time.Time) string {
	tok := RelayTicketPrefix + newID()
	b.vouchers[tok] = &voucher{g: g, sid: sid, exp: exp}
	return tok
}

// RedeemRelayTicket — одноразово віддає Grant (з Claims хаба) для relay-повтору
// глядача після відмови/відкату прямої ноги. Не викуповується, поки пряма
// сесія жива (щоб не мати двох ніг на один квиток), після VoucherTTL і після
// відкликання (S2).
func (b *Broker) RedeemRelayTicket(tok string) (Grant, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.vouchers[tok]
	if v == nil || v.exp.IsZero() || !b.now().Before(v.exp) {
		return Grant{}, false
	}
	delete(b.vouchers, tok)
	return v.g, true
}

// ViewerOfferReq — тіло /p2p/offer.
type ViewerOfferReq struct {
	Ticket string `json:"ticket"`
	SDP    string `json:"sdp"`
}

// ViewerOfferResp — 200: пряма нога; 409: {"fallback":"relay","reason":...}.
// RelayTicket — одноразовий квиток для /offer/viewer: у 409 дійсний одразу, у
// 200 — лише після відкату прямої ноги (ERP-квиток уже спожито тут).
type ViewerOfferResp struct {
	ID          string `json:"id,omitempty"`
	SDP         string `json:"sdp,omitempty"`
	Fallback    string `json:"fallback,omitempty"`
	Reason      string `json:"reason,omitempty"`
	RelayTicket string `json:"relay_ticket,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(v)
}

// fallback — відповідь «йди через хаб» + метрика + relay-квиток.
func (b *Broker) fallback(w http.ResponseWriter, g Grant, reason string) {
	b.m.Relay(reason)
	b.mu.Lock()
	tok := b.issueVoucher(g, "", b.now().Add(b.VoucherTTL))
	b.mu.Unlock()
	writeJSON(w, http.StatusConflict, ViewerOfferResp{Fallback: "relay", Reason: reason, RelayTicket: tok})
}

// HandleOffer — глядач просить пряму ногу.
func (b *Broker) HandleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req ViewerOfferReq
	if err := readJSON(r, &req); err != nil || req.SDP == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Авторизація — ДО будь-якої метрики/сесії: без квитка нічого нема.
	g, viewers, status, msg := b.hooks.Authorize(r, req.Ticket)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	b.m.Attempt()
	if viewers > 0 {
		b.fallback(w, g, "multi-viewer")
		return
	}
	b.mu.Lock()
	lost := b.reapLocked()
	if _, busy := b.byNode[g.Node]; busy {
		b.mu.Unlock()
		endAll(lost)
		b.fallback(w, g, "multi-viewer")
		return
	}
	s := &session{id: newID(), g: g, answer: make(chan answerMsg, 1)}
	b.sessions[s.id] = s
	b.byNode[g.Node] = s.id
	q := b.queue(g.Node)
	b.mu.Unlock()
	endAll(lost)

	if b.hooks.AuditStart != nil {
		s.end = b.hooks.AuditStart(g, s.id)
	}
	select {
	case q.offers <- AgentMsg{Type: "offer", Offer: &Offer{ID: s.id, Node: g.Node, User: g.User, Grant: g.Grant, SDP: req.SDP}}:
	default:
		b.finish(s.id, "fallback:agent-queue-full")
		b.fallback(w, g, "agent-queue-full")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.cfg.SignalTimeout)
	defer cancel()
	select {
	case a := <-s.answer:
		if a.err != "" {
			b.finish(s.id, "fallback:"+a.err)
			b.fallback(w, g, a.err)
			return
		}
		b.mu.Lock()
		tok := ""
		if !s.closed {
			tok = b.issueVoucher(g, s.id, time.Time{})
			s.voucher = tok
		}
		b.mu.Unlock()
		writeJSON(w, http.StatusOK, ViewerOfferResp{ID: s.id, SDP: a.sdp, RelayTicket: tok})
	case <-ctx.Done():
		// Агент міг підняти ногу саме зараз — хай рве її.
		b.mu.Lock()
		b.pushClose(g.Node, s.id)
		b.mu.Unlock()
		b.finish(s.id, "fallback:agent-timeout")
		b.fallback(w, g, "agent-timeout")
	}
}

// ended — закрита сесія, чий аудит end треба покликати поза b.mu.
type ended struct {
	end    func(string)
	reason string
}

func endAll(l []ended) {
	for _, e := range l {
		if e.end != nil {
			e.end(e.reason)
		}
	}
}

// closeLocked — під b.mu: вилучає сесію й звільняє ноду. Relay-квиток сесії
// стає дійсним лише при відкаті (fallback:*), інакше знищується.
func (b *Broker) closeLocked(id, reason string) (ended, bool) {
	s := b.sessions[id]
	if s == nil || s.closed {
		return ended{}, false
	}
	s.closed = true
	delete(b.sessions, id)
	if b.byNode[s.g.Node] == id {
		delete(b.byNode, s.g.Node)
	}
	if v := b.vouchers[s.voucher]; v != nil {
		if strings.HasPrefix(reason, "fallback:") {
			v.exp = b.now().Add(b.VoucherTTL)
		} else {
			delete(b.vouchers, s.voucher)
		}
	}
	return ended{end: s.end, reason: reason}, true
}

// finish закриває сесію (аудит end) і звільняє ноду. Ідемпотентно.
func (b *Broker) finish(id, reason string) {
	b.mu.Lock()
	e, ok := b.closeLocked(id, reason)
	b.mu.Unlock()
	if ok {
		endAll([]ended{e})
	}
}

// reapLocked — під b.mu: answered-сесії без підтвердження агента довше за
// SessionTTL закриваються як "agent-lost" (агент упав/перезапустився, загубив
// /p2p/result) — інакше нода назавжди «зайнята». Протерміновані relay-квитки
// видаляються. Аудит end повертається викликачеві, щоб покликати поза b.mu.
func (b *Broker) reapLocked() []ended {
	now := b.now()
	var out []ended
	for id, s := range b.sessions {
		if !s.answered.IsZero() && now.Sub(s.seen) > b.SessionTTL {
			b.pushClose(s.g.Node, id)
			if e, ok := b.closeLocked(id, "agent-lost"); ok {
				out = append(out, e)
			}
		}
	}
	for tok, v := range b.vouchers {
		if !v.exp.IsZero() && !now.Before(v.exp) {
			delete(b.vouchers, tok)
		}
	}
	return out
}

// HandlePoll — long-poll агента: GET /p2p/poll?node=...[&active=id1,id2].
// active — ноги, які агент реально тримає (AgentLegs.Active): це heartbeat
// (без нього answered-сесія вмирає за SessionTTL), а нога, якої хаб уже не
// знає (відкликана, закрита, протермінована), отримує "close". Якщо active
// передано, answered-сесії ноди, яких у списку нема, закриваються як
// "agent-lost" (агент перезапустився).
func (b *Broker) HandlePoll(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	if !b.hooks.AgentAuth(r, node) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	started := b.now()
	b.mu.Lock()
	var lost []ended
	if r.URL.Query().Has("active") {
		listed := map[string]bool{}
		for _, id := range strings.Split(r.URL.Query().Get("active"), ",") {
			if id = strings.TrimSpace(id); id == "" || len(listed) >= 64 {
				continue
			}
			listed[id] = true
			if s := b.sessions[id]; s != nil && s.g.Node == node {
				s.seen = started
			} else {
				b.pushClose(node, id)
			}
		}
		for id, s := range b.sessions {
			if s.g.Node == node && !listed[id] && !s.answered.IsZero() && s.answered.Before(started) {
				if e, ok := b.closeLocked(id, "agent-lost"); ok {
					lost = append(lost, e)
				}
			}
		}
	}
	lost = append(lost, b.reapLocked()...)
	q := b.queue(node)
	b.mu.Unlock()
	endAll(lost)
	t := time.NewTimer(b.PollWait)
	defer t.Stop()
	for {
		b.mu.Lock()
		if len(q.closes) > 0 {
			id := q.closes[0]
			q.closes = q.closes[1:]
			b.mu.Unlock()
			writeJSON(w, http.StatusOK, AgentMsg{Type: "close", ID: id, Reason: "closed-by-hub"})
			return
		}
		b.mu.Unlock()
		select {
		case m := <-q.offers:
			writeJSON(w, http.StatusOK, m)
			return
		case <-q.wake:
		case <-t.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// AgentAnswerReq — тіло /p2p/answer. Error непорожній = агент відмовив
// (напр. "consent"), і глядач іде через хаб.
type AgentAnswerReq struct {
	ID    string `json:"id"`
	Node  string `json:"node"`
	SDP   string `json:"sdp"`
	Error string `json:"error"`
}

// lookup — сесія, що належить саме цій ноді (агент чужої ноди не відповість).
func (b *Broker) lookup(id, node string) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sessions[id]
	if s == nil || s.g.Node != node {
		return nil
	}
	return s
}

// gone — сесії вже нема (відкликана/закрита/протермінована): 410, агент має
// порвати ногу, якщо вже підняв її.
func gone(w http.ResponseWriter) {
	http.Error(w, "session closed", http.StatusGone)
}

// HandleAnswer — агент віддає answer (або відмову).
func (b *Broker) HandleAnswer(w http.ResponseWriter, r *http.Request) {
	var req AgentAnswerReq
	if r.Method != http.MethodPost || readJSON(r, &req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !b.hooks.AgentAuth(r, req.Node) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s := b.lookup(req.ID, req.Node)
	if s == nil {
		gone(w)
		return
	}
	if req.Error == "" && req.SDP == "" {
		req.Error = "empty-answer"
	}
	if req.Error == "" {
		b.mu.Lock()
		now := b.now()
		s.answered, s.seen = now, now
		b.mu.Unlock()
	}
	select {
	case s.answer <- answerMsg{sdp: req.SDP, err: req.Error}:
	default:
	}
	w.WriteHeader(http.StatusNoContent)
}

// ResultReq — агент звітує стан прямої ноги (агент, а не глядач: глядач міг
// би збрехати про шлях і зіпсувати метрики).
type ResultReq struct {
	ID     string `json:"id"`
	Node   string `json:"node"`
	State  string `json:"state"`
	Pair   string `json:"pair,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// HandleResult — direct/turn/fallback/closed.
func (b *Broker) HandleResult(w http.ResponseWriter, r *http.Request) {
	var req ResultReq
	if r.Method != http.MethodPost || readJSON(r, &req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !b.hooks.AgentAuth(r, req.Node) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s := b.lookup(req.ID, req.Node)
	if s == nil {
		gone(w)
		return
	}
	b.Report(s.id, req.State, req.Pair, req.Reason)
	w.WriteHeader(http.StatusNoContent)
}

// Report — облік результату (і з HTTP, і напряму в тестах).
func (b *Broker) Report(id, state, pair, reason string) {
	b.mu.Lock()
	s := b.sessions[id]
	first := s != nil && !s.decided
	if first && (state == StateDirect || state == StateTURN || state == StateFallback) {
		s.decided = true
	}
	if s != nil && !s.answered.IsZero() {
		s.seen = b.now()
	}
	b.mu.Unlock()
	if s == nil {
		return
	}
	switch state {
	case StateDirect, StateTURN:
		if first {
			b.m.Direct(pair, state == StateTURN)
		}
	case StateFallback:
		if first {
			b.m.Relay(nonEmpty(reason, "ice"))
		}
		b.finish(id, "fallback:"+nonEmpty(reason, "ice"))
	case StateClosed:
		b.finish(id, nonEmpty(reason, "closed"))
	}
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// RevokeUser — S2-відкликання: закрити прямі сесії користувача (агент
// отримує "close" і рве ногу).
func (b *Broker) RevokeUser(user string) int {
	return b.RevokeWhere(func(g Grant) bool { return g.User == user })
}

// RevokeWhere — закрити прямі сесії, для яких match (нода, stale ERP тощо), і
// знищити їхні relay-квитки. "close" ставиться в чергу, що не губить
// повідомлень; навіть без неї агент отримає "close" на першому poll з
// ?active=<id>. Хаб НЕ може фізично перервати пряме медіа — це робить агент
// (AgentLegs.Handle); агент, що не полить, обрізається лише в обліку хаба.
func (b *Broker) RevokeWhere(match func(Grant) bool) int {
	b.mu.Lock()
	var ids []string
	for id, s := range b.sessions {
		if match(s.g) {
			ids = append(ids, id)
			b.pushClose(s.g.Node, id)
		}
	}
	for tok, v := range b.vouchers {
		if match(v.g) {
			delete(b.vouchers, tok)
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.finish(id, "revoked")
	}
	return len(ids)
}

// Active — к-сть живих прямих сесій.
func (b *Broker) Active() int {
	b.mu.Lock()
	lost := b.reapLocked()
	n := len(b.sessions)
	b.mu.Unlock()
	endAll(lost)
	return n
}
