package main

import (
	"testing"
	"time"
)

// Агент із -bitrate нижче підлоги хаба (1 Мбіт/с проти minBitrateBps 1,5) —
// стеля ноди startBps нижча за підлогу. Зріз на втратах мусить лишатись у
// межах стелі агента: раніше останній кламп до підлоги ставив ціль 1,5 —
// ВИЩЕ, ніж була (втрати «підіймали» бітрейт). Агент її клампить до своєї
// стелі (clampBitrate), а хаб далі вважав, що він уже на підлозі.
// Дзеркало агентського clampBitrate: нижня межа не вища за стелю.
func TestStepFloorNeverAboveAgentCeiling(t *testing.T) {
	start := minBitrateBps * 2 / 3
	c := newBitrateCtl(start)
	now := t0
	for i := 0; i < 10; i++ {
		now = now.Add(3 * time.Second)
		next, send := c.step(0.5, 0, now)
		if next.target > start {
			t.Fatalf("крок %d: втрати 50%% підняли ціль до %d при стелі агента %d", i, next.target, start)
		}
		if send && next.target >= c.target {
			t.Fatalf("крок %d: «зріз» на втратах віддав ціль %d, не нижчу за %d", i, next.target, c.target)
		}
		c = next
	}
	// REMB нижче за все — так само не вище стелі.
	if next, _ := c.withRemb(100_000, now.Add(5*time.Second)); next.target > start {
		t.Fatalf("REMB підняв ціль до %d при стелі %d", next.target, start)
	}
	// Звичайна нода (стеля вища за підлогу): підлога лишається minBitrateBps.
	n := bitrateCtl{target: minBitrateBps + 100_000, startBps: 8_000_000}
	if next, _ := n.step(0.5, 0, t0); next.target != minBitrateBps {
		t.Fatalf("звичайна нода: ціль %d, чекали підлогу %d", next.target, minBitrateBps)
	}
}
