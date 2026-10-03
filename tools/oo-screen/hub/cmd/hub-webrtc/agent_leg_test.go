package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// dialAgentLeg — «агент», який ПІДКЛЮЧИВСЯ і має відеотрек, але не надіслав
// ЖОДНОГО RTP-пакета. Саме так поводиться прод-агент з on-demand гейтингом: hub
// шле "pause" щойно відкриється control-канал і глядача нема, тож до першого
// глядача агент нічого не кодує і нічого не шле.
//
// Проходить рівно тим самим шляхом, що прод: POST /offer/agent -> handleOffer.
func dialAgentLeg(t *testing.T, node string) *webrtc.PeerConnection {
	pc, _, _ := dialAgentLegWith(t, node, false)
	return pc
}

// dialAgentLegWith — те саме, але з опційною ДРУГОЮ доріжкою (звук ПК, PCMU),
// рівно як її публікує живий агент під OO_SCREEN_AUDIO: одне зʼєднання, один
// offer, дві доріжки. Повертає доріжку звуку, щоб тест міг у неї писати.
// Третім значенням повертається control-канал "oosc-ctl" з БОКУ АГЕНТА — той
// самий, який живий агент відкриває завжди (гейтинг, bitrate_target, вибір
// монітора, пульс хаба).
func dialAgentLegWith(t *testing.T, node string, withAudio bool) (*webrtc.PeerConnection, *webrtc.TrackLocalStaticSample, *webrtc.DataChannel) {
	t.Helper()

	// MediaEngine рівно як у живого агента (newWebRTCAPI в oo-agent): ОДИН
	// зареєстрований відеокодек — свій. Дефолтний набір pion оголошував би
	// пʼять профілів H.264 одночасно, і «профіль цієї ноди» ставав би
	// невизначеним — чого в проді не буває.
	remote, err := agentAPI(t).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("agent pc: %v", err)
	}
	t.Cleanup(func() { _ = remote.Close() })

	// Канал створює АГЕНТ (він тут offerer) і до offer-а, як у проді.
	ctl, err := remote.CreateDataChannel("oosc-ctl", nil)
	if err != nil {
		t.Fatalf("agent ctl channel: %v", err)
	}

	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: h264FmtpLine,
	}, "video", "oo-screen")
	if err != nil {
		t.Fatalf("agent track: %v", err)
	}
	if _, err := remote.AddTrack(track); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}

	var atrk *webrtc.TrackLocalStaticSample
	if withAudio {
		if atrk, err = webrtc.NewTrackLocalStaticSample(audioCap, "audio", "oo-screen"); err != nil {
			t.Fatalf("agent audio track: %v", err)
		}
		if _, err := remote.AddTrack(atrk); err != nil {
			t.Fatalf("AddTrack(audio): %v", err)
		}
	}

	offer, err := remote.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(remote)
	if err := remote.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-gathered

	body, _ := json.Marshal(offerReq{SDP: remote.LocalDescription().SDP, Token: token, Node: node, Audio: withAudio})
	req := httptest.NewRequest(http.MethodPost, "/offer/agent", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	handleOffer("agent")(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /offer/agent: %d %s", w.Code, w.Body.String())
	}

	var ans answerResp
	if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil {
		t.Fatalf("answer json: %v", err)
	}
	if err := remote.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: ans.SDP,
	}); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
	return remote, atrk, ctl
}

// waitFor — умова протягом d, з кроком 20мс. Час тут про ICE на loopback, а не
// про логіку: перевіряємо ФАКТ переходу, а не його швидкість.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// TestPublisherReadyBeforeFirstFrame — ЖИВА ПОЛОМКА 30.08: перший глядач не міг
// зайти НІКОЛИ.
//
// ns.agentPC присвоювався всередині pc.OnTrack, тобто «публікатор є» означало
// «медіа вже прийшло». Але агент з on-demand гейтингом не кодує, поки нема
// глядача, а глядача не пускає authorizeViewer, поки hasAgent() хибне. Замкнене
// коло: людина тисне «OO», хаб віддає 404, і замість картинки виїжджає банер
// «OO втрачено, перемкнено на Mesh».
//
// Стенди цього не ловили, бо soak_probe ходить гілкою static-token, де
// authorizeViewer hasAgent() взагалі НЕ перевіряє (глядач може прийти раніше за
// агента й почекати). Тому тест саме в ticket-режимі — це прод.
func TestPublisherReadyBeforeFirstFrame(t *testing.T) {
	srv := fakeERP(t)
	defer srv.Close()

	prevBase, prevKey, prevReg := erpBase, hubKey, reg
	erpBase, hubKey = srv.URL, "test-key"
	reg = newRegistry()
	defer func() { erpBase, hubKey, reg = prevBase, prevKey, prevReg }()

	dialAgentLeg(t, "nodeA")

	ns := reg.get("nodeA")
	if ns == nil {
		t.Fatal("агентська нога не зареєструвала ноду в реєстрі")
	}
	// Агентська нога піднялась — цього достатньо, щоб нода вважалась готовою.
	// Жодного RTP не було й не буде, поки хаб не пустить глядача.
	if !waitFor(15*time.Second, ns.hasAgent) {
		t.Fatal("hasAgent() лишився false після піднятої агентської ноги: перший глядач не зайде НІКОЛИ")
	}

	postViewer := func(ticket string) int {
		body, _ := json.Marshal(offerReq{SDP: "invalid-sdp", Ticket: ticket})
		r := httptest.NewRequest(http.MethodPost, "/offer/viewer", strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		handleOffer("viewer")(w, r)
		return w.Code
	}

	// Глядач мусить бути ДОПУЩЕНИЙ: далі він падає на невалідному SDP (500), що
	// й доводить — fail-closed не спрацював, дійшли до негоціації.
	if code := postViewer("t-nodeA"); code == http.StatusNotFound {
		t.Fatalf("глядач отримав 404 при піднятій агентській нозі: %d", code)
	}

	// 🔴 Друга половина правди: fail-closed НЕ послаблено. Ноди, де агентської
	// ноги немає взагалі, як віддавали 404, так і віддають.
	if code := postViewer("t-nodeB"); code != http.StatusNotFound {
		t.Fatalf("nodeB без агентської ноги: got %d, want 404 fail-closed", code)
	}
}

// agentAPI — API «як у прод-агента»: один H.264-кодек (профіль хаба за
// замовчуванням) плюс PCMU під прапорцем звуку.
func agentAPI(t *testing.T) *webrtc.API {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: h264FmtpLine,
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "nack"}, {Type: "nack", Parameter: "pli"},
			},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		t.Fatalf("RegisterCodec(video): %v", err)
	}
	// PCMU реєструємо ЗАВЖДИ: прапорець звуку в агента свій, і тест
	// «агент шле звук, а хаб із вимкненим прапорцем його не бере» мусить
	// мати чим слати.
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: audioCap, PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		t.Fatalf("RegisterCodec(audio): %v", err)
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m))
}
