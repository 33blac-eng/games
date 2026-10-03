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

// ЖИВИЙ СТАН ПАРКУ 05.09.2026: агента перевели High -> Main, але новий білд
// доїхав лише на 2 ПК із 7. Решта 5 і далі кодують High. Хаб не перекодовує —
// він форвардить RTP як є, тож оголошений ним профіль мусить бути профілем
// АГЕНТА ЦІЄЇ НОДИ, а не однією константою на весь флот. Інакше глядач
// домовляється про Main, отримує High і бачить кашу замість чесної відмови.

const (
	plidHigh = "64001f"
	plidMain = "4d001f"
)

// h264API — API з MediaEngine, що знає РІВНО задані профілі H.264 (по одному PT
// на профіль). Саме так робить живий агент: реєструє один кодек — свій, з SPS
// власного енкодера. Дефолтний MediaEngine pion сюди не годиться: він оголошує
// цілий набір профілів, і «профіль цієї ноги» перестає бути визначеним.
func h264API(t *testing.T, plids ...string) *webrtc.API {
	t.Helper()
	m := &webrtc.MediaEngine{}
	for i, plid := range plids {
		if err := m.RegisterCodec(webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeH264,
				ClockRate:   90000,
				SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + plid,
				RTCPFeedback: []webrtc.RTCPFeedback{
					{Type: "nack"}, {Type: "nack", Parameter: "pli"},
				},
			},
			PayloadType: webrtc.PayloadType(102 + i),
		}, webrtc.RTPCodecTypeVideo); err != nil {
			t.Fatalf("RegisterCodec(%s): %v", plid, err)
		}
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m))
}

// dialAgentProfile — агент ноди, який кодує саме цим профілем: один трек, один
// кодек, повний шлях POST /offer/agent -> handleOffer.
func dialAgentProfile(t *testing.T, node, plid string) {
	t.Helper()
	pc, err := h264API(t, plid).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("agent pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	trk, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + plid,
	}, "video", "oo-screen")
	if err != nil {
		t.Fatalf("agent track: %v", err)
	}
	if _, err := pc.AddTrack(trk); err != nil {
		t.Fatalf("AddTrack: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-gathered

	body, _ := json.Marshal(offerReq{SDP: pc.LocalDescription().SDP, Token: token, Node: node})
	w := httptest.NewRecorder()
	handleOffer("agent")(w, httptest.NewRequest(http.MethodPost, "/offer/agent", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /offer/agent [%s]: %d %s", node, w.Code, w.Body.String())
	}
	var ans answerResp
	if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil {
		t.Fatalf("answer json: %v", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
	// Publisher вважається живим лише після Connected — глядача до того не
	// пустить authorizeViewer (fail-closed).
	if !waitFor(15*time.Second, reg.getOrCreate(node).hasAgent) {
		t.Fatalf("агентська нога [%s] не піднялась", node)
	}
}

// postViewerProfiles — глядач, який приймає рівно ці профілі, стукає з тікетом
// на ноду. Повертає код і тіло відповіді хаба.
func postViewerProfiles(t *testing.T, node string, plids ...string) (int, string) {
	t.Helper()
	pc, err := h264API(t, plids...).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("viewer pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatalf("AddTransceiver: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	<-gathered

	body, _ := json.Marshal(offerReq{SDP: pc.LocalDescription().SDP, Ticket: "t-" + node})
	w := httptest.NewRecorder()
	handleOffer("viewer")(w, httptest.NewRequest(http.MethodPost, "/offer/viewer", strings.NewReader(string(body))))
	return w.Code, w.Body.String()
}

// ticketMode вмикає ticket-режим із fakeERP і свіжим реєстром на час тесту.
func ticketMode(t *testing.T) {
	t.Helper()
	srv := fakeERP(t)
	t.Cleanup(srv.Close)
	prevBase, prevKey, prevReg := erpBase, hubKey, reg
	erpBase, hubKey, reg = srv.URL, "test-key", newRegistry()
	t.Cleanup(func() { erpBase, hubKey, reg = prevBase, prevKey, prevReg })
}

// answerProfile — profile-level-id з answer-у хаба (те, про що домовився глядач).
func answerProfile(t *testing.T, body string) string {
	t.Helper()
	var ans answerResp
	if err := json.Unmarshal([]byte(body), &ans); err != nil {
		t.Fatalf("answer json: %v (%s)", err, body)
	}
	p := sdpVideoProfile(ans.SDP)
	if p == "" {
		t.Fatalf("в answer-і хаба немає однозначного profile-level-id: %s", ans.SDP)
	}
	return p
}

// TestPerNodeProfileReachesViewer — ГОЛОВНИЙ тест: нода-High і нода-Main живуть
// одночасно (сьогоднішній парк), і глядач КОЖНОЇ отримує профіль СВОЄЇ ноди.
//
// Червоніє без правки: хаб оголошував h264FmtpLine (Main) обом, тож глядач
// ноди-High домовлявся про Main і отримував High-потік.
func TestPerNodeProfileReachesViewer(t *testing.T) {
	ticketMode(t)

	dialAgentProfile(t, "nodeHigh", plidHigh)
	dialAgentProfile(t, "nodeMain", plidMain)

	for node, want := range map[string]string{"nodeHigh": plidHigh, "nodeMain": plidMain} {
		code, body := postViewerProfiles(t, node, plidHigh, plidMain)
		if code != http.StatusOK {
			t.Fatalf("[%s] POST /offer/viewer: %d %s", node, code, body)
		}
		if got := answerProfile(t, body); !sameProfile(got, want) {
			t.Fatalf("[%s] хаб оголосив глядачеві %s, а агент ноди шле %s — глядач декодуватиме кашу",
				node, got, want)
		}
	}
}

// TestViewerWrongProfileStill415 — глядач, який не приймає профіль СВОЄЇ ноди,
// і далі отримує 415 з машинно-читаною причиною, і hub_profile у тілі — профіль
// ЦІЄЇ ноди, а не глобальна константа.
//
// Червоніє без правки: hub_profile був 4d001f для ноди, що шле High.
func TestViewerWrongProfileStill415(t *testing.T) {
	// 415 тепер СУВОРИЙ режим, а не дефолт: 05.09.2026 перша редакція H-18
	// відмовила вісьмом реальним спробам відкрити екран (Chrome оголошує
	// Constrained High 640c1f, хаб шле 64002a — за правилом підмножини
	// прапорців це «не збіг», хоча декодер той самий). Контракт відмови
	// лишається і перевіряється, але вмикається явно.
	t.Setenv("OO_SCREEN_STRICT_CODEC", "1")
	ticketMode(t)
	dialAgentProfile(t, "nodeHigh", plidHigh)

	code, body := postViewerProfiles(t, "nodeHigh", "42e01f")
	if code != http.StatusUnsupportedMediaType {
		t.Fatalf("baseline-глядач ноди-High: %d %s, want 415", code, body)
	}
	var why codecMismatch
	if err := json.Unmarshal([]byte(body), &why); err != nil {
		t.Fatalf("тіло 415 не JSON: %v (%s)", err, body)
	}
	if why.Error != "h264_profile_mismatch" {
		t.Fatalf("код причини %q, want h264_profile_mismatch", why.Error)
	}
	if why.HubProfile != plidHigh {
		t.Fatalf("hub_profile = %q, а нода шле %s — тіло 415 бреше глядачеві", why.HubProfile, plidHigh)
	}
	if len(why.ViewerProfiles) == 0 || !strings.Contains(strings.Join(why.ViewerProfiles, ","), "42e01f") {
		t.Fatalf("viewer_profiles = %v, чекали 42e01f", why.ViewerProfiles)
	}
}

// TestProfileUnknownYetIsHonest — агент ноди є, але профілю його потоку хаб не
// знає (offer без profile-level-id або з кількома різними). Тоді глядач дістає
// ЧЕСНУ відмову profile_unknown_yet, а не мовчазний дефолт.
//
// Червоніє без правки: хаб віддавав 200 OK з Main і глядач бачив сірий екран.
func TestProfileUnknownYetIsHonest(t *testing.T) {
	ticketMode(t)
	// Нода з живим publisher-ом, але без відомого профілю (сентинел-агент —
	// рівно як у TestViewerTicketRoutingFailClosed).
	ns := reg.getOrCreate("nodeX")
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{}
	ns.mu.Unlock()

	code, body := postViewerProfiles(t, "nodeX", plidMain)
	if code != http.StatusUnsupportedMediaType {
		t.Fatalf("профіль ноди невідомий, а хаб відповів %d %s — це і є тихий дефолт", code, body)
	}
	var why codecMismatch
	if err := json.Unmarshal([]byte(body), &why); err != nil {
		t.Fatalf("тіло 415 не JSON: %v (%s)", err, body)
	}
	if why.Error != profileUnknownYet {
		t.Fatalf("код причини %q, want %s", why.Error, profileUnknownYet)
	}
}

// TestSDPVideoProfileAmbiguous — два РІЗНІ профілі в offer-і агента означають
// «не знаю», а не «візьму перший-ліпший».
func TestSDPVideoProfileAmbiguous(t *testing.T) {
	one := sdpWithVideo("m=video 9 UDP/TLS/RTP/SAVPF 102",
		"a=rtpmap:102 H264/90000",
		"a=fmtp:102 packetization-mode=1;profile-level-id=64001f")
	if got := sdpVideoProfile(one); got != plidHigh {
		t.Fatalf("один профіль: got %q, want %s", got, plidHigh)
	}
	two := sdpWithVideo("m=video 9 UDP/TLS/RTP/SAVPF 102 104",
		"a=rtpmap:102 H264/90000",
		"a=fmtp:102 packetization-mode=1;profile-level-id=64001f",
		"a=rtpmap:104 H264/90000",
		"a=fmtp:104 packetization-mode=1;profile-level-id=4d001f")
	if got := sdpVideoProfile(two); got != "" {
		t.Fatalf("два різні профілі: got %q, want \"\" (невідомо)", got)
	}
}

// TestViewerWrongProfileWarnsByDefault — без OO_SCREEN_STRICT_CODEC хаб НЕ ріже
// сесію на неузгодженому профілі: детекція лишається (журнал + лічильник у
// /healthz), але людина далі отримує з'єднання.
//
// Червоніє без правки: дефолт віддавав 415 і забирав доступ у живих глядачів.
func TestViewerWrongProfileWarnsByDefault(t *testing.T) {
	t.Setenv("OO_SCREEN_STRICT_CODEC", "")
	ticketMode(t)
	dialAgentProfile(t, "nodeHigh2", plidHigh)

	before := rejectedProfileTotal.Load()
	code, body := postViewerProfiles(t, "nodeHigh2", "42e01f")
	if code == http.StatusUnsupportedMediaType {
		t.Fatalf("дефолт віддав 415 — доступ відібрано: %s", body)
	}
	if got := rejectedProfileTotal.Load(); got <= before {
		t.Fatalf("лічильник неузгоджень не зріс (%d -> %d) — детекцію теж загубили", before, got)
	}
}
