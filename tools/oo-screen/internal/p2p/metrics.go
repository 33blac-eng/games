package p2p

import (
	"fmt"
	"io"
	"sort"
	"sync"
)

// Metrics — частка direct vs relay і типи пар кандидатів. Лічильники
// монотонні (Prometheus counter); частку рахує сам Metrics для зручності.
type Metrics struct {
	mu       sync.Mutex
	attempts uint64
	direct   uint64
	turn     uint64
	relay    map[string]uint64 // причина відкату на hub-relay
	pairs    map[string]uint64 // "host/srflx" -> к-сть
}

// NewMetrics — порожні лічильники.
func NewMetrics() *Metrics {
	return &Metrics{relay: map[string]uint64{}, pairs: map[string]uint64{}}
}

// Attempt — глядач попросив пряму ногу.
func (m *Metrics) Attempt() { m.mu.Lock(); m.attempts++; m.mu.Unlock() }

// Direct — пряма нога встановлена (pair — "local/remote").
func (m *Metrics) Direct(pair string, viaTURN bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if viaTURN {
		m.turn++
	} else {
		m.direct++
	}
	m.pairs[pair]++
}

// Relay — сесія пішла через хаб (reason — чому).
func (m *Metrics) Relay(reason string) {
	m.mu.Lock()
	m.relay[reason]++
	m.mu.Unlock()
}

// Snapshot — копія для тестів/логів.
type Snapshot struct {
	Attempts, Direct, TURN, Relayed uint64
	RelayReasons                    map[string]uint64
	Pairs                           map[string]uint64
}

// DirectShare — direct / (direct+turn+relayed); 0, якщо сесій нема.
func (s Snapshot) DirectShare() float64 {
	tot := s.Direct + s.TURN + s.Relayed
	if tot == 0 {
		return 0
	}
	return float64(s.Direct) / float64(tot)
}

// Snapshot знімає поточні значення.
func (m *Metrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Attempts: m.attempts, Direct: m.direct, TURN: m.turn,
		RelayReasons: map[string]uint64{}, Pairs: map[string]uint64{}}
	for k, v := range m.relay {
		s.RelayReasons[k] = v
		s.Relayed += v
	}
	for k, v := range m.pairs {
		s.Pairs[k] = v
	}
	return s
}

// WriteProm пише метрики у текстовому форматі Prometheus.
func (m *Metrics) WriteProm(w io.Writer) error {
	s := m.Snapshot()
	var err error
	pf := func(format string, a ...any) {
		if err == nil {
			_, err = fmt.Fprintf(w, format, a...)
		}
	}
	pf("# HELP oo_hub_p2p_attempts_total Viewer requests for a direct (P2P) leg.\n# TYPE oo_hub_p2p_attempts_total counter\noo_hub_p2p_attempts_total %d\n", s.Attempts)
	pf("# HELP oo_hub_p2p_sessions_total Sessions by media path (direct, turn, relay=hub).\n# TYPE oo_hub_p2p_sessions_total counter\n")
	pf("oo_hub_p2p_sessions_total{path=\"direct\"} %d\n", s.Direct)
	pf("oo_hub_p2p_sessions_total{path=\"turn\"} %d\n", s.TURN)
	pf("oo_hub_p2p_sessions_total{path=\"relay\"} %d\n", s.Relayed)
	pf("# HELP oo_hub_p2p_direct_share Share of decided sessions that went direct (0..1).\n# TYPE oo_hub_p2p_direct_share gauge\noo_hub_p2p_direct_share %g\n", s.DirectShare())
	pf("# HELP oo_hub_p2p_relay_reason_total Fallbacks to the hub relay by reason.\n# TYPE oo_hub_p2p_relay_reason_total counter\n")
	for _, k := range sortedKeys(s.RelayReasons) {
		pf("oo_hub_p2p_relay_reason_total{reason=%q} %d\n", k, s.RelayReasons[k])
	}
	pf("# HELP oo_hub_p2p_candidate_pair_total Selected ICE candidate pair types (local/remote).\n# TYPE oo_hub_p2p_candidate_pair_total counter\n")
	for _, k := range sortedKeys(s.Pairs) {
		pf("oo_hub_p2p_candidate_pair_total{pair=%q} %d\n", k, s.Pairs[k])
	}
	return err
}

func sortedKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
