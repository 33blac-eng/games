package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// newPC — справжня PeerConnection: потрібна там, де тест доходить до Close()
// (dropViewer). Ключ мапи ns.viewers — теж вона.
func newPC(t *testing.T) *webrtc.PeerConnection {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("new pc: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

// addReadyViewer — ще один Connected глядач ноди: те саме, що робить
// setupViewerLeg + перехід у PeerConnectionStateConnected, але без реальної
// негоціації. Повертає ногу з живим pump-ом.
func addReadyViewer(t *testing.T, ns *nodeSession) *viewerLeg {
	t.Helper()
	vl := addViewer(ns, newPC(t), newViewerTrack(t), "u1")
	t.Cleanup(func() { removeViewer(ns, vl) }) // зупиняємо pump після тесту
	markViewerReady(ns, vl)
	recomputeBinding(ns)
	ns.mu.Lock()
	live := vl.live
	ns.mu.Unlock()
	if !live {
		t.Fatalf("[%s] viewer leg not live after Connected with publisher present", ns.nodeID)
	}
	return vl
}

// stalledViewer — модель ПОВІЛЬНОГО глядача без жодних таймінгів: нога без
// pump-а (черга ніхто не спорожняє) і черга вже повна. Наступний пакет у неї
// гарантовано не влізе.
func stalledViewer(t *testing.T, ns *nodeSession, pc *webrtc.PeerConnection) *viewerLeg {
	t.Helper()
	vl := &viewerLeg{
		pc:   pc,
		trk:  newViewerTrack(t),
		out:  make(chan *rtp.Packet, viewerQueueDepth),
		done: make(chan struct{}),
	}
	ns.mu.Lock()
	if ns.viewers == nil {
		ns.viewers = make(map[*webrtc.PeerConnection]*viewerLeg)
	}
	ns.viewers[pc] = vl
	vl.ready, vl.live = true, true
	ns.mu.Unlock()
	for i := 0; i < viewerQueueDepth; i++ {
		vl.out <- &rtp.Packet{}
	}
	return vl
}

// waitSent чекає, поки pump ноги віддасть у трек рівно want пакетів.
func waitSent(t *testing.T, vl *viewerLeg, want uint64, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := atomic.LoadUint64(&vl.sent); got == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s: sent = %d, want %d", what, atomic.LoadUint64(&vl.sent), want)
}

// onlyViewer — єдина нога ноди (тести, що стартують із readyNode).
func (ns *nodeSession) onlyViewer(t *testing.T) *viewerLeg {
	t.Helper()
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if len(ns.viewers) != 1 {
		t.Fatalf("ns.viewers = %d, want 1", len(ns.viewers))
	}
	for _, vl := range ns.viewers {
		return vl
	}
	return nil
}

func viewerCount(ns *nodeSession) int {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return len(ns.viewers)
}

// TestFanoutTwoViewersBothReceive — ЯДРО Ф1: два глядачі на ОДНІЙ ноді
// отримують ті самі кадри. Джерело одне, egress-простір спільний (lastOutSeq
// рахується один раз на ноду, а не по разу на глядача).
func TestFanoutTwoViewersBothReceive(t *testing.T) {
	ns := readyNode(t, "fanout-2") // перший глядач усередині
	v1 := ns.onlyViewer(t)
	v2 := addReadyViewer(t, ns)

	forwardN(ns, 5, 100, 900000)

	waitSent(t, v1, 5, "viewer1")
	waitSent(t, v2, 5, "viewer2")

	ns.mu.Lock()
	outSeq := ns.lastOutSeq
	ns.mu.Unlock()
	if outSeq != 4 {
		t.Fatalf("lastOutSeq = %d, want 4 (5 packets, ОДИН egress-простір на ноду)", outSeq)
	}
}

// TestFanoutViewerLeaveKeepsOther — відʼєднання одного глядача не рве потік
// іншому й не чіпає agent-ногу.
func TestFanoutViewerLeaveKeepsOther(t *testing.T) {
	ns := readyNode(t, "fanout-leave")
	v1 := ns.onlyViewer(t)
	v2 := addReadyViewer(t, ns)

	forwardN(ns, 3, 10, 1000)
	waitSent(t, v1, 3, "viewer1 before leave")
	waitSent(t, v2, 3, "viewer2 before leave")

	dropViewer(ns, v1, "тест: глядач пішов")

	forwardN(ns, 4, 13, 4000)
	waitSent(t, v2, 7, "viewer2 after viewer1 left")

	if got := atomic.LoadUint64(&v1.sent); got != 3 {
		t.Fatalf("viewer1 sent = %d after leaving, want 3 (нічого не мало доїхати)", got)
	}
	if n := viewerCount(ns); n != 1 {
		t.Fatalf("ns.viewers = %d after one leg dropped, want 1", n)
	}
	ns.mu.Lock()
	agentAlive := ns.agentPC != nil
	ns.mu.Unlock()
	if !agentAlive {
		t.Fatalf("agent leg torn down by a viewer leaving")
	}
}

// TestFanoutGateZeroPauseFirstResume — гейтинг: "pause" лише коли глядачів
// НУЛЬ, "resume" на ПЕРШОМУ. Перевіряємо предикат sendGate (hasReadyViewerLocked)
// і ознаку "перший", по якій setupViewerLeg вирішує скидати стелю бітрейту.
func TestFanoutGateZeroPauseFirstResume(t *testing.T) {
	ns := &nodeSession{nodeID: "gate"}
	ns.mu.Lock()
	ns.agentPC = &webrtc.PeerConnection{}
	ns.mu.Unlock()

	present := func() bool {
		ns.mu.Lock()
		defer ns.mu.Unlock()
		return hasReadyViewerLocked(ns)
	}

	if present() {
		t.Fatalf("нуль глядачів, а гейт каже resume")
	}

	// Нога є, але ще не Connected — це ще не глядач.
	v1 := addViewer(ns, newPC(t), newViewerTrack(t), "u1")
	t.Cleanup(func() { removeViewer(ns, v1) })
	if present() {
		t.Fatalf("нога додана, але не Connected — гейт має лишатись pause")
	}

	if first := markViewerReady(ns, v1); !first {
		t.Fatalf("markViewerReady(перший глядач) = false, want true (перехід 0->1 = resume)")
	}
	if !present() {
		t.Fatalf("перший глядач Connected, а гейт не resume")
	}

	v2 := addReadyViewerNotFirst(t, ns)

	// Пішов ПЕРШИЙ — другий лишився, паузи бути не має.
	dropViewer(ns, v1, "тест")
	if !present() {
		t.Fatalf("гейт впав у pause, хоч другий глядач ще дивиться")
	}

	// Пішов ОСТАННІЙ — тільки тепер pause.
	dropViewer(ns, v2, "тест")
	if present() {
		t.Fatalf("останній глядач пішов, а гейт не pause")
	}
}

// addReadyViewerNotFirst — другий і далі глядач: markViewerReady МУСИТЬ сказати
// "не перший", інакше hub скидав би стелю бітрейту на кожному приєднанні.
func addReadyViewerNotFirst(t *testing.T, ns *nodeSession) *viewerLeg {
	t.Helper()
	vl := addViewer(ns, newPC(t), newViewerTrack(t), "u2")
	t.Cleanup(func() { removeViewer(ns, vl) })
	if first := markViewerReady(ns, vl); first {
		t.Fatalf("markViewerReady(другий глядач) = true, want false (стелю скидати не можна)")
	}
	recomputeBinding(ns)
	return vl
}

// TestFanoutSlowViewerDoesNotBlockOthers — повільний глядач (черга повна, ніхто
// її не читає) не гальмує цикл форвардингу: швидкий отримує всі пакети.
//
// H-12 змінив другу половину контракту. Раніше повільну ногу рвали на ПЕРШОМУ
// ж переповненні; тепер вона переходить у drop-to-IDR і лишається в ноді —
// рвуть лише того, кому скидання кадрів не допомогло viewerOverflowStreakMax
// разів поспіль (див. TestSlowViewerDroppedOnlyAfterStreak).
func TestFanoutSlowViewerDoesNotBlockOthers(t *testing.T) {
	ns := readyNode(t, "fanout-slow")
	fast := ns.onlyViewer(t)
	slow := stalledViewer(t, ns, newPC(t))

	done := make(chan struct{})
	go func() {
		forwardN(ns, 3, 1, 90)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("forwardToViewers заблокувався на повільному глядачі")
	}

	waitSent(t, fast, 3, "швидкий глядач")

	ns.mu.Lock()
	_, stillThere := ns.viewers[slow.pc]
	discarding := slow.discarding
	ns.mu.Unlock()
	if !stillThere {
		t.Fatalf("повільного глядача відірвано на першому ж переповненні — H-12 скасовано")
	}
	if !discarding {
		t.Fatalf("повільний глядач не в drop-to-IDR, хоч його черга переповнилась")
	}
	if n := viewerCount(ns); n != 2 {
		t.Fatalf("ns.viewers = %d, want 2 (обидва живі, повільний — у drop-to-IDR)", n)
	}
}

// TestFanoutWorstViewerDrivesBitrate — правило адаптації при кількох глядачах:
// потік один на всіх, тож контролер веде НАЙГІРША нога, а не та, чий RR щойно
// прийшов. Прострочені RR (viewerRRStale) у розрахунок не входять.
func TestFanoutWorstViewerDrivesBitrate(t *testing.T) {
	ns := readyNode(t, "worst")
	bad := ns.onlyViewer(t)
	good := addReadyViewer(t, ns)

	now := time.Now()
	// Погана нога відзвітувала 30% втрат і великий jitter.
	if loss, jit, _ := worstViewerRR(ns, bad, 0.30, 9000, 0, now); loss != 0.30 || jit != 9000 {
		t.Fatalf("власний RR ноги спотворено: loss=%v jitter=%d", loss, jit)
	}
	// Тепер RR доброї ноги: вона бачить 0%, але керувати має найгірша.
	loss, jit, _ := worstViewerRR(ns, good, 0, 100, 0, now)
	if loss != 0.30 {
		t.Fatalf("worst loss = %v, want 0.30 (веде найгірший глядач, не останній RR)", loss)
	}
	if jit != 9000 {
		t.Fatalf("worst jitter = %d, want 9000 (веде найгірший глядач)", jit)
	}

	// Той самий RR доброї ноги, але звіт поганої вже протух -> вона не рахується.
	later := now.Add(viewerRRStale + time.Second)
	loss, jit, _ = worstViewerRR(ns, good, 0, 100, 0, later)
	if loss != 0 || jit != 100 {
		t.Fatalf("протухлий RR усе ще тягне вниз: loss=%v jitter=%d, want 0/100", loss, jit)
	}
}

// targetOf — поточна ціль контролера ноди.
func targetOf(ns *nodeSession) uint64 {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return ns.bitrate.target
}

// twoLegNode — нода з двома готовими глядачами й відомою стелею: bad уже
// відзвітував втрати, good чистий. agentCtrl нульовий, тож відправка цілі —
// тихий no-op: перевіряємо саме рішення контролера, без мережі.
func twoLegNode(t *testing.T, id string, badLoss float64, now time.Time) (ns *nodeSession, bad, good *viewerLeg) {
	t.Helper()
	ns = readyNode(t, id)
	bad = ns.onlyViewer(t)
	good = addReadyViewer(t, ns)
	setStartBitrate(ns, 8_000_000)
	worstViewerRR(ns, bad, badLoss, 0, 0, now)
	return ns, bad, good
}

// TestFanoutWorstLegDrivesController — правило "найгіршого глядача" доведене не
// на worstViewerRR, а на самій ЦІЛІ: RR далі приходять ЛИШЕ від чистої ноги, а
// ціль мусить поїхати вниз через ДРУГУ ногу, яка втрачає пакети. І навпаки:
// щойно погана нога відпала, ціль має відпустити й повзти назад.
func TestFanoutWorstLegDrivesController(t *testing.T) {
	now := t0
	ns, bad, good := twoLegNode(t, "worst-ctl", 0.30, now)

	for i := 1; i <= 5; i++ {
		now = now.Add(time.Second)
		loss, jit, ex := worstViewerRR(ns, good, 0, 0, 0, now)
		onReceiverReport(ns, loss, jit, ex, now)
	}
	cut := targetOf(ns)
	if cut >= 8_000_000 {
		t.Fatalf("ціль %d — контролер не побачив 30%% втрат СУСІДНЬОЇ ноги", cut)
	}

	// Погана нога пішла: її звіту більше не існує, тримати всіх унизу нема кому.
	removeViewer(ns, bad)
	for i := 1; i <= 40; i++ {
		now = now.Add(time.Second)
		loss, jit, ex := worstViewerRR(ns, good, 0, 0, 0, now)
		onReceiverReport(ns, loss, jit, ex, now)
	}
	if got := targetOf(ns); got <= cut {
		t.Fatalf("погана нога відпала, а ціль лишилась на %d (було %d) — не відпускає", got, cut)
	}
}

// НЕГАТИВНИЙ КОНТРОЛЬ до ДРУГОЇ половини: поки погана нога на місці й далі
// звітує втрати, ціль підніматись НЕ має. Інакше "відпустило, щойно вона
// відпала" доводило б лише те, що контролер узагалі вміє повзти вгору.
func TestFanoutWorstLegHoldsWhileBadStays(t *testing.T) {
	now := t0
	ns, bad, good := twoLegNode(t, "worst-ctl-hold", 0.30, now)

	for i := 1; i <= 40; i++ {
		now = now.Add(time.Second)
		worstViewerRR(ns, bad, 0.30, 0, 0, now) // її RR не протухає
		loss, jit, ex := worstViewerRR(ns, good, 0, 0, 0, now)
		onReceiverReport(ns, loss, jit, ex, now)
	}
	if got := targetOf(ns); got != minBitrateBps {
		t.Fatalf("негативний контроль: погана нога на місці, а ціль %d, не підлога %d", got, minBitrateBps)
	}
}

// НЕГАТИВНИЙ КОНТРОЛЬ: якби ціль вів ОСТАННІЙ звіт, а не найгірший, той самий
// сценарій не зрушив би її з місця. Тобто перевірка вище ловить саме правило, а
// не просто факт, що контролер уміє знижувати ціль.
func TestFanoutWorstLegNegativeControl(t *testing.T) {
	now := t0
	ns, _, _ := twoLegNode(t, "worst-ctl-neg", 0.30, now)

	for i := 1; i <= 5; i++ {
		now = now.Add(time.Second)
		onReceiverReport(ns, 0, 0, 0, now) // свідомо в обхід worstViewerRR
	}
	if got := targetOf(ns); got != 8_000_000 {
		t.Fatalf("негативний контроль: ціль %d зрушила БЕЗ правила найгіршого", got)
	}
}

// TestFanoutWorstLegDrivesRTT — RTT ведеться ОКРЕМО ПО КОЖНІЙ нозі, бо глядачі
// сидять у різних мережах: у далекої ноги абсолютний RTT великий НАЗАВЖДИ, у
// близької маленький, і порівнювати їх між собою безглуздо. Керує та, у якої
// більший ПРИРІСТ над власним мінімумом — тут це нога, під якою наливається
// буфер. Доводимо це так само, як правило найгіршого по втратах: у контролер
// ідуть RR лише ЧИСТОЇ далекої ноги, а ціль мусить поїхати вниз через ДРУГУ.
func TestFanoutWorstLegDrivesRTT(t *testing.T) {
	now := t0
	ns := readyNode(t, "worst-rtt")
	far := ns.onlyViewer(t)          // 250 мс стабільно: просто далеко, не затор
	bloated := addReadyViewer(t, ns) // 100 мс і росте на 80 мс за звіт
	setStartBitrate(ns, 8_000_000)

	for i := 1; i <= 10; i++ {
		now = now.Add(time.Second)
		worstViewerRR(ns, bloated, 0, 0, 20*time.Millisecond+time.Duration(i)*80*time.Millisecond, now)
		loss, jit, ex := worstViewerRR(ns, far, 0, 0, 250*time.Millisecond, now)
		onReceiverReport(ns, loss, jit, ex, now) // втрат НУЛЬ у обох
	}
	if got := targetOf(ns); got >= 8_000_000 {
		t.Fatalf("ціль %d при нульових втратах — приріст RTT СУСІДНЬОЇ ноги контролер не побачив", got)
	}
}

// TestStandingQueueHoldsTargetThroughFullPath — ДВІ правки разом, через увесь
// справжній шлях (worstViewerRR -> onReceiverReport), а не на чистому step().
//
// Питання рівно одне, і поодинці жоден із тестів на нього не відповідає: база
// приросту тепер старіє, тож теоретично вона могла б наздогнати чергу, приріст
// схлопнувся б, і блокування підйому тихо відкрилось би посеред затору. Тут
// черга СТОЇТЬ дві з половиною хвилини — довше за будь-який заміряний епізод,
// але коротше за вікно бази, — і ціль мусить лишитись на одному зрізі.
func TestStandingQueueHoldsTargetThroughFullPath(t *testing.T) {
	now := t0
	ns := readyNode(t, "standing-queue")
	vl := ns.onlyViewer(t)
	setStartBitrate(ns, 8_000_000)

	// Спокійний шлях: 10 с по 100 мс — це і є здорове дно ноги.
	for i := 0; i < 10; i++ {
		now = now.Add(time.Second)
		loss, jit, ex := worstViewerRR(ns, vl, 0, 0, 100*time.Millisecond, now)
		onReceiverReport(ns, loss, jit, ex, now)
	}
	if got := targetOf(ns); got != 8_000_000 {
		t.Fatalf("здоровий шлях 100мс уже збив ціль до %d", got)
	}

	// Черга налилась і СТОЇТЬ: 400 мс, приріст 300 мс, втрат нуль.
	for i := 0; i < 150; i++ {
		now = now.Add(time.Second)
		loss, jit, ex := worstViewerRR(ns, vl, 0, 0, 400*time.Millisecond, now)
		onReceiverReport(ns, loss, jit, ex, now)
	}
	// Один зріз по рівню (засувка) і НІ КРОКУ вгору, поки черга на місці.
	if got := targetOf(ns); got != 6_800_000 {
		t.Fatalf("черга стоїть 150с, а ціль %d замість 6800000 — або зрізів більше одного, або підйом відкрився посеред затору", got)
	}
}

// TestMinRTTAgesButOutlastsCongestion — БАЗА приросту мусить старіти, але
// повільніше за затор. Обидва боки в ОДНІЙ перевірці навмисно: порізно кожен
// проходиться зламаною логікою задарма — «база ніколи не старіє» (як було)
// пройде затор, «база старіє миттєво» пройде переїзд.
//
// Вхід в обох половинах ОДНАКОВИЙ — сталі 400 мс над базою 100 мс. Це і є суть:
// затор і нова базова лінія дають контролеру рівно один і той самий сигнал, і
// відрізняє їх ЛИШЕ тривалість. Тож вікно й перевіряємо годинником, не рівнями.
func TestMinRTTAgesButOutlastsCongestion(t *testing.T) {
	const tick = 2 * time.Second // RR приходить раз на кілька секунд

	vl := &viewerLeg{}
	now := t0
	vl.observeRTT(100*time.Millisecond, now) // здорове дно шляху

	// (б) ЗАТОР типової тривалості: черга налилась і тримається. 90 с — з
	// запасом більше за найдовший заміряний епізод (повільна рампа ~60 с).
	// База рушити НЕ має, інакше приріст схлопнеться і затор стане невидимим.
	for now.Sub(t0) < 90*time.Second {
		now = now.Add(tick)
		vl.observeRTT(400*time.Millisecond, now)
	}
	if ex := vl.rttExcess(); ex != 300*time.Millisecond {
		t.Fatalf("під час затору приріст %v, а не 300ms — база наздогнала чергу, затор невидимий", ex)
	}

	// (а) ТА САМА затримка, але надовго: людина переїхала, 400 мс тепер здорове
	// дно. За два вікна база мусить піднятись, приріст — впасти в нуль, інакше
	// контролер тримав би заниженою ціль на цілком здоровому зʼєднанні.
	for now.Sub(t0) <= 2*minRTTWindow {
		now = now.Add(tick)
		vl.observeRTT(400*time.Millisecond, now)
	}
	if ex := vl.rttExcess(); ex != 0 {
		t.Fatalf("через %v сталих 400ms приріст усе ще %v — база не старіє", now.Sub(t0), ex)
	}
}

// НЕГАТИВНИЙ КОНТРОЛЬ: та сама далека нога САМА, без сусідки з буфером. Сталий
// RTT 250 мс ціль рухати не має — інакше перевірка вище доводила б лише те, що
// великий RTT ріже завжди, і далекий глядач був би покараний за відстань.
func TestFanoutFarLegAloneDoesNotCut(t *testing.T) {
	now := t0
	ns := readyNode(t, "far-alone")
	far := ns.onlyViewer(t)
	setStartBitrate(ns, 8_000_000)

	for i := 1; i <= 30; i++ {
		now = now.Add(time.Second)
		loss, jit, ex := worstViewerRR(ns, far, 0, 0, 250*time.Millisecond, now)
		onReceiverReport(ns, loss, jit, ex, now)
	}
	if got := targetOf(ns); got != 8_000_000 {
		t.Fatalf("негативний контроль: далекий глядач зі СТАЛИМ RTT 250 мс збив ціль до %d", got)
	}
}
