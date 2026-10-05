package main

import (
	"testing"
	"time"
)

// Швидкий режим вимкнено — поведінка рівно стара (+5% раз на 10 с).
func TestFastUpOffIsLegacy(t *testing.T) {
	c := bitrateCtl{target: 2_000_000, startBps: 8_000_000}
	ups := 0
	for i := 1; i <= 60; i++ {
		var s bool
		c, s = c.step(0, 0, t0.Add(time.Duration(i)*time.Second))
		if s {
			ups++
		}
	}
	if ups > 6 {
		t.Fatalf("без fastUp підйомів %d за 60 с", ups)
	}
}

func TestFastUpRecoversFasterAfterCleanPeriod(t *testing.T) {
	slow := bitrateCtl{target: 1_000_000, startBps: 8_000_000}
	fast := slow
	fast.fastUp = true
	for i := 1; i <= 60; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		slow, _ = slow.step(0, 0, now)
		fast, _ = fast.step(0, 0, now)
	}
	if fast.target <= slow.target*2 {
		t.Fatalf("fast %d не помітно швидше за slow %d", fast.target, slow.target)
	}
	if fast.target > 8_000_000 {
		t.Fatalf("стелю перевищено: %d", fast.target)
	}
}

func TestFastUpNotBeforeCleanPeriodAndDebounced(t *testing.T) {
	c := bitrateCtl{target: 2_000_000, startBps: 8_000_000, fastUp: true}
	var last time.Time
	for i := 1; i <= 40; i++ {
		now := t0.Add(time.Duration(i) * time.Second)
		prev := c.target
		var s bool
		c, s = c.step(0, 0, now)
		if !s {
			continue
		}
		if now.Sub(t0) < fastUpAfter && float64(c.target) > float64(prev)*upFactor+1 {
			t.Fatalf("мультиплікативний крок до fastUpAfter на %v", now.Sub(t0))
		}
		if !last.IsZero() && now.Sub(last) < fastUpDebounce {
			t.Fatalf("підйом через %v < fastUpDebounce", now.Sub(last))
		}
		if float64(c.target) > float64(prev)*fastUpMaxStep+1 {
			t.Fatalf("крок %d -> %d більший за fastUpMaxStep", prev, c.target)
		}
		last = now
	}
}

// Гістерезис: після зрізу швидкий режим не йде вище cutFrom*fastUpCapFrac.
func TestFastUpHysteresisBelowCutLevel(t *testing.T) {
	c := bitrateCtl{target: 8_000_000, startBps: 8_000_000, fastUp: true}
	c, _ = c.step(0.3, 0, t0) // 8M -> 5.6M
	if c.cutFrom != 8_000_000 {
		t.Fatalf("cutFrom=%d", c.cutFrom)
	}
	c.target, c.cutFrom = 2_000_000, 4_000_000
	limit := uint64(4_000_000 * fastUpCapFrac)
	// B5: межа діє, поки cutFrom не забуто (cutFromHold: тепер 4-8 с замість
	// 60 с), тож вікно тесту — в межах витримки, а не 40 с: старий цикл пінив
	// саме повільне відновлення, яке B5 виправляє. failedProbes=2 подовжує
	// витримку до 8 с, щоб швидкий режим (після fastUpAfter 5 с) встиг
	// зрушити; межу видно, бо перевіряємо її на кожному кроці. Після забування — див. TestProbeBackoff* у bitrate_cong_test.go.
	c.failedProbes = 2
	c.cutFrom = 2_400_000 // межа 2.04M: досяжна одним кроком розгону (×1.15 < 2.04/2)
	limit = uint64(2_400_000 * fastUpCapFrac)
	for i := 1; i < int(c.cutFromHold()/time.Second); i++ {
		c, _ = c.step(0, 0, t0.Add(time.Duration(i)*time.Second))
		if c.cutFrom > 0 && c.target > limit {
			t.Fatalf("на %d с ціль %d вища за межу швидкого %d", i, c.target, limit)
		}
	}
	// До межі — швидко.
	if c.target != limit || c.cutFrom == 0 {
		t.Fatalf("ціль %d, межа швидкого %d, cutFrom %d", c.target, limit, c.cutFrom)
	}
}

// Вниз нічого не змінилось; сіра зона/черга обривають чистий період.
func TestFastUpKeepsDownDebounceAndResetsOnTrouble(t *testing.T) {
	c := bitrateCtl{target: 4_000_000, startBps: 8_000_000, fastUp: true, lastSent: t0}
	if n, s := c.step(0.3, 0, t0.Add(time.Second)); s || n.target != 4_000_000 {
		t.Fatalf("down-дебаунс порушено: %d %v", n.target, s)
	}
	c = bitrateCtl{target: 2_000_000, startBps: 8_000_000, fastUp: true, cleanSince: t0.Add(-time.Minute)}
	if n, _ := c.step(0.01, 0, t0); !n.cleanSince.IsZero() {
		t.Fatal("сіра зона не скинула cleanSince")
	}
	if _, ok := c.fastUpStep(t0); !ok {
		t.Fatal("чистий період минув, а швидкого кроку немає")
	}
	if n, _ := c.step(0, 200*time.Millisecond, t0); !n.cleanSince.IsZero() || n.target != c.target {
		t.Fatal("черга (excess >= rttUpClear) не зупинила швидкий підйом")
	}
}

func TestFastUpRespectsRemb(t *testing.T) {
	c := bitrateCtl{target: 1_000_000, startBps: 8_000_000, fastUp: true, remb: 1_500_000}
	for i := 1; i <= 60; i++ {
		c, _ = c.step(0, 0, t0.Add(time.Duration(i)*time.Second))
	}
	if c.target > 1_500_000 {
		t.Fatalf("REMB-стелю перевищено: %d", c.target)
	}
}

func TestRembSetsCutFrom(t *testing.T) {
	c := bitrateCtl{target: 8_000_000, startBps: 8_000_000, cleanSince: t0}
	c, s := c.withRemb(3_000_000, t0)
	if !s || c.cutFrom != 8_000_000 || !c.cleanSince.IsZero() {
		t.Fatalf("withRemb: send=%v cutFrom=%d", s, c.cutFrom)
	}
}

func TestContentCeiling(t *testing.T) {
	cases := []struct {
		name       string
		text       bool
		tgt, ceil  uint64
		fps, wantF int
	}{
		{"off-identity", false, 2_000_000, 8_000_000, 30, 30},
		{"text-at-ceiling", true, 8_000_000, 8_000_000, 30, 30},
		{"text-half", true, 4_000_000, 8_000_000, 30, 15},
		{"text-floor", true, 500_000, 8_000_000, 30, minTextFPS},
		{"text-low-fps", true, 1_000_000, 8_000_000, 4, 4},
		{"text-no-ceil", true, 1_000_000, 0, 30, 30},
	}
	for _, c := range cases {
		d := contentCeiling(c.text, c.tgt, c.ceil, c.fps)
		if d.FPSHint != c.wantF || d.Bps != c.tgt {
			t.Errorf("%s: %+v, чекали fps=%d bps=%d", c.name, d, c.wantF, c.tgt)
		}
	}
}

// REMB прийшов під час дебаунсу (ціль не зрізана), далі «чисто»: кламп до REMB
// у step() — це зріз (cutFrom, downDebounce), а не підйом.
func TestStepRembClampIsCut(t *testing.T) {
	c := bitrateCtl{target: 4_000_000, startBps: 8_000_000, fastUp: true, lastSent: t0,
		goodSince: t0.Add(-time.Minute), cleanSince: t0.Add(-time.Minute)}
	c, s := c.withRemb(2_000_000, t0.Add(time.Second))
	if s || c.target != 4_000_000 {
		t.Fatalf("withRemb мав бути задебаунсений: s=%v target=%d", s, c.target)
	}
	// 2.5 с після lastSent: fastUpDebounce(3с) ще не минув, downDebounce(2с) — так.
	c, s = c.step(0, 0, t0.Add(2500*time.Millisecond))
	if !s || c.target != 2_000_000 {
		t.Fatalf("кламп до REMB: s=%v target=%d, want true 2000000", s, c.target)
	}
	if c.cutFrom != 4_000_000 {
		t.Fatalf("cutFrom=%d, want 4000000", c.cutFrom)
	}
	if !c.cleanSince.IsZero() {
		t.Fatalf("серія «чисто» не обірвана зрізом")
	}
}

// cutFrom не вічний: тривалий чистий період його знімає, і швидкий режим
// знову може йти вище за 0.85× старого низького рівня.
func TestCutFromExpiresAfterCleanPeriod(t *testing.T) {
	c := bitrateCtl{target: 500_000, startBps: 8_000_000, fastUp: true, cutFrom: 600_000}
	for i := 1; i <= 120; i++ {
		c, _ = c.step(0, 0, t0.Add(time.Duration(i)*time.Second))
	}
	if c.cutFrom != 0 {
		t.Fatalf("cutFrom=%d не знято після тривалого чистого", c.cutFrom)
	}
	if c.target <= 2_000_000 {
		t.Fatalf("ціль %d застрягла біля старого cutFrom", c.target)
	}
}

// Повільний підйом пройшов рівень затору — cutFrom знято.
func TestCutFromClearedWhenTargetPassesIt(t *testing.T) {
	c := bitrateCtl{target: 1_000_000, startBps: 8_000_000, cutFrom: 1_040_000,
		goodSince: t0.Add(-time.Minute), cleanSince: t0}
	c, s := c.step(0, 0, t0.Add(time.Second))
	if !s || c.target < 1_040_000 {
		t.Fatalf("очікували підйом: s=%v target=%d", s, c.target)
	}
	if c.cutFrom != 0 {
		t.Fatalf("cutFrom=%d не знято після проходу рівня", c.cutFrom)
	}
}
