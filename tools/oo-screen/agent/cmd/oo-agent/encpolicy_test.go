package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/agent/encode"
	"github.com/organicoils/oo-screen/internal/control"
	"github.com/organicoils/oo-screen/internal/h264"
	"github.com/organicoils/oo-screen/internal/keyframe"
	"github.com/organicoils/oo-screen/internal/refine"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestRCPolicyFromEnv(t *testing.T) {
	if p := rcPolicyFromEnv(envMap(nil)); !p.RefineAfterIDR || p.RefineQPAware {
		t.Fatalf("дефолт: %+v (refine після IDR — увімкнено, QP-aware — ні)", p)
	}
	if p := rcPolicyFromEnv(envMap(map[string]string{"OO_SCREEN_REFINE_AFTER_IDR": "0"})); p.RefineAfterIDR {
		t.Fatal("OO_SCREEN_REFINE_AFTER_IDR=0 не вимкнув")
	}
	p := rcPolicyFromEnv(envMap(map[string]string{
		"OO_SCREEN_REFINE_AFTER_IDR": "1", "OO_SCREEN_REFINE_QP_AWARE": "on",
	}))
	if !p.RefineAfterIDR || !p.RefineQPAware {
		t.Fatalf("увімкнення: %+v", p)
	}
	if envBool(envMap(map[string]string{"X": "garbage"}), "X", true) != true {
		t.Fatal("сміття має давати дефолт")
	}
	if envInt(envMap(map[string]string{"X": "60"}), "X", 7, 0, 51) != 7 {
		t.Fatal("поза межами має давати дефолт")
	}
	if envInt(envMap(map[string]string{"X": "40"}), "X", 7, 0, 51) != 40 {
		t.Fatal("ціле в межах")
	}
}

func TestAUKindsPipelined(t *testing.T) {
	var k auKinds
	k.note(1, refine.Frame{Motion: true})
	k.note(2, refine.Frame{Refine: 22})
	if f, ok := k.take(2); !ok || f.Refine != 22 {
		t.Fatalf("take(2) = %+v %v", f, ok)
	}
	if f, ok := k.take(1); !ok || !f.Motion {
		t.Fatalf("take(1) = %+v %v", f, ok)
	}
	if _, ok := k.take(1); ok {
		t.Fatal("повторний take")
	}
	for i := 0; i < 100; i++ {
		k.note(time.Duration(i), refine.Frame{})
	}
	if len(k.m) != auKindsMax || len(k.order) != auKindsMax {
		t.Fatalf("не обмежено: %d/%d", len(k.m), len(k.order))
	}
	if _, ok := k.take(0); ok {
		t.Fatal("найстаріший не витіснено")
	}
}

// Наскрізно: справжній потік x264 (internal/h264/testdata), AU приходять із
// запізненням на кадр, як з апаратного MFT; refine бачить QP і пропускає
// крок QP 22 після IDR, кращого за нього.
func TestObserveAUsDrivesRefine(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "h264", "testdata", "qp-abr-main.h264"))
	if err != nil {
		t.Fatal(err)
	}
	h := h264.SplitAUs(b)
	t0 := time.Unix(0, 0)
	r := refine.New(refine.Config{QPAware: true, AfterKeyframe: true})
	var k auKinds
	qpr := h264.NewQPReader()
	kf := keyframe.New(keyframe.Config{GOPFrames: 300})
	// Кадр 0 (IDR, QP 24) подали як рух, AU повертається з наступним Encode.
	r.Motion(t0)
	k.note(0, refine.Frame{Motion: true})
	observeAUs(nil, &k, qpr, r, nil, t0, nil)
	k.note(time.Second, refine.Frame{})
	observeAUs([]encode.AU{{Data: h[0].Data, Keyframe: h[0].Keyframe, PTS: 0}}, &k, qpr, r, kf, t0, nil)
	if r.WorstQP() != 24 {
		t.Fatalf("worst %d після IDR QP 24", r.WorstQP())
	}
	qp, ok := r.Due(t0.Add(refine.DefaultIdle))
	if !ok || qp != 22 {
		t.Fatalf("refine %d %v: 22 < 24, крок потрібен", qp, ok)
	}
	// AU 5 — P-кадр із примусовим QP 18, поданий як refine 18.
	k.note(5, refine.Frame{Refine: 18})
	observeAUs([]encode.AU{{Data: h[1].Data, PTS: 99}, {Data: h[5].Data, PTS: 5}}, &k, qpr, r, kf, t0, nil)
	// Невідомий PTS (99, QP 31) — рух: worst 31, далі refine 18 -> 18.
	if r.WorstQP() != 18 {
		t.Fatalf("worst %d після refine 18", r.WorstQP())
	}
	if kf.Since() != 2 {
		t.Fatalf("keyframe.Since %d: IDR скидає, два P-кадри рахуються", kf.Since())
	}
}

func TestRCPolicyGOPAndBounds(t *testing.T) {
	if g := encoderGOP(300, rcPolicy{}); g != 300 {
		t.Fatalf("без IdleIDR GOP %d", g)
	}
	if g := encoderGOP(300, rcPolicy{IdleIDR: true}); g != 600 {
		t.Fatalf("IdleIDR: GOP MFT %d, хочемо запобіжник 600", g)
	}
	if g := encoderGOP(0, rcPolicy{IdleIDR: true}); g != 0 {
		t.Fatalf("дефолтний GOP має лишатись дефолтом, а не %d", g)
	}
	if _, _, ok := qpBounds(rcPolicy{}); ok {
		t.Fatal("межі без env")
	}
	if mn, mx, ok := qpBounds(rcPolicy{QPMin: 40, QPMax: 30}); !ok || mn != 0 || mx != 30 {
		t.Fatalf("min>max: %d %d %v", mn, mx, ok)
	}
	p := rcPolicyFromEnv(envMap(map[string]string{
		"OO_SCREEN_IDLE_IDR": "1", "OO_SCREEN_QP_MIN": "16", "OO_SCREEN_QP_MAX": "38", "OO_SCREEN_INTRA_REFRESH": "90",
	}))
	if !p.IdleIDR || p.QPMin != 16 || p.QPMax != 38 || p.IntraRefresh != 90 {
		t.Fatalf("env: %+v", p)
	}
}

// C2: QP кадрів з потоку доходять до enc_stats; типово телеметрію вимкнено.
func TestEncStatsQPWindow(t *testing.T) {
	if rcPolicyFromEnv(func(string) string { return "" }).EncTelemetry {
		t.Fatal("OO_SCREEN_ENC_TELEMETRY має бути типово вимкнено")
	}
	if !rcPolicyFromEnv(func(k string) string {
		if k == "OO_SCREEN_ENC_TELEMETRY" {
			return "1"
		}
		return ""
	}).EncTelemetry {
		t.Fatal("OO_SCREEN_ENC_TELEMETRY=1 не ввімкнуло")
	}

	b, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "h264", "testdata", "qp-abr-main.h264"))
	if err != nil {
		t.Fatal(err)
	}
	h := h264.SplitAUs(b)
	var k auKinds
	qpr := h264.NewQPReader()
	r := refine.New(refine.Config{})
	w := &qpWindow{}
	observeAUs([]encode.AU{{Data: h[0].Data, Keyframe: h[0].Keyframe}}, &k, qpr, r, nil, time.Now(), w)
	if w.n != 1 || w.last <= 0 || w.min != w.last || w.max != w.last {
		t.Fatalf("вікно QP: %+v", *w)
	}
	q := w.last
	m := w.encStatsMsg(7, "NVIDIA H.264 Encoder MFT", false, "ROIEnabled=M")
	if m.Type != control.TypeEncStats || m.QPLast != q || m.QPMin != q || m.QPMax != q || m.QPFrames != 1 || m.EncCaps != "ROIEnabled=M" {
		t.Fatalf("msg %+v", m)
	}
	if w.n != 0 || w.last != q {
		t.Fatalf("після звіту вікно не скинуто: %+v", *w)
	}
	var nilW *qpWindow
	nilW.add(20) // вимкнено — без паніки
}

// C3: прапорці збіжності й великого кадру типово вимкнені і доходять до
// refine.Config / меж QP.
func TestRefineConvergePolicy(t *testing.T) {
	off := rcPolicyFromEnv(func(string) string { return "" })
	if off.RefineConverge || off.LargeFrameQP != 0 {
		t.Fatalf("типово має бути вимкнено: %+v", off)
	}
	if c := refineConfig(off, time.Millisecond, 8_000_000); c.Converge {
		t.Fatal("Converge без прапорця")
	}
	env := map[string]string{"OO_SCREEN_REFINE_CONVERGE": "1", "OO_SCREEN_REFINE_TARGET_QP": "14", "OO_SCREEN_LARGE_FRAME_QP": "32"}
	on := rcPolicyFromEnv(func(k string) string { return env[k] })
	c := refineConfig(on, time.Millisecond, 8_000_000)
	if !c.Converge || c.TargetQP != 14 || c.ByteBudget != 1_000_000 {
		t.Fatalf("config %+v", c)
	}
	if _, _, ok := largeFrameBounds(on, false, 1, 2_000_000, 1920*1080); ok {
		t.Fatal("правило на нерухомому кадрі")
	}
	if mn, _, ok := largeFrameBounds(on, true, 1, 2_000_000, 1920*1080); !ok || mn != 32 {
		t.Fatalf("великий кадр на 2M: %d %v", mn, ok)
	}
	on.QPMax = 30
	if _, _, ok := largeFrameBounds(on, true, 1, 2_000_000, 1920*1080); ok {
		t.Fatal("MaxQP 30 < MinQP 32 — правило мусить поступитись")
	}
}
