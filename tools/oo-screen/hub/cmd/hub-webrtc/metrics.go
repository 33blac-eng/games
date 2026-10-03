package main

// metrics.go — Prometheus /metrics (text exposition format 0.0.4), написаний
// руками: без client_golang, щоб не тягнути залежність заради ~30 рядків.
//
// Слухач ОКРЕМИЙ і вмикається лише за OO_SCREEN_METRICS_ADDR (порожньо =
// вимкнено), як і pprof: на публічний порт сигналінгу метрики не потрапляють.
// Рекомендовано 127.0.0.1:порт.
//
// Кардинальність: єдина мітка — node (плюс reason/le там, де без них ніяк).
// Пер-глядацьких міток немає: втрати/RTT зведено до max/avg по нозі.

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// ttffBuckets — межі гістограми time-to-first-frame, секунди.
var ttffBuckets = []float64{0.1, 0.25, 0.5, 1, 2, 5}

// nodeMetrics — лічильники однієї ноди. Атомарні поля пишуться з гарячого
// шляху без ns.mu; решта — під mu (окремий від ns.mu, щоб скрейп не стояв на
// форвардингу).
type nodeMetrics struct {
	ingressBytes   atomic.Uint64
	ingressPackets atomic.Uint64
	frames         atomic.Uint64
	keyframes      atomic.Uint64

	pliFromViewers atomic.Uint64
	agentPLISent   atomic.Uint64
	keyframeReqs   atomic.Uint64 // недебаунсені requestKeyframe (за ns.lastKeyframeReq)

	nackPackets    atomic.Uint64
	nackSeqs       atomic.Uint64 // запитаних seq (усі NACK)
	nackHit        atomic.Uint64 // із закритих вікон: могли ретрансмітити
	nackUnanswered atomic.Uint64 // із закритих вікон: вже поза буфером

	viewerDrops atomic.Uint64

	// kfReqSeen — останній побачений ns.lastKeyframeReq. Під ns.mu.
	kfReqSeen time.Time

	mu        sync.Mutex
	haveTS    bool
	lastTS    uint32
	lastKeyTS uint32
	haveKey   bool
	winStart  time.Time
	winBytes  uint64
	winFrames uint64
	bps, fps  float64
	rateAt    time.Time
	keyTimes  []time.Time // ключові кадри за останню хвилину
	reason    string      // причина останньої зміни цілі бітрейту
	reasonAt  time.Time
	ttff      [7]uint64 // len(ttffBuckets)+1 (+Inf)
	ttffSum   float64
	ttffCount uint64
}

// metricsAgentPacket — гарячий шлях агентської ноги: байти, кадри (зміна RTP
// timestamp), ключові кадри (ключовий пакет нового AU).
func metricsAgentPacket(ns *nodeSession, pkt *rtp.Packet, now time.Time) {
	m := &ns.m
	n := uint64(len(pkt.Payload) + pkt.Header.MarshalSize())
	m.ingressBytes.Add(n)
	m.ingressPackets.Add(1)

	m.mu.Lock()
	newFrame := !m.haveTS || pkt.Timestamp != m.lastTS
	if newFrame {
		m.haveTS, m.lastTS = true, pkt.Timestamp
		m.frames.Add(1)
		m.winFrames++
	}
	if (!m.haveKey || pkt.Timestamp != m.lastKeyTS) && h264KeyPart(pkt.Payload) {
		m.haveKey, m.lastKeyTS = true, pkt.Timestamp
		m.keyframes.Add(1)
		m.keyTimes = append(m.keyTimes, now)
		m.trimKeysLocked(now)
	}
	m.winBytes += n
	if m.winStart.IsZero() {
		m.winStart = now
	} else if d := now.Sub(m.winStart); d >= time.Second {
		s := d.Seconds()
		m.bps = float64(m.winBytes*8) / s
		m.fps = float64(m.winFrames) / s
		m.rateAt = now
		m.winStart, m.winBytes, m.winFrames = now, 0, 0
	}
	m.mu.Unlock()
}

func (m *nodeMetrics) trimKeysLocked(now time.Time) {
	i := 0
	for i < len(m.keyTimes) && now.Sub(m.keyTimes[i]) > time.Minute {
		i++
	}
	if i > 0 {
		m.keyTimes = append(m.keyTimes[:0], m.keyTimes[i:]...)
	}
}

// metricsObserveKeyframeReq — кликати ПІД ns.mu: рахує нові (недебаунсені)
// keyframe-запити за зміною ns.lastKeyframeReq, не чіпаючи bitrate.go.
func metricsObserveKeyframeReqLocked(ns *nodeSession) {
	if t := ns.lastKeyframeReq; !t.IsZero() && !t.Equal(ns.m.kfReqSeen) {
		ns.m.kfReqSeen = t
		ns.m.keyframeReqs.Add(1)
	}
}

// metricsNack — один NACK від глядача; st — результат onNack (вікно закрите
// чи ні).
func metricsNack(ns *nodeSession, n *rtcp.TransportLayerNack, st nackStats) {
	ns.m.nackPackets.Add(1)
	var c uint64
	for i := range n.Nacks {
		n.Nacks[i].Range(func(uint16) bool { c++; return true })
	}
	ns.m.nackSeqs.Add(c)
	if st.closed {
		ns.m.nackHit.Add(st.hit)
		ns.m.nackUnanswered.Add(st.req - st.hit)
	}
}

// bitrateTarget — поточна ціль контролера (лише читання стану bitrate.go).
func bitrateTarget(ns *nodeSession) uint64 {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.bitrate.target
}

// metricsNoteBitrate — після виклику контролера: якщо ціль змінилась
// відносно prev, запамʼятати причину. reason=="" — вивести з напрямку.
func metricsNoteBitrate(ns *nodeSession, prev uint64, reason string, lossFrac float64) {
	cur := bitrateTarget(ns)
	if cur == prev && reason != "reset" {
		return
	}
	if reason == "" {
		switch {
		case cur > prev:
			reason = "recover"
		case lossFrac > 0:
			reason = "loss"
		default:
			reason = "rtt"
		}
	}
	ns.m.mu.Lock()
	ns.m.reason, ns.m.reasonAt = reason, time.Now()
	ns.m.mu.Unlock()
}

// metricsTTFF — перший пакет пішов у трек нової ноги глядача.
func metricsTTFF(ns *nodeSession, d time.Duration) {
	s := d.Seconds()
	m := &ns.m
	m.mu.Lock()
	i := sort.SearchFloat64s(ttffBuckets, s)
	m.ttff[i]++
	m.ttffSum += s
	m.ttffCount++
	m.mu.Unlock()
}

// --- egress ---

var (
	egressMu  sync.Mutex
	egressSet = map[*egressConn]struct{}{}
)

func registerEgress(e *egressConn) {
	egressMu.Lock()
	egressSet[e] = struct{}{}
	egressMu.Unlock()
}

func unregisterEgress(e *egressConn) {
	egressMu.Lock()
	delete(egressSet, e)
	egressMu.Unlock()
}

// --- експозиція ---

type promWriter struct {
	w    *bufio.Writer
	seen map[string]bool
}

func (p *promWriter) head(name, typ, help string) {
	if p.seen[name] {
		return
	}
	p.seen[name] = true
	fmt.Fprintf(p.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func promLabels(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[i])
		b.WriteString(`="`)
		b.WriteString(promEscape(kv[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func promEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func promFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func (p *promWriter) sample(name, typ, help string, v float64, kv ...string) {
	p.head(name, typ, help)
	fmt.Fprintf(p.w, "%s%s %s\n", name, promLabels(kv...), promFloat(v))
}

// nodeSnap — знімок однієї ноди для експозиції.
type nodeSnap struct {
	node                      string
	agent                     bool
	viewers                   int
	bps, fps, kpm             float64
	target                    uint64
	reason                    string
	lossMax, lossAvg          float64
	rttMax, rttAvg            float64
	gopBytes, gopPackets      int
	ttff                      [7]uint64
	ttffSum                   float64
	ttffCount                 uint64
	ingressBytes, ingressPkts uint64
	frames, keyframes         uint64
	pli, agentPLI, kfReqs     uint64
	nackPk, nackSeq, nackHit  uint64
	nackUnans, viewerDrops    uint64
}

func snapNode(ns *nodeSession, now time.Time) nodeSnap {
	s := nodeSnap{node: ns.nodeID, agent: ns.hasAgent()}
	ns.mu.Lock()
	s.viewers = len(ns.viewers)
	s.target = ns.bitrate.target
	s.gopBytes, s.gopPackets = ns.gop.bytes, len(ns.gop.pkts)
	var n int
	for _, vl := range ns.viewers {
		if !vl.ready || vl.lastRR.IsZero() || now.Sub(vl.lastRR) > viewerRRStale {
			continue
		}
		n++
		s.lossAvg += vl.loss
		s.rttAvg += vl.rtt.Seconds()
		s.lossMax = math.Max(s.lossMax, vl.loss)
		s.rttMax = math.Max(s.rttMax, vl.rtt.Seconds())
	}
	ns.mu.Unlock()
	if n > 0 {
		s.lossAvg /= float64(n)
		s.rttAvg /= float64(n)
	}

	m := &ns.m
	m.mu.Lock()
	if !m.rateAt.IsZero() && now.Sub(m.rateAt) <= 3*time.Second {
		s.bps, s.fps = m.bps, m.fps
	}
	m.trimKeysLocked(now)
	s.kpm = float64(len(m.keyTimes))
	s.reason = m.reason
	s.ttff, s.ttffSum, s.ttffCount = m.ttff, m.ttffSum, m.ttffCount
	m.mu.Unlock()

	s.ingressBytes, s.ingressPkts = m.ingressBytes.Load(), m.ingressPackets.Load()
	s.frames, s.keyframes = m.frames.Load(), m.keyframes.Load()
	s.pli, s.agentPLI, s.kfReqs = m.pliFromViewers.Load(), m.agentPLISent.Load(), m.keyframeReqs.Load()
	s.nackPk, s.nackSeq = m.nackPackets.Load(), m.nackSeqs.Load()
	s.nackHit, s.nackUnans = m.nackHit.Load(), m.nackUnanswered.Load()
	s.viewerDrops = m.viewerDrops.Load()
	return s
}

// writeMetrics пише повну експозицію для заданих нод.
func writeMetrics(out io.Writer, nodes []*nodeSession, now time.Time) error {
	snaps := make([]nodeSnap, 0, len(nodes))
	for _, ns := range nodes {
		snaps = append(snaps, snapNode(ns, now))
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].node < snaps[j].node })

	p := &promWriter{w: bufio.NewWriter(out), seen: map[string]bool{}}
	agents := 0
	for _, s := range snaps {
		if s.agent {
			agents++
		}
	}
	p.sample("oo_hub_nodes", "gauge", "Nodes in the registry.", float64(len(snaps)))
	p.sample("oo_hub_agents", "gauge", "Nodes with a connected agent leg.", float64(agents))

	type g struct {
		name, typ, help string
		val             func(nodeSnap) float64
	}
	perNode := []g{
		{"oo_hub_node_agent_connected", "gauge", "1 if the node has an agent leg.", func(s nodeSnap) float64 {
			if s.agent {
				return 1
			}
			return 0
		}},
		{"oo_hub_viewers", "gauge", "Viewer legs per node.", func(s nodeSnap) float64 { return float64(s.viewers) }},
		{"oo_hub_agent_ingress_bps", "gauge", "Agent video ingress bitrate over the last ~1s window, bits/s.", func(s nodeSnap) float64 { return s.bps }},
		{"oo_hub_agent_ingress_bytes_total", "counter", "Agent video ingress bytes (RTP header+payload).", func(s nodeSnap) float64 { return float64(s.ingressBytes) }},
		{"oo_hub_agent_ingress_packets_total", "counter", "Agent video ingress RTP packets.", func(s nodeSnap) float64 { return float64(s.ingressPkts) }},
		{"oo_hub_agent_fps", "gauge", "Frames/s from agent (distinct RTP timestamps) over the last ~1s window.", func(s nodeSnap) float64 { return s.fps }},
		{"oo_hub_agent_frames_total", "counter", "Frames from agent (distinct RTP timestamps).", func(s nodeSnap) float64 { return float64(s.frames) }},
		{"oo_hub_agent_keyframes_total", "counter", "Keyframes (key access units) from agent.", func(s nodeSnap) float64 { return float64(s.keyframes) }},
		{"oo_hub_agent_keyframes_per_minute", "gauge", "Keyframes from agent in the last 60s.", func(s nodeSnap) float64 { return s.kpm }},
		{"oo_hub_bitrate_target_bps", "gauge", "Bitrate controller target, bits/s (0 = not initialised).", func(s nodeSnap) float64 { return float64(s.target) }},
		{"oo_hub_viewer_loss_fraction_max", "gauge", "Max viewer-reported RR fraction lost (fresh RRs only).", func(s nodeSnap) float64 { return s.lossMax }},
		{"oo_hub_viewer_loss_fraction_avg", "gauge", "Avg viewer-reported RR fraction lost (fresh RRs only).", func(s nodeSnap) float64 { return s.lossAvg }},
		{"oo_hub_viewer_rtt_seconds_max", "gauge", "Max viewer RTT from RR LSR/DLSR, seconds.", func(s nodeSnap) float64 { return s.rttMax }},
		{"oo_hub_viewer_rtt_seconds_avg", "gauge", "Avg viewer RTT from RR LSR/DLSR, seconds.", func(s nodeSnap) float64 { return s.rttAvg }},
		{"oo_hub_nack_packets_total", "counter", "RTCP NACK packets received from viewers.", func(s nodeSnap) float64 { return float64(s.nackPk) }},
		{"oo_hub_nack_requested_total", "counter", "Sequence numbers requested by viewer NACKs.", func(s nodeSnap) float64 { return float64(s.nackSeq) }},
		{"oo_hub_nack_retransmitted_total", "counter", "NACKed seqs still in the retransmit buffer (closed windows).", func(s nodeSnap) float64 { return float64(s.nackHit) }},
		{"oo_hub_nack_unanswered_total", "counter", "NACKed seqs already outside the retransmit buffer (closed windows).", func(s nodeSnap) float64 { return float64(s.nackUnans) }},
		{"oo_hub_viewer_pli_total", "counter", "PLIs received from viewers.", func(s nodeSnap) float64 { return float64(s.pli) }},
		{"oo_hub_agent_pli_sent_total", "counter", "RTCP PLIs sent to the agent.", func(s nodeSnap) float64 { return float64(s.agentPLI) }},
		{"oo_hub_keyframe_requests_total", "counter", "Non-debounced keyframe requests to the agent (ctl or PLI fallback).", func(s nodeSnap) float64 { return float64(s.kfReqs) }},
		{"oo_hub_gop_cache_bytes", "gauge", "GOP cache size, bytes.", func(s nodeSnap) float64 { return float64(s.gopBytes) }},
		{"oo_hub_gop_cache_packets", "gauge", "GOP cache size, packets.", func(s nodeSnap) float64 { return float64(s.gopPackets) }},
		{"oo_hub_viewer_queue_drops_total", "counter", "Packets not queued to a live viewer (queue full or drop-to-IDR).", func(s nodeSnap) float64 { return float64(s.viewerDrops) }},
	}
	for _, d := range perNode {
		for _, s := range snaps {
			p.sample(d.name, d.typ, d.help, d.val(s), "node", s.node)
		}
	}
	for _, s := range snaps {
		if s.reason != "" {
			p.sample("oo_hub_bitrate_target_reason", "gauge", "Reason of the last bitrate target change (value is always 1).", 1, "node", s.node, "reason", s.reason)
		}
	}
	const h = "oo_hub_viewer_ttff_seconds"
	for _, s := range snaps {
		p.head(h, "histogram", "Time from viewer leg creation to its first video packet written, seconds.")
		var cum uint64
		for i, b := range ttffBuckets {
			cum += s.ttff[i]
			fmt.Fprintf(p.w, "%s_bucket%s %d\n", h, promLabels("node", s.node, "le", promFloat(b)), cum)
		}
		cum += s.ttff[len(ttffBuckets)]
		fmt.Fprintf(p.w, "%s_bucket%s %d\n", h, promLabels("node", s.node, "le", "+Inf"), cum)
		fmt.Fprintf(p.w, "%s_sum%s %s\n", h, promLabels("node", s.node), promFloat(s.ttffSum))
		fmt.Fprintf(p.w, "%s_count%s %d\n", h, promLabels("node", s.node), s.ttffCount)
	}

	// egress (egress.go): спільний на процес, без мітки ноди.
	var depth, capa int
	var full, errs uint64
	egressMu.Lock()
	for e := range egressSet {
		depth += len(e.q)
		capa += cap(e.q)
		full += e.full.Load()
		errs += e.errs.Load()
	}
	egressMu.Unlock()
	p.sample("oo_hub_egress_queue_depth", "gauge", "Packets waiting in the egress writer queue.", float64(depth))
	p.sample("oo_hub_egress_queue_capacity", "gauge", "Egress writer queue capacity.", float64(capa))
	p.sample("oo_hub_egress_queue_full_total", "counter", "Egress enqueues that found the queue full (writer blocked).", float64(full))
	p.sample("oo_hub_egress_write_errors_total", "counter", "Egress UDP write errors (dropped datagrams).", float64(errs))

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	p.sample("oo_hub_goroutines", "gauge", "Number of goroutines.", float64(runtime.NumGoroutine()))
	p.sample("oo_hub_heap_alloc_bytes", "gauge", "Heap bytes allocated and in use.", float64(ms.HeapAlloc))
	p.sample("oo_hub_heap_sys_bytes", "gauge", "Heap bytes obtained from the OS.", float64(ms.HeapSys))
	if rss, ok := processRSS(); ok {
		p.sample("oo_hub_process_resident_memory_bytes", "gauge", "Resident set size, bytes.", float64(rss))
	}
	return p.w.Flush()
}

// processRSS — RSS з /proc/self/statm (Linux); ok=false деінде.
func processRSS() (uint64, bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * uint64(os.Getpagesize()), true
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = writeMetrics(w, reg.all(), time.Now())
}

// startMetricsServer — окремий слухач /metrics; порожня адреса = вимкнено.
func startMetricsServer(addr string) {
	if addr == "" {
		return
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			log.Printf("WARNING: metrics on non-loopback %s — тримай його за фаєрволом", addr)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", handleMetrics)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() {
		log.Printf("metrics on %s/metrics", addr)
		log.Printf("metrics exited: %v", srv.ListenAndServe())
	}()
}
