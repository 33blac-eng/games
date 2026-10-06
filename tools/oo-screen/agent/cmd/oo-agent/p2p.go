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
// Що пряма нога несе, крім відео й вводу (N6, хвіст):
//   - звук: та сама Opus/PCMU-доріжка, що на relay (OO_SCREEN_AUDIO), якщо
//     браузер запропонував m=audio; кадри — ті самі, що йдуть хабу;
//   - шар курсора: канал oosc-cursor, який відкриває браузер (config.
//     cursorLayer), підключається до того самого cursorPub (-cursor-layer);
//   - локальний контролер бітрейту (-p2p-bwe / OO_SCREEN_P2P_BWE=1, дефолт
//     вимкнено): хаба між енкодером і глядачем нема, тож bitrate_target хаба
//     сюди не доходить. internal/bwe.LegCtl на RTCP самої ноги (RR-втрати,
//     REMB; з OO_SCREEN_DELAYBWE=1 — ще transport-cc і той самий детектор
//     затримки, що в хабі). Без прапорця енкодер тримає останню ціль хаба.
//
// Чого НЕ несе: текстові тайли, додаткові монітори (F6) — лише relay.
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

	"github.com/organicoils/oo-screen/internal/bwe"
	"github.com/organicoils/oo-screen/internal/cursorproto"
	"github.com/organicoils/oo-screen/internal/p2p"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
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
	audio *webrtc.TrackLocalStaticSample // nil — без звуку
	ctl   *bwe.LegCtl                    // nil — без локального контролера
	twcc  *bwe.TWCC                      // nil — без transport-cc
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

	// audio — нести звук (audioEnabled); audioCap — кодек доріжки.
	audio    bool
	audioCap webrtc.RTPCodecCapability
	// cursor — куди підключати oosc-cursor прямої ноги (nil — шар вимкнено).
	cursor *cursorFan
	// bwe — локальний контролер бітрейту прямої ноги; delayBWE — разом з
	// transport-cc і детектором затримки. startBps/ceilBps — поточна ціль і
	// стеля енкодера; onBitrate — нова ціль (через той самий шлях, що й
	// bitrate_target хаба: застосовує кадровий цикл).
	bwe       bool
	delayBWE  bool
	startBps  func() uint64
	ceilBps   uint64
	onBitrate func(bps uint64)

	// Арбітраж цілі: енкодер один на всіх, тож застосовуємо МІНІМУМ з цілей
	// усіх живих прямих ніг і останньої цілі хаба (relay-глядачі). Інакше
	// здорова нога (REMB/+5 %) перебивала б зріз ноги з втратами, а локальний
	// контролер — зріз хаба.
	bmu    sync.Mutex
	legBps map[string]uint64
	hubBps uint64 // 0 — хаб ціль не ставив
	// hubIdle — у хаба НЕМА relay-глядачів (його gate "pause" або хаб відпав):
	// тоді остання ціль хаба — застаріла і енкодер не тримає. Типово false:
	// старий хаб без gate лишається в мінімумі, як до цього.
	hubIdle bool
	lastBps uint64

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
		legBps:   map[string]uint64{},
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
	ent := p2pLegEntry{track: track}
	tracks := []webrtc.TrackLocal{track}
	if a.audio && offersAudio(o.SDP) {
		at, aerr := webrtc.NewTrackLocalStaticSample(a.audioCap, "audio", "oo-screen-p2p")
		if aerr == nil {
			ent.audio = at
			tracks = append(tracks, at)
		}
	}
	if a.bwe {
		start, ceil := a.ceilBps, a.ceilBps
		if a.startBps != nil {
			if b := a.startBps(); b > 0 {
				start = b
			}
		}
		ent.ctl = bwe.NewLegCtl(start, ceil)
		if a.delayBWE {
			ent.twcc = bwe.NewTWCC()
		}
	}
	leg, sdp, err := p2p.NewAgentLeg(o, p2p.AgentLegOptions{
		Config: a.cfg, SettingEngine: a.se, Consent: a.consent,
		Tracks:  tracks,
		Setup:   ent.setup,
		OnInput: a.input,
		OnDataChannel: func(dc *webrtc.DataChannel) {
			if dc.Label() == cursorproto.ChannelLabel && a.cursor != nil {
				a.cursor.attach(id, dc)
			}
		},
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
	// F9: і в дозволі шару курсора — поки браузер ноги не відкрив
	// oosc-cursor, вказівник лишається в кадрі (кадри підуть лише після ICE).
	if a.cursor != nil {
		a.cursor.legAdd(id)
	}
	a.legs.Add(id, leg)
	a.mu.Lock()
	ent.leg = leg
	a.entries[id] = ent
	a.mu.Unlock()
	for _, s := range leg.PC.GetSenders() {
		go a.readRTCP(id, s, ent, s.Track() == webrtc.TrackLocal(track))
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

// setup — MediaEngine/interceptor-и ноги: transport-cc і запис відправок
// лише з delayBWE (інакше SDP ноги — рівно як до N6-хвоста).
func (e p2pLegEntry) setup(me *webrtc.MediaEngine, reg *interceptor.Registry) error {
	if e.twcc == nil {
		return nil
	}
	if err := me.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: sdp.TransportCCURI}, webrtc.RTPCodecTypeVideo); err != nil {
		return err
	}
	me.RegisterFeedback(webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC}, webrtc.RTPCodecTypeVideo)
	reg.Add(&bwe.RecorderFactory{TWCC: e.twcc})
	return nil
}

// offersAudio — чи браузер запропонував m=audio (config.audio плеєра).
func offersAudio(sdpText string) bool {
	for _, l := range strings.Split(sdpText, "\n") {
		if strings.HasPrefix(l, "m=audio ") {
			return true
		}
	}
	return false
}

// readRTCP — PLI/FIR -> IDR; для відео-sender-а з контролером — ще RR,
// REMB і TWCC у bwe.LegCtl.
func (a *p2pAgent) readRTCP(id string, s *webrtc.RTPSender, e p2pLegEntry, video bool) {
	for {
		pkts, _, err := s.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch pk := p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				if a.onKeyframe != nil {
					a.onKeyframe()
				}
			case *rtcp.ReceiverReport:
				if video && e.ctl != nil {
					a.onRR(id, s, e, pk)
				}
			case *rtcp.ReceiverEstimatedMaximumBitrate:
				if video && e.ctl != nil {
					a.legTarget(id)(e.ctl.OnRemb(uint64(pk.Bitrate), time.Now()))
				}
			case *rtcp.TransportLayerCC:
				if video && e.ctl != nil && e.twcc != nil {
					a.legTarget(id)(e.ctl.OnTWCC(e.twcc.OnFeedback(pk), time.Now()))
				}
			}
		}
	}
}

func (a *p2pAgent) onRR(id string, s *webrtc.RTPSender, e p2pLegEntry, rr *rtcp.ReceiverReport) {
	ssrc := map[uint32]bool{}
	for _, enc := range s.GetParameters().Encodings {
		ssrc[uint32(enc.SSRC)] = true
	}
	for _, r := range rr.Reports {
		if ssrc[r.SSRC] {
			a.legTarget(id)(e.ctl.OnLoss(float64(r.FractionLost)/256, time.Now()))
		}
	}
}

// legTarget — приймач (ціль, змінилась) LegCtl ноги id.
func (a *p2pAgent) legTarget(id string) func(bps uint64, changed bool) {
	return func(bps uint64, changed bool) { a.applyBitrate(id, bps, changed) }
}

func (a *p2pAgent) applyBitrate(id string, bps uint64, changed bool) {
	if !changed {
		return
	}
	a.bmu.Lock()
	a.legBps[id] = bps
	a.arbitrateLocked()
	a.bmu.Unlock()
}

// setHubTarget — bitrate_target хаба (relay-глядачі) іде в той самий арбітраж.
func (a *p2pAgent) setHubTarget(bps uint64) {
	a.bmu.Lock()
	a.hubBps = bps
	a.arbitrateLocked()
	a.bmu.Unlock()
}

// setHubViewers — присутність relay-глядачів за gate хаба ("resume"/"pause").
// Без них ціль хаба виходить з мінімуму; з новим глядачем — повертається
// (хаб однаково пришле свіжий bitrate_target, коли зʼїде його BWE).
func (a *p2pAgent) setHubViewers(present bool) {
	a.bmu.Lock()
	if a.hubIdle == present {
		a.hubIdle = !present
		a.arbitrateLocked()
	}
	a.bmu.Unlock()
}

// forgetLeg — нога пішла: її ціль більше не тримає енкодер.
func (a *p2pAgent) forgetLeg(id string) {
	a.bmu.Lock()
	if _, ok := a.legBps[id]; ok {
		delete(a.legBps, id)
		a.arbitrateLocked()
	}
	a.bmu.Unlock()
}

// arbitrateLocked — мінімум ненульових цілей; викликає onBitrate лише на зміну.
// onBitrate (main) лише кладе ціль для кадрового циклу — під bmu безпечно і
// зберігає порядок застосувань.
func (a *p2pAgent) arbitrateLocked() {
	var m uint64
	for _, b := range a.legBps {
		if b > 0 && (m == 0 || b < m) {
			m = b
		}
	}
	if !a.hubIdle && a.hubBps > 0 && (m == 0 || a.hubBps < m) {
		m = a.hubBps
	}
	if m == 0 || m == a.lastBps || a.onBitrate == nil {
		return
	}
	a.lastBps = m
	a.onBitrate(m)
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
	a.forgetLeg(id)
	if a.cursor != nil {
		a.cursor.legDrop(id)
	}
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

// writeAudio — той самий звуковий кадр, що пішов на хаб, у кожну живу пряму
// ногу зі звуком.
func (a *p2pAgent) writeAudio(data []byte, dur time.Duration) {
	a.mu.Lock()
	var tracks []*webrtc.TrackLocalStaticSample
	for _, e := range a.entries {
		if e.audio != nil {
			tracks = append(tracks, e.audio)
		}
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
