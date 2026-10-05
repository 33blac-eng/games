package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// postControl — один запит на /control з тілом як його шле консоль.
func postControl(body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/control", strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	handleControl(w, r)
	return w
}

// withTicketMode вмикає ticket-режим із фейковим ERP на час тесту і повертає
// свіжий реєстр. Дзеркалить TestViewerTicketRoutingFailClosed — навмисно тим
// самим fakeERP, щоб /control перевірявся ТИМ САМИМ контуром, що й /offer/viewer.
func withTicketMode(t *testing.T) {
	t.Helper()
	srv := fakeERP(t)
	prevBase, prevKey, prevReg := erpBase, hubKey, reg
	erpBase, hubKey = srv.URL, "test-key"
	reg = newRegistry()
	t.Cleanup(func() {
		erpBase, hubKey, reg = prevBase, prevKey, prevReg
		srv.Close()
	})
}

// nodeWithAgent — нода з живим publisher-ом і відомим списком моніторів.
func nodeWithAgent(id string, list []outputInfo, active int) *nodeSession {
	ns := reg.getOrCreate(id)
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{} // сентинел: hasAgent перевіряє лише != nil
	ns.mu.Unlock()
	setOutputs(ns, list, active)
	return ns
}

// TestControlFailClosed — НЕГАТИВНИЙ КОНТРОЛЬ авторизації: перемикач монітора не
// сміє бути слабшим за перегляд. Без квитка, з квитком на чужу/безагентну ноду і
// з порожнім node у claims — жодного доступу до чужого екрана.
func TestControlFailClosed(t *testing.T) {
	withTicketMode(t)
	nodeWithAgent("nodeA", []outputInfo{{Index: 0, Width: 1920, Height: 1080, Primary: true}}, 0)

	zero := 0
	cases := []struct {
		name string
		body any
		want int
	}{
		{"без квитка", controlReq{}, http.StatusForbidden},
		{"статичний токен у ticket-режимі не рятує",
			controlReq{offerReq: offerReq{Token: token}}, http.StatusForbidden},
		{"квиток на ноду без publisher-а",
			controlReq{offerReq: offerReq{Ticket: "t-nodeB"}}, http.StatusNotFound},
		{"порожній node у квитку",
			controlReq{offerReq: offerReq{Ticket: "t-"}}, http.StatusForbidden},
		{"node з тіла запиту ІГНОРУЄТЬСЯ (беремо лише з claims)",
			controlReq{offerReq: offerReq{Ticket: "t-nodeB", Node: "nodeA"}, Output: &zero}, http.StatusNotFound},
	}
	for _, c := range cases {
		if got := postControl(c.body).Code; got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// TestControlListsOutputs — чисте читання (без "output") віддає саме те, що
// приніс offer агента. Це і є джерело для перемикача в консолі.
func TestControlListsOutputs(t *testing.T) {
	withTicketMode(t)
	want := []outputInfo{
		{Index: 0, Width: 1920, Height: 1080, Primary: true},
		{Index: 1, Width: 2560, Height: 1440},
	}
	nodeWithAgent("nodeA", want, 1)

	w := postControl(controlReq{offerReq: offerReq{Ticket: "t-nodeA"}})
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got controlResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad json: %v (%s)", err, w.Body.String())
	}
	if got.Active != 1 || len(got.Outputs) != 2 || got.Outputs[1].Width != 2560 {
		t.Fatalf("got %+v, want active=1 і два монітори з offer-а", got)
	}
}

// TestControlSelectWithoutAgentChannel — агент без control-каналу (старий
// білд або канал ще не відкрився) НЕ має отримувати мовчазний «успіх»:
// консоль мусить побачити відмову, інакше вона намалює перемкнутий монітор,
// якого ніхто не перемикав.
func TestControlSelectWithoutAgentChannel(t *testing.T) {
	withTicketMode(t)
	ns := nodeWithAgent("nodeA", []outputInfo{{Index: 0}, {Index: 1}}, 0)

	one := 1
	if got := postControl(controlReq{offerReq: offerReq{Ticket: "t-nodeA"}, Output: &one}).Code; got != http.StatusConflict {
		t.Fatalf("got %d, want 409 (канал агента не відкритий)", got)
	}
	if snap := outputsSnapshot(ns); snap.Active != 0 {
		t.Fatalf("active зсунувся на %d попри невдалу відправку", snap.Active)
	}
}

// TestControlRejectsNegativeOutput — індекс монітора не буває відʼємним;
// відсікаємо до control-каналу.
func TestControlRejectsNegativeOutput(t *testing.T) {
	withTicketMode(t)
	nodeWithAgent("nodeA", nil, 0)

	neg := -1
	if got := postControl(controlReq{offerReq: offerReq{Ticket: "t-nodeA"}, Output: &neg}).Code; got != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", got)
	}
}

// TestOutputsSnapshotIsCopy — знімок не має ділити масив із ns: наступний offer
// агента перепише список, і консоль читала б його з-під рук.
func TestOutputsSnapshotIsCopy(t *testing.T) {
	ns := &nodeSession{nodeID: "n"}
	setOutputs(ns, []outputInfo{{Index: 0, Width: 1920}}, 0)
	snap := outputsSnapshot(ns)
	setOutputs(ns, []outputInfo{{Index: 0, Width: 800}}, 0)
	if snap.Outputs[0].Width != 1920 {
		t.Fatalf("знімок змінився разом із ns: %d", snap.Outputs[0].Width)
	}
}

// TestSendSelectOutputNoChannel — прямий негативний контроль на відправник:
// немає відкритого "oosc-ctl" -> false, а не тихе ігнорування.
func TestSendSelectOutputNoChannel(t *testing.T) {
	ns := &nodeSession{nodeID: "n"}
	if sendSelectOutput(ns, 1) {
		t.Fatal("sendSelectOutput повернув true без control-каналу")
	}
}
