// N6: пряма нога агент↔браузер на боці oo-agent. Механіка (згода S3, grant,
// ICE-монітор, реєстр ніг) — internal/p2p; тут лише цикл агента:
//
//	GET /p2p/poll?node=…&active=<id,…>  (heartbeat живих ніг)
//	  "offer" → згода → NewAgentLeg → AgentLegs.Add → POST /p2p/answer
//	  "close" → AgentLegs.Handle (S2-відкликання, agent-lost, таймаут)
//	OnState(direct/turn/fallback/closed) → POST /p2p/result
//
// Платформонезалежний (тест на Linux). main.go (windows) лише вмикає його за
// -p2p / OO_SCREEN_P2P=1 і подає кадри (writeVideo) та ввід (onInput).
// Дефолт — вимкнено.
//
// Чого пряма нога НЕ несе (лишається на relay): звук, шар курсора, текстові
// тайли, oosc-ctl (bitrate_target хаба). Енкодер тримає ту ціль, яку хаб
// поставив востаннє.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/organicoils/oo-screen/internal/p2p"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"golang.org/x/time/rate"
)

// Стеля подій вводу на прямій нозі — хаб тут не стоїть, тож обмежує агент.
const (
	p2pInputRate  = 250
	p2pInputBurst = 500
)

type p2pLegEntry struct {
	leg   *p2p.AgentLeg
	track *webrtc.TrackLocalStaticSample
}

type p2pAgent struct {
	cfg    p2p.Config
	base   func() string // http(s)://host:port хаба
	node   string
	token  func() string
	client *http.Client
	// consent — S3 (nil = політика off). AgentLeg сам перевіряє його до
	// answer, на кожну подію вводу і посеред сесії.
	consent p2p.Consent
	// requestConsent — попросити згоду (діалог) і чекати її не довше ctx.
	// nil = лише поточний consent.Allowed().
	requestConsent func(ctx context.Context) bool
	onInput        func([]byte) // вже розгорнута подія (input.Event JSON)
	onActive       func(n int)  // к-сть живих ніг змінилась
	onKeyframe     func()       // PLI/FIR від браузера
	se             *webrtc.SettingEngine
	retryMin       time.Duration

	legs    p2p.AgentLegs
	mu      sync.Mutex
	entries map[string]p2pLegEntry
	lim     *rate.Limiter
}

func newP2PAgent(cfg p2p.Config, base func() string, node string, token func() string) *p2pAgent {
	return &p2pAgent{
		cfg: cfg, base: base, node: node, token: token,
		// > PollWait брокера (25 с) із запасом.
		client:   &http.Client{Timeout: 45 * time.Second},
		entries:  map[string]p2pLegEntry{},
		lim:      rate.NewLimiter(p2pInputRate, p2pInputBurst),
		retryMin: time.Second,
	}
}

// p2pHubBase — "http://h:4470/offer/agent" → "http://h:4470".
func p2pHubBase(hubAddr string) string {
	u, err := url.Parse(hubAddr)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (a *p2pAgent) req(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, a.base()+path, rd)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+a.token())
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return a.client.Do(r)
}

// post — відповідь хаба на answer/result; 410 = сесії вже нема.
func (a *p2pAgent) post(path string, body any) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := a.req(ctx, http.MethodPost, path, body)
	if err != nil {
		log.Printf("oo-agent: p2p %s: %v", path, err)
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// run — long-poll цикл до ctx.Done(); на виході рве всі ноги.
func (a *p2pAgent) run(ctx context.Context) {
	defer func() {
		a.legs.CloseAll()
		a.prune()
	}()
	backoff := a.retryMin
	for ctx.Err() == nil {
		m, err := a.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("oo-agent: p2p poll: %v (повтор через %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		backoff = a.retryMin
		if a.legs.Handle(m) {
			log.Printf("oo-agent: p2p close %s (%s)", shortID(m.ID), m.Reason)
			a.prune()
			continue
		}
		if m.Type == "offer" && m.Offer != nil {
			a.handleOffer(ctx, *m.Offer)
		}
		a.prune()
	}
}

func (a *p2pAgent) poll(ctx context.Context) (p2p.AgentMsg, error) {
	var m p2p.AgentMsg
	q := url.Values{"node": {a.node}, "active": {a.legs.Active()}}
	resp, err := a.req(ctx, http.MethodGet, "/p2p/poll?"+q.Encode(), nil)
	if err != nil {
		return m, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m)
		return m, err
	case http.StatusNoContent:
		return m, nil
	default:
		return m, errors.New("poll status " + resp.Status)
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (a *p2pAgent) consentOK(ctx context.Context) bool {
	if a.consent == nil || a.consent.Allowed() {
		return true
	}
	if a.requestConsent == nil {
		return false
	}
	// Хаб чекає answer SignalTimeout; лишаємо запас на сам answer.
	wait := a.cfg.SignalTimeout - 2*time.Second
	if wait <= 0 {
		wait = time.Second
	}
	c, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return a.requestConsent(c)
}

func (a *p2pAgent) handleOffer(ctx context.Context, o p2p.Offer) {
	if o.Node != a.node {
		a.post("/p2p/answer", p2p.AgentAnswerReq{ID: o.ID, Node: a.node, Error: "wrong-node"})
		return
	}
	if !a.consentOK(ctx) {
		log.Printf("oo-agent: p2p %s: згоди нема — глядач піде через хаб", shortID(o.ID))
		a.post("/p2p/answer", p2p.AgentAnswerReq{ID: o.ID, Node: a.node, Error: "consent"})
		a.notifyActive()
		return
	}
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264Fmtp(),
	}, "video", "oo-screen-p2p")
	if err != nil {
		a.post("/p2p/answer", p2p.AgentAnswerReq{ID: o.ID, Node: a.node, Error: "error"})
		return
	}
	id := o.ID
	leg, sdp, err := p2p.NewAgentLeg(o, p2p.AgentLegOptions{
		Config: a.cfg, SettingEngine: a.se, Consent: a.consent,
		Tracks:  []webrtc.TrackLocal{track},
		OnInput: a.input,
		OnState: func(state, pair, reason string) {
			log.Printf("oo-agent: p2p %s: %s %s %s", shortID(id), state, pair, reason)
			if state == p2p.StateFallback || state == p2p.StateClosed {
				a.drop(id)
			}
			a.post("/p2p/result", p2p.ResultReq{ID: id, Node: a.node, State: state, Pair: pair, Reason: reason})
		},
	})
	if err != nil {
		reason := "error"
		if errors.Is(err, p2p.ErrNoConsent) {
			reason = "consent"
		}
		log.Printf("oo-agent: p2p %s: нога не піднялась: %v", shortID(id), err)
		a.post("/p2p/answer", p2p.AgentAnswerReq{ID: id, Node: a.node, Error: reason})
		a.notifyActive()
		return
	}
	// Реєстрація ДО answer: наступний poll уже має назвати ногу в active.
	a.legs.Add(id, leg)
	a.mu.Lock()
	a.entries[id] = p2pLegEntry{leg: leg, track: track}
	a.mu.Unlock()
	for _, s := range leg.PC.GetSenders() {
		go a.readRTCP(s)
	}
	if st := a.post("/p2p/answer", p2p.AgentAnswerReq{ID: id, Node: a.node, SDP: sdp}); st == http.StatusGone || st == 0 {
		// Хаб уже не знає сесії (таймаут/відкликання) або недосяжний.
		_ = leg.Close()
		a.drop(id)
		return
	}
	a.notifyActive()
	if a.onKeyframe != nil {
		a.onKeyframe() // новий глядач — IDR одразу
	}
}

func (a *p2pAgent) readRTCP(s *webrtc.RTPSender) {
	for {
		pkts, _, err := s.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if a.onKeyframe != nil {
					a.onKeyframe()
				}
			}
		}
	}
}

// input — подія з oosc-input прямої ноги (grant і згоду вже перевірив
// AgentLeg). Браузер шле той самий конверт, що й на relay: {ticket, event};
// хаб тут конверт не знімає, тож знімаємо ми. Тікет агент перевірити не може
// (ERP-квиток спожив хаб) — ногу автентифікує сама сигналізація хаба (DTLS
// прив'язаний до SDP, що пройшов через /p2p/offer з квитком).
func (a *p2pAgent) input(data []byte) {
	if !a.lim.Allow() {
		return
	}
	var env struct {
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Event) == 0 || string(env.Event) == "null" {
		return
	}
	if a.onInput != nil {
		a.onInput(env.Event)
	}
}

// drop — прибрати ногу з fan-out-у кадрів.
func (a *p2pAgent) drop(id string) {
	a.mu.Lock()
	_, had := a.entries[id]
	delete(a.entries, id)
	a.mu.Unlock()
	if had {
		a.notifyActive()
	}
}

// prune — прибрати ноги, які закрились (close хаба, згода, вихід).
func (a *p2pAgent) prune() {
	var gone []string
	a.mu.Lock()
	for id, e := range a.entries {
		if c, _ := e.leg.Closed(); c {
			gone = append(gone, id)
		}
	}
	a.mu.Unlock()
	for _, id := range gone {
		a.drop(id)
	}
}

func (a *p2pAgent) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries)
}

func (a *p2pAgent) notifyActive() {
	if a.onActive != nil {
		a.onActive(a.count())
	}
}

// writeVideo — той самий AU, що пішов на хаб, у кожну живу пряму ногу.
func (a *p2pAgent) writeVideo(data []byte, dur time.Duration) {
	a.mu.Lock()
	if len(a.entries) == 0 {
		a.mu.Unlock()
		return
	}
	tracks := make([]*webrtc.TrackLocalStaticSample, 0, len(a.entries))
	for _, e := range a.entries {
		tracks = append(tracks, e.track)
	}
	a.mu.Unlock()
	for _, t := range tracks {
		_ = t.WriteSample(media.Sample{Data: data, Duration: dur})
	}
}

// p2pConfig — -p2p або OO_SCREEN_P2P=1 (той самий прапорець, що в хабі).
func p2pConfig(flagOn bool, stun string, getenv func(string) string) p2p.Config {
	cfg := p2p.ConfigFromEnv(getenv)
	if flagOn {
		cfg.Enabled = true
	}
	if stun != "" {
		cfg.STUN = nil
		for _, u := range strings.Split(stun, ",") {
			if u = strings.TrimSpace(u); u != "" {
				cfg.STUN = append(cfg.STUN, u)
			}
		}
	}
	return cfg
}
