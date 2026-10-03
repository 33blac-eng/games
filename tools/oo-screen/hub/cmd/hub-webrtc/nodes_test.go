package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"
)

// nodesFixture — реєстр із двома нодами: nodeA має живого publisher-а, nodeB ні,
// плюс безіменна T1-нода. Повертає прибирання глобалів.
func nodesFixture(t *testing.T, key string) {
	t.Helper()
	prevKey, prevReg := hubKey, reg
	hubKey, reg = key, newRegistry()
	t.Cleanup(func() { hubKey, reg = prevKey, prevReg })

	for _, n := range []string{"nodeA", ""} {
		ns := reg.getOrCreate(n)
		ns.mu.Lock()
		ns.agentPC = &webrtc.PeerConnection{}
		ns.mu.Unlock()
	}
	reg.getOrCreate("nodeB") // viewer чекає, publisher-а немає
}

func getNodes(key string, setHeader bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	if setHeader {
		r.Header.Set("X-OO-Hub-Key", key)
	}
	w := httptest.NewRecorder()
	handleNodes(w, r)
	return w
}

// TestNodesSecretRequired — ГОЛОВНА перевірка маршруту: без валідного спільного
// секрету він не віддає ЖОДНОГО node_id. Перелік вузлів — інвентар парку, а хаб
// стоїть у відкритому інтернеті.
func TestNodesSecretRequired(t *testing.T) {
	nodesFixture(t, "test-key")

	for _, tc := range []struct {
		name      string
		key       string
		setHeader bool
	}{
		{"без заголовка", "", false},
		{"порожній ключ", "", true},
		{"чужий ключ", "not-the-key", true},
		{"майже той самий", "test-ke", true},
	} {
		w := getNodes(tc.key, tc.setHeader)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: got %d, want 403", tc.name, w.Code)
		}
		if strings.Contains(w.Body.String(), "node") {
			t.Fatalf("%s: тіло розкриває вузли: %q", tc.name, w.Body.String())
		}
	}
}

// TestNodesFailsClosedWithoutHubKey — хаб піднято БЕЗ OO_SCREEN_HUB_KEY: маршрут
// вимкнено, а не «пускає всіх». Порожній ключ у конфігу + порожній у запиті не
// сміють збігтись.
func TestNodesFailsClosedWithoutHubKey(t *testing.T) {
	nodesFixture(t, "")

	for _, tc := range []struct {
		key       string
		setHeader bool
	}{{"", false}, {"", true}, {"whatever", true}} {
		w := getNodes(tc.key, tc.setHeader)
		if w.Code != http.StatusForbidden {
			t.Fatalf("hubKey=\"\", key=%q: got %d, want 403", tc.key, w.Code)
		}
		if strings.Contains(w.Body.String(), "node") {
			t.Fatalf("hubKey=\"\": тіло розкриває вузли: %q", w.Body.String())
		}
	}
}

// TestNodesListsOnlyLivePublishers — з валідним ключем віддається рівно те, що
// вирішує 404 у authorizeViewer: ноди з ns.hasAgent(). Нода без publisher-а і
// безіменна T1-нода в перелік не потрапляють.
func TestNodesListsOnlyLivePublishers(t *testing.T) {
	nodesFixture(t, "test-key")

	w := getNodes("test-key", true)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}

	var resp nodesResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json %q: %v", w.Body.String(), err)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0] != "nodeA" {
		t.Fatalf("got %v, want [nodeA]", resp.Nodes)
	}

	// Publisher помер -> нода зникає з переліку тієї ж миті.
	ns := reg.get("nodeA")
	ns.mu.Lock()
	ns.agentPC = nil
	ns.mu.Unlock()

	w = getNodes("test-key", true)
	var after nodesResp
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatalf("bad json %q: %v", w.Body.String(), err)
	}
	if len(after.Nodes) != 0 {
		t.Fatalf("after publisher death: got %v, want []", after.Nodes)
	}
}
