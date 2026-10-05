package bwe

import (
	"sync"
	"time"
)

// Ручки контролера по втратах — ті самі, що в хабі (hub-webrtc/bitrate.go
// бере їх звідси; історія замірів — там).
const (
	MinBitrateBps = 300_000
	LossHighFrac  = 0.02  // >2% втрат — ріжемо
	LossLowFrac   = 0.005 // <=0.5% — чисто, кандидат на підйом
	DownFactor    = 0.7   // крок вниз
	UpFactor      = 1.05  // крок вгору

	GoodStreak   = 5 * time.Second  // скільки поспіль має бути чисто до підйому
	DownDebounce = 2 * time.Second  // не частіше однієї зміни вниз
	UpDebounce   = 10 * time.Second // не частіше одного підйому
)

// LegCtl — локальний контролер цілі ОДНІЄЇ ноги, де енкодер і нога в одному
// процесі (N6: пряма нога агента, хаба між ними нема). Входи — RTCP самої
// ноги: FractionLost із RR, REMB, TWCC-фідбек (детектор затримки). Логіка —
// базова частина контролера хаба: асиметричний AIMD по втратах (0.7 вниз,
// +5% вгору після 5 с чистоти, дебаунси 2/10 с), REMB як стеля, зріз по
// затримці DelayCut з утриманням підйому DelayUpHold. Чого тут НЕМА з хаба
// (свідомо, див. TZ-GENERAL N6): RTT-плечі (pion-агент без SR не має RTT),
// B4 «черга+NACK», B5 проби/швидкий підйом, P1 проба дублікатами.
//
// Потокобезпечний: RTCP-цикли ноги кличуть з різних горутин.
type LegCtl struct {
	mu          sync.Mutex
	target      uint64
	ceil        uint64
	remb        uint64
	goodSince   time.Time
	lastSent    time.Time
	delayOverAt time.Time
	reason      string
}

// NewLegCtl — контролер зі стартовою ціллю start і стелею ceil (0 = start).
func NewLegCtl(start, ceil uint64) *LegCtl {
	if ceil == 0 {
		ceil = start
	}
	if ceil < MinBitrateBps {
		ceil = MinBitrateBps
	}
	if start > ceil {
		start = ceil
	}
	if start < MinBitrateBps {
		start = MinBitrateBps
	}
	return &LegCtl{target: start, ceil: ceil}
}

// Target — поточна ціль і причина останньої зміни.
func (c *LegCtl) Target() (uint64, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target, c.reason
}

func (c *LegCtl) heldLocked(now time.Time) bool {
	return !c.delayOverAt.IsZero() && now.Sub(c.delayOverAt) < DelayUpHold
}

func (c *LegCtl) applyLocked(next uint64, now time.Time, reason string) (uint64, bool) {
	if next < MinBitrateBps {
		next = MinBitrateBps
	}
	if next > c.ceil {
		next = c.ceil
	}
	if c.remb > 0 && next > c.remb {
		next = c.remb
		if next < MinBitrateBps {
			next = MinBitrateBps
		}
	}
	if next == c.target {
		return c.target, false
	}
	c.target, c.lastSent, c.reason = next, now, reason
	return next, true
}

// OnLoss — FractionLost з RR (0..1). Повертає нову ціль і чи вона змінилась.
func (c *LegCtl) OnLoss(frac float64, now time.Time) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case frac > LossHighFrac:
		c.goodSince = time.Time{}
		if !c.lastSent.IsZero() && now.Sub(c.lastSent) < DownDebounce {
			return c.target, false
		}
		return c.applyLocked(uint64(float64(c.target)*DownFactor), now, "loss")
	case frac <= LossLowFrac && !c.heldLocked(now):
		if c.goodSince.IsZero() {
			c.goodSince = now
		}
		if now.Sub(c.goodSince) < GoodStreak || (!c.lastSent.IsZero() && now.Sub(c.lastSent) < UpDebounce) {
			return c.target, false
		}
		next, ok := c.applyLocked(uint64(float64(c.target)*UpFactor), now, "recover")
		if ok {
			c.goodSince = now // серія рахується заново від цього підйому
		}
		return next, ok
	default:
		c.goodSince = time.Time{} // сіра зона: тримаємо, серію збиваємо
		return c.target, false
	}
}

// OnRemb — оцінка смуги від глядача: стеля; поточна ціль вище — зріз одразу
// (з дебаунсом DownDebounce). Підйому за REMB нема.
func (c *LegCtl) OnRemb(bps uint64, now time.Time) (uint64, bool) {
	if bps == 0 {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remb = bps
	if bps >= c.target || (!c.lastSent.IsZero() && now.Sub(c.lastSent) < DownDebounce) {
		return c.target, false
	}
	c.goodSince = time.Time{}
	return c.applyLocked(bps, now, "remb")
}

// OnTWCC — результат TWCC.OnFeedback -> DelayCut (той самий, що в хабі).
func (c *LegCtl) OnTWCC(f Feedback, now time.Time) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.Over && Limited(f.Acked, f.Sent) {
		c.delayOverAt = now
	}
	next, congested, cut := DelayCut(DelayIn{
		Over: f.Over, Acked: f.Acked, Sent: f.Sent, WinStart: f.WinStart, Now: now,
		Target: c.target, LastSent: c.lastSent, LastWasDelay: c.reason == "delay",
		Held: c.heldLocked(now), Floor: MinBitrateBps,
	})
	if congested {
		c.goodSince = time.Time{}
	}
	if !cut {
		return c.target, false
	}
	return c.applyLocked(next, now, "delay")
}
