package main

import (
	"errors"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/internal/contentmode"
	"github.com/organicoils/oo-screen/internal/control"
)

// Режим «Відео» (-video-mode, дефолт вимкнено; internal/contentmode). Один
// автомат Text / Normal / Video замість окремого текстового детектора; з
// вимкненим прапорцем він рівно textmode (NoVideo), і поведінка агента
// тотожна попередній.
//
// З прапорцем:
//   - PTS тікає з кроком 1/videoFPS (videoTickFPS), щоб 60 к/с мали власні
//     мітки; параметр FPS енкодера лишається -fps (UNVERIFIED: як MS MFT CBR
//     розподіляє біти при фактичних 60 к/с проти заявлених 30 — не виміряно);
//   - кадри вмісту йдуть не частіше contentmode.FPS(mode): Normal/Text — -fps,
//     Video — до -video-fps, якщо апаратний енкодер встигає (EWMA кодування),
//     софт — ніколи (swlimit не дає частоти, вищої за -fps);
//   - на зміну режиму (і кожні videoCtlRepeat у Video) агент шле hub
//     content_mode — hub може підняти ціль бітрейту в межах стелі.

// videoCtlRepeat — як часто повторювати content_mode "video", поки режим
// триває: переживає реконект і дає hub нагоду на наступний крок підйому.
const videoCtlRepeat = 5 * time.Second

// videoGapSlack — частка кадрового інтервалу, яку тримає гейт: DXGI віддає
// кадри з джитером, і рівно 1/fps різав би кожен другий кадр 30-к/с контенту.
const videoGapSlack = 0.9

// videoTickFPS — роздільність годинника PTS.
func videoTickFPS(enabled bool, fps, videoFPS int) int {
	if enabled && videoFPS > fps {
		return videoFPS
	}
	return fps
}

// videoModeGap — мінімальний інтервал між кадрами вмісту з -video-mode
// (0 = гейта немає: прапорець вимкнено).
func videoModeGap(enabled bool, mode contentmode.Mode, in contentmode.FPSInput) time.Duration {
	if !enabled {
		return 0
	}
	g := contentmode.Gap(contentmode.FPS(mode, in))
	return time.Duration(float64(g) * videoGapSlack)
}

// encEWMA — EWMA часу кодування (с) для рішення про 60 к/с.
func encEWMA(prev float64, d time.Duration) float64 {
	s := d.Seconds()
	if s < 0 {
		s = 0
	}
	if prev <= 0 {
		return s
	}
	return prev + 0.2*(s-prev)
}

// videoCtlDue — чи слати content_mode зараз.
func videoCtlDue(enabled, flipped bool, mode contentmode.Mode, sinceSent time.Duration) bool {
	if !enabled {
		return false
	}
	return flipped || (mode == contentmode.Video && sinceSent >= videoCtlRepeat)
}

// ctlSender — транспорт, що вміє слати control-повідомлення hub-у.
type ctlSender interface {
	sendCtl(m control.Msg) error
}

var errNoCtl = errors.New("control channel not open")

func (t *webrtcTransport) sendCtl(m control.Msg) error {
	if t.ctl == nil {
		return errNoCtl
	}
	return control.Write(ctlDCWriter{t.ctl}, m)
}

// ctlDCWriter — DataChannel як io.Writer для control.Write.
type ctlDCWriter struct{ dc *webrtc.DataChannel }

func (w ctlDCWriter) Write(p []byte) (int, error) { return len(p), w.dc.Send(p) }

// sendContentMode шле режим, якщо транспорт уміє (WT — ні).
func sendContentMode(tp any, seq uint64, mode contentmode.Mode) error {
	cs, ok := tp.(ctlSender)
	if !ok {
		return errNoCtl
	}
	return cs.sendCtl(control.ContentMode(seq, mode.String()))
}
