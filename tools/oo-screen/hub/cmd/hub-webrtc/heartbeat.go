// Пульс хабом у бік агента — і явне «ноги більше немає».
//
// 🔴 Навіщо. Агент з on-demand гейтингом без глядача не кодує і не шле нічого.
// Consent freshness (RFC 7675) у pion рахується від ВЛАСНОЇ активності, тож у
// такого агента таймер не заводиться і PeerConnectionState замерзає на
// "connected" — навіть коли хаба вже нема. Живий випадок 30.08: хаб
// перезапустили о 08:44, а агент вісім хвилин вважав себе підключеним, поки
// хаб давно зняв публікатора. Хвилинне викочування хаба перетворювалось на
// десятки хвилин недоступності транспорту для всього парку.
//
// Тому хаб мусить дати агентові ознаку життя, яка існує І НА ПАУЗІ. Такою є
// рівний пульс у наявний control-канал "oosc-ctl": він іде НЕЗАЛЕЖНО від того,
// чи є глядач, тож тиша в ньому однозначна — на відміну від тиші в медіа- чи
// RTCP-тракті, яка на простої є нормою.
package main

import (
	"log"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/internal/control"
)

// startHubHeartbeat заводить пульс для ЦІЄЇ агентської ноги.
//
// Своя горутина, а не робота в колбеку: усі колбеки pion виконуються на його
// власних горутинах (ICE/SCTP), і затримка в них зупиняє обробку STUN. Тут із
// колбека робиться рівно `go` — без замків і без вводу-виводу.
func startHubHeartbeat(ns *nodeSession, dc *webrtc.DataChannel) {
	go func() {
		t := time.NewTicker(control.HeartbeatInterval)
		defer t.Stop()
		for range t.C {
			if !sendHeartbeat(ns, dc) {
				return
			}
		}
	}()
}

// sendHeartbeat — один удар. false = цей канал більше не наш: він закритий
// (нога впала) або ногу витіснив новий агент тієї ж ноди, і пульс тепер веде
// ЙОГО горутина. Саме тому порівнюємо з конкретним dc, а не просто беремо
// поточний: інакше після заміни агента пульсували б дві горутини.
func sendHeartbeat(ns *nodeSession, dc *webrtc.DataChannel) bool {
	cur := ctlChan(ns)
	if cur == nil || cur != dc {
		return false
	}
	seq := atomic.AddUint64(&ns.ctlSeq, 1)
	if err := control.Write(dcWriter{dc}, control.Heartbeat(seq)); err != nil {
		log.Printf("hub heartbeat [node=%s]: %v", ns.nodeID, err)
		return false
	}
	return true
}

// sendShutdown — явне «ноги більше немає» тим самим control-каналом, ПЕРЕД тим
// як хаб її закриє.
//
// Повідомлення надійніше за сам Close: закриття транспорту, який уже впав,
// агентові нікуди не доїде, а от там, де транспорт ЖИВИЙ і ногу знімає сам хаб,
// агент дізнається про це негайно, а не через поріг тиші. Тихий no-op, поки
// каналу нема — старий агент без "oosc-ctl" лишається на власному сторожі.
//
// leg — нога, про яку йдеться; nil = «уся нода» (хаб іде). Канал береться
// лише якщо він належить саме leg: подія старої ноги, що доживає поруч із
// новою, не має права сказати «закрито» в канал НОВОЇ — агент би послухався
// і зробив зайвий реконект.
func sendShutdown(ns *nodeSession, leg *webrtc.PeerConnection, reason string) {
	// Власника й сам канал — під ОДНИМ локом: інакше між перевіркою і читанням
	// OnDataChannel нової ноги встиг би підмінити agentCtrl, і shutdown старої
	// пішов би в канал нової.
	ns.mu.Lock()
	dc := ns.agentCtrl
	if leg != nil && ns.agentChanPC != leg {
		dc = nil
	}
	ns.mu.Unlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return
	}
	seq := atomic.AddUint64(&ns.ctlSeq, 1)
	if err := control.Write(dcWriter{dc}, control.Shutdown(seq, reason)); err != nil {
		log.Printf("sendShutdown [node=%s]: %v", ns.nodeID, err)
	}
}
