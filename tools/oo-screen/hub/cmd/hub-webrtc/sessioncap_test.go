package main

import (
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Стеля сесії — груба засувка проти забутої вкладки: 31.08 одна така тримала
// сесію пʼять годин, і весь той час агент кодував, а запис ріс.

func withSessionCap(t *testing.T, d time.Duration) {
	t.Helper()
	prev := sessionCap
	t.Cleanup(func() { sessionCap = prev })
	sessionCap = d
}

// Забуту ногу сторож рве сам. Перевіряємо саму watchSessionCap, а не її
// перемальовану копію: dropViewer знімає ногу з мапи ноди — це й спостерігаємо.
func TestSessionCapDropsForgottenViewer(t *testing.T) {
	withSessionCap(t, 50*time.Millisecond)

	pc := newClosablePC(t)
	ns := &nodeSession{nodeID: "node-cap"}
	vl := &viewerLeg{pc: pc, out: make(chan *rtp.Packet, 1), done: make(chan struct{})}
	ns.viewers = map[*webrtc.PeerConnection]*viewerLeg{pc: vl}

	go watchSessionCap(ns, vl, sessionCap)

	deadline := time.After(eventGuard)
	for {
		ns.mu.Lock()
		n := len(ns.viewers)
		ns.mu.Unlock()
		if n == 0 {
			return // сторож зняв ногу — саме те, чого чекали
		}
		select {
		case <-deadline:
			t.Fatal("стеля не спрацювала — забута сесія жила б вічно")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// newClosablePC — найдешевший справжній PeerConnection: dropViewer кличе на
// ньому Close(), тож підробка тут не годиться.
func newClosablePC(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// Нога, знята вчасно, стелю не будить: сторож мусить вийти по done і НЕ
// чіпати ноду. Без цієї половини «фікс» можна було б зробити, рвучи все підряд.
func TestSessionCapSilentWhenViewerLeavesEarly(t *testing.T) {
	withSessionCap(t, 2*time.Second)

	pc := newClosablePC(t)
	ns := &nodeSession{nodeID: "node-early"}
	vl := &viewerLeg{pc: pc, out: make(chan *rtp.Packet, 1), done: make(chan struct{})}
	ns.viewers = map[*webrtc.PeerConnection]*viewerLeg{pc: vl}

	returned := make(chan struct{})
	go func() { watchSessionCap(ns, vl, sessionCap); close(returned) }()

	close(vl.done) // глядач пішов сам, задовго до стелі

	select {
	case <-returned:
	case <-time.After(eventGuard):
		t.Fatal("сторож не вийшов після зняття ноги — лишилась горутина до самої стелі")
	}
	ns.mu.Lock()
	n := len(ns.viewers)
	ns.mu.Unlock()
	if n != 1 {
		t.Errorf("сторож зачепив ногу, яку зняли без нього: у мапі %d", n)
	}
}

// Нуль вимикає стелю зовсім — свідомий режим для довгого налагодження.
func TestSessionCapZeroDisables(t *testing.T) {
	withSessionCap(t, 0)
	if sessionCap > 0 {
		t.Fatal("нуль мав вимкнути стелю")
	}
	// watchSessionCap з нульовою стелею мусить вийти одразу, не лишивши
	// горутини й не чіпаючи ногу.
	ns := &nodeSession{nodeID: "node-off"}
	vl := &viewerLeg{done: make(chan struct{})}
	returned := make(chan struct{})
	go func() { watchSessionCap(ns, vl, sessionCap); close(returned) }()
	select {
	case <-returned:
	case <-time.After(eventGuard):
		t.Fatal("з вимкненою стелею сторож не вийшов — лишилась горутина")
	}
}

// envDuration: нерозбірне значення не має тихо ставати типовим мовчки — воно
// логується, а типове повертається (людина не лишається з хибним переконанням,
// що її налаштування діє).
func TestEnvDurationParsing(t *testing.T) {
	const def = 90 * time.Minute
	t.Setenv("OO_TEST_DUR", "")
	if got := envDuration("OO_TEST_DUR", def); got != def {
		t.Errorf("порожнє -> %v, want %v", got, def)
	}
	t.Setenv("OO_TEST_DUR", "45m")
	if got := envDuration("OO_TEST_DUR", def); got != 45*time.Minute {
		t.Errorf("45m -> %v", got)
	}
	t.Setenv("OO_TEST_DUR", "не-тривалість")
	if got := envDuration("OO_TEST_DUR", def); got != def {
		t.Errorf("сміття -> %v, want типове %v", got, def)
	}
}

// 🔴 Найважливіший тест файлу: сторожа СПРАВДІ підключено до бойового шляху.
//
// Решта тестів кличуть watchSessionCap напряму — і тому лишаються зеленими,
// якщо прибрати його виклик з addViewer (перевірено: негативний контроль не
// почервонів). Отже потрібен тест, який іде тим самим входом, що й жива нога.
func TestAddViewerArmsSessionCap(t *testing.T) {
	withSessionCap(t, 50*time.Millisecond)

	pc := newClosablePC(t)
	trk, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "probe")
	if err != nil {
		t.Fatalf("NewTrackLocalStaticRTP: %v", err)
	}

	ns := &nodeSession{nodeID: "node-armed"}
	addViewer(ns, pc, trk, "user-1")

	deadline := time.After(eventGuard)
	for {
		ns.mu.Lock()
		n := len(ns.viewers)
		ns.mu.Unlock()
		if n == 0 {
			return // addViewer завів сторожа, і той зняв ногу
		}
		select {
		case <-deadline:
			t.Fatal("addViewer не завів сторожа стелі — забуті сесії жили б вічно")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
