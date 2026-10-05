//go:build windows

package main

import (
	"context"
	"errors"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/organicoils/oo-screen/agent/capture"
	"github.com/organicoils/oo-screen/agent/encode"
	"github.com/organicoils/oo-screen/internal/control"
)

// agent — стан живого агента після старту: транспорт до хаба, колбеки
// control-каналу, кадровий цикл і його лічильники. Раніше все це було
// локальними змінними й замиканнями однієї 850-рядкової main(); тепер кожна
// частина — окремий метод, а спільний стан видно в одному місці.
//
// Хто що чіпає:
//   - колбеки pion/QUIC (onGate, onBitrateTarget, onSelectOutput,
//     onKeyframeRequest, onDown) — лише атомарні прапорці й канали;
//   - tp — під tpMu (кадровий цикл, sender, звук, сторож доступності);
//   - решта полів нижче «кадрового циклу» — ВИКЛЮЧНО горутина run().
type agent struct {
	s             *stream
	session       *sessionWatch
	transportKind string
	hubAddr       string
	// frameInterval рахуємо ДО dial: webrtcTransport бере його як тривалість
	// першого AU (sampleDuration), і кожен реконект створює транспорт заново.
	frameInterval time.Duration

	// on-demand гейтинг: дефолт — НЕ пауза (безпечний фолбек = стара always-on
	// поведінка, якщо hub не шле сигналів). Hub шле "pause" щойно відкриється
	// control-канал і глядача нема, тож без глядача агент іде в паузу за ~мс.
	gatePaused atomic.Bool
	// gateSeen — «хаб цієї сесії вже сказав своє слово про гейт». Потрібен
	// РІВНО одному місцю: A-28-паузі на час реконекту, яка мусить відрізнити
	// «прапорець стоїть, бо його поставили ми» від «прапорець стоїть, бо так
	// вирішив новий хаб». Без цього відновлення після дозвону затирало б
	// свіжий pause, що прийшов по щойно відкритому контрол-каналу.
	gateSeen      atomic.Bool
	bitrateWanted bitrateTarget
	// pcDown — друга (і головна) причина реконекту поряд із txErrCh: стан
	// PeerConnection. Буфер 1 + неблокуючий запис: причина потрібна одна, а
	// обробник стану pion блокувати не можна.
	pcDown chan string

	tpMu      sync.Mutex // guards tp across reconnects
	tp        transport
	txErrCh   chan error
	sendQueue chan sendJob
	queued    atomic.Int64 // # AUs enqueued but not yet sent — admission signal
	dropped   atomic.Int64
	sent      atomic.Int64

	// --- кадровий цикл (лише горутина run) ---
	seq        uint64 // envelope/output frame sequence (wt only; monotonic per AU sent)
	captureSeq uint64 // input frame counter, drives encoder PTS independent of drops/output seq
	lastLog    time.Time
	keepalives int // скільки разів переслали останній кадр (нерухомий екран)
	throttled  int // скільки кадрів викинув бюджет CPU софт-енкодера чи «Швидкість»
	// lastSeqAt — стінний час кадру, від якого рахуємо зсув PTS. Не час
	// виклику NextFrame: між кадрами ще є кодування й відправка. Ставимо
	// перед самим циклом — рукостискання транспорту до потоку не належить.
	lastSeqAt time.Time
	// lastAdmitAt — стінний час ОСТАННЬОГО кадру, який admission пропустив
	// далі. Окремо від lastSeqAt: той рухається і на дропнутих кадрах (PTS
	// іде за стінним годинником), а межу паузи треба міряти саме по тому,
	// що дійсно поїхало в транспорт.
	lastAdmitAt time.Time
	// A-03: поки екрана нема (лок-скрін, UAC, капчер відновлює дублікацію),
	// шлемо повтор останнього keepalive-AU — «нічого не змінилось» P-кадру.
	// Декодер глядача копіює референс, сторож у браузері бачить свіжий кадр і
	// не рве сесію на кожному UAC. AU дійсний лише для ТОГО енкодера, що його
	// видав (інша геометрія/SPS = сміття), тому памʼятаємо й енкодер.
	lastStillAU     *encode.AU
	lastStillEnc    *encode.Encoder
	lastStillSentAt time.Time
	// lastFrameEncoded — s.lastFrame уже пройшов через енкодер (keepStillAU).
	lastFrameEncoded bool
	reacqBackoff     time.Duration
	suspended        bool      // A-17: дублікацію віддано на паузі
	lastIDRAt        time.Time // A-31: дебаунс IDR за запитом
	// logFirstSoftFrame: одноразове діагностичне логування геометрії CPU-кадру.
	// Краш на Computer (Intel, native 1920x1200, encode 1920x1080) не
	// відтворюється на NVIDIA-боксі, тож коли агент піде на той ПК — ці цифри
	// (розміри, страйди, чи Y/UV не nil) покажуть, ЩО саме приходить у submit,
	// замість голого access violation. Друкуємо рівно раз, щоб не спамити лог.
	logFirstSoftFrame sync.Once
}

type sendJob struct {
	au  encode.AU
	seq uint64
}

func newAgent(s *stream, session *sessionWatch, transportKind, hubAddr string) *agent {
	return &agent{
		s: s, session: session, transportKind: transportKind, hubAddr: hubAddr,
		frameInterval: time.Second / time.Duration(s.fps),
		pcDown:        make(chan string, 1),
		txErrCh:       make(chan error, 1),
		sendQueue:     make(chan sendJob, 8),
		lastLog:       time.Now(),
		reacqBackoff:  reacquireBackoffMin,
	}
}

// dial — нове зʼєднання з хабом з колбеками цього агента.
func (a *agent) dial() (transport, error) {
	return dial(a.transportKind, a.hubAddr, a.frameInterval, a.onKeyframeRequest, a.onGate, a.onBitrateTarget, a.onSelectOutput, a.onDown)
}

// transport — поточний транспорт (реконект його міняє).
func (a *agent) transport() transport {
	a.tpMu.Lock()
	defer a.tpMu.Unlock()
	return a.tp
}

// onKeyframeRequest — спільний колбек для обох транспортів: WT читає
// keyframe_request з control-стріму hub-а, WebRTC отримує RTCP PLI.
// В обох випадках реакція та сама — примусовий IDR (§5.5).
// A-13/A-31: колбеки pion не беруть мʼютекс енкодера (Encode тримає його до
// 500 мс) і не форсують IDR самі — лише піднімають прапорець, який кадровий
// цикл застосовує з дебаунсом. Шторм PLI від глядачів = один IDR на 300 мс,
// а не IDR на кожен кадр.
func (a *agent) onKeyframeRequest() { a.s.wantIDR.Store(true) }

func (a *agent) onGate(resume bool) {
	a.gateSeen.Store(true)
	// Лише перемикаємо прапорець — саме звільнення/підняття капчера робить
	// кадровий цикл (releaseCapture/reacquireCapture), бо капчер не
	// thread-safe і його не можна чіпати з цього колбека (інша горутина).
	if resume {
		if a.gatePaused.CompareAndSwap(true, false) {
			a.s.wantIDR.Store(true) // новий глядач має отримати IDR негайно
			log.Printf("oo-agent: viewer present — resuming (capture reacquired in frame loop)")
		}
	} else if a.gatePaused.CompareAndSwap(false, true) {
		log.Printf("oo-agent: no viewer — pausing (capture released in frame loop)")
	}
}

// onBitrateTarget — hub просить іншу CBR-ціль. Крутимо ручку на живому
// енкодері: переоткриття MFT коштувало б зміни епохи (§5.5).
func (a *agent) onBitrateTarget(want uint64) {
	bps, ok := clampBitrate(want, startBitrateBps)
	if !ok {
		return // без валідного bitrate_bps повідомлення ігноруємо
	}
	// «Не готово» = і пауза, і вікно resume, поки капчер ще піднімається
	// (encoder==nil). Хаб шле resume, а ОДРАЗУ за ним resetBitrate, тож ціль
	// регулярно прилітає саме в це вікно: без перевірки на nil-encoder
	// applyBitrate тихо викидав би її на нульовому енкодері, і потік стартував
	// би з дефолтним бітрейтом замість замовленого (знайшов Codex-рев'ю).
	// Відкладена ціль застосується в кадровому циклі одразу після reacquire.
	// A-13: завжди через кадровий цикл (take() перед Encode), ніколи з
	// колбека — SetBitrate бере той самий мʼютекс, що й Encode.
	a.bitrateWanted.set(bps, true)
	log.Printf("oo-agent: bitrate -> %d bps (застосує кадровий цикл)", bps)
}

// onSelectOutput — hub попросив інший монітор (control §select_output; сам
// запит приходить із консолі ЕРП). Тут лише КЛАДЕМО намір: перемикання
// капчера робить кадровий цикл, бо capture.Capturer «NOT safe for concurrent
// use», а ми в горутині DataChannel/QUIC-стріму. Неіснуючий індекс відсіє
// SwitchOutput проти живої енумерації — залишимось на поточному моніторі.
func (a *agent) onSelectOutput(idx int) {
	log.Printf("oo-agent: select_output -> %d (застосує кадровий цикл)", idx)
	a.s.requestOutput(idx)
}

func (a *agent) onDown(reason string) {
	select {
	case a.pcDown <- reason:
	default:
	}
}

// applyBitrate — ЄДИНЕ місце, де ціль реально лягає в енкодер. Після
// успішної зміни одразу IDR (тим самим шляхом, що й keyframe_request):
// без нього поточний GOP догравається старим квантуванням і нова ціль
// проявиться аж через ~2с, тобто саме тоді, коли вона вже не потрібна.
func (a *agent) applyBitrate(bps int) {
	e := a.s.encoder()
	if e == nil {
		return // капчер звільнено на паузі — ціль застосується на reacquire
	}
	if err := e.SetBitrate(bps); err != nil {
		log.Printf("oo-agent: SetBitrate(%d): %v", bps, err)
		return
	}
	// Запамʼятовуємо ЖИВУ ціль: SwitchOutput відкриває новий енкодер саме з
	// нею, інакше перемикання монітора мовчки скасовувало б притискання хаба.
	a.s.bitrateBps.Store(int64(bps))
	log.Printf("oo-agent: bitrate -> %d bps", bps)
	if err := e.ForceIDR(); err != nil {
		log.Printf("oo-agent: ForceIDR (bitrate change): %v", err)
	}
}

// runSender — один ordered sender: усі AU (у т.ч. кілька з одного enc.Encode
// виклику, напр. IDR+trailing delta AU з тієї самої кодованої картинки) ідуть
// через sendQueue і відправляються СТРОГО послідовно однією горутиною. Раніше
// кожен AU спамив власну goroutine.send — конкурентні send() на той самий
// стрім могли інтерлівитись/переставлятись місцями, і кожна goroutine
// незалежно скидала спільний inFlight, ламаючи admission-контроль.
func (a *agent) runSender() {
	for job := range a.sendQueue {
		if err := a.transport().send(job.au, job.seq); err != nil {
			select {
			case a.txErrCh <- err:
			default:
			}
		} else {
			a.sent.Add(1)
		}
		a.queued.Add(-1)
	}
}

// watchAvailability — доступність картинки для хаба (16.09.2026, hub
// unavailable.go). На екрані блокування DuplicateOutput приречений, а консоль
// ЕРП без цього сигналу йшла в OO, чекала 8с кадру й лише потім брала Mesh.
// Раз на секунду звіряємо стан сесії з тим, що вже сказали ЦІЙ нозі: реконект
// = нова нога = кажемо заново (хаб на новій нозі скидає причину).
func (a *agent) watchAvailability(ctx context.Context) {
	var said transport
	saidLocked, seq := false, uint64(0)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cur := a.transport()
		locked := a.session.Locked()
		if cur == said && locked == saidLocked {
			continue
		}
		w, ok := cur.(*webrtcTransport)
		if !ok {
			continue
		}
		reason := ""
		if locked {
			reason = "session-locked"
		}
		seq++
		if w.sendFallbackReason(seq, reason) {
			said, saidLocked = cur, locked
		}
	}
}

func (a *agent) sendAsync(au encode.AU) {
	mySeq := a.seq
	a.seq++
	a.queued.Add(1)
	a.sendQueue <- sendJob{au: au, seq: mySeq}
}

// pts — мітка наступного кадру за стінним годинником (див. tick).
func (a *agent) pts(now time.Time) time.Duration {
	a.captureSeq += seqAdvance(now.Sub(a.lastSeqAt), a.frameInterval)
	a.lastSeqAt = now
	return time.Duration(a.captureSeq) * time.Second / time.Duration(a.s.fps)
}

// sendStillKeepalive — повтор останнього «нічого не змінилось» AU (A-03).
func (a *agent) sendStillKeepalive() {
	if a.lastStillAU == nil || a.gatePaused.Load() || time.Since(a.lastStillSentAt) < keepaliveAfter {
		return
	}
	if e := a.s.encoder(); e != nil && e != a.lastStillEnc {
		return
	}
	now := time.Now()
	au := *a.lastStillAU
	au.PTS = a.pts(now)
	a.sendAsync(au)
	a.lastStillSentAt = now
	a.keepalives++
}

// suspendCapture віддає DXGI-дублікацію (A-17): девайс і MFT живуть далі,
// resume коштує один DuplicateOutput. why — для журналу.
func (a *agent) suspendCapture(why string) {
	if a.s.cap != nil && !a.suspended {
		a.s.cap.Suspend()
		a.suspended = true
		log.Printf("oo-agent: %s — desktop duplication released (екран вільний для Mesh)", why)
	}
}

// reconnect рве поточний транспорт і дзвонить заново, поки не вийде або ctx
// не погасять.
//
// 🚨 A-28. Реконект — це НЕ мить: хаб може лежати хвилинами, і всі ці
// хвилини dial крутиться з бек-офом. До цієї зміни агент увесь той час
// тримав DXGI-дублікацію відкритою (кадровий цикл стоїть тут, але
// капчер живий), а звукова горутина — своя, вона не стоїть — кодувала
// й слала пакети в мертвий транспорт. Дублікація в простої це та сама
// регресія 01.09, від якої рятує гейт: MeshCentral на цьому ж ПК не
// може захопити екран і рве свою desktop-сесію.
//
// Тому на час дозвону ставимо ТОЙ САМИЙ gatePaused, що й гейт хаба
// (його читає і звук), і віддаємо дублікацію через Suspend (A-17) —
// девайс і MFT лишаються, resume коштує один DuplicateOutput.
//
// Знімається прапорець лише якщо його поставили МИ і новий хаб за час
// дозвону нічого про гейт не сказав — див. reconnectGate.restore.
func (a *agent) reconnect(ctx context.Context) {
	rg := beginReconnectGate(&a.gatePaused, &a.gateSeen)
	defer rg.restore()
	a.suspendCapture("reconnect")
	backoff := reconnectBackoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		a.tpMu.Lock()
		a.tp.close()
		a.tpMu.Unlock()

		newTp, err := a.dial()
		if err != nil {
			wait := jitterBackoff(backoff)
			log.Printf("oo-agent: reconnect failed, retry in %s: %v", wait.Round(time.Millisecond), err)
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		a.tpMu.Lock()
		a.tp = newTp
		a.tpMu.Unlock()
		// Власне close() старого PeerConnection теж дає "closed", а невдала
		// спроба dial — свій. Ці сигнали вже неактуальні: гасимо, інакше
		// наступний прохід циклу переподключався б поверх щойно піднятої сесії.
		select {
		case <-a.pcDown:
		default:
		}
		// A-32: у sendQueue лежать P-кадри старої сесії; новий хаб без
		// референсу їх не декодує, а декодер глядача — тим паче.
		for drained := false; !drained; {
			select {
			case <-a.sendQueue:
				a.queued.Add(-1)
			default:
				drained = true
			}
		}
		// Reference frames on the other side are gone: force a fresh IDR
		// so the new session decodes from scratch (§5.5). Капчер може бути
		// звільнений (реконект стався на паузі) — тоді IDR дасть reacquire.
		if e := a.s.encoder(); e != nil {
			if err := e.ForceIDR(); err != nil {
				log.Printf("oo-agent: ForceIDR after reconnect: %v", err)
			}
		}
		log.Printf("oo-agent: reconnected via %s to %s", a.transportKind, a.hubAddr)
		return
	}
}

// sleep — пауза кадрового циклу; false = агента зупиняють.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// run — кадровий цикл до зупинки агента, потім закриває транспорт.
func (a *agent) run(ctx context.Context) {
	log.Printf("oo-agent: streaming %s -> %s (fps=%d bitrate=%d)", a.transportKind, a.hubAddr, a.s.fps, startBitrateBps)
	a.lastSeqAt = time.Now()
	a.lastAdmitAt = a.lastSeqAt
	for a.tick(ctx) {
	}
	log.Printf("oo-agent: shutting down (sent=%d dropped=%d keepalives=%d throttled=%d)", a.sent.Load(), a.dropped.Load(), a.keepalives, a.throttled)
	a.tpMu.Lock()
	a.tp.close()
	a.tpMu.Unlock()
}

// tick — один прохід кадрового циклу; false = агента зупиняють. Порядок:
// причини реконекту → гейт/лок/капчер → відкладені накази хаба → кадр →
// admission → кодування й відправка.
func (a *agent) tick(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if a.reconnectIfDown(ctx) {
		return true
	}
	if ready, alive := a.screenReady(ctx); !ready {
		return alive
	}
	a.suspended = false

	// Енкодер (пере)відкрився з іншим профілем/рівнем, ніж оголосила нога:
	// новий offer, інакше глядач рахує буфер за чужим рівнем (fmtpStale).
	a.tpMu.Lock()
	wt, isRTC := a.tp.(*webrtcTransport)
	stale := isRTC && wt.fmtpStale()
	a.tpMu.Unlock()
	if stale {
		log.Printf("oo-agent: енкодер кодує fmtp=%s, а нога оголосила %s — новий offer", h264Fmtp(), wt.fmtp)
		a.reconnect(ctx)
		return true
	}

	a.applyPending()

	frame, still, ok, alive := a.grabFrame(ctx)
	if !ok {
		return alive
	}
	now := time.Now()
	// PTS іде за СТІННИМ годинником, а не за лічильником кадрів: скільки
	// кадр справді чекали, на стільки й зсуваємо. Тут же, до admission —
	// щоб і викинутий кадр не лишав RTP позаду стінного часу.
	pts := a.pts(now)
	if !a.admit(frame, still, now) {
		return true
	}
	a.encodeAndSend(frame, still, pts)
	return true
}

// reconnectIfDown — причини реконекту, що видно в будь-якому стані (і на
// паузі). true = реконект відбувся, прохід закінчено.
func (a *agent) reconnectIfDown(ctx context.Context) bool {
	select {
	case err := <-a.txErrCh:
		log.Printf("oo-agent: transport error, reconnecting: %v", err)
		a.reconnect(ctx)
		return true
	case reason := <-a.pcDown:
		// 🚨 Саме цієї гілки бракувало 30.08 01:11. txErrCh нижче нічого не
		// ловить, поки агент на паузі: без глядача він не шле — отже й не
		// помиляється. Стан PeerConnection видно й на паузі.
		log.Printf("oo-agent: %s, reconnecting", reason)
		a.reconnect(ctx)
		return true
	default:
	}
	// 🚨 Друга (і єдина, що працює на паузі) причина реконекту: хаб
	// замовк. Гілка pcDown вище тут безсила за побудовою — стан
	// PeerConnection замерзає на "connected", бо агент, який нічого не шле,
	// не заводить consent-таймер (див. hubLiveness). Перевірка стоїть саме
	// тут, ДО гейта: на паузі цикл обертається кожні 50 мс, тобто сторож
	// живий рівно тоді, коли він потрібен.
	if hubLive.silent(time.Now()) {
		log.Printf("oo-agent: хаб мовчить довше за %s — рвемо і перепідключаємось", control.HeartbeatTimeout)
		a.reconnect(ctx)
		return true
	}
	return false
}

// screenReady — чи можна знімати кадр: є глядач, сесія не заблокована, капчер
// і енкодер підняті. ready=false — прохід закінчено (вже почекали); alive=false
// — агента зупиняють.
func (a *agent) screenReady(ctx context.Context) (ready, alive bool) {
	// on-demand: без глядача не захоплюємо й не кодуємо — і ЗВІЛЬНЯЄМО
	// капчер, щоб не тримати DXGI-дублікацію виводу (інакше MeshCentral на
	// цьому ж ПК не може захопити екран і рве свою desktop-сесію — регресія
	// 01.09). PeerConnection і RTCP/DataChannel-читачі лишаються живими, тож
	// щойно hub пришле "resume", наступний прохід підніме капчер заново.
	if a.gatePaused.Load() {
		a.suspendCapture("no viewer")
		return false, sleep(ctx, 50*time.Millisecond)
	}
	// Глядач зʼявився, а капчер було звільнено на паузі — піднімаємо заново
	// (той самий вивід, свіжий енкодер+IDR). Невдача — коротка пауза й
	// повтор, потік не рветься назавжди.
	// A-39: на лок-скріні DuplicateOutput приречений (E_ACCESSDENIED), і
	// кожна спроба — це ще й новий D3D-девайс на порожньому місці. Поки
	// сесія заблокована, не пробуємо взагалі: сесію тримає keepalive, а
	// unlock зніме паузу негайно (нижче).
	if a.session.Locked() && a.s.encoder() == nil {
		a.sendStillKeepalive()
		return false, sleep(ctx, 250*time.Millisecond)
	}
	// Розблокували — пробуємо ЗАРАЗ, а не через залишок бек-офу, який міг
	// дорости до keepaliveAfter поки екран був замкнений.
	if a.session.takeUnlocked() {
		a.reacqBackoff = reacquireBackoffMin
	}
	if a.s.encoder() != nil {
		return true, true
	}
	if err := a.s.reacquireCapture(); err != nil {
		wait := jitterBackoff(a.reacqBackoff)
		log.Printf("oo-agent: reacquire capture failed: %v (retry in %s)", err, wait.Round(time.Millisecond))
		a.sendStillKeepalive()
		if !sleep(ctx, wait) {
			return false, false
		}
		// Стеля = keepaliveAfter: довша пауза лишила б сторож без кадру.
		if a.reacqBackoff *= 2; a.reacqBackoff > keepaliveAfter {
			a.reacqBackoff = keepaliveAfter
		}
		return false, true
	}
	a.reacqBackoff = reacquireBackoffMin
	log.Printf("oo-agent: viewer present — reacquired screen capture")
	return true, true
}

// applyPending застосовує те, що хаб попросив з іншої горутини: ціль
// бітрейту, IDR, інший монітор. Лише звідси — капчер і енкодер не thread-safe.
func (a *agent) applyPending() {
	// Ціль, що прийшла на паузі, застосовується тут — на першому кадрі
	// після відновлення, поки в MFT знову йдуть кадри.
	if bps := a.bitrateWanted.take(); bps != 0 {
		a.applyBitrate(bps)
	}
	// A-31: IDR за запитом — лише звідси, з дебаунсом.
	if a.s.wantIDR.Swap(false) && time.Since(a.lastIDRAt) >= idrDebounce {
		if err := a.s.encoder().ForceIDR(); err != nil {
			log.Printf("oo-agent: ForceIDR (request): %v", err)
		}
		a.lastIDRAt = time.Now()
	}
	// Перемикання монітора — РІВНО ТУТ, у кадровому циклі, і ніде більше:
	// капчер не є thread-safe, а між NextFrame і Encode його міняти не
	// можна взагалі (кадр аліасить його буфери). Запит прийшов із іншої
	// горутини через requestOutput; помилка (неіснуючий індекс, зайнятий
	// вихід) лишає нас на поточному моніторі — потік не рветься.
	if idx, ok := a.s.pending.take(); ok {
		if err := a.s.SwitchOutput(idx); err != nil {
			log.Printf("oo-agent: switch output -> %d: %v", idx, err)
		}
	}
}

// grabFrame — наступний кадр: свіжий, повтор останнього на нерухомому екрані
// (still) або перший через GDI. ok=false — кадру нема, прохід закінчено;
// alive=false — агента зупиняють.
func (a *agent) grabFrame(ctx context.Context) (frame *capture.NV12Frame, still, ok, alive bool) {
	s := a.s
	// capture.NextFrame сама крутиться на DXGI-таймаутах і на нерухомому
	// екрані не повернеться НІКОЛИ (її ж коментар: "Give it a deadline if
	// you need 'no news' reported back"). Дедлайн — єдиний спосіб дізнатись,
	// що екран стоїть, а не що ми ще чекаємо.
	waitCtx, cancelWait := context.WithTimeout(ctx, keepaliveAfter)
	frame, err := s.cap.NextFrame(waitCtx)
	cancelWait()
	// A-01: капчер міг пережити ACCESS_LOST і жити вже на іншому девайсі.
	if dev, gen := s.cap.Device(), s.cap.Generation(); encoderStale(dev, gen, s.encDev, s.encGen) {
		s.lastFrame = nil // аліасив буфери/текстуру старого девайса
		if dev == 0 {
			// Дублікацію втрачено, капчер ще відновлює її (лок/UAC):
			// кадру нема, сесію тримає повтор keepalive (A-03).
			a.sendStillKeepalive()
			return nil, false, false, true
		}
		if serr := s.syncEncoderToCapture(); serr != nil {
			log.Printf("oo-agent: encoder rebuild after device change failed: %v — releasing capture", serr)
			s.releaseCapture()
			return nil, false, false, true
		}
	}
	switch {
	case err == nil:
		s.lastFrame, a.lastFrameEncoded = frame, false
		return frame, false, true, true
	case ctx.Err() != nil:
		return nil, false, false, false // зупиняють агента, а не просто екран стоїть
	case shouldKeepalive(err, a.gatePaused.Load(), s.lastFrame != nil):
		// Екран нерухомий: пересилаємо ОСТАННІЙ кадр. Декодер отримує
		// крихітний P-кадр «нічого не змінилось», сторож у браузері бачить
		// свіжий кадр і не рве сесію.
		a.keepalives++
		return s.lastFrame, true, true, true
	case shouldRearm(err, a.gatePaused.Load(), s.lastFrame != nil):
		// Стартували на вже нерухомому екрані (або щойно перемкнули монітор
		// на нерухомий — SwitchOutput скидає lastFrame саме в цей стан):
		// беремо поточний робочий стіл через GDI, бо DXGI на такому екрані
		// не віддасть нічого.
		gdi, gerr := s.cap.GDIFrame()
		if gerr != nil {
			log.Printf("oo-agent: capture.GDIFrame: %v (retrying)", gerr)
			time.Sleep(100 * time.Millisecond)
			return nil, false, false, true
		}
		log.Printf("oo-agent: перший кадр знято через GDI (екран нерухомий)")
		s.lastFrame, a.lastFrameEncoded = gdi, false
		return gdi, false, true, true
	case errors.Is(err, context.DeadlineExceeded):
		// Дедлайн був, але слати не можна: глядача нема (пауза). Порожній
		// кадр тут не вигадуємо — декодеру нема з чого будувати картинку,
		// а сторожа ми б обдурили.
		return nil, false, false, true
	case errors.Is(err, capture.ErrClosed), errors.Is(err, capture.ErrAccessLost), errors.Is(err, capture.ErrNotAvailable):
		// A-02: капчер закрив себе на OOS_ERROR або здався відновлювати
		// дублікацію (лок-скрін/RDP). Раніше цикл крутив ErrClosed 10/с
		// навічно. Звільняємо; reacquire вище підніме заново з бек-офом, а
		// до того сесію тримає keepalive (A-03).
		log.Printf("oo-agent: capture unavailable: %v — releasing, will reacquire", err)
		s.releaseCapture()
		a.sendStillKeepalive()
		return nil, false, false, true
	default:
		log.Printf("oo-agent: capture.NextFrame: %v (retrying)", err)
		time.Sleep(100 * time.Millisecond)
		return nil, false, false, true
	}
}

// admit — чи кодувати цей кадр. Усі відмови — ДО кодування, поки кадр іще
// нічого не коштував, і всі міряються від lastAdmitAt.
func (a *agent) admit(frame *capture.NV12Frame, still bool, now time.Time) bool {
	// A-08: рух миші без змін на столі (DXGI: LastPresentTime==0) — не
	// привід кодувати 60 повних кадрів/с. Курсор скомпоновано в кадр, тож
	// зовсім пропускати не можна — тримаємо ≤15 к/с.
	if frame.MouseOnly && !still && now.Sub(a.lastAdmitAt) < mouseOnlyGap {
		return false
	}
	// C1: «Швидкість» глядача. Keepalive (still) не чіпаємо — він і так раз
	// на keepaliveAfter, а без нього сторож браузера рве сесію.
	if gap := maxFpsGap(int(maxFpsWanted.Load()), a.s.fps); gap > 0 && !still && now.Sub(a.lastAdmitAt) < gap {
		a.throttled++
		return false
	}
	// Бюджет CPU софтверного енкодера: зайві кадри викидаємо ДО кодування,
	// у тому самому місці й з тією ж логікою, що admission нижче — саме
	// тут кадр іще нічого не коштував. -force-software не чіпаємо: це
	// свідомий вибір оператора, а не машина, яка не тягне.
	if a.s.software && !a.s.forceSoftware {
		if gap := softwareFrameGap(runtime.NumCPU(), a.s.encW*a.s.encH, a.s.fps); gap > 0 && now.Sub(a.lastAdmitAt) < gap {
			a.throttled++
			return false
		}
	}
	// Admission (§5.5): якщо відправка ПОПЕРЕДНЬОГО кадру ще блокує
	// транспорт — дропаємо ЩОЙНО ЗАХОПЛЕНИЙ кадр ДО кодування. Референсні
	// кадри енкодера лишаються цілі (ми нічого йому не подавали), тому
	// IDR після дропу не потрібен. Верхню межу паузи, яку ці дропи мають
	// право створити, тримає admissionFloor — інакше дроп зʼїдав і
	// keepalive-кадр, і сесія гинула на сторожі браузера.
	if !shouldAdmit(a.queued.Load(), now.Sub(a.lastAdmitAt)) {
		a.dropped.Add(1)
		if time.Since(a.lastLog) >= 5*time.Second {
			log.Printf("oo-agent: dropped %d frames in last %s (transport busy), sent=%d",
				a.dropped.Load(), time.Since(a.lastLog).Round(time.Millisecond), a.sent.Load())
			a.dropped.Store(0)
			a.lastLog = time.Now()
		}
		return false
	}
	a.lastAdmitAt = now
	return true
}

// encodeAndSend кодує кадр, ставить AU у чергу відправки й запамʼятовує
// «нічого не змінилось» AU для keepalive.
func (a *agent) encodeAndSend(frame *capture.NV12Frame, still bool, pts time.Duration) {
	s := a.s
	encFrame := encode.Frame{PTS: pts}
	if s.software {
		encFrame.Y = frame.Y
		encFrame.UV = frame.UV
		encFrame.YStride = frame.YStride
		encFrame.UVStride = frame.UVStride
		a.logFirstSoftFrame.Do(func() {
			log.Printf("oo-agent: first software frame: cap %dx%d enc %dx%d | Y!=nil=%v len(Y)=%d YStride=%d | UV!=nil=%v len(UV)=%d UVStride=%d | cursor(vis=%v comp=%v shape=%v)",
				frame.Width, frame.Height, s.encW, s.encH,
				frame.Y != nil, len(frame.Y), frame.YStride,
				frame.UV != nil, len(frame.UV), frame.UVStride,
				frame.CursorVisible, frame.CursorComposited, frame.CursorShape)
		})
		// Захист: якщо капчер віддав кадр без CPU-площин (напр. режим
		// readback раптом злетів), НЕ передаємо nil у C — там memcpy(nil)
		// = access violation. Логуємо і пропускаємо кадр замість краху.
		if frame.Y == nil || frame.UV == nil {
			log.Printf("oo-agent: software frame with nil planes (Y!=nil=%v UV!=nil=%v) — skipping (no CPU readback?)",
				frame.Y != nil, frame.UV != nil)
			return
		}
	} else {
		encFrame.Texture = frame.Texture
		// A-06: без покоління енкодер лишив би закешовану input-view на
		// СТАРІЙ текстурі, якщо перебудований капчер отримав ту саму адресу.
		encFrame.TextureGen = frame.TextureGen
	}
	aus, err := s.encoder().Encode(encFrame)
	if err != nil {
		log.Printf("oo-agent: encode.Encode: %v", err)
		// A-12: енкодер завис (статус OOS_ENC_WEDGED); перебудова через той
		// самий шлях, що й для ACCESS_LOST.
		if errors.Is(err, encode.ErrWedged) {
			s.releaseCapture()
		}
		return
	}
	for _, au := range aus {
		a.sendAsync(au)
	}
	encodedBefore := a.lastFrameEncoded
	a.lastFrameEncoded = true // frame тут завжди == s.lastFrame
	if keepStillAU(still, encodedBefore, aus) {
		cp := aus[0]
		a.lastStillAU, a.lastStillEnc, a.lastStillSentAt = &cp, s.encoder(), time.Now()
	}

	if time.Since(a.lastLog) >= 5*time.Second {
		log.Printf("oo-agent: sent=%d dropped=%d keepalives=%d throttled=%d", a.sent.Load(), a.dropped.Load(), a.keepalives, a.throttled)
		a.dropped.Store(0)
		a.lastLog = time.Now()
	}
}
