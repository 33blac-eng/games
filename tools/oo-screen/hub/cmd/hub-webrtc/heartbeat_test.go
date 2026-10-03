package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/internal/control"
)

// TestHubHeartbeatReachesPausedAgent — пульс мусить доходити до агента ТОДІ,
// коли глядача немає.
//
// Саме цей стан і був аварією 30.08: агент на паузі не шле нічого, consent
// freshness у нього не рахується, PeerConnectionState замерзає на "connected",
// і смерті хаба він не бачить узагалі. Пульс — єдина ознака життя, яка існує
// на паузі, тому тест іде рівно тим шляхом, що прод: POST /offer/agent ->
// handleOffer -> setupAgentLeg -> OnDataChannel("oosc-ctl") -> OnOpen.
//
// Жодного очікування тікера: удар робиться явно, а тікер — три рядки stdlib.
func TestHubHeartbeatReachesPausedAgent(t *testing.T) {
	srv := fakeERP(t)
	defer srv.Close()
	prevBase, prevKey, prevReg := erpBase, hubKey, reg
	erpBase, hubKey = srv.URL, "test-key"
	reg = newRegistry()
	defer func() { erpBase, hubKey, reg = prevBase, prevKey, prevReg }()

	got := make(chan []byte, 8)
	_, _, ctl := dialAgentLegWith(t, "nodeA", false)
	ctl.OnMessage(func(m webrtc.DataChannelMessage) { got <- append([]byte(nil), m.Data...) })

	ns := reg.get("nodeA")
	if ns == nil {
		t.Fatal("агентська нога не зареєструвала ноду")
	}
	// Канал мусить доїхати до ns — це і є доказ, що OnOpen відпрацював, тобто
	// що пульс справді заведено (startHubHeartbeat висить на тому ж OnOpen).
	if !waitFor(15*time.Second, func() bool { return ctlChan(ns) != nil }) {
		t.Fatal("control-канал агента так і не став відкритим на боці хаба")
	}
	// Глядача немає й не було: агент на паузі, жодного RTP у тракті.
	if hasReadyViewer(ns) {
		t.Fatal("тест має перевіряти саме ПАУЗУ, а глядач знайшовся")
	}

	hubCtl := ctlChan(ns)
	if !sendHeartbeat(ns, hubCtl) {
		t.Fatal("sendHeartbeat відмовився бити у відкритий канал")
	}
	if m := waitHeartbeat(t, got); m.Type != control.TypeHeartbeat {
		t.Fatalf("до агента приїхало %q, а не heartbeat", m.Type)
	}

	// Ногу цієї ноди витіснив НОВИЙ агент — і в хаба тепер інший
	// control-канал. Стара горутина пульсу мусить завершитись, інакше в одну
	// ноду пульсували б дві, кожна зі свого покоління.
	dialAgentLegWith(t, "nodeA", false)
	if !waitFor(15*time.Second, func() bool {
		cur := ctlChan(ns)
		return cur != nil && cur != hubCtl
	}) {
		t.Fatal("другий агент ноди не перехопив control-канал — заміну перевірити нема на чому")
	}
	if sendHeartbeat(ns, hubCtl) {
		t.Fatal("пульсує в канал, який уже не належить цій нозі")
	}
}

// waitHeartbeat дістає перше повідомлення, яке агент розібрав як control.Msg.
// Гейт ("pause") приходить першим і сюди не рахується — він текстовий.
func waitHeartbeat(t *testing.T, got <-chan []byte) control.Msg {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case raw := <-got:
			if !bytes.HasPrefix(raw, []byte("{")) {
				continue // текстовий "pause"/"resume" старого протоколу
			}
			if raw[len(raw)-1] != '\n' {
				raw = append(raw, '\n')
			}
			m, err := control.Read(bufio.NewReader(bytes.NewReader(raw)))
			if err != nil {
				t.Fatalf("агент не зміг розібрати повідомлення хаба %q: %v", raw, err)
			}
			return m
		case <-deadline:
			t.Fatal("до агента не приїхало жодного control-повідомлення")
			return control.Msg{}
		}
	}
}

// TestSendHeartbeatNoChannel — негативний контроль відправника: немає
// відкритого "oosc-ctl" -> false, а не паніка і не тихий «успіх». Саме за цим
// false горутина пульсу й завершується.
func TestSendHeartbeatNoChannel(t *testing.T) {
	if sendHeartbeat(&nodeSession{nodeID: "n"}, nil) {
		t.Fatal("sendHeartbeat повернув true без control-каналу")
	}
}

// TestSendShutdownNoChannel — те саме для явного «ноги більше немає»: старий
// агент без каналу не має валити хаб, коли той знімає публікатора.
func TestSendShutdownNoChannel(t *testing.T) {
	sendShutdown(&nodeSession{nodeID: "n"}, "publisher lost: failed")
}

// TestPublisherLostTellsAgent — хаб, знімаючи публікатора, мусить сказати про
// це агентові явним shutdown-ом, а не тихо забути ногу.
//
// Живий транспорт до впалого агента не піднімеш, тому перевіряється сам
// ЛАНЦЮГ: shutdown, зібраний хабом, агент розбирає як control.Shutdown — тобто
// як те, за чим його сторож рве з'єднання негайно, без очікування порогу.
func TestPublisherLostTellsAgent(t *testing.T) {
	srv := fakeERP(t)
	defer srv.Close()
	prevBase, prevKey, prevReg := erpBase, hubKey, reg
	erpBase, hubKey = srv.URL, "test-key"
	reg = newRegistry()
	defer func() { erpBase, hubKey, reg = prevBase, prevKey, prevReg }()

	got := make(chan []byte, 8)
	_, _, ctl := dialAgentLegWith(t, "nodeB", false)
	ctl.OnMessage(func(m webrtc.DataChannelMessage) { got <- append([]byte(nil), m.Data...) })

	ns := reg.get("nodeB")
	if ns == nil {
		t.Fatal("агентська нога не зареєструвала ноду")
	}
	if !waitFor(15*time.Second, func() bool { return ctlChan(ns) != nil }) {
		t.Fatal("control-канал агента так і не став відкритим на боці хаба")
	}

	sendShutdown(ns, "publisher lost: "+webrtc.PeerConnectionStateFailed.String())
	m := waitHeartbeat(t, got)
	if m.Type != control.TypeShutdown {
		t.Fatalf("до агента приїхало %q, а не shutdown", m.Type)
	}
	if !strings.Contains(m.Reason, "publisher lost") {
		t.Fatalf("причина %q не називає втрату публікатора — агент не зрозуміє, що сталось", m.Reason)
	}
}

// hasReadyViewer — hasReadyViewerLocked під замком, лише для тестів.
func hasReadyViewer(ns *nodeSession) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return hasReadyViewerLocked(ns)
}
