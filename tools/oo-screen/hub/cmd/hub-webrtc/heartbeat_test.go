package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
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
	if !waitFor(func() bool { return ctlChan(ns) != nil }) {
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
	if !waitFor(func() bool {
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
	deadline := time.After(eventGuard)
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
	sendShutdown(&nodeSession{nodeID: "n"}, nil, "publisher lost: failed")
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
	if !waitFor(func() bool { return ctlChan(ns) != nil }) {
		t.Fatal("control-канал агента так і не став відкритим на боці хаба")
	}

	sendShutdown(ns, nil, "publisher lost: "+webrtc.PeerConnectionStateFailed.String())
	m := waitHeartbeat(t, got)
	if m.Type != control.TypeShutdown {
		t.Fatalf("до агента приїхало %q, а не shutdown", m.Type)
	}
	if !strings.Contains(m.Reason, "publisher lost") {
		t.Fatalf("причина %q не називає втрату публікатора — агент не зрозуміє, що сталось", m.Reason)
	}
}

// hasReadyViewerLocked — чи є у ноди ХОЧА Б ОДИН Connected глядач, БЕЗ огляду
// на те, дивиться він зараз чи згорнув вкладку. Це відповідь на питання «чи
// хтось іще тут», а НЕ предикат гейтингу (ним із F-39 став
// hasVisibleViewerLocked). Кликати під ns.mu.
// Лише для тестів: у проді гейт — hasVisibleViewerLocked.
func hasReadyViewerLocked(ns *nodeSession) bool {
	for _, vl := range ns.viewers {
		if vl.ready {
			return true
		}
	}
	return false
}

// hasReadyViewer — hasReadyViewerLocked під замком, лише для тестів.
func hasReadyViewer(ns *nodeSession) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return hasReadyViewerLocked(ns)
}

// TestIdleReconnectKeepsNode — агент без глядача перепідключився: стара нога A
// на хабі ще жива (consent-failed прийде за 25–30 с), нова B Connected, але
// треку не шле (пауза). Хаб мусить одразу закрити A і тримати ноду на B; смерть
// A не має ні знімати publisher-а, ні слати «закрито» в канал B.
// Поверни в Connected-гілку setupAgentLeg «лише коли agentPC == nil» — впаде
// кожна з трьох перевірок нижче.
func TestIdleReconnectKeepsNode(t *testing.T) {
	withTicketMode(t)
	quietNDJSON(t, nil)

	_, _, _ = dialAgentLegWith(t, "nodeR", false)
	ns := reg.get("nodeR")
	if ns == nil || !waitFor(func() bool { return ctlChan(ns) != nil }) {
		t.Fatal("нога A не піднялась")
	}
	ns.mu.Lock()
	legA := ns.agentPC
	ns.mu.Unlock()

	_, _, ctlB := dialAgentLegWith(t, "nodeR", false)
	gotB := agentMsgs(ctlB)
	if !waitFor(func() bool {
		ns.mu.Lock()
		b := ns.agentChanPC != nil && ns.agentChanPC != legA
		ns.mu.Unlock()
		return b && ctlChan(ns) != nil
	}) {
		t.Fatal("канал ноги B не відкрився")
	}

	if !waitFor(func() bool { return legA.ConnectionState() == webrtc.PeerConnectionStateClosed }) {
		t.Errorf("стара нога A лишилась %s після Connected нової — доживе до consent-failed", legA.ConnectionState())
	}
	// Та сама смерть A, що в проді приходить через 25–30 с.
	_ = legA.Close()
	time.Sleep(300 * time.Millisecond)

	if !ns.hasAgent() {
		t.Error("смерть старої ноги зняла publisher-а при живій новій — нода 404")
	}
	quiet := time.After(time.Second)
	for {
		select {
		case m := <-gotB:
			if m.Type == control.TypeShutdown {
				t.Fatalf("у канал НОВОЇ ноги прийшов shutdown від старої: %q", m.Reason)
			}
			continue
		case <-quiet:
		}
		break
	}
}

// TestShutdownOnlyToOwnLeg — «ноги більше немає» від ноги X іде лише в канал
// самої X. Подія старої ноги, що доживає поруч із новою, не має права сказати
// «закрито» в канал НОВОЇ (heartbeat.go, sendShutdown). Зроби перевірку leg
// завжди істинною — shutdown для чужої ноги доїде.
func TestShutdownOnlyToOwnLeg(t *testing.T) {
	withTicketMode(t)
	quietNDJSON(t, nil)
	_, _, ctl := dialAgentLegWith(t, "nodeS", false)
	got := agentMsgs(ctl)
	ns := reg.get("nodeS")
	if ns == nil || !waitFor(func() bool { return ctlChan(ns) != nil }) {
		t.Fatal("control-канал агента не відкрився")
	}
	ns.mu.Lock()
	own := ns.agentChanPC
	ns.mu.Unlock()

	sendShutdown(ns, &webrtc.PeerConnection{}, "publisher lost: failed") // чужа (стара) нога
	quiet := time.After(500 * time.Millisecond)
	for {
		select {
		case m := <-got:
			if m.Type == control.TypeShutdown {
				t.Fatal("shutdown від чужої ноги доїхав у канал живої")
			}
			continue
		case <-quiet:
		}
		break
	}
	sendShutdown(ns, own, "publisher lost: failed")
	if m := waitMsg(t, got, control.TypeShutdown); !strings.Contains(m.Reason, "publisher lost") {
		t.Fatalf("причина %q", m.Reason)
	}
}

// TestShutdownWaitsForInFlightOffer — R5-G6: SIGTERM посеред /offer/*, що чекає
// ICE-gathering. ListenAndServe повертається одразу, а вихід main по ньому рвав
// би запит. serveUntilSignal мусить повернутись лише після того, як запит
// відповів. Прибери <-drained — повернеться до відповіді.
func TestShutdownWaitsForInFlightOffer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	entered, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release // «ICE-gathering»
		_, _ = io.WriteString(w, "answer")
	})}
	sig := make(chan os.Signal, 1)
	notified := make(chan struct{})
	served := make(chan error, 1)
	go func() { served <- serveUntilSignal(srv, sig, func() { close(notified) }) }()

	var resp *http.Response
	got := make(chan error, 1)
	go func() {
		for i := 0; ; i++ { // слухач ще міг не піднятись
			r, err := http.Post("http://"+addr+"/offer/viewer", "application/json", strings.NewReader("{}"))
			if err == nil || i == 50 {
				resp = r
				got <- err
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	select {
	case <-entered:
	case <-time.After(eventGuard):
		t.Fatal("запит не дійшов до обробника")
	}

	sig <- syscall.SIGTERM
	<-notified
	select {
	case err := <-served:
		t.Fatalf("serveUntilSignal повернувся (%v), поки offer ще в дорозі — main вийшов би й обірвав його", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	if err := <-got; err != nil {
		t.Fatalf("offer обірвано: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "answer" {
		t.Fatalf("тіло %q, want answer", body)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveUntilSignal: %v", err)
		}
	case <-time.After(eventGuard):
		t.Fatal("serveUntilSignal не повернувся після дренажу")
	}
}
