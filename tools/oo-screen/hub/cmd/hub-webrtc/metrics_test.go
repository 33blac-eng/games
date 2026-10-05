package main

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

var (
	promSampleRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{([a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*"(?:,[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\]|\\.)*")*)\})? (\S+)$`)
	promTypeRe   = regexp.MustCompile(`^# TYPE ([a-zA-Z_:][a-zA-Z0-9_:]*) (counter|gauge|histogram|summary|untyped)$`)
	promHelpRe   = regexp.MustCompile(`^# HELP ([a-zA-Z_:][a-zA-Z0-9_:]*) .+$`)
)

// parseProm — мінімальний строгий парсер text exposition 0.0.4: кожен рядок —
// HELP, TYPE або семпл; TYPE раз на метрику й до її семплів; значення —
// float. Повертає "name{labels}" -> value.
func parseProm(t *testing.T, body string) map[string]float64 {
	t.Helper()
	types := map[string]string{}
	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			t.Fatalf("empty line in exposition")
		case strings.HasPrefix(line, "# TYPE"):
			m := promTypeRe.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("bad TYPE: %q", line)
			}
			if _, dup := types[m[1]]; dup {
				t.Fatalf("duplicate TYPE for %s", m[1])
			}
			types[m[1]] = m[2]
		case strings.HasPrefix(line, "# HELP"):
			if !promHelpRe.MatchString(line) {
				t.Fatalf("bad HELP: %q", line)
			}
		case strings.HasPrefix(line, "#"):
			t.Fatalf("unexpected comment: %q", line)
		default:
			m := promSampleRe.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("bad sample: %q", line)
			}
			name := m[1]
			base := name
			for _, suf := range []string{"_bucket", "_sum", "_count"} {
				if b := strings.TrimSuffix(name, suf); b != name && types[b] == "histogram" {
					base = b
				}
			}
			if _, ok := types[base]; !ok {
				t.Fatalf("sample before TYPE: %q", line)
			}
			v, err := strconv.ParseFloat(m[4], 64)
			if err != nil {
				t.Fatalf("bad value in %q: %v", line, err)
			}
			key := name + m[2]
			if _, dup := out[key]; dup {
				t.Fatalf("duplicate series %s", key)
			}
			out[key] = v
		}
	}
	return out
}

func metricsBody(t *testing.T, nodes []*nodeSession, now time.Time) map[string]float64 {
	t.Helper()
	var buf bytes.Buffer
	if err := writeMetrics(&buf, nodes, now); err != nil {
		t.Fatal(err)
	}
	return parseProm(t, buf.String())
}

func TestMetricsExpositionParses(t *testing.T) {
	a := &nodeSession{nodeID: `pc-"1"\x`}
	b := &nodeSession{nodeID: "pc-2"}
	metricsTTFF(a, 300*time.Millisecond)
	a.m.mu.Lock()
	a.m.reason = "loss"
	a.m.mu.Unlock()
	got := metricsBody(t, []*nodeSession{b, a}, time.Now())

	for _, k := range []string{
		"oo_hub_nodes", "oo_hub_goroutines", "oo_hub_heap_alloc_bytes",
		"oo_hub_egress_queue_depth", `oo_hub_viewers{node="pc-2"}`,
		`oo_hub_viewer_ttff_seconds_bucket{node="pc-2",le="+Inf"}`,
		`oo_hub_bitrate_target_reason{node="pc-\"1\"\\x",reason="loss"}`,
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	if got["oo_hub_nodes"] != 2 {
		t.Errorf("nodes = %v", got["oo_hub_nodes"])
	}
	// Гістограма кумулятивна: 0.3 с не потрапляє в 0.1/0.25, потрапляє з 0.5.
	n := `node="pc-\"1\"\\x"`
	if got[`oo_hub_viewer_ttff_seconds_bucket{`+n+`,le="0.25"}`] != 0 ||
		got[`oo_hub_viewer_ttff_seconds_bucket{`+n+`,le="0.5"}`] != 1 ||
		got[`oo_hub_viewer_ttff_seconds_bucket{`+n+`,le="+Inf"}`] != 1 ||
		got[`oo_hub_viewer_ttff_seconds_count{`+n+`}`] != 1 {
		t.Errorf("ttff histogram wrong: %v", got)
	}
}

func TestMetricsValuesUpdate(t *testing.T) {
	ns := &nodeSession{nodeID: "n1"}
	t0 := time.Now()
	// 31 кадр по 2 пакети за ~1.0 с (30 fps), перший — ключовий (IDR).
	for f := 0; f <= 30; f++ {
		at := t0.Add(time.Duration(f) * time.Second / 30)
		for i := 0; i < 2; i++ {
			nal := byte(0x41) // non-IDR slice
			if f == 0 {
				nal = 0x65 // IDR
			}
			pkt := &rtp.Packet{Header: rtp.Header{Version: 2, Timestamp: uint32(f * 3000), SequenceNumber: uint16(f*2 + i)}, Payload: append([]byte{nal}, make([]byte, 99)...)}
			metricsAgentPacket(ns, pkt, at)
		}
	}
	nack := &rtcp.TransportLayerNack{Nacks: []rtcp.NackPair{{PacketID: 10, LostPackets: 0b11}}} // 3 seq
	metricsNack(ns, nack, nackStats{})
	metricsNack(ns, nack, nackStats{closed: true, req: 6, hit: 4})
	ns.m.viewerDrops.Add(2)
	ns.m.pliFromViewers.Add(1)
	ns.mu.Lock()
	ns.lastKeyframeReq = t0
	metricsObserveKeyframeReqLocked(ns)
	metricsObserveKeyframeReqLocked(ns) // той самий запит — не рахується вдруге
	ns.bitrate.target = 1_500_000
	ns.mu.Unlock()
	metricsNoteBitrate(ns, 2_000_000, "", 0.1)

	got := metricsBody(t, []*nodeSession{ns}, t0.Add(time.Second))
	l := `{node="n1"}`
	want := map[string]float64{
		"oo_hub_agent_frames_total" + l:                         31,
		"oo_hub_agent_keyframes_total" + l:                      1,
		"oo_hub_agent_keyframes_per_minute" + l:                 1,
		"oo_hub_agent_ingress_packets_total" + l:                62,
		"oo_hub_nack_packets_total" + l:                         2,
		"oo_hub_nack_requested_total" + l:                       6,
		"oo_hub_nack_retransmitted_total" + l:                   4,
		"oo_hub_nack_unanswered_total" + l:                      2,
		"oo_hub_viewer_queue_drops_total" + l:                   2,
		"oo_hub_viewer_pli_total" + l:                           1,
		"oo_hub_keyframe_requests_total" + l:                    1,
		"oo_hub_bitrate_target_bps" + l:                         1_500_000,
		`oo_hub_bitrate_target_reason{node="n1",reason="loss"}`: 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	// Вікно закрилось на 30-му кадрі (рівно 1 с); допуск 29..31.5 fps, щоб
	// не прив’язуватись до межі вікна.
	if fps := got["oo_hub_agent_fps"+l]; fps < 29 || fps > 31.5 {
		t.Errorf("fps = %v", fps)
	}
	if bps := got["oo_hub_agent_ingress_bps"+l]; bps < 40_000 || bps > 60_000 {
		t.Errorf("bps = %v", bps) // ~62 пакети * 112 Б * 8 ≈ 55 кбіт/с
	}
	// Застарілий рейт (агент замовк) не має висіти: через 10 с — нуль.
	later := metricsBody(t, []*nodeSession{ns}, t0.Add(10*time.Second))
	if later["oo_hub_agent_fps"+l] != 0 || later["oo_hub_agent_keyframes_per_minute"+l] != 1 {
		t.Errorf("stale: fps=%v kpm=%v", later["oo_hub_agent_fps"+l], later["oo_hub_agent_keyframes_per_minute"+l])
	}
	if old := metricsBody(t, []*nodeSession{ns}, t0.Add(2*time.Minute)); old["oo_hub_agent_keyframes_per_minute"+l] != 0 {
		t.Errorf("kpm after 2m = %v", old["oo_hub_agent_keyframes_per_minute"+l])
	}
}
