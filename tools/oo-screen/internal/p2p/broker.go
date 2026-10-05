package p2p

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
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
	id      string
	g       Grant
	answer  chan answerMsg
	end     func(string)
	decided bool // результат (direct/turn/fallback) уже враховано в метриках
	closed  bool
}

type answerMsg struct {
	sdp string
	err string
}

// Broker — хабова сигналізація прямої ноги.
type Broker struct {
	cfg   Config
	hooks Hooks
	m     *Metrics

	mu       sync.Mutex
	sessions map[string]*session
	byNode   map[string]string        // node -> id активної прямої сесії
	queues   map[string]chan AgentMsg // node -> черга для poll агента
	// PollWait — скільки тримати long-poll агента (тести зменшують).
	PollWait time.Duration
}

// NewBroker — брокер; Metrics спільні з /metrics хаба.
func NewBroker(cfg Config, h Hooks) *Broker {
	return &Broker{cfg: cfg, hooks: h, m: NewMetrics(),
		sessions: map[string]*session{}, byNode: map[string]string{},
		queues: map[string]chan AgentMsg{}, PollWait: 25 * time.Second}
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

func (b *Broker) queue(node string) chan AgentMsg {
	q := b.queues[node]
	if q == nil {
		q = make(chan AgentMsg, 8)
		b.queues[node] = q
	}
	return q
}

func newID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// ViewerOfferReq — тіло /p2p/offer.
type ViewerOfferReq struct {
	Ticket string `json:"ticket"`
	SDP    string `json:"sdp"`
}

// ViewerOfferResp — 200: пряма нога; 409: {"fallback":"relay","reason":...}.
type ViewerOfferResp struct {
	ID       string `json:"id,omitempty"`
	SDP      string `json:"sdp,omitempty"`
	Fallback string `json:"fallback,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(v)
}

// fallback — відповідь «йди через хаб» + метрика.
func (b *Broker) fallback(w http.ResponseWriter, reason string) {
	b.m.Relay(reason)
	writeJSON(w, http.StatusConflict, ViewerOfferResp{Fallback: "relay", Reason: reason})
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
		b.fallback(w, "multi-viewer")
		return
	}
	b.mu.Lock()
	if _, busy := b.byNode[g.Node]; busy {
		b.mu.Unlock()
		b.fallback(w, "multi-viewer")
		return
	}
	s := &session{id: newID(), g: g, answer: make(chan answerMsg, 1)}
	b.sessions[s.id] = s
	b.byNode[g.Node] = s.id
	q := b.queue(g.Node)
	b.mu.Unlock()

	if b.hooks.AuditStart != nil {
		s.end = b.hooks.AuditStart(g, s.id)
	}
	select {
	case q <- AgentMsg{Type: "offer", Offer: &Offer{ID: s.id, Node: g.Node, User: g.User, Grant: g.Grant, SDP: req.SDP}}:
	default:
		b.finish(s.id, "fallback:agent-queue-full")
		b.fallback(w, "agent-queue-full")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), b.cfg.SignalTimeout)
	defer cancel()
	select {
	case a := <-s.answer:
		if a.err != "" {
			b.finish(s.id, "fallback:"+a.err)
			b.fallback(w, a.err)
			return
		}
		writeJSON(w, http.StatusOK, ViewerOfferResp{ID: s.id, SDP: a.sdp})
	case <-ctx.Done():
		b.finish(s.id, "fallback:agent-timeout")
		b.fallback(w, "agent-timeout")
	}
}

// finish закриває сесію (аудит end) і звільняє ноду. Ідемпотентно.
func (b *Broker) finish(id, reason string) {
	b.mu.Lock()
	s := b.sessions[id]
	if s == nil || s.closed {
		b.mu.Unlock()
		return
	}
	s.closed = true
	delete(b.sessions, id)
	if b.byNode[s.g.Node] == id {
		delete(b.byNode, s.g.Node)
	}
	b.mu.Unlock()
	if s.end != nil {
		s.end(reason)
	}
}

// HandlePoll — long-poll агента: GET /p2p/poll?node=...
func (b *Broker) HandlePoll(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	if !b.hooks.AgentAuth(r, node) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	b.mu.Lock()
	q := b.queue(node)
	b.mu.Unlock()
	t := time.NewTimer(b.PollWait)
	defer t.Stop()
	select {
	case m := <-q:
		writeJSON(w, http.StatusOK, m)
	case <-t.C:
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
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
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	if req.Error == "" && req.SDP == "" {
		req.Error = "empty-answer"
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
		http.Error(w, "no such session", http.StatusNotFound)
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

// RevokeWhere — закрити прямі сесії, для яких match (нода, stale ERP тощо).
func (b *Broker) RevokeWhere(match func(Grant) bool) int {
	b.mu.Lock()
	var ids []string
	for id, s := range b.sessions {
		if match(s.g) {
			ids = append(ids, id)
			select {
			case b.queue(s.g.Node) <- AgentMsg{Type: "close", ID: id, Reason: "revoked"}:
			default:
			}
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
	defer b.mu.Unlock()
	return len(b.sessions)
}
