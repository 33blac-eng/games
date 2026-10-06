package main

import (
	"log"
	"time"
)

// Режим «Відео» (агент -video-mode, internal/contentmode): агент шле
// content_mode {"mode":"video"} і просить більше біт під рух великої площі.
// Hub це ПРОХАННЯ, а не наказ: ціль піднімається лише
//   - не вище стелі ноди (startBps = -bitrate агента), REMB і рівня, на якому
//     востаннє бачили затор (cutFrom);
//   - лише коли мережа зараз чиста (goodSince — серія «чисто» триває);
//   - не частіше videoBoostDebounce і кроком ×videoBoostFactor.
//
// Вихід із «Відео» ціль не знижує: вона вже доведена мережею, а знижує її
// звичайна адаптація (втрати/затримка/REMB). Агент без прапорця нічого не шле,
// і поведінка хаба тотожна попередній.
const (
	videoBoostFactor   = 1.5
	videoBoostDebounce = 3 * time.Second
)

// videoBoost — ЧИСТА: крок підйому цілі для режиму «Відео».
func (c bitrateCtl) videoBoost(now time.Time) (bitrateCtl, bool) {
	if c.startBps == 0 || c.target >= c.startBps || c.goodSince.IsZero() {
		return c, false
	}
	if !c.lastSent.IsZero() && now.Sub(c.lastSent) < videoBoostDebounce {
		return c, false
	}
	limit := c.startBps
	if c.remb > 0 && c.remb < limit {
		limit = c.remb
	}
	if c.cutFrom > 0 && c.cutFrom < limit {
		limit = c.cutFrom
	}
	next := uint64(float64(c.target) * videoBoostFactor)
	if next > limit {
		next = limit
	}
	if next <= c.target {
		return c, false
	}
	c.target, c.lastSent, c.reason = next, now, "video"
	return c, true
}

// onContentMode — content_mode від агента цієї ноди.
func onContentMode(ns *nodeSession, mode string, now time.Time) {
	ns.mu.Lock()
	was := ns.videoMode
	ns.videoMode = mode == "video"
	ns.mu.Unlock()
	if was != (mode == "video") {
		log.Printf("content_mode [node=%s]: %s", ns.nodeID, mode)
	}
	if mode == "video" {
		tryVideoBoost(ns, now)
	}
}

// tryVideoBoost — підйом, якщо нода в «Відео» і videoBoost дозволяє. Кличеться
// на вході в режим і після кожного RR, поки режим триває.
func tryVideoBoost(ns *nodeSession, now time.Time) {
	ns.mu.Lock()
	if !ns.videoMode {
		ns.mu.Unlock()
		return
	}
	if ns.bitrate.startBps == 0 {
		ns.bitrate = newBitrateCtl(ns.ceilingBps())
	}
	prev := ns.bitrate.target
	next, send := ns.bitrate.videoBoost(now)
	ns.bitrate = next
	ns.mu.Unlock()
	if !send {
		return
	}
	log.Printf("content_mode [node=%s]: video boost %d -> %d bps (ceiling %d)", ns.nodeID, prev, next.target, next.startBps)
	sendBitrateTarget(ns, next.target, 0, 0, 0, false)
}
