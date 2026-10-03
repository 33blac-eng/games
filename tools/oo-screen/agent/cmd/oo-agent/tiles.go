// Текстові тайли на боці агента (bench/quality/STAGE3-444.md, варіант B):
// коли refine нерухомого екрана завершився, агент один раз читає BGRA
// робочого столу, вибирає тайли з кольоровим текстом (internal/tiles) і шле
// їх lossless PNG каналом "oosc-tiles". Будь-який змінений кадр — invalidate
// ДО того, як цей кадр піде в енкодер.
//
// 🔴 ПРАПОРЕЦЬ -text-tiles, ТИПОВО ВИМКНЕНО: без нього каналу немає в SDP,
// readback не робиться, і агент поводиться бітово як до цього файла.
//
// Без build-тегів: уся логіка, окрім readback-у (capture.ReadBGRA, Windows),
// тестується на Linux.
package main

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/organicoils/oo-screen/internal/tiles"
	"github.com/pion/webrtc/v4"
	"golang.org/x/time/rate"
)

// textTilesEnabled — прапорець -text-tiles.
var textTilesEnabled bool

const (
	// tilesRateBytes — стеля швидкості тайлів (500 КБ/с ≈ 4 Мбіт/с): 2 МБ
	// епізод іде ~4 с і не душить відео на типовому каналі.
	tilesRateBytes = 500 * 1024
	// tilesMaxBuffered — понад стільки в SCTP-буфері каналу чекаємо.
	tilesMaxBuffered = 512 * 1024
)

var (
	// tilesDC — відкритий канал тайлів поточної сесії (nil — немає).
	tilesDC atomic.Pointer[webrtc.DataChannel]
	// tileEps — епоха/епізоди; одна на процес, щоб епоха росла й через реконекти.
	tileEps = &tiles.Episodes{MinInterval: tiles.DefaultMinInterval}
	// tilesBusy — епізод зараз будується (не більше одного одночасно).
	tilesBusy atomic.Bool
	// tilesLastMotion — коли кадровий цикл востаннє бачив змінений кадр
	// (зокрема перший GDI-кадр нерухомого екрана). Нуль — кадру ще не було.
	// Лише кадровий цикл.
	tilesLastMotion time.Time
)

// tilesIdleAfter — скільки екран має стояти, щоб на шляху БЕЗ refine
// (софт-енкодер, -refine=false) почати епізод. Тригер перевіряється на
// still-кадрі (keepalive раз на keepaliveAfter), тож фактична затримка —
// перший keepalive після цього порогу.
const tilesIdleAfter = 500 * time.Millisecond

// tilesReady — чи пора починати епізод на цьому still-кадрі.
// refineGated: refine на цьому шляху живий (апаратний енкодер, -refine) —
// тоді чекаємо його завершення, щоб тайли лягли на вже дошліфоване відео.
// Інакше — власний таймер простою від tilesLastMotion. «Раз на епоху»
// забезпечує tiles.Episodes.Start.
func tilesReady(now time.Time, refineGated, refineComplete bool) bool {
	if !textTilesEnabled || tilesLastMotion.IsZero() {
		return false
	}
	if refineGated {
		return refineComplete
	}
	return now.Sub(tilesLastMotion) >= tilesIdleAfter
}

// tilesCursorExclude — зона вказівника на момент readback-у: курсор
// скомпоновано в BGRA, і тайл під ним заморозив би старе зображення
// стрілки поверх живого відео.
func tilesCursorExclude(visible bool, x, y int) []tiles.Rect {
	if !visible {
		return nil
	}
	return []tiles.Rect{tiles.CursorExclude(x, y)}
}

// tilesStill — зараз піде still-повтор (keepalive) поточної картинки. Якщо
// в цій епосі вже почато епізод, ДО кадру шлемо анонс tiles.TypeStill:
// плеєр зараховує його як кредит і ховає тайли на кадрі без кредиту
// (змінений кадр, що обігнав свій invalidate).
func tilesStill() {
	if !textTilesEnabled {
		return
	}
	dc := tilesDC.Load()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	if msg := tileEps.Still(); msg != nil {
		_ = dc.Send(msg)
	}
}

// tileChannel is what the sender needs from a DataChannel (fake in tests).
type tileChannel interface {
	Send([]byte) error
	BufferedAmount() uint64
	ReadyState() webrtc.DataChannelState
}

// addTilesChannel створює канал тайлів у pc (до offer-а). Лише під прапорцем.
func addTilesChannel(pc *webrtc.PeerConnection) error {
	if !textTilesEnabled {
		return nil
	}
	dc, err := pc.CreateDataChannel(tiles.ChannelLabel, nil)
	if err != nil {
		return err
	}
	dc.OnOpen(func() {
		tilesDC.Store(dc)
		// Нова сесія: усе, що летіло старим каналом, недійсне; глядачі
		// мають отримати тайли нерухомого екрана без нового руху.
		_ = dc.Send(tileEps.Reset())
		log.Printf("oo-agent: text tiles channel open")
	})
	dc.OnClose(func() { tilesDC.CompareAndSwap(dc, nil) })
	return nil
}

// tilesMotion — змінений кадр. Шле invalidate, якщо поточна епоха щось
// почала. Кличеться з кадрового циклу ДО кодування кадру.
func tilesMotion() {
	if !textTilesEnabled {
		return
	}
	tilesLastMotion = time.Now()
	inv := tileEps.Motion()
	if inv == nil {
		return
	}
	if dc := tilesDC.Load(); dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
		if err := dc.Send(inv); err != nil {
			log.Printf("oo-agent: tiles invalidate: %v", err)
		}
	}
}

// tilesStatic — екран нерухомий (tilesReady). read — readback BGRA
// (кличеться синхронно, з кадрового циклу); кодування й відправка — у
// горутині. exclude — зони без тайлів (вказівник, tilesCursorExclude).
func tilesStatic(ctx context.Context, now time.Time, read func() ([]byte, int, int, error), exclude []tiles.Rect) {
	if !textTilesEnabled || tilesBusy.Load() {
		return
	}
	dc := tilesDC.Load()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	epoch, frame, ok := tileEps.Start(now)
	if !ok {
		return
	}
	t0 := time.Now()
	pix, w, h, err := read()
	if err != nil {
		log.Printf("oo-agent: tiles readback: %v", err)
		return
	}
	readDur := time.Since(t0)
	tilesBusy.Store(true)
	go func() {
		defer tilesBusy.Store(false)
		img := tiles.Image{Pix: pix, Stride: w * 4, W: w, H: h}
		lim := rate.NewLimiter(tilesRateBytes, tiles.MaxMessage)
		t1 := time.Now()
		st := sendEpisode(ctx, dc, img, epoch, frame, tiles.SelectConfig{Exclude: exclude},
			tiles.DefaultEpisodeBytes, lim, tileEps.Current)
		log.Printf("oo-agent: text tiles epoch=%d: %d/%d tiles, %d B, capped=%v aborted=%v (readback %v, build+send %v)",
			epoch, st.Sent, st.Selected, st.Bytes, st.Capped, st.Aborted,
			readDur.Round(time.Millisecond), time.Since(t1).Round(time.Millisecond))
	}()
}

// sendEpisode будує й шле один епізод з дотриманням бюджету, швидкості й
// буфера каналу; обривається, щойно епоха перестала бути поточною.
func sendEpisode(ctx context.Context, dc tileChannel, img tiles.Image, epoch, frame uint32,
	cfg tiles.SelectConfig, budget int, lim *rate.Limiter, current func(uint32) bool) tiles.Stats {
	return tiles.Build(img, epoch, frame, cfg, budget, func(msg []byte) bool {
		for {
			if !current(epoch) || dc.ReadyState() != webrtc.DataChannelStateOpen || ctx.Err() != nil {
				return false
			}
			if dc.BufferedAmount() <= tilesMaxBuffered {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if lim != nil {
			if err := lim.WaitN(ctx, len(msg)); err != nil {
				return false
			}
		}
		if !current(epoch) { // епоха могла змінитись, поки чекали
			return false
		}
		return dc.Send(msg) == nil
	})
}
