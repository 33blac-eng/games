// Package p2p — N6: пряме медіа агент↔браузер там, де NAT дозволяє, з
// автоматичним відкатом на звичний шлях через хаб.
//
// Розподіл ролей:
//   - Хаб лишається ЄДИНОЮ точкою сигналізації й авторизації (Broker): він
//     споживає ERP-квиток, вирішує, чи можна йти напряму (лише один глядач на
//     ноді — багато глядачів лишаються на fan-out хаба), пише аудит S4 так само,
//     як для relay-сесій, і рахує метрики direct/relay та типи пар кандидатів.
//   - Агент (AgentLeg) приймає offer ЛИШЕ з рук хаба (автентифікований poll),
//     сам перевіряє згоду S3 до відповіді й на кожну подію вводу, і сам
//     відкидає ввід, якщо в квитку grant != "control" — рівно та сама правда,
//     що й judgeInput на relay-нозі.
//   - Monitor стежить за ICE: не зʼєдналися за ConnectTimeout, failed або
//     надовго disconnected (деградація) — ICE restart (якщо дано restart) і,
//     якщо не допомогло, Fallback: глядач іде звичним /offer/viewer.
//
// Дефолт — вимкнено (OO_SCREEN_P2P != "1"): Broker не реєструє маршрути, і
// хаб поводиться рівно як раніше.
package p2p

import (
	"errors"
	"os"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// InputLabel — мітка каналу вводу на прямій нозі (та сама, що на relay).
const InputLabel = "oosc-input"

// GrantControl — єдиний grant, що відчиняє ввід.
const GrantControl = "control"

// Помилки, які хаб перетворює на «йди через relay».
var (
	ErrDisabled    = errors.New("p2p: disabled")
	ErrMultiViewer = errors.New("p2p: node already has viewers (hub fan-out)")
	ErrNoConsent   = errors.New("p2p: no consent on the PC")
)

// Config — налаштування N6.
type Config struct {
	Enabled bool
	// STUN — список stun:-URL (OO_SCREEN_P2P_STUN, через кому).
	STUN []string
	// TURN — необовʼязковий; усі три поля мають сенс лише разом.
	TURNURL, TURNUser, TURNPass string
	// ConnectTimeout — скільки чекати ICE connected до відкату.
	ConnectTimeout time.Duration
	// DegradeTimeout — скільки терпіти disconnected до restart/відкату.
	DegradeTimeout time.Duration
	// MaxICERestarts — скільки ICE restart спробувати до відкату.
	MaxICERestarts int
	// SignalTimeout — скільки хаб чекає answer від агента.
	SignalTimeout time.Duration
}

// DefaultConfig — вимкнено, розумні таймаути.
func DefaultConfig() Config {
	return Config{
		ConnectTimeout: 8 * time.Second,
		DegradeTimeout: 4 * time.Second,
		MaxICERestarts: 1,
		SignalTimeout:  10 * time.Second,
	}
}

// ConfigFromEnv читає OO_SCREEN_P2P* (getenv == nil → os.Getenv).
func ConfigFromEnv(getenv func(string) string) Config {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := DefaultConfig()
	c.Enabled = getenv("OO_SCREEN_P2P") == "1"
	for _, u := range strings.Split(getenv("OO_SCREEN_P2P_STUN"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.STUN = append(c.STUN, u)
		}
	}
	c.TURNURL = getenv("OO_SCREEN_P2P_TURN_URL")
	c.TURNUser = getenv("OO_SCREEN_P2P_TURN_USER")
	c.TURNPass = getenv("OO_SCREEN_P2P_TURN_PASS")
	if d, err := time.ParseDuration(getenv("OO_SCREEN_P2P_CONNECT_TIMEOUT")); err == nil && d > 0 {
		c.ConnectTimeout = d
	}
	return c
}

// ICEServers — STUN + (опційно) TURN для прямої ноги.
func (c Config) ICEServers() []webrtc.ICEServer {
	var out []webrtc.ICEServer
	for _, u := range c.STUN {
		out = append(out, webrtc.ICEServer{URLs: []string{u}})
	}
	if c.TURNURL != "" && c.TURNUser != "" && c.TURNPass != "" {
		out = append(out, webrtc.ICEServer{
			URLs:           []string{c.TURNURL},
			Username:       c.TURNUser,
			Credential:     c.TURNPass,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}
	return out
}

// Grant — те, що хаб знає про глядача після споживання квитка.
type Grant struct {
	User  string `json:"user"`
	Org   string `json:"org,omitempty"`
	Node  string `json:"node"`
	Grant string `json:"grant"`
}

// Offer — те, що агент отримує з poll.
type Offer struct {
	ID    string `json:"id"`
	Node  string `json:"node"`
	User  string `json:"user"`
	Grant string `json:"grant"`
	SDP   string `json:"sdp"`
}

// AgentMsg — повідомлення хаб → агент через poll.
type AgentMsg struct {
	Type   string `json:"type"` // "offer" | "close"
	Offer  *Offer `json:"offer,omitempty"`
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Стан прямої ноги, який агент повідомляє хабу (/p2p/result).
const (
	StateDirect   = "direct"   // ICE connected без relay-кандидата
	StateTURN     = "turn"     // connected, але через TURN (не hub, але й не direct)
	StateFallback = "fallback" // пряма нога не вдалася — глядач іде через хаб
	StateClosed   = "closed"   // сесія завершилась штатно
)

// PairType — "local/remote" типи вибраної пари, напр. "host/srflx".
func PairType(p *webrtc.ICECandidatePair) string {
	if p == nil || p.Local == nil || p.Remote == nil {
		return "unknown"
	}
	return p.Local.Typ.String() + "/" + p.Remote.Typ.String()
}

// IsRelayPair — чи хоч один бік пари — TURN relay.
func IsRelayPair(p *webrtc.ICECandidatePair) bool {
	return p != nil && p.Local != nil && p.Remote != nil &&
		(p.Local.Typ == webrtc.ICECandidateTypeRelay || p.Remote.Typ == webrtc.ICECandidateTypeRelay)
}

// SelectedPair — вибрана пара (через SCTP-транспорт; nil, якщо ще нема).
func SelectedPair(pc *webrtc.PeerConnection) *webrtc.ICECandidatePair {
	sctp := pc.SCTP()
	if sctp == nil || sctp.Transport() == nil || sctp.Transport().ICETransport() == nil {
		return nil
	}
	p, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil {
		return nil
	}
	return p
}
