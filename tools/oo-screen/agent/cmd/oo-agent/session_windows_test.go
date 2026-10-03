//go:build windows

// Windows-only: these tests use the stream type (agent/capture + agent/encode,
// which do not build on Linux: dxgi.c/mft.c carry no build constraint) and the
// Win32 named mutex behind acquireNamedInstance.
package main

import (
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/control"
)

// Пункт 127.1 — handleCtlMessage НЕ БЛОКУЄ.
//
// Колбеки control-каналу живуть на горутині pion: та сама горутина обробляє
// STUN/DTLS і читає DataChannel. Затримка в ній — це не «повільна реакція», це
// стоячий транспорт. Тому кожен колбек має право лише підняти прапорець
// (s.wantIDR, bitrateWanted, s.pending), а роботу робить кадровий цикл.
//
// Негативний контроль (те, що робить гейт здатним почервоніти): колбек, який
// спить довше за бюджет, валить тест — тобто тест міряє САМЕ час, а не факт
// виклику.
func TestHandleCtlMessageDoesNotBlock(t *testing.T) {
	const budget = 50 * time.Millisecond

	msgs := [][]byte{
		mustCtl(t, control.KeyframeRequest(1)),
		mustCtl(t, control.BitrateTarget(2, 2_000_000)),
		mustCtl(t, control.SelectOutput(3, 0)),
		[]byte("resume"),
		[]byte("pause"),
	}

	var s stream
	var bitrateWanted bitrateTarget

	start := time.Now()
	for _, m := range msgs {
		handleCtlMessage(m,
			func() { s.wantIDR.Store(true) },
			func(bool) {},
			func(want uint64) {
				if bps, ok := clampBitrate(want, 8_000_000); ok {
					bitrateWanted.set(bps, true)
				}
			},
			func(idx int) { s.requestOutput(idx) },
		)
	}
	if el := time.Since(start); el > budget {
		t.Fatalf("handleCtlMessage на %d повідомленнях зайняв %s (бюджет %s) — колбек робить роботу замість прапорця",
			len(msgs), el.Round(time.Millisecond), budget)
	}

	// І ефект таки є: прапорці підняті, значить швидкість не куплена тим, що
	// повідомлення просто впали в нікуди.
	if !s.wantIDR.Load() {
		t.Fatal("keyframe_request не підняв wantIDR")
	}
	if got := bitrateWanted.take(); got != 2_000_000 {
		t.Fatalf("bitrate_target не поклав ціль: take()=%d, чекали 2000000", got)
	}
	if _, ok := s.pending.take(); !ok {
		t.Fatal("select_output не поклав запит у pending")
	}
}

// Пункт 127.2 — A-36: другий екземпляр мусить себе впізнати.
//
// Імʼя тестове, не бойове: інакше живий агент на цій же машині робив би тест
// то зеленим, то червоним (див. acquireNamedInstance).
func TestAcquireSingleInstanceDetectsDuplicate(t *testing.T) {
	name := `Local\oo-screen-agent-test-` + t.Name()

	release, dup := acquireNamedInstance(name)
	if dup {
		t.Fatalf("перший захват уже вважає себе дублікатом (імʼя %s зайняте?)", name)
	}
	if release == nil {
		t.Fatal("перший захват не повернув release")
	}

	release2, dup2 := acquireNamedInstance(name)
	if !dup2 {
		t.Fatal("другий захват НЕ побачив першого — A-36 не працює, два агенти рвали б DXGI один одному")
	}
	release2()

	// Негативний контроль: після звільнення імʼя знову вільне. Без цього тест
	// проходив би й на реалізації, яка завжди каже duplicate=true.
	release()
	release3, dup3 := acquireNamedInstance(name)
	if dup3 {
		t.Fatal("імʼя лишилось зайнятим після release — мʼютекс не звільняється")
	}
	release3()
}
