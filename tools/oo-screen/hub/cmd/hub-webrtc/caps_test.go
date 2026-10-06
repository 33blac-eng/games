package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/control"
	"github.com/pion/webrtc/v4"
)

// C1: звіт getCapabilities глядача доходить до /metrics лише з прапорцем.
func TestViewerCapsMetrics(t *testing.T) {
	old, oldStats := capsTelemetryEnabled, viewerCapsStats
	t.Cleanup(func() { capsTelemetryEnabled, viewerCapsStats = old, oldStats })
	viewerCapsStats = &capsCounters{}

	var req offerReq
	body := `{"sdp":"x","ticket":"t","caps":{"h264":["42e01f","F4001F","f4001f","zz","640c1f"],"vp9":[0,1],"av1":[0,1,9]}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil || req.Caps == nil {
		t.Fatalf("caps не розібрано: %v %+v", err, req)
	}

	capsTelemetryEnabled = false
	recordViewerCaps(req.Caps)
	var buf bytes.Buffer
	if err := writeMetrics(&buf, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("oo_hub_viewer_caps")) {
		t.Fatalf("без прапорця метрик бути не повинно")
	}

	capsTelemetryEnabled = true
	recordViewerCaps(req.Caps)
	recordViewerCaps(&viewerCaps{H264: []string{"42e01f"}})
	buf.Reset()
	if err := writeMetrics(&buf, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	m := parseProm(t, buf.String())
	want := map[string]float64{
		`oo_hub_viewer_caps_reports_total`:                   2,
		`oo_hub_viewer_h264_profile_total{profile="42e01f"}`: 2,
		`oo_hub_viewer_h264_profile_total{profile="f4001f"}`: 1,
		`oo_hub_viewer_h264_profile_total{profile="640c1f"}`: 1,
		`oo_hub_viewer_h264_profile_total{profile="other"}`:  1,
		`oo_hub_viewer_vp9_profile_total{profile="1"}`:       1,
		`oo_hub_viewer_av1_profile_total{profile="other"}`:   1,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
}

// Кардинальність обмежена: сміття від клієнта не роздуває /metrics.
func TestViewerCapsCardinality(t *testing.T) {
	c := &capsCounters{}
	for i := 0; i < 100; i++ {
		c.observe(&viewerCaps{H264: []string{fmt.Sprintf("64%04x", i)}})
	}
	if len(c.h264) > capsMaxLabels+1 {
		t.Fatalf("міток %d > %d", len(c.h264), capsMaxLabels+1)
	}
}

// C2: enc_stats агента -> /metrics (CODECAPI-властивості й QP), лише з прапорцем.
func TestAgentEncStatsMetrics(t *testing.T) {
	old := capsTelemetryEnabled
	t.Cleanup(func() {
		capsTelemetryEnabled = old
		agentEncMu.Lock()
		delete(agentEncBy, "enc-n")
		agentEncMu.Unlock()
	})
	ns := &nodeSession{nodeID: "enc-n", viewers: map[*webrtc.PeerConnection]*viewerLeg{}}
	msg := control.Msg{V: control.Version, Type: control.TypeEncStats, Seq: 1,
		Encoder: "NVIDIA H.264 Encoder MFT", EncCaps: "ROIEnabled=M LTRBufferControl=- MaxQP=S Evil=M",
		QPLast: 18, QPMin: 16, QPMax: 30, QPFrames: 40}

	capsTelemetryEnabled = false
	handleAgentCtl(ns, ctlBytes(t, msg), time.Now())
	var buf bytes.Buffer
	_ = writeMetrics(&buf, []*nodeSession{ns}, time.Now())
	if bytes.Contains(buf.Bytes(), []byte("oo_hub_agent_codecapi")) {
		t.Fatal("без прапорця метрик бути не повинно")
	}

	capsTelemetryEnabled = true
	handleAgentCtl(ns, ctlBytes(t, msg), time.Now())
	handleAgentCtl(ns, ctlBytes(t, msg), time.Now())
	buf.Reset()
	if err := writeMetrics(&buf, []*nodeSession{ns}, time.Now()); err != nil {
		t.Fatal(err)
	}
	m := parseProm(t, buf.String())
	want := map[string]float64{
		`oo_hub_agent_codecapi{node="enc-n",prop="ROIEnabled",state="modifiable"}`:                1,
		`oo_hub_agent_codecapi{node="enc-n",prop="LTRBufferControl",state="unsupported"}`:         1,
		`oo_hub_agent_codecapi{node="enc-n",prop="MaxQP",state="supported"}`:                      1,
		`oo_hub_agent_frame_qp{node="enc-n",stat="last"}`:                                         18,
		`oo_hub_agent_frame_qp{node="enc-n",stat="min"}`:                                          16,
		`oo_hub_agent_frame_qp{node="enc-n",stat="max"}`:                                          30,
		`oo_hub_agent_qp_frames_total{node="enc-n"}`:                                              80,
		`oo_hub_agent_encoder_info{node="enc-n",encoder="NVIDIA H.264 Encoder MFT",software="0"}`: 1,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if bytes.Contains(buf.Bytes(), []byte("Evil")) {
		t.Error("невідома властивість просочилась у мітки")
	}
}
