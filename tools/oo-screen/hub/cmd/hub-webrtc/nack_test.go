package main

import (
	"testing"
	"time"

	"github.com/pion/rtcp"
)

// nackFor — NACK рівно на перелічені seq: одна NackPair на кожен, порожня
// бітова маска. Так кількість запитаних seq у тесті дорівнює len(seqs) точно, а
// не «PacketID плюс скільки там бітів».
func nackFor(seqs ...uint16) *rtcp.TransportLayerNack {
	n := &rtcp.TransportLayerNack{MediaSSRC: 1}
	for _, s := range seqs {
		n.Nacks = append(n.Nacks, rtcp.NackPair{PacketID: s})
	}
	return n
}

// legAt — нога, яка вже віддала sent пакетів і стоїть на seq highest. Реальний
// PeerConnection тут не потрібен: onNack працює лише зі станом ноги, ns.mu і
// «зараз».
func legAt(sent uint64, highest uint16) *viewerLeg {
	return &viewerLeg{sent: sent, lastSeq: uint32(highest)}
}

// seqRange — count послідовних seq, починаючи з from (уперед).
func seqRange(from uint16, count int) []uint16 {
	out := make([]uint16, count)
	for i := range out {
		out[i] = from + uint16(i)
	}
	return out
}

// TestNackRecoverableWindow — умова, за якою pion rtpbuffer.Get віддасть пакет.
// Найважливіші тут не «свіжий/старий», а два краї: seq З МАЙБУТНЬОГО (хаб його
// ще не писав) і перехід через межу uint16 — саме на них наївне порівняння
// ламається.
func TestNackRecoverableWindow(t *testing.T) {
	const window = sharedNackSize // типовий nackBufferSize
	cases := []struct {
		name    string
		highest uint16
		seq     uint16
		want    bool
	}{
		{"найновіший", 1000, 1000, true},
		{"на краю вікна", 5000, 5000 - (window - 1), true},
		{"на один за краєм", 5000, 5000 - window, false},
		{"давно випав", 1000, 65000, false},
		{"з майбутнього", 1000, 1001, false},
		{"через межу uint16", 1000, 65535, true},
		{"через межу, задалеко", 500, 60000, false},
	}
	for _, c := range cases {
		if got := nackRecoverable(c.highest, c.seq, window); got != c.want {
			t.Errorf("%s: nackRecoverable(%d,%d,%d) = %v, want %v",
				c.name, c.highest, c.seq, window, got, c.want)
		}
	}
}

// TestNackWindowForShortLeg — нога, що підключилась щойно, не має в кільці
// нічого старшого за власний перший пакет: вікно обрізається довжиною сесії,
// інакше частку задоволених було б завищено на старті кожного глядача.
func TestNackWindowForShortLeg(t *testing.T) {
	if got := nackWindowFor(10); got != 10 {
		t.Fatalf("nackWindowFor(10) = %d, want 10", got)
	}
	if got := nackWindowFor(uint64(nackBufferSize - 1)); got != uint16(nackBufferSize-1) {
		t.Fatalf("nackWindowFor(%d) = %d, want %d", nackBufferSize-1, got, nackBufferSize-1)
	}
	if got := nackWindowFor(10000); got != uint16(nackBufferSize) {
		t.Fatalf("nackWindowFor(10000) = %d, want %d", got, nackBufferSize)
	}
}

// TestNackHighRatioKeepsNack — НЕГАТИВНИЙ КОНТРОЛЬ: усе запитане лежить у
// буфері, NACK працює. Вікно закривається, частка рахується, але на PLI не
// переходимо. Якщо цей тест червоний — правило смикає keyframe на здоровій нозі.
func TestNackHighRatioKeepsNack(t *testing.T) {
	ns := &nodeSession{nodeID: "n"}
	vl := legAt(5000, 1000)
	t0 := time.Now()

	// Усі 20 seq — у вікні (1000 назад від найновішого).
	good := nackFor(seqRange(981, 20)...)
	if st := onNack(ns, vl, good, t0); st.closed {
		t.Fatalf("вікно закрилось одразу: %+v", st)
	}
	st := onNack(ns, vl, good, t0.Add(nackWindow))
	if !st.closed {
		t.Fatalf("вікно не закрилось після %v", nackWindow)
	}
	if st.ratio != 1 {
		t.Fatalf("ratio = %.2f (hit=%d req=%d), want 1.00", st.ratio, st.hit, st.req)
	}
	if st.escalate {
		t.Fatalf("перехід на PLI при 100%% задоволених NACK")
	}
}

// TestNackLowRatioEscalatesToPLI — ГЕЙТ МАЄ ВМІТИ ЗАЧЕРВОНІТИ: жоден запитаний
// seq у буфер не потрапляє (усі старші за вікно), тобто ретрансмісія цій нозі
// не допомагає взагалі -> переходимо на keyframe і ставимо витримку.
func TestNackLowRatioEscalatesToPLI(t *testing.T) {
	ns := &nodeSession{nodeID: "n"}
	vl := legAt(50_000, 40_000)
	t0 := time.Now()

	// 20 seq на 5000 пакетів назад — гарантовано за межами кільця на 1024.
	bad := nackFor(seqRange(35_000, 20)...)
	onNack(ns, vl, bad, t0)
	st := onNack(ns, vl, bad, t0.Add(nackWindow))
	if !st.closed {
		t.Fatalf("вікно не закрилось")
	}
	if st.ratio != 0 {
		t.Fatalf("ratio = %.2f (hit=%d req=%d), want 0.00", st.ratio, st.hit, st.req)
	}
	if !st.escalate {
		t.Fatalf("NACK не задовольняється взагалі, а переходу на PLI немає")
	}
	ns.mu.Lock()
	until := vl.pliUntil
	ns.mu.Unlock()
	if want := t0.Add(nackWindow + nackFallbackHold); !until.Equal(want) {
		t.Fatalf("pliUntil = %v, want %v", until, want)
	}
}

// TestNackTooFewSamplesNoEscalation — НЕГАТИВНИЙ КОНТРОЛЬ на поріг вибірки:
// кілька загублених пакетів за дві секунди — це не «NACK не працює», це просто
// кілька загублених пакетів. Судити нема на чому.
func TestNackTooFewSamplesNoEscalation(t *testing.T) {
	ns := &nodeSession{nodeID: "n"}
	vl := legAt(50_000, 40_000)
	t0 := time.Now()

	bad := nackFor(seqRange(35_000, 4)...) // 4 < nackMinSamples
	onNack(ns, vl, bad, t0)
	st := onNack(ns, vl, bad, t0.Add(nackWindow))
	if !st.closed || st.req >= nackMinSamples {
		t.Fatalf("тест непридатний: closed=%v req=%d (потрібно < %d)", st.closed, st.req, nackMinSamples)
	}
	if st.escalate {
		t.Fatalf("перехід на PLI з вибіркою %d запитів", st.req)
	}
}

// TestNackNoPacketsSentNoJudgement — нога ще нічого не віддала: буфера
// ретрансмісії в неї фізично немає, і 0/0 не має читатись як «0%».
func TestNackNoPacketsSentNoJudgement(t *testing.T) {
	ns := &nodeSession{nodeID: "n"}
	vl := legAt(0, 0)
	st := onNack(ns, vl, nackFor(seqRange(1, 20)...), time.Now())
	if st.closed || st.escalate {
		t.Fatalf("судимо ногу без жодного відданого пакета: %+v", st)
	}
}

// TestNackFallbackHoldThenRetry — витримка робить рівно те, що обіцяє: після
// переходу на PLI наступне погане вікно всередині nackFallbackHold агента НЕ
// смикає, а після витримки правило пробує знову (а не залипає назавжди).
func TestNackFallbackHoldThenRetry(t *testing.T) {
	ns := &nodeSession{nodeID: "n"}
	vl := legAt(50_000, 40_000)
	bad := nackFor(seqRange(35_000, 20)...)
	t0 := time.Now()

	// Вікно 1: t0..t0+2s -> перехід на PLI, витримка до t0+5s.
	onNack(ns, vl, bad, t0)
	if st := onNack(ns, vl, bad, t0.Add(nackWindow)); !st.escalate {
		t.Fatalf("перше погане вікно не дало переходу на PLI")
	}

	// Вікно 2: t0+2s..t0+4s -> ще всередині витримки, мовчимо.
	onNack(ns, vl, bad, t0.Add(nackWindow))
	if st := onNack(ns, vl, bad, t0.Add(2*nackWindow)); st.escalate {
		t.Fatalf("повторний PLI всередині витримки %v", nackFallbackHold)
	}

	// Вікно 3: t0+4s..t0+6s -> витримка (5s) минула, пробуємо знову.
	onNack(ns, vl, bad, t0.Add(2*nackWindow))
	if st := onNack(ns, vl, bad, t0.Add(3*nackWindow)); !st.escalate {
		t.Fatalf("після витримки правило залипло: повторного переходу на PLI немає")
	}
}

// TestPLIGateDebounces — підсилювач каскаду в fanout: N глядачів однієї ноди,
// що втратили той самий пакет, мають коштувати агентові ОДИН keyframe.
func TestPLIGateDebounces(t *testing.T) {
	ns := &nodeSession{nodeID: "n"}
	t0 := time.Now()

	if !pliGate(ns, t0) {
		t.Fatalf("перший PLI заблоковано")
	}
	// Дев'ять інших глядачів у ту саму мілісекунду.
	for i := 0; i < 9; i++ {
		if pliGate(ns, t0.Add(time.Millisecond)) {
			t.Fatalf("глядач #%d пробив дебаунс: агент отримає N keyframe на одну втрату", i+2)
		}
	}
	if pliGate(ns, t0.Add(pliDebnc-time.Millisecond)) {
		t.Fatalf("PLI пройшов раніше за дебаунс %v", pliDebnc)
	}
	if !pliGate(ns, t0.Add(pliDebnc)) {
		t.Fatalf("PLI не пройшов після дебаунсу %v — глядач лишиться без картинки", pliDebnc)
	}
}
