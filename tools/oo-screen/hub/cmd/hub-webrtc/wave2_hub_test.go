// Гейти контрактів C1/C2/C3 (D:\Claude\tmp\oo-screen-fix\CONTRACT.md) на боці
// хаба. Кожен уміє почервоніти: під кожним — що зняти, щоб він упав.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/internal/control"
)

// ── C1: «Якість» / «Швидкість» ─────────────────────────────────────────────

// agentMsgs збирає control-повідомлення, що доїхали до агента, за типом.
func agentMsgs(ctl *webrtc.DataChannel) <-chan control.Msg {
	out := make(chan control.Msg, 64)
	ctl.OnMessage(func(m webrtc.DataChannelMessage) {
		if msg, err := control.Read(bufioLine(m.Data)); err == nil {
			out <- msg
		}
	})
	return out
}

// bufioLine — DataChannel-кадр як рядок для control.Read (термінатор хаб і так
// ставить; текстові "pause"/"resume" просто не розберуться).
func bufioLine(b []byte) *bufio.Reader {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		b = append(append([]byte(nil), b...), '\n')
	}
	return bufio.NewReader(bytes.NewReader(b))
}

func waitMsg(t *testing.T, ch <-chan control.Msg, typ string) control.Msg {
	t.Helper()
	deadline := time.After(eventGuard)
	for {
		select {
		case m := <-ch:
			if m.Type == typ {
				return m
			}
		case <-deadline:
			t.Fatalf("до агента не доїхало %q", typ)
			return control.Msg{}
		}
	}
}

func postControlRaw(body string) (int, controlResp) {
	r := httptest.NewRequest(http.MethodPost, "/control", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleControl(w, r)
	var resp controlResp
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	return w.Code, resp
}

// TestControlQualityAndSpeed — повний шлях C1: POST /control -> стеля
// контролера ноди + max_fps агенту по живому "oosc-ctl". Зніми setBitrateCap у
// handleControl — впаде стеля; зніми sendMaxFps — не доїде max_fps.
func TestControlQualityAndSpeed(t *testing.T) {
	withTicketMode(t)
	quietNDJSON(t, nil)
	_, _, ctl := dialAgentLegWith(t, "nodeA", false)
	got := agentMsgs(ctl)
	ns := reg.get("nodeA")
	if ns == nil || !waitFor(func() bool { return ctlChan(ns) != nil }) {
		t.Fatal("control-канал агента не відкрився")
	}
	ns.mu.Lock()
	agentCeil := ns.ceilingBps()
	ns.mu.Unlock()

	// Стелі тулбара належать глядачам: без жодного глядача стеля нічия і не
	// має дочекатись наступного (його resetBitrate стартував би з чужої).
	if _, resp := postControlRaw(`{"ticket":"t-nodeA","max_bitrate_bps":3000000}`); resp.MaxBitrateBps != nil {
		t.Fatalf("стеля без глядачів лишилась: %+v", resp)
	}
	ns.mu.Lock()
	orphanCap := ns.capBps
	ns.mu.Unlock()
	if orphanCap != 0 {
		t.Fatalf("capBps=%d без глядачів, want 0", orphanCap)
	}

	vl := addViewer(ns, newPC(t), newViewerTrack(t), "u1")
	t.Cleanup(func() { removeViewer(ns, vl) })

	code, resp := postControlRaw(`{"ticket":"t-nodeA","max_bitrate_bps":3000000,"max_fps":15}`)
	if code != http.StatusOK {
		t.Fatalf("POST /control: %d", code)
	}
	if resp.MaxBitrateBps == nil || *resp.MaxBitrateBps != 3_000_000 || resp.MaxFps == nil || *resp.MaxFps != 15 {
		t.Fatalf("відповідь не несе застосованих стель: %+v", resp)
	}
	ns.mu.Lock()
	target, start := ns.bitrate.target, ns.bitrate.startBps
	ns.mu.Unlock()
	if target != 3_000_000 || start != 3_000_000 {
		t.Fatalf("контролер: target=%d start=%d, want 3000000/3000000", target, start)
	}
	if m := waitMsg(t, got, control.TypeMaxFps); m.Fps != 15 {
		t.Fatalf("max_fps агенту = %d, want 15", m.Fps)
	}

	// Стеля тримає підйом: сотня чистих RR не перелізе 3 Мбіт/с.
	now := time.Now()
	for i := 0; i < 100; i++ {
		onReceiverReport(ns, 0, 0, 0, now.Add(time.Duration(i)*11*time.Second))
	}
	ns.mu.Lock()
	target = ns.bitrate.target
	ns.mu.Unlock()
	if target > 3_000_000 {
		t.Fatalf("ціль %d перелізла стелю тулбара", target)
	}

	// Поля немає — нічого не змінюється (вибір монітора не скидає якість).
	if _, resp = postControlRaw(`{"ticket":"t-nodeA"}`); resp.MaxBitrateBps == nil || *resp.MaxBitrateBps != 3_000_000 || resp.MaxFps == nil {
		t.Fatalf("читання без полів зняло стелі: %+v", resp)
	}

	// Кламп: нижче підлоги -> minBitrateBps, вище агента -> стеля агента, fps -> 60.
	if _, resp = postControlRaw(`{"ticket":"t-nodeA","max_bitrate_bps":100}`); *resp.MaxBitrateBps != minBitrateBps {
		t.Fatalf("кламп знизу: %d, want %d", *resp.MaxBitrateBps, minBitrateBps)
	}
	if _, resp = postControlRaw(`{"ticket":"t-nodeA","max_bitrate_bps":99000000,"max_fps":500}`); *resp.MaxBitrateBps != agentCeil || *resp.MaxFps != maxFpsCeil {
		t.Fatalf("кламп згори: %d/%d, want %d/%d", *resp.MaxBitrateBps, *resp.MaxFps, agentCeil, maxFpsCeil)
	}

	// null -> стелі зняті, агент отримує «без стелі» (60).
	code, resp = postControlRaw(`{"ticket":"t-nodeA","max_bitrate_bps":null,"max_fps":null}`)
	if code != http.StatusOK || resp.MaxBitrateBps != nil || resp.MaxFps != nil {
		t.Fatalf("null не зняв стелі: %d %+v", code, resp)
	}
	ns.mu.Lock()
	start = ns.bitrate.startBps
	ns.mu.Unlock()
	if start != agentCeil {
		t.Fatalf("після null стеля контролера %d, want %d (агента)", start, agentCeil)
	}

	// Сміття -> 400, нічого не застосовано.
	for _, b := range []string{`{"ticket":"t-nodeA","max_bitrate_bps":"x"}`, `{"ticket":"t-nodeA","max_fps":0}`, `{"ticket":"t-nodeA","max_bitrate_bps":-5}`} {
		if code, _ := postControlRaw(b); code != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400", b, code)
		}
	}
	// Монітор із битим полем поруч не перемикається: 400 = «нічого не
	// застосовано» має бути правдою. Застосуй output до валідації — впаде.
	ns.mu.Lock()
	before := ns.activeOutput
	ns.mu.Unlock()
	if code, _ := postControlRaw(`{"ticket":"t-nodeA","output":1,"max_fps":0}`); code != http.StatusBadRequest {
		t.Fatalf("output+битий max_fps -> %d, want 400", code)
	}
	ns.mu.Lock()
	after := ns.activeOutput
	ns.mu.Unlock()
	if after != before {
		t.Fatalf("400, а монітор перемкнено: %d -> %d", before, after)
	}
	quiet := time.After(300 * time.Millisecond)
	for {
		select {
		case m := <-got:
			if m.Type == control.TypeSelectOutput {
				t.Fatal("400, а агент отримав select_output")
			}
			continue
		case <-quiet:
		}
		break
	}
}

// TestBitrateCapClearedWhenLastViewerLeaves — стеля належить глядачам: пішов
// останній — наступний починає зі стелі агента. Зніми clearViewerCapsLocked у
// removeViewer — і стеля переживе сесію.
func TestBitrateCapClearedWhenLastViewerLeaves(t *testing.T) {
	quietNDJSON(t, nil)
	ns := &nodeSession{nodeID: "cap", startBps: 8_000_000}
	a, b := quietViewer(ns), quietViewer(ns)

	setBitrateCap(ns, 2_000_000)
	ns.mu.Lock()
	ns.maxFps = 10
	ns.mu.Unlock()

	removeViewer(ns, a)
	ns.mu.Lock()
	still := ns.capBps == 2_000_000 && ns.ceilingBps() == 2_000_000 && ns.maxFps == 10
	ns.mu.Unlock()
	if !still {
		t.Fatal("стелю знято, хоч глядач ще лишився")
	}

	removeViewer(ns, b)
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if ns.capBps != 0 || ns.maxFps != 0 || ns.ceilingBps() != 8_000_000 || ns.bitrate.startBps != 8_000_000 {
		t.Fatalf("після останнього глядача: cap=%d fps=%d ceil=%d ctlStart=%d",
			ns.capBps, ns.maxFps, ns.ceilingBps(), ns.bitrate.startBps)
	}
}

// TestCapAdaptationDoesNotOutliveViewer — ціль, набута ПІД стелею «Якості»
// глядача A (RR-крок у останні 30 с), не є вивченою адаптацією шляху для B.
// Прибери скидання контролера в clearViewerCapsLocked — B отримає 1,4M, яких
// не обирав, і повзтиме вгору хвилинами.
func TestCapAdaptationDoesNotOutliveViewer(t *testing.T) {
	quietNDJSON(t, nil)
	ns := &nodeSession{nodeID: "cap-learn", startBps: 8_000_000}
	a := quietViewer(ns)
	setBitrateCap(ns, 1_500_000)
	ns.mu.Lock()
	ns.bitrate.target = 1_400_000 // RR зі втратами зсунув ціль під стелею
	ns.bitrate.lastSent = time.Now()
	ns.mu.Unlock()

	removeViewer(ns, a)
	quietViewer(ns)
	resetBitrate(ns)

	ns.mu.Lock()
	defer ns.mu.Unlock()
	if ns.bitrate.target != 8_000_000 {
		t.Fatalf("новий глядач стартує з %d, want 8000000 (стеля агента)", ns.bitrate.target)
	}
}

// TestNullQualityKeepsLearnedTarget — R5-G6: тулбар шле max_bitrate_bps:null на
// типовій «Якості» (рівень 5) з кожною зміною «Швидкості» і на повторі після
// реконекту. Стелі нема — знімати нічого, а вивчена ціль контролера (затор,
// 2 Мбіт/с) мусить лишитись. Прибери умову !(bpsNull && !hadCap) у
// handleControl — ціль злетить на стелю агента.
func TestNullQualityKeepsLearnedTarget(t *testing.T) {
	withTicketMode(t)
	quietNDJSON(t, nil)
	ns := nodeWithAgent("nodeA", []outputInfo{{Index: 0, Width: 1920, Height: 1080, Primary: true}}, 0)
	setStartBitrate(ns, 8_000_000)
	quietViewer(ns)
	ns.mu.Lock()
	ns.bitrate.target = 2_000_000
	ns.bitrate.lastSent = time.Now()
	ns.mu.Unlock()

	code, resp := postControlRaw(`{"ticket":"t-nodeA","max_bitrate_bps":null,"max_fps":null}`)
	if code != http.StatusOK || resp.MaxBitrateBps != nil {
		t.Fatalf("POST /control: %d %+v", code, resp)
	}
	ns.mu.Lock()
	target := ns.bitrate.target
	ns.mu.Unlock()
	if target != 2_000_000 {
		t.Fatalf("null без стелі: ціль %d, want 2000000 (вивчена адаптація)", target)
	}
}

// TestRepeatedQualityKeepsLearnedTarget — R6-G6: родич null-випадку з числом.
// Людина обрала «Якість» 5 Мбіт/с, затор зрізав ціль до 2 Мбіт/с, потім вона
// міняє лише «Швидкість» — а тулбар (ooToolbarLimits) щоразу повторює поточну
// стелю числом, як і live-повтор після реконекту. Та сама стеля — не новий
// вибір: ціль мусить лишитись 2 Мбіт/с. Нова стеля — так, стрибок одразу.
// Прибери порівняння з ns.capBps у handleControl — ціль злетить на 5 Мбіт/с.
func TestRepeatedQualityKeepsLearnedTarget(t *testing.T) {
	withTicketMode(t)
	quietNDJSON(t, nil)
	_, _, ctl := dialAgentLegWith(t, "nodeA", false)
	got := agentMsgs(ctl)
	ns := reg.get("nodeA")
	if ns == nil || !waitFor(func() bool { return ctlChan(ns) != nil }) {
		t.Fatal("control-канал агента не відкрився")
	}
	setStartBitrate(ns, 8_000_000)
	vl := addViewer(ns, newPC(t), newViewerTrack(t), "u1")
	t.Cleanup(func() { removeViewer(ns, vl) })

	if code, _ := postControlRaw(`{"ticket":"t-nodeA","max_bitrate_bps":5000000,"max_fps":30}`); code != http.StatusOK {
		t.Fatalf("POST /control: %d", code)
	}
	waitMsg(t, got, control.TypeMaxFps)
	ns.mu.Lock()
	ns.bitrate.target = 2_000_000 // затор: контролер з'їхав нижче стелі
	ns.mu.Unlock()

	// Змінено лише «Швидкість»; «Якість» їде та сама.
	if code, _ := postControlRaw(`{"ticket":"t-nodeA","max_bitrate_bps":5000000,"max_fps":15}`); code != http.StatusOK {
		t.Fatalf("POST /control: %d", code)
	}
	if m := waitMsg(t, got, control.TypeMaxFps); m.Fps != 15 {
		t.Fatalf("max_fps агенту = %d, want 15", m.Fps)
	}
	ns.mu.Lock()
	target := ns.bitrate.target
	ns.mu.Unlock()
	if target != 2_000_000 {
		t.Fatalf("та сама стеля 5M повторно: ціль %d, want 2000000 (вивчена адаптація)", target)
	}

	// Інша стеля — це вибір людини: ціль стрибає на неї одразу.
	if code, _ := postControlRaw(`{"ticket":"t-nodeA","max_bitrate_bps":4000000}`); code != http.StatusOK {
		t.Fatalf("POST /control: %d", code)
	}
	ns.mu.Lock()
	target = ns.bitrate.target
	ns.mu.Unlock()
	if target != 4_000_000 {
		t.Fatalf("нова стеля 4M: ціль %d, want 4000000", target)
	}
}

// ── C2: oosc-input-move ─────────────────────────────────────────────────────

// TestMoveChannelOnlyMouseMove — чиста половина C2. Зніми перевірку типу в
// judgeViewerInput — кнопка пройде move-каналом.
func TestMoveChannelOnlyMouseMove(t *testing.T) {
	now := time.Now()
	button := map[string]any{"v": 1, "type": "mouse_button", "button": 0, "down": true}

	if v, _, why := judgeViewerInput(inputMoveChannelLabel, msg(t, testTicket, move(0.1, 0.2)), testTicket, grantControl, freshLimiter(), now); v != inputAccept {
		t.Fatalf("mouse_move у move-каналі: %v %s", v, why)
	}
	if v, _, _ := judgeViewerInput(inputMoveChannelLabel, msg(t, testTicket, button), testTicket, grantControl, freshLimiter(), now); v != inputDrop {
		t.Fatalf("кнопка в move-каналі: вердикт %v, want drop", v)
	}
	// Звичайний канал кнопки пропускає, як і раніше.
	if v, _, _ := judgeViewerInput(inputChannelLabel, msg(t, testTicket, button), testTicket, grantControl, freshLimiter(), now); v != inputAccept {
		t.Fatalf("кнопка в oosc-input: вердикт %v, want accept", v)
	}
	// Засувки ті самі: чужий тікет у move-каналі рве сесію.
	if v, _, _ := judgeViewerInput(inputMoveChannelLabel, msg(t, "чужий", move(0, 0)), testTicket, grantControl, freshLimiter(), now); v != inputKill {
		t.Fatalf("чужий тікет у move-каналі: %v, want kill", v)
	}
}

// pairUp зʼєднує два PeerConnection у процесі. setup виконується на offerer-і
// ДО offer-а (канали, створені пізніше, вимагали б ренегоціації).
func pairUp(t *testing.T, setup func(offerer *webrtc.PeerConnection), answerer *webrtc.PeerConnection) *webrtc.PeerConnection {
	t.Helper()
	offerer, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = offerer.Close() })
	setup(offerer)
	offer, err := offerer.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	g1 := webrtc.GatheringCompletePromise(offerer)
	if err := offerer.SetLocalDescription(offer); err != nil {
		t.Fatalf("offerer SetLocalDescription: %v", err)
	}
	<-g1
	if err := answerer.SetRemoteDescription(*offerer.LocalDescription()); err != nil {
		t.Fatalf("answerer SetRemoteDescription: %v", err)
	}
	ans, err := answerer.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	g2 := webrtc.GatheringCompletePromise(answerer)
	if err := answerer.SetLocalDescription(ans); err != nil {
		t.Fatalf("answerer SetLocalDescription: %v", err)
	}
	<-g2
	if err := offerer.SetRemoteDescription(*answerer.LocalDescription()); err != nil {
		t.Fatalf("offerer SetRemoteDescription: %v", err)
	}
	return offerer
}

// TestMoveChannelReachesAgent — жива половина C2: браузер відкриває
// "oosc-input-move" як у контракті ({ordered:false, maxRetransmits:0}), хаб
// приймає його і пересилає рух агентові; кнопку з того ж каналу — ні. Зніми
// мітку inputMoveChannelLabel з OnDataChannel в attachViewerInput — рух не
// доїде.
func TestMoveChannelReachesAgent(t *testing.T) {
	ns := &nodeSession{nodeID: "move"}
	vl := quietViewer(ns)

	// Агентська половина: агент відкриває "oosc-input", хаб кладе його в ns.
	hubAgentPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hubAgentPC.Close() })
	hubAgentPC.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			ns.mu.Lock()
			ns.agentInput = dc
			ns.mu.Unlock()
		})
	})
	atAgent := make(chan []byte, 8)
	pairUp(t, func(agent *webrtc.PeerConnection) {
		dc, err := agent.CreateDataChannel(inputChannelLabel, nil)
		if err != nil {
			t.Fatal(err)
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) { atAgent <- append([]byte(nil), m.Data...) })
	}, hubAgentPC)

	// Глядацька половина.
	hubViewerPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hubViewerPC.Close() })
	attachViewerInput(ns, vl, hubViewerPC, testTicket, grantControl)
	var moveDC *webrtc.DataChannel
	opened := make(chan struct{})
	ordered, zero := false, uint16(0)
	pairUp(t, func(browser *webrtc.PeerConnection) {
		moveDC, err = browser.CreateDataChannel(inputMoveChannelLabel, &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &zero})
		if err != nil {
			t.Fatal(err)
		}
		moveDC.OnOpen(func() { close(opened) })
	}, hubViewerPC)

	select {
	case <-opened:
	case <-time.After(eventGuard):
		t.Fatal("move-канал не відкрився")
	}
	if !waitFor(func() bool {
		ns.mu.Lock()
		defer ns.mu.Unlock()
		return ns.agentInput != nil
	}) {
		t.Fatal("канал вводу агента не відкрився")
	}

	button := map[string]any{"v": 1, "type": "mouse_button", "button": 0, "down": true}
	_ = moveDC.Send(msg(t, testTicket, button)) // має бути відкинута
	_ = moveDC.Send(msg(t, testTicket, move(0.25, 0.75)))

	select {
	case got := <-atAgent:
		if !bytes.Contains(got, []byte(`"mouse_move"`)) {
			t.Fatalf("до агента першим доїхало не mouse_move: %s", got)
		}
		if bytes.Contains(got, []byte("ticket")) {
			t.Fatalf("тікет протік до агента: %s", got)
		}
	case <-time.After(eventGuard):
		t.Fatal("рух із oosc-input-move не доїхав до агента")
	}
	select {
	case extra := <-atAgent:
		t.Fatalf("з move-каналу до агента доїхало зайве: %s", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestViewerLeaveReleasesAgentInput — глядач із керуванням зник (обрив,
// стеля сесії, закрита вкладка): агентові мусить піти release_all, інакше
// затиснута ним кнопка лишиться затиснутою на чужому ПК. Прибери горутину на
// vl.done в attachViewerInput — release_all не доїде.
func TestViewerLeaveReleasesAgentInput(t *testing.T) {
	ns := &nodeSession{nodeID: "release"}
	vl := quietViewer(ns)

	hubAgentPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hubAgentPC.Close() })
	hubAgentPC.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			ns.mu.Lock()
			ns.agentInput = dc
			ns.mu.Unlock()
		})
	})
	atAgent := make(chan []byte, 8)
	pairUp(t, func(agent *webrtc.PeerConnection) {
		dc, err := agent.CreateDataChannel(inputChannelLabel, nil)
		if err != nil {
			t.Fatal(err)
		}
		dc.OnMessage(func(m webrtc.DataChannelMessage) { atAgent <- append([]byte(nil), m.Data...) })
	}, hubAgentPC)

	hubViewerPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hubViewerPC.Close() })
	attachViewerInput(ns, vl, hubViewerPC, testTicket, grantControl)
	var in *webrtc.DataChannel
	opened := make(chan struct{})
	pairUp(t, func(browser *webrtc.PeerConnection) {
		in, err = browser.CreateDataChannel(inputChannelLabel, nil)
		if err != nil {
			t.Fatal(err)
		}
		in.OnOpen(func() { close(opened) })
	}, hubViewerPC)
	select {
	case <-opened:
	case <-time.After(eventGuard):
		t.Fatal("канал вводу глядача не відкрився")
	}
	if !waitFor(func() bool {
		ns.mu.Lock()
		defer ns.mu.Unlock()
		return ns.agentInput != nil
	}) {
		t.Fatal("канал вводу агента не відкрився")
	}

	down := map[string]any{"v": 1, "type": "mouse_button", "button": "left", "down": true}
	_ = in.Send(msg(t, testTicket, down))
	select {
	case <-atAgent:
	case <-time.After(eventGuard):
		t.Fatal("натискання не доїхало до агента")
	}

	removeViewer(ns, vl) // глядач зник, key-up не буде
	select {
	case got := <-atAgent:
		if !bytes.Contains(got, []byte(`"release_all"`)) {
			t.Fatalf("після виходу глядача агенту доїхало %s, want release_all", got)
		}
	case <-time.After(eventGuard):
		t.Fatal("глядач пішов із затиснутою кнопкою, а release_all агенту не пішов")
	}
}

// ── C3: ICE restart ─────────────────────────────────────────────────────────

// TestViewerICERestart — /offer/viewer віддає leg (32 hex), а
// /offer/viewer/restart переукладає ICE на ТІЙ САМІЙ PeerConnection. Зніми
// маршрут чи поле Leg — тест не збереться або впаде.
func TestViewerICERestart(t *testing.T) {
	withAudioFlag(t, false) // свіжий реєстр; T1 static-token режим
	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	if _, err := remote.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	offerAndPost := func(opts *webrtc.OfferOptions, path string, h http.HandlerFunc, body func(sdp string) any) answerResp {
		t.Helper()
		offer, err := remote.CreateOffer(opts)
		if err != nil {
			t.Fatalf("CreateOffer: %v", err)
		}
		g := webrtc.GatheringCompletePromise(remote)
		if err := remote.SetLocalDescription(offer); err != nil {
			t.Fatalf("SetLocalDescription: %v", err)
		}
		<-g
		raw, _ := json.Marshal(body(remote.LocalDescription().SDP))
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s: %d %s", path, w.Code, w.Body.String())
		}
		var ans answerResp
		if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil {
			t.Fatalf("answer json: %v", err)
		}
		if ans.Type != "answer" {
			t.Fatalf("%s: type=%q, want answer", path, ans.Type)
		}
		if err := remote.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
			t.Fatalf("SetRemoteDescription: %v", err)
		}
		return ans
	}

	first := offerAndPost(nil, "/offer/viewer", handleOffer("viewer"), func(sdp string) any {
		return offerReq{SDP: sdp, Token: token}
	})
	if len(first.Leg) != 32 || strings.Trim(first.Leg, "0123456789abcdef") != "" {
		t.Fatalf("leg %q — не 32 hex", first.Leg)
	}
	ns, vl := findViewerBySession(first.Leg)
	if ns == nil {
		t.Fatal("leg не веде до живої ноги")
	}
	pcBefore := vl.pc

	second := offerAndPost(&webrtc.OfferOptions{ICERestart: true}, "/offer/viewer/restart", handleViewerRestart, func(sdp string) any {
		return restartReq{Leg: first.Leg, SDP: sdp, Type: "offer"}
	})
	if iceUfrag(second.SDP) == "" || iceUfrag(second.SDP) == iceUfrag(first.SDP) {
		t.Fatalf("ICE restart не змінив ufrag хаба: %q -> %q", iceUfrag(first.SDP), iceUfrag(second.SDP))
	}
	ns.mu.Lock()
	n := len(ns.viewers)
	ns.mu.Unlock()
	if _, vl2 := findViewerBySession(first.Leg); vl2 == nil || vl2.pc != pcBefore || n != 1 {
		t.Fatalf("restart мусить лишити ту саму ногу й той самий PC (ніг: %d)", n)
	}

	// Нога вмерла — leg більше нічого не відчиняє.
	removeViewer(ns, vl)
	raw, _ := json.Marshal(restartReq{Leg: first.Leg, SDP: "v=0", Type: "offer"})
	w := httptest.NewRecorder()
	handleViewerRestart(w, httptest.NewRequest(http.MethodPost, "/offer/viewer/restart", bytes.NewReader(raw)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("мертвий leg: %d, want 404", w.Code)
	}
}

// TestViewerRestartRejects — невідомий leg 404, не POST 405, OPTIONS 204 з CORS.
func TestViewerRestartRejects(t *testing.T) {
	cases := []struct {
		method, body string
		want         int
	}{
		{http.MethodPost, `{"leg":"deadbeefdeadbeefdeadbeefdeadbeef","sdp":"v=0","type":"offer"}`, http.StatusNotFound},
		{http.MethodPost, `{"sdp":"v=0","type":"offer"}`, http.StatusNotFound},
		{http.MethodPost, `{"leg":"x","sdp":"v=0","type":"answer"}`, http.StatusBadRequest},
		{http.MethodGet, ``, http.StatusMethodNotAllowed},
		{http.MethodOptions, ``, http.StatusNoContent},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		handleViewerRestart(w, httptest.NewRequest(c.method, "/offer/viewer/restart", strings.NewReader(c.body)))
		if w.Code != c.want {
			t.Errorf("%s %s -> %d, want %d", c.method, c.body, w.Code, c.want)
		}
		if w.Header().Get("Access-Control-Allow-Origin") == "" {
			t.Errorf("%s: без CORS-заголовка", c.method)
		}
	}
}

func iceUfrag(sdp string) string {
	for _, l := range strings.Split(sdp, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "a=ice-ufrag:"); ok {
			return v
		}
	}
	return ""
}

// TestControlMaxFpsWithoutAgentChannel — стеля fps без каналу агента = 409 (як
// і вибір монітора), а null при знятій стелі — не команда, тож 200.
func TestControlMaxFpsWithoutAgentChannel(t *testing.T) {
	withTicketMode(t)
	nodeWithAgent("nodeA", nil, 0)
	if code, _ := postControlRaw(`{"ticket":"t-nodeA","max_fps":15}`); code != http.StatusConflict {
		t.Fatalf("max_fps без каналу: %d, want 409", code)
	}
	if code, resp := postControlRaw(`{"ticket":"t-nodeA","max_fps":null}`); code != http.StatusOK || resp.MaxFps != nil {
		t.Fatalf("max_fps:null без стелі: %d %+v, want 200 і null", code, resp)
	}
}

// R7-G6: стеля нижче minBitrateBps зберігається як minBitrateBps, тож повтор тієї ж
// «Якості» (тулбар шле її з кожною зміною «Швидкості») мусить порівнюватись уже
// приведеною — інакше кожен повтор скидав би ціль і слав агенту bitrate_target.
func TestRepeatedSubMinQualityIsNoop(t *testing.T) {
	withTicketMode(t)
	quietNDJSON(t, nil)
	_, _, ctl := dialAgentLegWith(t, "nodeA", false)
	got := agentMsgs(ctl)
	ns := reg.get("nodeA")
	if ns == nil || !waitFor(func() bool { return ctlChan(ns) != nil }) {
		t.Fatal("control-канал агента не відкрився")
	}
	vl := addViewer(ns, newPC(t), newViewerTrack(t), "u1")
	t.Cleanup(func() { removeViewer(ns, vl) })

	body := `{"ticket":"t-nodeA","max_bitrate_bps":100000}`
	if code, _ := postControlRaw(body); code != http.StatusOK {
		t.Fatalf("POST /control: %d", code)
	}
	if m := waitMsg(t, got, control.TypeBitrateTarget); m.BitrateBps != minBitrateBps {
		t.Fatalf("bitrate_target = %d, want %d (підлога)", m.BitrateBps, minBitrateBps)
	}
	if code, _ := postControlRaw(body); code != http.StatusOK {
		t.Fatalf("POST /control: %d", code)
	}
	quiet := time.After(1500 * time.Millisecond)
	for {
		select {
		case m := <-got:
			if m.Type == control.TypeBitrateTarget {
				t.Fatalf("повтор стелі 100000 (= підлога) знову послав bitrate_target %d", m.BitrateBps)
			}
		case <-quiet:
			return
		}
	}
}

// Хвиля 10: до агента рух миші йде move-каналом, а кнопки/клавіші — ні.
func TestIsMouseMoveRouting(t *testing.T) {
	if !isMouseMove([]byte(`{"v":1,"type":"mouse_move","x":0.1,"y":0.2}`)) {
		t.Fatal("mouse_move мав іти move-каналом")
	}
	for _, s := range []string{
		`{"v":1,"type":"mouse_button","button":"left","down":true}`,
		`{"v":1,"type":"key","down":true,"scancode":30}`,
		`{"v":1,"type":"key","down":true,"unicode":109,"note":"mouse_move"}`,
	} {
		if isMouseMove([]byte(s)) {
			t.Fatalf("%s не мусить іти ненадійним каналом", s)
		}
	}
}
