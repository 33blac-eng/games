// Гейти хвилі 1 по хабу: H-26 (скид адаптації), H-33 (алокації на пакет),
// H-28 (нескинхронізований NDJSON), пункт 40 (REMB), пункт 41 (GOP-кеш),
// F-11 (ренегоціація viewer-ноги).
//
// Кожен тут вміє почервоніти: під кожним стоїть коментар «зніми правку X — і
// цей тест падає», бо гейт, який зелений і до фікса, вартий рівно нуля.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// quietNDJSON відводить NDJSON у нікуди на час тесту: інакше семпли сиплються в
// вивід прогону й ще й псують рахунок алокацій.
func quietNDJSON(t *testing.T, w *bytes.Buffer) {
	t.Helper()
	prev := ndjsonOut
	if w == nil {
		ndjsonOut = discardWriter{}
	} else {
		ndjsonOut = w
	}
	t.Cleanup(func() { ndjsonOut = prev })
}

// ── H-26 ────────────────────────────────────────────────────────────────────

// TestResetBitrateKeepsFreshAdaptation — свіжа адаптація ПЕРЕЖИВАЄ прихід
// глядача. Зніми умову kept у resetBitrate — і ціль тут стане стелею.
func TestResetBitrateKeepsFreshAdaptation(t *testing.T) {
	ns := &nodeSession{nodeID: "keep", startBps: 8_000_000}
	ns.bitrate = newBitrateCtl(8_000_000)
	ns.bitrate.target = 2_000_000
	ns.bitrate.lastSent = time.Now().Add(-2 * time.Second)

	resetBitrate(ns)

	ns.mu.Lock()
	got := ns.bitrate.target
	ns.mu.Unlock()
	if got != 2_000_000 {
		t.Fatalf("ціль після приходу глядача = %d, want 2000000 (адаптацію віком 2с скидати не можна)", got)
	}
}

// TestResetBitrateDropsStaleAdaptation — адаптація, старша за вікно, таки
// скидається: інша сесія, інший канал, чужі втрати.
func TestResetBitrateDropsStaleAdaptation(t *testing.T) {
	ns := &nodeSession{nodeID: "stale", startBps: 8_000_000}
	ns.bitrate = newBitrateCtl(8_000_000)
	ns.bitrate.target = 2_000_000
	ns.bitrate.lastSent = time.Now().Add(-bitrateKeepWindow - time.Second)

	resetBitrate(ns)

	ns.mu.Lock()
	got := ns.bitrate.target
	ns.mu.Unlock()
	if got != 8_000_000 {
		t.Fatalf("ціль після застарілої адаптації = %d, want 8000000 (стеля)", got)
	}
}

// ── пункт 40: REMB ──────────────────────────────────────────────────────────

func TestRembLowersTargetAndCapsRise(t *testing.T) {
	now := time.Now()
	c := newBitrateCtl(8_000_000)

	c, send := c.withRemb(3_000_000, now)
	if !send || c.target != 3_000_000 {
		t.Fatalf("withRemb(3 Мбіт) -> send=%v target=%d, want true/3000000", send, c.target)
	}

	// Друга оцінка одразу — дебаунс тримає.
	if _, send = c.withRemb(2_000_000, now.Add(100*time.Millisecond)); send {
		t.Fatalf("withRemb двічі підряд пройшов дебаунс")
	}

	// Оцінка ВИЩА за ціль нічого не піднімає: вгору веде лише step() по втратах.
	if c2, send := c.withRemb(9_000_000, now.Add(time.Minute)); send || c2.target != 3_000_000 {
		t.Fatalf("REMB підняв ціль сам: send=%v target=%d", send, c2.target)
	}

	// І головне: стеля тримається далі, скільки б чистих RR не прийшло.
	c.remb = 3_000_000
	for i := 0; i < 200; i++ {
		var send bool
		c, send = c.step(0, 0, now.Add(time.Duration(i)*11*time.Second))
		_ = send
	}
	if c.target > 3_000_000 {
		t.Fatalf("ціль %d перелізла через REMB-стелю 3000000", c.target)
	}
}

func TestRembZeroIsNoop(t *testing.T) {
	c := newBitrateCtl(8_000_000)
	got, send := c.withRemb(0, time.Now())
	if send || got.target != 8_000_000 || got.remb != 0 {
		t.Fatalf("порожня оцінка змінила контролер: %+v send=%v", got, send)
	}
}

// ── H-28: NDJSON ────────────────────────────────────────────────────────────

// TestNDJSONConcurrentWriters — писарі з різних горутин. Буфер НЕ потокобезпечний
// саме навмисно: прибери ndjsonMu — і -race валить цей тест, а без -race
// поламані рядки видно в перевірці JSON нижче.
func TestNDJSONConcurrentWriters(t *testing.T) {
	var buf bytes.Buffer
	quietNDJSON(t, &buf)

	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				logNDJSON("viewer", "node-with-a-fairly-long-identifier", uint16(i), uint32(w))
			}
		}(w)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != writers*each {
		t.Fatalf("рядків %d, want %d — записи злиплись", len(lines), writers*each)
	}
	for i, ln := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(ln), &v); err != nil {
			t.Fatalf("рядок %d не JSON (%v): %q", i, err, ln)
		}
	}
}

// ── H-33: алокації ──────────────────────────────────────────────────────────

// quietViewer — нога БЕЗ pump-а: черга нікуди не витікає, тож видно рівно те,
// що поклав forwardToViewers.
func quietViewer(ns *nodeSession) *viewerLeg {
	vl := &viewerLeg{
		pc:    &webrtc.PeerConnection{},
		out:   make(chan *rtp.Packet, viewerQueueDepth),
		done:  make(chan struct{}),
		ready: true,
		live:  true,
	}
	ns.mu.Lock()
	if ns.viewers == nil {
		ns.viewers = make(map[*webrtc.PeerConnection]*viewerLeg)
	}
	ns.viewers[vl.pc] = vl
	ns.mu.Unlock()
	return vl
}

func nonKeyPacket(i int) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i) * 3000},
		Payload: []byte{0x21, 0x01, 0x02}, // NAL type 1 — не ключовий
	}
}

// TestForwardAllocsDoNotScaleWithViewers — H-33: пакет ділиться між ногами, а не
// клонується під кожну. Поверни `&rtp.Packet{...}` всередину циклу по глядачах —
// і 8 ніг дадуть на 7 алокацій більше, тест почервоніє.
func TestForwardAllocsDoNotScaleWithViewers(t *testing.T) {
	quietNDJSON(t, nil)

	measure := func(viewers int) float64 {
		ns := &nodeSession{nodeID: "allocs"}
		ns.agentPC = &webrtc.PeerConnection{}
		for i := 0; i < viewers; i++ {
			quietViewer(ns)
		}
		atomic.StoreUint64(&viewerSeqSample, 0) // семпл на 100-му пакеті — геть із заміру
		i := 0
		return testing.AllocsPerRun(50, func() {
			i++
			forwardToViewers(ns, agentGen1, nonKeyPacket(i))
		})
	}

	one, eight := measure(1), measure(8)
	if eight > one {
		t.Fatalf("алокацій на пакет: 1 глядач %.1f, 8 глядачів %.1f — пакет клонується під кожного", one, eight)
	}
	if one > 2 {
		t.Fatalf("алокацій на пакет при одному глядачеві %.1f, want <= 2", one)
	}
}

// ── пункт 41: GOP-кеш ───────────────────────────────────────────────────────

func TestH264KeyPart(t *testing.T) {
	cases := []struct {
		name string
		p    []byte
		want bool
	}{
		{"порожній", nil, false},
		{"одиничний IDR (5)", []byte{0x65, 0x11}, true},
		{"одиничний SPS (7)", []byte{0x67, 0x42}, true},
		{"одиничний PPS (8)", []byte{0x68, 0xCE}, true},
		{"одиничний P (1)", []byte{0x41, 0x9A}, false},
		{"STAP-A зі SPS", []byte{0x78, 0x00, 0x02, 0x67, 0x42, 0x00, 0x02, 0x68, 0xCE}, true},
		{"STAP-A без ключових", []byte{0x78, 0x00, 0x02, 0x41, 0x9A}, false},
		{"FU-A початок IDR", []byte{0x7C, 0x85}, true},
		{"FU-A середина IDR", []byte{0x7C, 0x05}, false},
		{"FU-A початок P", []byte{0x7C, 0x81}, false},
	}
	for _, c := range cases {
		if got := h264KeyPart(c.p); got != c.want {
			t.Errorf("h264KeyPart(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func keyPacket(i int) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i) * 3000},
		Payload: []byte{0x78, 0, 2, 0x67, 0x42, 0, 2, 0x68, 0xCE, 0, 2, 0x65, 0x88}, // STAP-A: SPS+PPS+IDR
	}
}

// TestGopPrimesNewViewer — пункт 41: нога, що стала live, першою читає кеш від
// останнього IDR. Прибери виклик primeViewerLocked у recomputeBinding — черга
// нової ноги лишиться порожньою і тест впаде.
func TestGopPrimesNewViewer(t *testing.T) {
	quietNDJSON(t, nil)

	ns := &nodeSession{nodeID: "gop"}
	ns.agentPC = &webrtc.PeerConnection{}
	first := quietViewer(ns)

	// SPS, потім три P-кадри — рівно те, що має лежати в кеші.
	forwardToViewers(ns, agentGen1, keyPacket(1))
	for i := 2; i <= 4; i++ {
		forwardToViewers(ns, agentGen1, nonKeyPacket(i))
	}
	if len(first.out) != 4 {
		t.Fatalf("перша нога отримала %d пакетів, want 4", len(first.out))
	}

	// Друга нога приходить посеред GOP.
	second := &viewerLeg{
		pc:   &webrtc.PeerConnection{},
		out:  make(chan *rtp.Packet, viewerQueueDepth),
		done: make(chan struct{}),
	}
	ns.mu.Lock()
	ns.viewers[second.pc] = second
	ns.mu.Unlock()
	markViewerReady(ns, second)
	recomputeBinding(ns)

	if len(second.out) != 4 {
		t.Fatalf("нова нога отримала %d кешованих пакетів, want 4 (сірий екран до наступного IDR)", len(second.out))
	}
	if !viewerPrimed(ns, second) {
		t.Fatalf("нога напоєна кешем, але primed=false — hub усе одно попросить зайвий IDR")
	}
	// Порядок і нумерація — egress-простір ноди, той самий, що бачила перша нога.
	prev := uint16(0)
	for i := 0; i < 4; i++ {
		p := <-second.out
		if i > 0 && p.SequenceNumber != prev+1 {
			t.Fatalf("кеш віддано не по порядку: %d після %d", p.SequenceNumber, prev)
		}
		prev = p.SequenceNumber
	}
}

// TestGopResetsOnNewKeyframe — новий ключовий набір ОБНУЛЯЄ кеш: віддавати
// глядачеві хвіст двох GOP означало б відтворити йому вже застаріле відео.
func TestGopResetsOnNewKeyframe(t *testing.T) {
	quietNDJSON(t, nil)
	ns := &nodeSession{nodeID: "gop-reset"}
	ns.agentPC = &webrtc.PeerConnection{}
	quietViewer(ns)

	forwardToViewers(ns, agentGen1, keyPacket(1))
	forwardToViewers(ns, agentGen1, nonKeyPacket(2))
	forwardToViewers(ns, agentGen1, keyPacket(3)) // новий IDR
	forwardToViewers(ns, agentGen1, nonKeyPacket(4))

	ns.mu.Lock()
	n := len(ns.gop.replay())
	ns.mu.Unlock()
	if n != 2 {
		t.Fatalf("у кеші %d пакетів, want 2 (лише останній ключовий набір і те, що після нього)", n)
	}
}

// TestGopOverflowDisablesPriming — розірваний хвіст не віддається взагалі:
// краще чесна пауза до keyframe, ніж картинка з діркою.
func TestGopOverflowDisablesPriming(t *testing.T) {
	quietNDJSON(t, nil)
	var g gopCache
	g.note(keyPacket(1))
	for i := 2; i <= gopMaxPackets+10; i++ {
		g.note(nonKeyPacket(i))
	}
	if !g.overflow {
		t.Fatalf("кеш прийняв понад %d пакетів без ознаки переповнення", gopMaxPackets)
	}
	if g.replay() != nil {
		t.Fatalf("переповнений кеш усе одно віддається глядачеві")
	}
}

// ── F-11: ренегоціація ──────────────────────────────────────────────────────

// TestFindViewerBySessionOnlyWhileAlive — ключ ренегоціації живе рівно стільки,
// скільки сама нога. Це і є вся його безпека: відкликали глядача — dropViewer
// зняв ногу — ключ більше нічого не відчиняє.
func TestFindViewerBySessionOnlyWhileAlive(t *testing.T) {
	ns := reg.getOrCreate("reneg-node")
	t.Cleanup(func() { reg.remove("reneg-node", ns) })

	vl := quietViewer(ns)
	id, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	ns.mu.Lock()
	vl.sessionID = id
	ns.mu.Unlock()

	gotNS, gotVL := findViewerBySession(id)
	if gotNS != ns || gotVL != vl {
		t.Fatalf("живу ногу за session_id не знайдено")
	}
	if n, _ := findViewerBySession(""); n != nil {
		t.Fatalf("порожній session_id щось знайшов")
	}
	if n, _ := findViewerBySession("00000000000000000000000000000000"); n != nil {
		t.Fatalf("чужий session_id щось знайшов")
	}

	removeViewer(ns, vl)
	if n, _ := findViewerBySession(id); n != nil {
		t.Fatalf("ключ мертвої ноги ще працює — це і був би обхід відкликання")
	}
}

// TestRenegotiateUnknownSession404 — невідомий ключ не створює нічого і не
// споживає квитка: чесні 404, далі глядач іде звичайним шляхом.
func TestRenegotiateUnknownSession404(t *testing.T) {
	body := `{"sdp":"v=0","session_id":"deadbeefdeadbeefdeadbeefdeadbeef"}`
	r := httptest.NewRequest(http.MethodPost, "/offer/viewer", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleOffer("viewer")(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("код %d, want 404 (тіло: %q)", w.Code, w.Body.String())
	}
}

// TestRenegotiateRejectsConcurrent — два offer-и на одну ногу одночасно ламали б
// стан pion; другий має отримати 409, а не InvalidStateError.
func TestRenegotiateRejectsConcurrent(t *testing.T) {
	ns := reg.getOrCreate("reneg-busy")
	t.Cleanup(func() { reg.remove("reneg-busy", ns) })
	vl := quietViewer(ns)
	t.Cleanup(func() { removeViewer(ns, vl) })

	id, _ := newSessionID()
	ns.mu.Lock()
	vl.sessionID = id
	ns.mu.Unlock()
	vl.renegotiating.Store(true) // «перший offer уже в роботі»

	w := httptest.NewRecorder()
	renegotiateViewer(w, offerReq{SessionID: id, SDP: "v=0"})
	if w.Code != http.StatusConflict {
		t.Fatalf("код %d, want 409", w.Code)
	}
}
