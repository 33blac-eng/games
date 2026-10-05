package main

import (
	"testing"
	"time"
)

func probeCtl(target, start uint64) bitrateCtl {
	return bitrateCtl{target: target, startBps: start, probeOn: true, cleanSince: t0}
}

func TestProbeOffByDefault(t *testing.T) {
	if probeEnabled || paceEnabled {
		t.Fatal("проба/пейсинг мають бути вимкнені без env")
	}
	c := bitrateCtl{target: 2_000_000, startBps: 8_000_000, cleanSince: t0}
	if _, _, ok := c.probeDue(t0.Add(10 * time.Second)); ok {
		t.Fatal("проба без probeOn")
	}
}

func TestProbeDueGates(t *testing.T) {
	now := t0.Add(5 * time.Second)
	c := probeCtl(2_000_000, 8_000_000)
	c2, rate, ok := c.probeDue(now)
	if !ok || rate != 3_000_000 || !c2.probing { // перша після зрізу ×1.5
		t.Fatalf("ok=%v rate=%d", ok, rate)
	}
	if _, _, ok := c2.probeDue(now); ok {
		t.Fatal("друга проба поверх першої")
	}
	// Не чисто достатньо довго.
	c3 := probeCtl(2_000_000, 8_000_000)
	c3.cleanSince = now.Add(-200 * time.Millisecond)
	if _, _, ok := c3.probeDue(now); ok {
		t.Fatal("проба без probeClean")
	}
	// Вже біля стелі.
	if _, _, ok := probeCtl(7_300_000, 8_000_000).probeDue(now); ok {
		t.Fatal("проба при цілі ≥ 90% стелі")
	}
	// REMB — теж стеля проби.
	c4 := probeCtl(2_000_000, 8_000_000)
	c4.remb = 3_000_000
	c4.probeLastOK = true
	if _, rate, _ := c4.probeDue(now); rate != 3_300_000 {
		t.Fatalf("rate=%d, want 3.3M (REMB×1.1)", rate)
	}
}

func TestProbeDoneOKRaisesTarget(t *testing.T) {
	now := t0.Add(5 * time.Second)
	c, _, _ := probeCtl(2_000_000, 8_000_000).probeDue(now)
	c.cutFrom, c.probeLvl = 2_500_000, 2_500_000
	c, send := c.probeDone(probeOK, now.Add(time.Second))
	if !send || c.target != 2_550_000 || c.probing || c.cutFrom != 0 || c.probeLvl != 0 {
		t.Fatalf("send=%v target=%d probing=%v cutFrom=%d", send, c.target, c.probing, c.cutFrom)
	}
	// Після успіху — ×2 (розгін).
	if _, rate, ok := c.probeDue(c.probeNextAt); !ok || rate != 5_100_000 {
		t.Fatalf("після успіху rate=%d ok=%v", rate, ok)
	}
	// Біля стелі: проба 1.1×стелі дає ≥ 90%.
	c = probeCtl(6_000_000, 8_000_000)
	c.probeLastOK = true
	c, rate, _ := c.probeDue(now)
	if rate != 8_800_000 {
		t.Fatalf("rate=%d", rate)
	}
	c, _ = c.probeDone(probeOK, now)
	if c.target < 7_200_000 || c.target > 8_000_000 {
		t.Fatalf("target=%d, want 90..100%% стелі", c.target)
	}
}

func TestProbeFailBacksOff(t *testing.T) {
	now := t0.Add(5 * time.Second)
	c := probeCtl(3_000_000, 8_000_000)
	var gaps []time.Duration
	var rates []uint64
	for i := 0; i < 5; i++ {
		var rate uint64
		var ok bool
		c, rate, ok = c.probeDue(now)
		if !ok {
			t.Fatalf("i=%d: проба не стартувала", i)
		}
		rates = append(rates, rate)
		var send bool
		c, send = c.probeDone(probeFailed, now)
		if send || c.target != 3_000_000 {
			t.Fatal("невдала проба змінила ціль відео")
		}
		gaps = append(gaps, c.probeNextAt.Sub(now))
		now = c.probeNextAt
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	for i := range want {
		if gaps[i] != want[i] {
			t.Fatalf("gaps=%v", gaps)
		}
	}
	if rates[0] != 4_500_000 || rates[1] != 3_750_000 || rates[2] != 3_449_999 {
		t.Fatalf("rates=%v", rates)
	}
	// Inconclusive — не рахується невдачею.
	c, _, _ = c.probeDue(now)
	f := c.probeFails
	c, _ = c.probeDone(probeInconclusive, now)
	if c.probeFails != f {
		t.Fatal("inconclusive збільшив лічильник")
	}
}

func TestProbeMuteBlocksCuts(t *testing.T) {
	now := t0.Add(5 * time.Second)
	c := probeCtl(4_000_000, 8_000_000)
	c.probeMuteUntil = now.Add(time.Second)
	c, sent := c.stepSig(0.05, 100*time.Millisecond, congSignals{preLoss: 0.3}, now)
	if sent || c.target != 4_000_000 {
		t.Fatalf("зріз у вікні проби: sent=%v target=%d", sent, c.target)
	}
	c, sent = c.stepSig(0.05, 100*time.Millisecond, congSignals{preLoss: 0.3}, now.Add(2*time.Second))
	if !sent || c.target >= 4_000_000 {
		t.Fatalf("після вікна зріз мав бути: sent=%v target=%d", sent, c.target)
	}
}

func TestProbeVerdict(t *testing.T) {
	mk := func(sent uint64, nacks int32) *legProbe {
		p := &legProbe{start: t0, end: t0.Add(probeDur), bps: 4_000_000}
		p.sentBytes.Store(sent)
		p.nacks.Store(nacks)
		return p
	}
	full := uint64(4_000_000 / 8 * probeDur.Seconds())
	if v := probeVerdict([]*legProbe{mk(full, 0)}); v != probeOK {
		t.Fatalf("v=%v", v)
	}
	if v := probeVerdict([]*legProbe{mk(full, 0), mk(full, 3)}); v != probeFailed {
		t.Fatalf("v=%v", v)
	}
	if v := probeVerdict([]*legProbe{mk(full/10, 0)}); v != probeInconclusive {
		t.Fatalf("v=%v", v)
	}
	if v := probeVerdict(nil); v != probeInconclusive {
		t.Fatalf("v=%v", v)
	}
}

func TestNoteProbeNack(t *testing.T) {
	vl := &viewerLeg{}
	if vl.noteProbeNack(1, t0) {
		t.Fatal("NACK без проби віднесено до проби")
	}
	p := &legProbe{start: t0, end: t0.Add(probeDur), evalUntil: t0.Add(time.Second)}
	vl.probe.Store(p)
	if !vl.noteProbeNack(2, t0.Add(100*time.Millisecond)) || p.aborted.Load() {
		t.Fatal("2 NACK — ще не обрив")
	}
	vl.noteProbeNack(1, t0.Add(200*time.Millisecond))
	if !p.aborted.Load() {
		t.Fatal("3 NACK мали обірвати пробу")
	}
	if vl.noteProbeNack(1, t0.Add(2*time.Second)) {
		t.Fatal("NACK після evalUntil віднесено до проби")
	}
}

func TestPadOwed(t *testing.T) {
	if o := padOwed(8_000_000, t0, t0.Add(100*time.Millisecond), 40_000); o != 60_000 {
		t.Fatalf("owed=%d", o)
	}
	if o := padOwed(8_000_000, t0, t0.Add(100*time.Millisecond), 200_000); o >= 0 {
		t.Fatalf("owed=%d", o)
	}
}

// Пейсер: сплеск IDR 250 КБ при цілі 8M розтягується на ~S/(1.5×1 МБ/с).
func TestPacerSpreadsBurst(t *testing.T) {
	var p pacer
	p.setRate(8_000_000)
	now := t0
	var total time.Duration
	for i := 0; i < 210; i++ { // ~250 КБ пакетами по 1200
		d := p.wait(1200, now)
		now = now.Add(d)
		total += d
	}
	want := time.Duration((252_000.0 - p.capacity()) / (1_000_000 * paceMul) * float64(time.Second))
	if total < want-10*time.Millisecond || total > want+10*time.Millisecond {
		t.Fatalf("сплеск розтягнуто на %v, want ~%v", total, want)
	}
	// Рівний потік нижче за rate — без затримок.
	var q pacer
	q.setRate(8_000_000)
	now = t0
	for i := 0; i < 100; i++ {
		now = now.Add(2 * time.Millisecond) // 600 КБ/с < 1.5 МБ/с
		if d := q.wait(1200, now); d != 0 {
			t.Fatalf("i=%d: затримка %v на рівному потоці", i, d)
		}
	}
	var off pacer
	if off.wait(1_000_000, t0) != 0 {
		t.Fatal("нульовий пейсер затримав")
	}
}

// Модель стелі (як capLink) + проба: успіх, якщо rate ≤ cap/0.875 (черга
// 100 мс не переповнюється за 700 мс). Це МОДЕЛЬ контролера, не мережа —
// справжній замір у bench/RESULTS-network.md (P1).
func TestProbeCapModelRecovery(t *testing.T) {
	for _, capBps := range []uint64{2_000_000, 4_000_000} {
		c := bitrateCtl{target: 8_000_000, startBps: 8_000_000, probeOn: true}
		now := t0
		var probeEnd time.Time
		var probeOKv bool
		under := -1
		recover := -1
		over := 0
		for sec := 1; sec <= 120; sec++ {
			now = now.Add(time.Second)
			cap := capBps
			if sec > 40 {
				cap = 0
			}
			if !probeEnd.IsZero() && !now.Before(probeEnd) {
				res := probeFailed
				if probeOKv {
					res = probeOK
				}
				c, _ = c.probeDone(res, now)
				probeEnd = time.Time{}
			}
			var loss float64
			var ex time.Duration
			var sig congSignals
			if cap > 0 {
				loss, ex, sig = capLink(c.target, cap)
			}
			c, _ = c.stepSig(loss, ex, sig, now)
			var rate uint64
			var ok bool
			c, rate, ok = c.probeDue(now)
			if ok {
				probeEnd = now.Add(probeDur + probeGrace)
				probeOKv = cap == 0 || float64(rate)*0.875 <= float64(cap)
			}
			if sec <= 40 && under < 0 && c.target <= capBps {
				under = sec
			}
			if sec <= 40 && sec > 10 && c.target > capBps {
				over++
			}
			if sec > 40 && recover < 0 && c.target >= 7_200_000 {
				recover = sec - 40
			}
		}
		t.Logf("cap %d: ціль ≤ стелі за %d с, над стелею %d/30 с, ≥90%% після зняття за %d с", capBps, under, over, recover)
		if under < 0 || under > 5 || over > 6 {
			t.Fatalf("cap %d: under=%d over=%d", capBps, under, over)
		}
		if recover < 0 || recover > 15 {
			t.Fatalf("cap %d: відновлення %d с, want ≤ 15", capBps, recover)
		}
	}
}
