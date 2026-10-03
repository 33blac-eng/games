package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// B4: модель вузького місця hub->viewer з мілкою чергою (як у
// bench/RESULTS-network.md, п. 5): поки ціль вища за стелю, черга повна
// (приріст ~100 мс), а частка пакетів, які глядач NACK-ає, = 1 − cap/ціль.
// RR FractionLost при цьому в сірій зоні (NACK відновлює) — 1%.
func capLink(target, capBps uint64) (loss float64, excess time.Duration, sig congSignals) {
	if target <= capBps {
		return 0, 0, congSignals{}
	}
	pre := 1 - float64(capBps)/float64(target)
	return 0.01, 100 * time.Millisecond, congSignals{preLoss: pre}
}

type capSim struct {
	c    bitrateCtl
	now  time.Time
	cuts int
	ups  int
}

func newCapSim(start uint64) *capSim {
	return &capSim{c: bitrateCtl{target: start, startBps: start, fastUp: true}, now: t0}
}

// run ганяє RR раз на секунду secs секунд під стелею capBps (0 = без стелі).
func (s *capSim) run(t *testing.T, secs int, capBps uint64, each func(sec int, target uint64)) {
	t.Helper()
	for i := 1; i <= secs; i++ {
		s.now = s.now.Add(time.Second)
		var loss float64
		var ex time.Duration
		var sig congSignals
		if capBps > 0 {
			loss, ex, sig = capLink(s.c.target, capBps)
		}
		prev := s.c.target
		var sent bool
		s.c, sent = s.c.stepSig(loss, ex, sig, s.now)
		if sent && s.c.target < prev {
			s.cuts++
		}
		if sent && s.c.target > prev {
			s.ups++
		}
		if each != nil {
			each(i, s.c.target)
		}
	}
}

func TestCongestionNeedsQueue(t *testing.T) {
	// Рівномірні 10% без черги (далекий глядач, RTT 200 мс) — не затор.
	if ok, _ := congestion(0, congSignals{preLoss: 0.10, plis: 5}, false); ok {
		t.Fatal("втрати без черги прочитано як затор")
	}
	// Черга без NACK (джитер ±40 мс) — не затор.
	if ok, _ := congestion(80*time.Millisecond, congSignals{}, true); ok {
		t.Fatal("джитер без NACK прочитано як затор")
	}
	if ok, f := congestion(60*time.Millisecond, congSignals{preLoss: 0.05}, false); !ok || f != rttDownFactor {
		t.Fatalf("NACK 5%% + черга: ok=%v f=%v, want true %v", ok, f, rttDownFactor)
	}
	if ok, _ := congestion(60*time.Millisecond, congSignals{plis: congPLIs}, false); !ok {
		t.Fatal("PLI-rate + черга не спрацював")
	}
	if ok, _ := congestion(60*time.Millisecond, congSignals{preLoss: 0.01}, true); !ok {
		t.Fatal("тренд затримки + NACK 1% не спрацював")
	}
	if ok, _ := congestion(60*time.Millisecond, congSignals{preLoss: 0.01}, false); ok {
		t.Fatal("NACK 1% без тренду — це ще сіра зона")
	}
	// Глибина кроку — за доставленою швидкістю, у межах [congCutMin, 0.85].
	if _, f := congestion(100*time.Millisecond, congSignals{preLoss: 0.5}, false); f < 0.449 || f > 0.451 {
		if f != congCutMin {
			t.Fatalf("f=%v", f)
		}
	}
	if _, f := congestion(100*time.Millisecond, congSignals{preLoss: 0.9}, false); f != congCutMin {
		t.Fatalf("f=%v, want %v", f, congCutMin)
	}
}

func TestRTTLowTrend(t *testing.T) {
	var c bitrateCtl
	var hit bool
	for _, ms := range []int{10, 45, 60, 75} {
		c, hit = c.rttLowTrend(time.Duration(ms) * time.Millisecond)
	}
	if !hit {
		t.Fatal("стійке зростання над 40 мс не помічено")
	}
	c, hit = c.rttLowTrend(50 * time.Millisecond)
	if hit {
		t.Fatal("спад не обнулив серію")
	}
}

// Критерій B4: під стелею 2/4 Мбіт/с ціль ≤ стелі за ≤ 5 с.
func TestCapConvergesWithin5s(t *testing.T) {
	for _, capBps := range []uint64{2_000_000, 4_000_000} {
		s := newCapSim(8_000_000)
		at := -1
		s.run(t, 10, capBps, func(sec int, target uint64) {
			if at < 0 && target <= capBps {
				at = sec
			}
		})
		if at < 0 || at > 5 {
			t.Fatalf("стеля %d: ціль ≤ стелі на %d с (ціль %d), want ≤ 5", capBps, at, s.c.target)
		}
	}
}

// Той самий сценарій ДО B4 (нульові сигнали) — ціль не сходиться: це і є вада.
func TestCapWithoutSignalsIsBlind(t *testing.T) {
	s := newCapSim(8_000_000)
	for i := 0; i < 10; i++ {
		s.now = s.now.Add(time.Second)
		loss, ex, _ := capLink(s.c.target, 2_000_000)
		s.c, _ = s.c.step(loss, ex, s.now)
	}
	if s.c.target <= 2_000_000 {
		t.Skip("старий контролер уже сходиться — модель стелі не відтворює ваду")
	}
}

// Сталa стеля — без пилки: проби над стелею рідшають (експоненційний відступ),
// і невдала проба відкочується на рівень до неї, а не множником униз.
func TestProbeBackoffNoOscillationAtStableCap(t *testing.T) {
	capBps := uint64(4_000_000)
	s := newCapSim(8_000_000)
	s.run(t, 10, capBps, nil)
	s.cuts, s.ups = 0, 0
	over := 0
	min, max := s.c.target, s.c.target
	s.run(t, 120, capBps, func(_ int, target uint64) {
		if target > capBps {
			over++
		}
		if target > max {
			max = target
		}
		if target < min {
			min = target
		}
	})
	if s.cuts > 15 {
		t.Fatalf("за 120 с на сталій стелі %d зрізів — пилка", s.cuts)
	}
	// Проба — це за визначенням трохи над стелею; модель ще й бачить затор із
	// запізненням на один RR. Межа — третина часу і не вище +35%.
	if over > 40 || float64(max) > float64(capBps)*1.35 {
		t.Fatalf("над стелею %d с із 120, максимум %d", over, max)
	}
	if float64(min) < float64(capBps)*0.8 {
		t.Fatalf("ціль падала до %d при стелі %d", min, capBps)
	}
	if s.c.failedProbes == 0 {
		t.Fatal("невдалі проби не враховано")
	}
}

// Критерій B5: після зняття стелі ≤ 15 с до 90% від 8 Мбіт/с. Стеля тримається
// 40 с, як у bench (cap-*); найгірший випадок фази проби — перевіряємо кілька
// тривалостей стелі.
func TestRecoveryAfterCapLifted(t *testing.T) {
	for _, capBps := range []uint64{2_000_000, 4_000_000} {
		for _, hold := range []int{20, 30, 40} {
			s := newCapSim(8_000_000)
			s.run(t, hold, capBps, nil)
			at := -1
			s.run(t, 60, 0, func(sec int, target uint64) {
				if at < 0 && target >= 7_200_000 {
					at = sec
				}
			})
			if at < 0 || at > 15 {
				t.Errorf("стеля %d, %d с: 90%% за %d с (ціль %d), want ≤ 15", capBps, hold, at, s.c.target)
			}
		}
	}
}

// Рівномірні втрати без черги не ріжуть ціль (NACK їх рятує, B4 тут не діє).
func TestRandomLossWithoutQueueKeepsTarget(t *testing.T) {
	c := bitrateCtl{target: 8_000_000, startBps: 8_000_000, fastUp: true}
	now := t0
	for i := 0; i < 60; i++ {
		now = now.Add(time.Second)
		c, _ = c.stepSig(0.01, 0, congSignals{preLoss: 0.10}, now)
	}
	if c.target != 8_000_000 {
		t.Fatalf("ціль %d при рівномірних втратах без черги", c.target)
	}
}

func TestFastUpDefaultOn(t *testing.T) {
	if !fastRecoveryDefault {
		t.Skip("OO_SCREEN_BITRATE_FASTUP=0 у середовищі")
	}
	if !newBitrateCtl(8_000_000).fastUp {
		t.Fatal("B5: fastUp має бути увімкнено за замовчуванням")
	}
}

func TestLegCongestionPreLoss(t *testing.T) {
	ns := &nodeSession{nodeID: "cong", viewers: map[*webrtc.PeerConnection]*viewerLeg{}}
	a, b := &viewerLeg{ready: true}, &viewerLeg{ready: true}
	ns.viewers[&webrtc.PeerConnection{}] = a
	ns.viewers[&webrtc.PeerConnection{}] = b
	now := t0
	a.lastRR, b.lastRR = now, now

	atomic.StoreUint64(&a.sent, 100)
	legCongestion(ns, a, now) // відкрили інтервал
	atomic.StoreUint64(&a.sent, 200)
	ns.mu.Lock()
	a.noteNackSeqs([]uint16{1, 2, 3, 4, 5})
	a.noteNackSeqs([]uint16{3, 4, 5}) // повторний NACK тих самих seq
	ns.mu.Unlock()
	notePLI(ns, a)
	notePLI(ns, a)
	sig := legCongestion(ns, a, now)
	if sig.preLoss < 0.049 || sig.preLoss > 0.051 || sig.plis != 2 {
		t.Fatalf("sig=%+v, want preLoss 0.05 plis 2", sig)
	}
	// Повтор seq 5 через межу інтервалу не рахується двічі.
	atomic.StoreUint64(&a.sent, 300)
	ns.mu.Lock()
	a.noteNackSeqs([]uint16{5, 6})
	ns.mu.Unlock()
	if sig = legCongestion(ns, a, now); sig.preLoss < 0.009 || sig.preLoss > 0.011 || sig.plis != 0 {
		t.Fatalf("sig=%+v, want preLoss 0.01", sig)
	}
	// Найгірша нога веде: b бачить 0.01 від a.
	atomic.StoreUint64(&b.sent, 10) // < legMinSent: інтервал b не закривається
	if sig = legCongestion(ns, b, now); sig.preLoss < 0.009 {
		t.Fatalf("worst leg не врахована: %+v", sig)
	}
	// Протухла нога не тягне.
	if sig = legCongestion(ns, b, now.Add(viewerRRStale+time.Second)); sig.preLoss != 0 {
		t.Fatalf("протухла нога врахована: %+v", sig)
	}
}

// Стала затримка, що з'явилась посеред сесії (rtt-200ms у стенді), + 1%
// рівномірних втрат: черги немає, після першого зрізу приріст не спадає —
// стає базою, і контролер не їде до підлоги.
func TestStepDelayRebasesAfterUselessCut(t *testing.T) {
	c := bitrateCtl{target: 8_000_000, startBps: 8_000_000, fastUp: true}
	now := t0
	cuts := 0
	for i := 0; i < 30; i++ {
		now = now.Add(time.Second)
		prev := c.target
		c, _ = c.stepSig(0.005, 200*time.Millisecond, congSignals{preLoss: 0.01}, now)
		if c.target < prev {
			cuts++
		}
	}
	if cuts > 2 || c.target < 5_000_000 {
		t.Fatalf("стала затримка: %d зрізів, ціль %d", cuts, c.target)
	}
}
