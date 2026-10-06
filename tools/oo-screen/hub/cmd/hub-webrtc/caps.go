package main

// caps.go — телеметрія можливостей (WORLD-COMPARISON-2026 §5 п. 1, C1).
//
// Глядач (desktop-oo-codec444.js, config.reportCaps) кладе в тіло
// /offer/viewer те, що його браузер оголошує в
// RTCRtpReceiver.getCapabilities('video'): profile-level-id H.264, profile-id
// VP9 і profile AV1. Хаб лише рахує ці відповіді в /metrics — щоб рішення про
// H.264 4:4:4 (f4001f) спиралось на частку реальних глядачів, а не на віру.
//
// Лише під OO_SCREEN_CAPS_TELEMETRY=1 (типово вимкнено): без прапорця поле
// caps розбирається JSON-ом і ігнорується.

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/organicoils/oo-screen/internal/control"
)

var capsTelemetryEnabled = os.Getenv("OO_SCREEN_CAPS_TELEMETRY") == "1"

// viewerCaps — поле caps тіла /offer/viewer.
type viewerCaps struct {
	H264 []string `json:"h264,omitempty"` // profile-level-id, напр. "42e01f", "f4001f"
	VP9  []int    `json:"vp9,omitempty"`  // profile-id
	AV1  []int    `json:"av1,omitempty"`  // profile
}

// capsMaxLabels — стеля різних значень мітки на кодек: тіло /offer/viewer
// пише будь-хто з квитком, а кардинальність /metrics має лишатись малою.
const capsMaxLabels = 16

// h264KnownProfiles — profile_idc, які мають сенс як мітка (Baseline, Main,
// Extended, High, High10, High422, High444). Решта — "other".
var h264KnownProfiles = map[string]bool{"42": true, "4d": true, "58": true, "64": true, "6e": true, "7a": true, "f4": true}

type capsCounters struct {
	mu      sync.Mutex
	reports uint64
	h264    map[string]uint64
	vp9     map[string]uint64
	av1     map[string]uint64
}

var viewerCapsStats = &capsCounters{}

// normH264Profile — нормалізований profile-level-id (6 hex, нижній регістр)
// або "other".
func normH264Profile(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 6 {
		return "other"
	}
	if _, err := strconv.ParseUint(s, 16, 32); err != nil {
		return "other"
	}
	if !h264KnownProfiles[s[:2]] {
		return "other"
	}
	return s
}

func capsBump(m map[string]uint64, k string) {
	if _, ok := m[k]; !ok && len(m) >= capsMaxLabels {
		k = "other"
	}
	m[k]++
}

// observe рахує один звіт глядача. Повтори всередині звіту — один раз.
func (c *capsCounters) observe(v *viewerCaps) {
	if v == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.h264 == nil {
		c.h264, c.vp9, c.av1 = map[string]uint64{}, map[string]uint64{}, map[string]uint64{}
	}
	c.reports++
	seen := map[string]bool{}
	for _, p := range v.H264 {
		k := normH264Profile(p)
		if !seen["h"+k] {
			seen["h"+k] = true
			capsBump(c.h264, k)
		}
	}
	for _, p := range v.VP9 {
		k := "other"
		if p >= 0 && p <= 3 {
			k = strconv.Itoa(p)
		}
		if !seen["v"+k] {
			seen["v"+k] = true
			capsBump(c.vp9, k)
		}
	}
	for _, p := range v.AV1 {
		k := "other"
		if p >= 0 && p <= 2 {
			k = strconv.Itoa(p)
		}
		if !seen["a"+k] {
			seen["a"+k] = true
			capsBump(c.av1, k)
		}
	}
}

// recordViewerCaps — точка входу з handleOffer (лише авторизований глядач).
func recordViewerCaps(v *viewerCaps) {
	if !capsTelemetryEnabled {
		return
	}
	viewerCapsStats.observe(v)
}

func writeCapsMap(p *promWriter, name, help string, m map[string]uint64) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p.sample(name, "counter", help, float64(m[k]), "profile", k)
	}
}

// write — експозиція лічильників (лише коли прапорець увімкнено).
func (c *capsCounters) write(p *promWriter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p.sample("oo_hub_viewer_caps_reports_total", "counter", "Viewer offers that carried RTCRtpReceiver.getCapabilities('video').", float64(c.reports))
	writeCapsMap(p, "oo_hub_viewer_h264_profile_total", "Viewers whose browser advertises this H.264 profile-level-id for receive.", c.h264)
	writeCapsMap(p, "oo_hub_viewer_vp9_profile_total", "Viewers whose browser advertises this VP9 profile-id for receive.", c.vp9)
	writeCapsMap(p, "oo_hub_viewer_av1_profile_total", "Viewers whose browser advertises this AV1 profile for receive.", c.av1)
}

// --- C2: можливості й QP енкодера агента (control enc_stats) ---

// agentEnc — останній enc_stats кожної ноди. Ключ — node id; записи нод,
// яких уже нема в реєстрі, просто не потрапляють в експозицію (writeMetrics
// ходить по живих нодах), а місце обмежене кількістю нод.
type agentEnc struct {
	encoder  string
	software bool
	caps     map[string]string
	qpLast   int
	qpMin    int
	qpMax    int
	qpFrames uint64 // накопичено за всі вікна
}

var (
	agentEncMu sync.Mutex
	agentEncBy = map[string]*agentEnc{}
)

func validQP(q int) int {
	if q < 0 || q > 51 {
		return 0
	}
	return q
}

// onAgentEncStats — enc_stats від агента (unavailable.go: handleAgentCtl).
func onAgentEncStats(ns *nodeSession, m control.Msg) {
	if !capsTelemetryEnabled || ns == nil {
		return
	}
	name := m.Encoder
	if len(name) > 64 {
		name = name[:64]
	}
	agentEncMu.Lock()
	defer agentEncMu.Unlock()
	a := agentEncBy[ns.nodeID]
	if a == nil {
		a = &agentEnc{}
		agentEncBy[ns.nodeID] = a
	}
	a.encoder, a.software = name, m.Software
	a.caps = control.ParseEncCaps(m.EncCaps)
	a.qpLast, a.qpMin, a.qpMax = validQP(m.QPLast), validQP(m.QPMin), validQP(m.QPMax)
	if m.QPFrames > 0 {
		a.qpFrames += uint64(m.QPFrames)
	}
}

func writeAgentEncMetrics(p *promWriter, nodes []string) {
	agentEncMu.Lock()
	defer agentEncMu.Unlock()
	for _, n := range nodes {
		a := agentEncBy[n]
		if a == nil {
			continue
		}
		sw := "0"
		if a.software {
			sw = "1"
		}
		p.sample("oo_hub_agent_encoder_info", "gauge", "Agent encoder (MFT friendly name); value is always 1.", 1, "node", n, "encoder", a.encoder, "software", sw)
		props := make([]string, 0, len(a.caps))
		for k := range a.caps {
			props = append(props, k)
		}
		sort.Strings(props)
		for _, k := range props {
			p.sample("oo_hub_agent_codecapi", "gauge", "Agent MFT CODECAPI property state (ICodecAPI IsSupported/IsModifiable); value is always 1.", 1, "node", n, "prop", k, "state", a.caps[k])
		}
		for _, q := range []struct {
			stat string
			v    int
		}{{"last", a.qpLast}, {"min", a.qpMin}, {"max", a.qpMax}} {
			if q.v > 0 {
				p.sample("oo_hub_agent_frame_qp", "gauge", "Agent frame QP parsed from the bitstream over the last report window (last/min/max).", float64(q.v), "node", n, "stat", q.stat)
			}
		}
		p.sample("oo_hub_agent_qp_frames_total", "counter", "Agent frames whose QP was parsed from the bitstream.", float64(a.qpFrames), "node", n)
	}
}
