package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/internal/control"
)

func legAudioLive(ns *nodeSession, vl *viewerLeg) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return vl.audioLive
}

// TestHiddenViewerKeepsAudioOnRequest — звук окремо від відео (16.09.2026):
// консоль показує картинку Mesh, а звук бере в OO. Прихований глядач з
// audio:true не отримує відео, але отримує звук і тримає агента в "resume".
// Негативний контроль — той самий глядач без audio: F-39 як було.
func TestHiddenViewerKeepsAudioOnRequest(t *testing.T) {
	ns := readyNode(t, "vis-audio-only")
	vl := ns.onlyViewer(t)

	setViewerHidden(ns, vl, true)
	if legAudioLive(ns, vl) || gatePresent(ns) {
		t.Fatalf("без audio прихований глядач досі отримує звук або тримає агента")
	}

	setViewerAudio(ns, vl, true)
	if legLive(ns, vl) {
		t.Fatalf("глядачеві лише зі звуком публікується відео")
	}
	if !legAudioLive(ns, vl) {
		t.Fatalf("глядач попросив звук, а звук йому не йде")
	}
	if !gatePresent(ns) {
		t.Fatalf("єдиний глядач просить звук, а агент на паузі — звуку не буде")
	}

	setViewerAudio(ns, vl, false)
	if legAudioLive(ns, vl) || gatePresent(ns) {
		t.Fatalf("глядач відмовився від звуку, а звук або гейт лишились")
	}
}

// TestUnhideFromAudioSendsNoPause — R6-G6: єдиний глядач ховався зі звуком і
// повертається (POST {hidden:false}). Він присутній весь час — агентові не
// сміє доїхати "pause" (з ним Suspend DXGI і перевідкриття WASAPI). Порядок
// «спершу звук, потім видимість» на поверненні саме його й слав. Повний шлях
// до агента: POST /viewer/visibility -> sendGate -> живий "oosc-ctl".
func TestUnhideFromAudioSendsNoPause(t *testing.T) {
	withTicketMode(t)
	quietNDJSON(t, nil)
	_, _, ctl := dialAgentLegWith(t, "nodeA", false)
	texts := make(chan string, 64)
	beats := make(chan uint64, 64)
	ctl.OnMessage(func(m webrtc.DataChannelMessage) {
		if msg, err := control.Read(bufioLine(m.Data)); err == nil {
			if msg.Type == control.TypeHeartbeat {
				beats <- msg.Seq
			}
			return
		}
		texts <- string(m.Data)
	})
	ns := reg.get("nodeA")
	if ns == nil || !waitFor(func() bool { return ctlChan(ns) != nil }) {
		t.Fatal("control-канал агента не відкрився")
	}
	// Перший гейт OnOpen (глядача ще нема) — "pause"; далі він тесту не заважає.
	select {
	case g := <-texts:
		if g != "pause" {
			t.Fatalf("гейт на OnOpen без глядача = %q, want pause", g)
		}
	case <-time.After(eventGuard):
		t.Fatal("гейт на OnOpen не доїхав")
	}

	vl := quietViewer(ns)
	t.Cleanup(func() { removeViewer(ns, vl) })
	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	ns.mu.Lock()
	vl.sessionID = id
	ns.mu.Unlock()

	post := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		handleViewerVisibility(w, httptest.NewRequest(http.MethodPost, "/viewer/visibility", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("POST /viewer/visibility: %d %q", w.Code, w.Body.String())
		}
	}
	// drain — усі гейти, що хаб послав до цієї миті: канал упорядкований, тож
	// усе до нашого heartbeat-а доїде раніше за нього.
	drain := func() []string {
		t.Helper()
		if !sendHeartbeat(ns, ctlChan(ns)) {
			t.Fatal("heartbeat-маркер не пішов")
		}
		mark := atomic.LoadUint64(&ns.ctlSeq)
		var gates []string
		deadline := time.After(eventGuard)
		for {
			select {
			case g := <-texts:
				gates = append(gates, g)
			case seq := <-beats:
				if seq >= mark {
					return gates
				}
			case <-deadline:
				t.Fatal("heartbeat-маркер не доїхав")
			}
		}
	}

	post(`{"session_id":"` + id + `","hidden":true,"audio":true}`)
	for _, g := range drain() {
		if g != "resume" {
			t.Fatalf("сховався зі звуком: агенту поїхало %q", g)
		}
	}
	post(`{"session_id":"` + id + `","hidden":false}`)
	gates := drain()
	for _, g := range gates {
		if g != "resume" {
			t.Fatalf("повернувся з прихованого-зі-звуком: агенту поїхало %q (усі гейти %v)", g, gates)
		}
	}
	ns.mu.Lock()
	wantAudio := vl.wantAudio
	ns.mu.Unlock()
	if legHidden(ns, vl) || wantAudio {
		t.Fatalf("після повернення нога лишилась прихованою або з окремим проханням звуку")
	}
}
