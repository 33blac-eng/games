package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
)

// Проба наскрізь через pump: нога з кількома пакетами в ring-і за probeDur
// мусить реально дописати дублями ≥ probeFull від rate, і вердикт — OK.
// Це писар у процесі (трек незвʼязаний, мережі нема) — доводить лише, що pump
// встигає генерувати rate, а не що канал його несе.
func TestProbeThroughPump(t *testing.T) {
	// Керований тікер проби: під навантаженням справжній тікер губив тики
	// (pump не отримував процесор), і вікно проби недобирало байтів.
	ticks := make(chan time.Time)
	prevTicker := newProbeTicker
	newProbeTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	t.Cleanup(func() { newProbeTicker = prevTicker })
	ns := readyNode(t, "probe-pump")
	vl := ns.onlyViewer(t)
	for i := 0; i < 8; i++ {
		forwardToViewers(ns, agentGen1, &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i) * 3000},
			Payload: make([]byte, 1100),
		})
	}
	waitSent(t, vl, 8, "priming")
	now := time.Now()
	p := &legProbe{start: now, end: now.Add(probeDur), bps: 4_000_000, evalUntil: now.Add(probeDur + probeGrace)}
	vl.setProbe(p)
	// Тики по годиннику проби, крок probeTick, до кінця вікна включно.
	// Канал небуферизований: кожна відправка повертається лише тоді, коли
	// pump уже обробив попередній тик, — без вікон сну.
	for at := now.Add(probeTick); !at.After(p.end); at = at.Add(probeTick) {
		ticks <- at
	}
	ticks <- p.end // гарантує, що останній тик у вікні вже оброблено
	vl.setProbe(nil)
	want := float64(p.bps) / 8 * probeDur.Seconds()
	got := float64(p.sentBytes.Load())
	t.Logf("pump: %.0f / %.0f байт (%.1f%%), дублі %d, sent=%d", got, want, 100*got/want, p.padBytes.Load(), atomic.LoadUint64(&vl.sent))
	if p.padBytes.Load() == 0 {
		t.Fatal("pump не дописав жодного дубля")
	}
	if got > want*1.05 {
		t.Fatalf("переплата: %.0f > %.0f", got, want)
	}
	if v := probeVerdict([]*legProbe{p}); v != probeOK {
		t.Fatalf("v=%v при %.1f%%", v, 100*got/want)
	}
	if n := atomic.LoadUint64(&vl.sent); n != 8 {
		t.Fatalf("дублі пораховано в vl.sent: %d", n)
	}
	// Обірвана проба — більше не дописує.
	now = time.Now()
	q := &legProbe{start: now, end: now.Add(probeDur), bps: 4_000_000, evalUntil: now.Add(time.Second)}
	q.aborted.Store(true)
	vl.setProbe(q)
	for i := 1; i <= 20; i++ {
		ticks <- now.Add(time.Duration(i) * probeTick)
	}
	ticks <- now.Add(probeDur) // останній тик у вікні вже оброблено
	vl.setProbe(nil)
	if q.padBytes.Load() != 0 {
		t.Fatalf("обірвана проба дописала %d", q.padBytes.Load())
	}
}

// Пейсинг увімкнено (як OO_SCREEN_PACE=1): нога доставляє всі пакети.
func TestPaceOnDelivers(t *testing.T) {
	old := paceEnabled
	paceEnabled = true
	t.Cleanup(func() { paceEnabled = old })
	ns := readyNode(t, "pace-on")
	vl := ns.onlyViewer(t)
	forwardN(ns, 50, 10, 0)
	waitSent(t, vl, 50, "pace on")
}
