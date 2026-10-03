package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// gatePresent — предикат, який sendGate віддає агенту: true = "resume".
func gatePresent(ns *nodeSession) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return hasVisibleViewerLocked(ns)
}

func legLive(ns *nodeSession, vl *viewerLeg) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return vl.live
}

func legHidden(ns *nodeSession, vl *viewerLeg) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return vl.hidden
}

// TestVisibilityPauseOnlyWhenAllHidden — ЯДРО F-39 на кількох глядачах: агент
// іде в "pause" ЛИШЕ коли приховались УСІ, і прокидається від ПЕРШОГО, хто
// повернувся. Один прихований глядач із двох не сміє зупинити потік сусідові.
func TestVisibilityPauseOnlyWhenAllHidden(t *testing.T) {
	ns := readyNode(t, "vis-all-hidden")
	v1 := ns.onlyViewer(t)
	v2 := addReadyViewer(t, ns)

	if !gatePresent(ns) {
		t.Fatalf("двоє дивляться, а гейт не resume")
	}

	// Перший сховався — другий ще дивиться: агент кодує далі.
	setViewerHidden(ns, v1, true)
	if !gatePresent(ns) {
		t.Fatalf("гейт впав у pause, хоч другий глядач ще дивиться")
	}
	if legLive(ns, v1) {
		t.Fatalf("прихованому глядачеві все ще публікується трек")
	}
	if !legLive(ns, v2) {
		t.Fatalf("видимий глядач перестав отримувати потік через сусіда")
	}

	// Сховались УСІ — ось тепер pause.
	setViewerHidden(ns, v2, true)
	if gatePresent(ns) {
		t.Fatalf("усі глядачі приховані, а агент не на паузі")
	}
	if legLive(ns, v2) {
		t.Fatalf("другому прихованому глядачеві все ще публікується трек")
	}

	// Повернувся ОДИН — resume від першого ж.
	setViewerHidden(ns, v2, false)
	if !gatePresent(ns) {
		t.Fatalf("глядач повернувся, а агент лишився на паузі")
	}
	if !legLive(ns, v2) {
		t.Fatalf("глядач повернувся, а трек йому не публікується")
	}
	if legLive(ns, v1) {
		t.Fatalf("повернення одного глядача розховало іншого")
	}
}

// TestVisibilityHiddenViewerGetsNoPackets — прихованому реально не йдуть
// пакети, а після повернення йдуть знову. Це і є економія трафіку й CPU у
// людини: важіль не в браузері, а тут.
func TestVisibilityHiddenViewerGetsNoPackets(t *testing.T) {
	ns := readyNode(t, "vis-no-packets")
	v1 := ns.onlyViewer(t)
	v2 := addReadyViewer(t, ns)

	forwardN(ns, 3, 100, 900000)
	waitSent(t, v1, 3, "до приховування")
	waitSent(t, v2, 3, "до приховування")

	setViewerHidden(ns, v2, true)
	forwardN(ns, 4, 103, 909000)
	waitSent(t, v1, 7, "видимий під час приховування сусіда")
	if got := atomic.LoadUint64(&v2.sent); got != 3 {
		t.Fatalf("прихований отримав %d пакетів, want 3 (нічого нового)", got)
	}

	setViewerHidden(ns, v2, false)
	forwardN(ns, 2, 107, 921000)
	waitSent(t, v1, 9, "видимий після повернення сусіда")
	// Повернутій нозі, крім двох нових, міг поїхати ще й кеш GOP (пункт 41) —
	// тому перевіряємо не точну цифру, а що потік ВІДНОВИВСЯ.
	if got := atomic.LoadUint64(&v2.sent); got <= 3 {
		t.Fatalf("глядач повернувся, а потік не відновився: sent = %d, want > 3", got)
	}
}

// TestVisibilityHiddenIsNotGone — прихований глядач НЕ дорівнює тому, що пішов.
// Нога лишається в реєстрі ноди, лишається Connected, і її session_id так само
// відчиняє ренегоціацію — тобто повернення не потребує ні нового квитка, ні
// нової PeerConnection. Плюс: hasReadyViewerLocked («хтось іще тут») і
// hasVisibleViewerLocked («хтось дивиться») навмисно розходяться.
func TestVisibilityHiddenIsNotGone(t *testing.T) {
	ns := reg.getOrCreate("vis-not-gone")
	t.Cleanup(func() { reg.remove("vis-not-gone", ns) })
	vl := quietViewer(ns)
	t.Cleanup(func() { removeViewer(ns, vl) })

	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	ns.mu.Lock()
	vl.sessionID = id
	ns.mu.Unlock()

	setViewerHidden(ns, vl, true)

	if n := viewerCount(ns); n != 1 {
		t.Fatalf("ns.viewers = %d після приховування, want 1 (прихований — не той, що пішов)", n)
	}
	ns.mu.Lock()
	ready := vl.ready
	stillHere := hasReadyViewerLocked(ns)
	watching := hasVisibleViewerLocked(ns)
	ns.mu.Unlock()
	if !ready {
		t.Fatalf("приховування скинуло vl.ready — це вже «відпав», а не «сховався»")
	}
	if !stillHere {
		t.Fatalf("hasReadyViewerLocked = false: хаб вважає прихованого глядача відсутнім")
	}
	if watching {
		t.Fatalf("hasVisibleViewerLocked = true у прихованого — гейт не спрацює")
	}
	if gotNS, gotVL := findViewerBySession(id); gotNS != ns || gotVL != vl {
		t.Fatalf("session_id прихованої ноги перестав працювати — повернення вимагало б нового квитка")
	}
}

// TestVisibilityHTTPHidesAndRestores — наскрізний шлях глядача: POST із його ж
// session_id ховає ногу і повертає її назад, а у відповіді видно стан і те, чи
// пішов агенту "pause".
func TestVisibilityHTTPHidesAndRestores(t *testing.T) {
	ns := reg.getOrCreate("vis-http")
	t.Cleanup(func() { reg.remove("vis-http", ns) })
	vl := quietViewer(ns)
	t.Cleanup(func() { removeViewer(ns, vl) })

	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	ns.mu.Lock()
	vl.sessionID = id
	ns.mu.Unlock()

	post := func(body string) visibilityResp {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/viewer/visibility", strings.NewReader(body))
		w := httptest.NewRecorder()
		handleViewerVisibility(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("код %d, want 200 (тіло: %q)", w.Code, w.Body.String())
		}
		var resp visibilityResp
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("відповідь не JSON: %v (%q)", err, w.Body.String())
		}
		return resp
	}

	resp := post(`{"session_id":"` + id + `","hidden":true}`)
	if !resp.Hidden || !resp.AgentPaused {
		t.Fatalf("hidden=%v agent_paused=%v, want true/true (єдиний глядач сховався)", resp.Hidden, resp.AgentPaused)
	}
	if !legHidden(ns, vl) {
		t.Fatalf("POST hidden=true не сховав ногу")
	}

	resp = post(`{"session_id":"` + id + `","hidden":false}`)
	if resp.Hidden || resp.AgentPaused {
		t.Fatalf("hidden=%v agent_paused=%v, want false/false (глядач повернувся)", resp.Hidden, resp.AgentPaused)
	}
	if legHidden(ns, vl) {
		t.Fatalf("POST hidden=false не повернув ногу")
	}

	// Поля hidden узагалі немає — безпечний бік: видимий.
	resp = post(`{"session_id":"` + id + `"}`)
	if resp.Hidden {
		t.Fatalf("запит без поля hidden сховав ногу — дефолт має бути «дивиться»")
	}
}

// TestVisibilityBadRequestsChangeNothing — урок H-18 у явному вигляді: жоден
// кривий запит не сміє нікого позбавити картинки. Невідомий ключ, битий JSON і
// не той метод дають 4xx і НУЛЬ наслідків для живого глядача.
func TestVisibilityBadRequestsChangeNothing(t *testing.T) {
	ns := reg.getOrCreate("vis-bad")
	t.Cleanup(func() { reg.remove("vis-bad", ns) })
	vl := quietViewer(ns)
	t.Cleanup(func() { removeViewer(ns, vl) })

	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	ns.mu.Lock()
	vl.sessionID = id
	ns.mu.Unlock()

	cases := []struct {
		name, method, body string
		want               int
	}{
		{"чужий session_id", http.MethodPost, `{"session_id":"deadbeefdeadbeefdeadbeefdeadbeef","hidden":true}`, http.StatusNotFound},
		{"порожній session_id", http.MethodPost, `{"hidden":true}`, http.StatusNotFound},
		{"битий JSON", http.MethodPost, `{"session_id":`, http.StatusBadRequest},
		{"GET", http.MethodGet, ``, http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/viewer/visibility", strings.NewReader(c.body))
		w := httptest.NewRecorder()
		handleViewerVisibility(w, r)
		if w.Code != c.want {
			t.Fatalf("%s: код %d, want %d (тіло: %q)", c.name, w.Code, c.want, w.Body.String())
		}
		if legHidden(ns, vl) {
			t.Fatalf("%s: живого глядача сховало кривим запитом", c.name)
		}
		if !gatePresent(ns) {
			t.Fatalf("%s: гейт впав у pause через кривий запит", c.name)
		}
	}
}

// TestVisibilityInputUnhides — страховка в бік «слати»: подія вводу від
// прихованої ноги знімає прихованість. Людина клацає — отже, дивиться, хай
// навіть її POST загубився.
func TestVisibilityInputUnhides(t *testing.T) {
	ns := readyNode(t, "vis-input")
	vl := ns.onlyViewer(t)

	setViewerHidden(ns, vl, true)
	if gatePresent(ns) {
		t.Fatalf("єдиний глядач сховався, а гейт не pause")
	}

	unhideViewer(ns, vl)
	if legHidden(ns, vl) {
		t.Fatalf("unhideViewer не зняв прихованість")
	}
	if !gatePresent(ns) {
		t.Fatalf("після події вводу гейт лишився pause")
	}
	if !legLive(ns, vl) {
		t.Fatalf("після події вводу трек глядачеві не відновлено")
	}

	// Повтор на вже видимій нозі — дешевий no-op, нічого не ламає.
	unhideViewer(ns, vl)
	if legHidden(ns, vl) || !gatePresent(ns) {
		t.Fatalf("повторний unhideViewer зіпсував стан")
	}
}
