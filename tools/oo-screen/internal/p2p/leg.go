package p2p

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Consent — гейт S3 агента (consent.Gate задовольняє). nil = політика off.
type Consent interface {
	Allowed() bool
}

// AgentLegOptions — що агент дає прямій нозі.
type AgentLegOptions struct {
	Config        Config
	SettingEngine *webrtc.SettingEngine // nil = дефолт
	Consent       Consent
	Tracks        []webrtc.TrackLocal
	// OnInput — подія вводу, що пройшла grant+consent.
	OnInput func([]byte)
	// OnState — direct/turn/fallback (+pair або причина). Агент шле це в
	// /p2p/result.
	OnState func(state, pair, reason string)
	// ConsentPoll — як часто перевіряти згоду посеред сесії; відкликання
	// («Завершити») рве PeerConnection — і відео, і ввід. 0 = 250 мс.
	ConsentPoll time.Duration
}

// AgentLeg — пряма нога на боці агента (answerer).
type AgentLeg struct {
	PC        *webrtc.PeerConnection
	offer     Offer
	opt       AgentLegOptions
	dropped   atomic.Int64
	delivered atomic.Int64
	closed    atomic.Bool
	done      chan struct{}
	once      sync.Once
	reason    atomic.Value // string: чому нога закрилась
}

// NewAgentLeg перевіряє згоду, піднімає PeerConnection і повертає answer SDP
// (non-trickle: кандидати вже в SDP). ErrNoConsent — без згоди нога не
// створюється взагалі: жодного кадру не піде.
func NewAgentLeg(o Offer, opt AgentLegOptions) (*AgentLeg, string, error) {
	if opt.Consent != nil && !opt.Consent.Allowed() {
		return nil, "", ErrNoConsent
	}
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return nil, "", err
	}
	se := webrtc.SettingEngine{}
	if opt.SettingEngine != nil {
		se = *opt.SettingEngine
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: opt.Config.ICEServers()})
	if err != nil {
		return nil, "", err
	}
	l := &AgentLeg{PC: pc, offer: o, opt: opt, done: make(chan struct{})}
	for _, t := range opt.Tracks {
		if _, err := pc.AddTrack(t); err != nil {
			_ = pc.Close()
			return nil, "", err
		}
	}
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != InputLabel {
			return
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) { l.handleInput(m.Data) })
	})
	Monitor(pc, opt.Config, nil, func(state, pair, reason string) {
		if l.closed.Load() {
			return // нога вже закрита (згода/хаб) — її кінець уже звітовано
		}
		if opt.OnState != nil {
			opt.OnState(state, pair, reason)
		}
		if state == StateFallback {
			l.shutdown("fallback:"+reason, false) // глядач іде через хаб — ногу рвемо
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.SDP}); err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(ans); err != nil {
		_ = pc.Close()
		return nil, "", err
	}
	<-done
	if opt.Consent != nil {
		go l.watchConsent()
	}
	return l, pc.LocalDescription().SDP, nil
}

// watchConsent — S3 посеред сесії: на relay хаб може перерізати потік, а на
// прямій нозі — лише агент. Згода зникла → PeerConnection рветься (і кадри,
// і ввід), хабу йде StateClosed("consent-withdrawn").
func (l *AgentLeg) watchConsent() {
	every := l.opt.ConsentPoll
	if every <= 0 {
		every = 250 * time.Millisecond
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-t.C:
			if !l.opt.Consent.Allowed() {
				l.shutdown("consent-withdrawn", true)
				return
			}
		}
	}
}

// shutdown — ідемпотентно рве ногу; report — чи звітувати хабу StateClosed.
func (l *AgentLeg) shutdown(reason string, report bool) {
	l.once.Do(func() {
		l.closed.Store(true)
		l.reason.Store(reason)
		close(l.done)
		_ = l.PC.Close()
		if report && l.opt.OnState != nil {
			l.opt.OnState(StateClosed, "", reason)
		}
	})
}

// Closed — чи нога вже закрита, і чому.
func (l *AgentLeg) Closed() (bool, string) {
	r, _ := l.reason.Load().(string)
	return l.closed.Load(), r
}

// handleInput — та сама правда, що judgeInput на relay-нозі: grant із квитка
// (хаб передав його в offer), і згода, перевірена на КОЖНУ подію (її можна
// відкликати посеред сесії кнопкою «Завершити»).
func (l *AgentLeg) handleInput(data []byte) {
	if l.offer.Grant != GrantControl || (l.opt.Consent != nil && !l.opt.Consent.Allowed()) {
		l.dropped.Add(1)
		return
	}
	l.delivered.Add(1)
	if l.opt.OnInput != nil {
		l.opt.OnInput(data)
	}
}

// InputCounts — (доставлено, відкинуто).
func (l *AgentLeg) InputCounts() (delivered, dropped int64) {
	return l.delivered.Load(), l.dropped.Load()
}

// Close рве ногу.
// Без звіту хабу: викликач (агент на "close" від хаба) і так знає.
func (l *AgentLeg) Close() error {
	l.shutdown("closed", false)
	return nil
}

// AgentLegs — реєстр прямих ніг агента: Active() — значення ?active= для
// /p2p/poll (heartbeat), Handle — виконує "close" від хаба (S2-відкликання,
// agent-lost, таймаут сигналізації). Без цього хаб не може зупинити пряме
// медіа: він лише сигналізує.
type AgentLegs struct {
	mu   sync.Mutex
	legs map[string]*AgentLeg
}

// Add — зареєструвати ногу ДО відправки answer (інакше poll, що стартує
// після answer, не назве її, і хаб вважатиме її втраченою).
func (a *AgentLegs) Add(id string, l *AgentLeg) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.legs == nil {
		a.legs = map[string]*AgentLeg{}
	}
	a.legs[id] = l
}

// Active — id живих ніг через кому (закриті прибираються).
func (a *AgentLegs) Active() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []string
	for id, l := range a.legs {
		if c, _ := l.Closed(); c {
			delete(a.legs, id)
			continue
		}
		ids = append(ids, id)
	}
	return strings.Join(ids, ",")
}

// Handle — обробити повідомлення хаба; true, якщо це був "close".
func (a *AgentLegs) Handle(m AgentMsg) bool {
	if m.Type != "close" {
		return false
	}
	a.mu.Lock()
	l := a.legs[m.ID]
	delete(a.legs, m.ID)
	a.mu.Unlock()
	if l != nil {
		l.shutdown(nonEmpty(m.Reason, "closed-by-hub"), false)
	}
	return true
}

// CloseAll — рве всі ноги (вихід агента).
func (a *AgentLegs) CloseAll() {
	a.mu.Lock()
	legs := a.legs
	a.legs = nil
	a.mu.Unlock()
	for _, l := range legs {
		_ = l.Close()
	}
}

// Monitor стежить за ICE і рівно один раз кличе report з direct/turn, і
// щонайбільше один раз — з fallback. restart (лише в offerer-а — браузера)
// пробується до cfg.MaxICERestarts разів при деградації; nil = одразу відкат.
func Monitor(pc *webrtc.PeerConnection, cfg Config, restart func() error, report func(state, pair, reason string)) {
	var (
		mu         sync.Mutex
		connected  bool
		fellBack   bool
		restarts   int
		degradeGen int
	)
	fall := func(reason string) {
		mu.Lock()
		if fellBack {
			mu.Unlock()
			return
		}
		fellBack = true
		mu.Unlock()
		report(StateFallback, "", reason)
	}
	connectTO := cfg.ConnectTimeout
	if connectTO <= 0 {
		connectTO = DefaultConfig().ConnectTimeout
	}
	degradeTO := cfg.DegradeTimeout
	if degradeTO <= 0 {
		degradeTO = DefaultConfig().DegradeTimeout
	}
	time.AfterFunc(connectTO, func() {
		mu.Lock()
		ok := connected
		mu.Unlock()
		if !ok {
			fall("connect-timeout")
		}
	})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		switch s {
		case webrtc.ICEConnectionStateConnected:
			mu.Lock()
			first := !connected
			connected = true
			degradeGen++
			mu.Unlock()
			if first {
				p := SelectedPair(pc)
				st := StateDirect
				if IsRelayPair(p) {
					st = StateTURN
				}
				report(st, PairType(p), "")
			}
		case webrtc.ICEConnectionStateFailed:
			mu.Lock()
			canRestart := restart != nil && restarts < cfg.MaxICERestarts
			if canRestart {
				restarts++
			}
			mu.Unlock()
			if canRestart && restart() == nil {
				return
			}
			fall("ice-failed")
		case webrtc.ICEConnectionStateDisconnected:
			mu.Lock()
			degradeGen++
			gen := degradeGen
			mu.Unlock()
			time.AfterFunc(degradeTO, func() {
				mu.Lock()
				still := gen == degradeGen
				canRestart := still && restart != nil && restarts < cfg.MaxICERestarts
				if canRestart {
					restarts++
				}
				mu.Unlock()
				if !still {
					return
				}
				if canRestart && restart() == nil {
					return
				}
				fall("degraded")
			})
		}
	})
}
