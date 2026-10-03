package main

// security_test.go — тести безпекового аудиту (tools/oo-screen/SECURITY-AUDIT.md).
// Кожен тест — одна властивість із таблиці звіту (ідентифікатор у назві: SecHxx).
// Тести з суфіксом KnownFAIL фіксують ПОТОЧНУ вразливу поведінку: вони мусять
// впасти, коли вразливість виправлять, — тоді їх треба перевернути.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"golang.org/x/time/rate"
)

// secERP — заглушка ERP: на consume віддає задані claims; рахує виклики.
func secERP(t *testing.T, status int, claims string) (*atomic.Int32, string) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-OO-Hub-Key") != "sec-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(claims))
	}))
	t.Cleanup(srv.Close)
	return &calls, srv.URL
}

// secTicketMode вмикає ticket-режим із заглушкою ERP і чистим реєстром.
func secTicketMode(t *testing.T, status int, claims string) *atomic.Int32 {
	t.Helper()
	calls, url := secERP(t, status, claims)
	prevBase, prevKey, prevReg := erpBase, hubKey, reg
	erpBase, hubKey, reg = url, "sec-key", newRegistry()
	t.Cleanup(func() { erpBase, hubKey, reg = prevBase, prevKey, prevReg })
	return calls
}

// secPublisher реєструє ноду з сентинел-агентом (hasAgent()==true).
func secPublisher(id string) *nodeSession {
	ns := reg.getOrCreate(id)
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{}
	ns.mu.Unlock()
	return ns
}

func secPost(t *testing.T, h http.HandlerFunc, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(body))
	req.Header.Set("Origin", "https://evil.example")
	h(rec, req)
	return rec
}

// SecH01: у ticket-режимі глядач без квитка — 403, і ERP навіть не питаємо.
func TestSecH01ViewerWithoutTicketRejected(t *testing.T) {
	calls := secTicketMode(t, 200, `{"user_id":"u","node_id":"A","grant":"control"}`)
	_, _, st, _ := authorizeViewer(offerReq{})
	if st != http.StatusForbidden || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d, want 403 і 0 викликів ERP", st, calls.Load())
	}
}

// SecH02: статичний агентський токен НЕ відчиняє viewer-ногу в ticket-режимі.
func TestSecH02StaticTokenUselessForViewerInTicketMode(t *testing.T) {
	secTicketMode(t, 200, `{"user_id":"u","node_id":"A"}`)
	_, _, st, _ := authorizeViewer(offerReq{Token: token})
	if st != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", st)
	}
}

// SecH03: node береться ЛИШЕ з claims — поле "node" у тілі запиту глядача
// ігнорується (глядач ноди A не дістанеться ноди B).
func TestSecH03ViewerBoundToTicketNode(t *testing.T) {
	secTicketMode(t, 200, `{"user_id":"u","node_id":"A","grant":"view"}`)
	secPublisher("A")
	secPublisher("B")
	ns, _, st, _ := authorizeViewer(offerReq{Ticket: "t", Node: "B"})
	if st != 0 || ns == nil || ns.nodeID != "A" {
		t.Fatalf("st=%d ns=%v, want нода A з квитка", st, ns)
	}
}

// SecH04: ERP відмовив / недосяжний / claims без node — fail-closed 403.
func TestSecH04ConsumeFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"erp 409 (replay)", 409, `{"error":"used"}`},
		{"erp 500", 500, ``},
		{"no node", 200, `{"user_id":"u","grant":"control"}`},
		{"bad json", 200, `<html>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			secTicketMode(t, c.status, c.body)
			secPublisher("A")
			if _, _, st, _ := authorizeViewer(offerReq{Ticket: "t"}); st != http.StatusForbidden {
				t.Fatalf("status=%d, want 403", st)
			}
		})
	}
	// ERP недосяжний.
	prevBase, prevKey := erpBase, hubKey
	erpBase, hubKey = "http://127.0.0.1:1", "k"
	defer func() { erpBase, hubKey = prevBase, prevKey }()
	if _, _, st, _ := authorizeViewer(offerReq{Ticket: "t"}); st != http.StatusForbidden {
		t.Fatalf("unreachable erp: status=%d, want 403", st)
	}
}

// SecH05: квиток на ноду без живого агента — 404, нода не створюється.
func TestSecH05NoPublisherNoNode(t *testing.T) {
	secTicketMode(t, 200, `{"user_id":"u","node_id":"ghost"}`)
	if _, _, st, _ := authorizeViewer(offerReq{Ticket: "t"}); st != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", st)
	}
	if reg.get("ghost") != nil {
		t.Fatal("authorizeViewer створив ноду для неіснуючого publisher-а")
	}
}

// SecH06: канал вводу — view-квиток, чужий квиток, нога без квитка => kill;
// завеликий кадр => drop; флуд обрізається token-bucket-ом.
func TestSecH06InputChannelGates(t *testing.T) {
	ev := func(tk string) []byte {
		b, _ := json.Marshal(map[string]any{"ticket": tk, "event": map[string]any{"v": 1, "type": "key"}})
		return b
	}
	now := time.Now()
	lim := func() *rate.Limiter { return rate.NewLimiter(inputRatePerSec, inputBurst) }
	if v, _, _ := judgeInput(ev("T"), "T", "view", lim(), now); v != inputKill {
		t.Fatalf("view-grant: verdict %v, want kill", v)
	}
	if v, _, _ := judgeInput(ev("T"), "", "control", lim(), now); v != inputKill {
		t.Fatalf("no-ticket leg: verdict %v, want kill", v)
	}
	if v, _, _ := judgeInput(ev("OTHER"), "T", "control", lim(), now); v != inputKill {
		t.Fatalf("foreign ticket: verdict %v, want kill", v)
	}
	big := append(ev("T"), bytes.Repeat([]byte(" "), inputMaxBytes)...)
	if v, _, _ := judgeInput(big, "T", "control", lim(), now); v != inputDrop {
		t.Fatalf("oversize: verdict %v, want drop", v)
	}
	l := lim()
	accepted := 0
	for i := 0; i < 1000; i++ {
		if v, _, _ := judgeInput(ev("T"), "T", "control", l, now); v == inputAccept {
			accepted++
		}
	}
	if accepted != inputBurst {
		t.Fatalf("за мить прийнято %d подій, want рівно burst=%d", accepted, inputBurst)
	}
}

// SecH07: тіло /offer/* обмежене maxOfferBody (256 КБ) — більше => 400.
func TestSecH07OfferBodyLimit(t *testing.T) {
	secTicketMode(t, 200, `{}`)
	body := []byte(`{"sdp":"` + strings.Repeat("a", maxOfferBody+10) + `"}`)
	for _, leg := range []string{"agent", "viewer"} {
		if rec := secPost(t, handleOffer(leg), body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d, want 400", leg, rec.Code)
		}
	}
	if rec := secPost(t, handleControl, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("control: status=%d, want 400", rec.Code)
	}
	if rec := secPost(t, handleViewerVisibility, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("visibility: status=%d, want 400", rec.Code)
	}
}

// SecH08: лише POST на /offer/* (GET — 405).
func TestSecH08OfferMethod(t *testing.T) {
	rec := httptest.NewRecorder()
	handleOffer("viewer")(rec, httptest.NewRequest(http.MethodGet, "/offer/viewer", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405", rec.Code)
	}
}

// SecH09: CORS у ticket-режимі — лише origin ERP, не "*" і не віддзеркалений Origin.
func TestSecH09CORSPinnedToERP(t *testing.T) {
	prev := erpBase
	erpBase = "https://total.example.com/api"
	defer func() { erpBase = prev }()
	rec := secPost(t, handleOffer("viewer"), []byte(`{`))
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://total.example.com" {
		t.Fatalf("ACAO=%q, want https://total.example.com", got)
	}
}

// SecH10: агентський токен — constant-time, порожній/чужий не проходить.
func TestSecH10AgentTokenRequired(t *testing.T) {
	if tokenMatches("") || tokenMatches(token+"x") || !tokenMatches(token) {
		t.Fatal("tokenMatches поводиться неправильно")
	}
	secTicketMode(t, 200, `{}`)
	rec := secPost(t, handleOffer("agent"), []byte(`{"sdp":"x","token":"wrong","node":"A"}`))
	if rec.Code != http.StatusUnauthorized || reg.get("A") != nil {
		t.Fatalf("status=%d node=%v, want 401 і жодної ноди", rec.Code, reg.get("A"))
	}
}

// SecH11 (KnownFAIL): ОДИН спільний агентський токен на весь парк, а node
// агент називає сам. Хто має токен (лежить на кожному ПК, у командному рядку
// schtask) — реєструється під БУДЬ-ЯКОЮ нодою, витісняє справжнього агента і
// отримує ввід глядачів. Тут: валідний токен + сміттєвий SDP => нода "victim"
// уже створена в реєстрі ще до відмови 400 (заодно — ріст реєстру).
func TestSecH11AgentCanClaimAnyNodeKnownFAIL(t *testing.T) {
	secTicketMode(t, 200, `{}`)
	b, _ := json.Marshal(offerReq{SDP: "garbage", Token: token, Node: "victim"})
	rec := secPost(t, handleOffer("agent"), b)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (bad sdp)", rec.Code)
	}
	if reg.get("victim") == nil {
		t.Fatal("нода не створилась — вразливість, схоже, виправлено: переверніть тест")
	}
}

// SecH12: /nodes — лише з X-OO-Hub-Key; порожній hubKey = маршрут закритий.
func TestSecH12NodesRequireHubKey(t *testing.T) {
	prev := hubKey
	defer func() { hubKey = prev }()
	hubKey = ""
	rec := httptest.NewRecorder()
	handleNodes(rec, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("empty hubKey: status=%d, want 403", rec.Code)
	}
	hubKey = "k"
	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/nodes", nil)
	r.Header.Set("X-OO-Hub-Key", "K")
	handleNodes(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong key: status=%d, want 403", rec.Code)
	}
}

// SecH13: session_id — 128 біт crypto/rand; порожній/невідомий не знаходить ногу.
func TestSecH13SessionIDUnguessable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id, err := newSessionID()
		if err != nil || len(id) != 32 || seen[id] {
			t.Fatalf("id=%q err=%v (дубль=%v)", id, err, seen[id])
		}
		seen[id] = true
	}
	if ns, _ := findViewerBySession(""); ns != nil {
		t.Fatal("порожній session_id знайшов ногу")
	}
	rec := secPost(t, handleViewerVisibility, []byte(`{"session_id":"deadbeef","hidden":true}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("visibility unknown session: %d, want 404", rec.Code)
	}
	rec = secPost(t, handleOffer("viewer"), []byte(`{"session_id":"deadbeef","sdp":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("renegotiate unknown session: %d, want 404", rec.Code)
	}
}

// SecH14 (FIXED): стеля viewer-ніг на ноду — понад неї 429 ДО створення PC.
func TestSecH14ViewerCapPerNode(t *testing.T) {
	prevBase, prevReg, prevCap := erpBase, reg, maxViewersPerNode
	erpBase, reg, maxViewersPerNode = "", newRegistry(), 2
	defer func() { erpBase, reg, maxViewersPerNode = prevBase, prevReg, prevCap }()

	ns := secPublisher(agentNodeIDEnv)
	ns.mu.Lock()
	ns.viewers = map[*webrtc.PeerConnection]*viewerLeg{
		{}: {}, // сентинели, як у тестах fanout
	}
	ns.viewers[&webrtc.PeerConnection{}] = &viewerLeg{}
	ns.mu.Unlock()

	b, _ := json.Marshal(offerReq{SDP: "x", Token: token})
	if rec := secPost(t, handleOffer("viewer"), b); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", rec.Code)
	}
}

// SecH15 (FIXED): записи сесій — каталог 0700, файл 0600.
func TestSecH15RecordingPermissions(t *testing.T) {
	withRecordFlag(t, true)
	dir := filepath.Join(t.TempDir(), "rec")
	prevDir := recordDir
	recordDir = dir
	defer func() { recordDir = prevDir }()

	// Пишемо через open() напряму — без корпусу: реальний SPS 1920x1080.
	r := &recorder{nodeID: "node", dir: dir,
		sps: []byte{0x67, 0x64, 0x00, 0x2A, 0xAC, 0xD9, 0x40, 0x78, 0x02, 0x27, 0xE5, 0x84,
			0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xF0, 0x3C, 0x60, 0xC6, 0x58},
		pps: []byte{0x68, 0xEE, 0x3C, 0x80}}
	if !r.open() {
		t.Fatal("open() не створив файл")
	}
	_ = r.f.Close()

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != recordDirPerm {
		t.Fatalf("каталог %v, want %v", di.Mode().Perm(), os.FileMode(recordDirPerm))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("файлів %d, want 1", len(entries))
	}
	fi, _ := entries[0].Info()
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("файл запису %v — доступний групі/іншим", fi.Mode().Perm())
	}
}

// SecH16: node_id з агента не дає path traversal в імені запису.
func TestSecH16RecordNameSanitized(t *testing.T) {
	for _, id := range []string{"../../etc/passwd", `..\..\win`, "a/b", strings.Repeat("x", 500), "", "\x00"} {
		got := safeNodeID(id)
		if strings.ContainsAny(got, "/\\\x00") || len(got) > 64 || got == "" || got == ".." || got == "." {
			t.Fatalf("safeNodeID(%q)=%q", id, got)
		}
	}
}

// SecH17: runtime-revoke за user рве лише ноги цього user-а; за node — усі ноги
// й саму ноду з реєстру.
func TestSecH17RevokeScoped(t *testing.T) {
	prev := reg
	reg = newRegistry()
	defer func() { reg = prev }()
	ns := secPublisher("A")
	ns.mu.Lock()
	ns.agentPC = nil
	ns.mu.Unlock()
	v1 := addViewer(ns, newPC(t), newViewerTrack(t), "alice")
	v2 := addViewer(ns, newPC(t), newViewerTrack(t), "bob")
	t.Cleanup(func() { removeViewer(ns, v1); removeViewer(ns, v2) })

	dropUserViewers(ns, "alice")
	ns.mu.Lock()
	_, aliceLeft := ns.viewers[v1.pc]
	_, bobLeft := ns.viewers[v2.pc]
	ns.mu.Unlock()
	if aliceLeft || !bobLeft {
		t.Fatalf("alice=%v bob=%v, want false/true", aliceLeft, bobLeft)
	}
	closeNode(ns)
	if reg.get("A") != nil {
		t.Fatal("closeNode не прибрав ноду з реєстру")
	}
}

// SecH18 (KnownFAIL): /control приймає квиток із grant=view і перемикає монітор
// (впливає на картинку ВСІХ глядачів ноди). Відмова тут — 409 «нема control-
// каналу», а не 403: grant ніде не перевіряється.
func TestSecH18ControlIgnoresGrantKnownFAIL(t *testing.T) {
	secTicketMode(t, 200, `{"user_id":"u","node_id":"A","grant":"view"}`)
	secPublisher("A")
	rec := secPost(t, handleControl, []byte(`{"ticket":"t","output":1}`))
	if rec.Code == http.StatusForbidden {
		t.Fatal("view-квиток отримав 403 — вразливість виправлено: переверніть тест")
	}
}

// SecH19: помилки назовні не несуть нутрощів (H-17) і не віддзеркалюють квиток.
func TestSecH19NoTicketEchoInErrors(t *testing.T) {
	secTicketMode(t, 403, `denied`)
	secPublisher("A")
	const tk = "SECRET-TICKET-123"
	rec := secPost(t, handleOffer("viewer"), []byte(`{"sdp":"x","ticket":"`+tk+`"}`))
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), tk) {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// SecH20: HTTP-сервер має таймаути (slowloris) — перевіряємо через джерело,
// бо main() не тестовний без запуску процесу.
func TestSecH20ServerTimeoutsPresent(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ReadHeaderTimeout:", "ReadTimeout:", "WriteTimeout:", "IdleTimeout:"} {
		if !bytes.Contains(src, []byte(k)) {
			t.Fatalf("у http.Server немає %s", k)
		}
	}
	if !bytes.Contains(src, []byte("token == defaultToken")) {
		t.Fatal("main() більше не забороняє дефолтний токен")
	}
}
