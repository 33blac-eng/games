// Fanout (Ф1) — кілька глядачів на ОДНУ ноду: джерело одне (agent-нога),
// приймачів N. Кожен глядач — окрема viewerLeg зі своїм TrackLocalStaticRTP,
// своєю чергою і своїм pump-ом. Цикл форвардингу НІКОЛИ не пише в трек напряму:
// він лише розкладає той самий переписаний пакет по чергах (неблокуючий send),
// тож повільний глядач гальмує тільки себе. Приєднання/відпадання однієї ноги не
// рве потік іншим і не чіпає agent-ногу.
package main

import (
	"log"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

const (
	// viewerQueueDepth — глибина черги ОДНІЄЇ viewer-ноги. ~830 пакетів/с на
	// 8 Мбіт/с, тож 512 ≈ 0.6с відставання: коротке моргання мережі черга
	// переживе, систематичне відставання — ні. H-12: переповнення саме по собі
	// ногу вже НЕ рве — див. viewerOverflowStreakMax нижче.
	// ponytail: стеля — черга рахує ПАКЕТИ, не байти й не кадри; на дуже
	// різному розмірі пакетів поріг у секундах "пливе". Апгрейд робиться на
	// місці: лічильник байтів у pump замість глибини каналу.
	viewerQueueDepth = 512

	// viewerRRStale — RR, старший за це, більше не описує стан ноги (фонову
	// вкладку браузер тротлить, RTCP рідшає). Не даємо застиглому семплу вічно
	// тримати ВСІХ глядачів унизу за правилом "найгіршого".
	viewerRRStale = 15 * time.Second

	// H-12. Переповнення черги — це НЕ вирок нозі. Одне моргання мережі, один
	// сплеск бітрейту після IDR, одна пригальмована вкладка — і 512 пакетів
	// закінчувались, а хаб рвав людині сесію цілком: замість секунди підвисання
	// вона отримувала «OO втрачено» і перемикання на Mesh. Тому спершу
	// drop-to-IDR: ноги перестають наповнювати, доки не прийде ключовий кадр
	// (черга за цей час розбирається сама), і замовляється позачерговий IDR.
	//
	// viewerOverflowStreakMax — скільки таких епізодів ПОСПІЛЬ нога має пройти,
	// перш ніж її таки рвуть. Три — бо один епізод це шум, два ще можуть бути
	// одним і тим самим морганням, а три поспіль у межах вікна означають, що
	// глядач систематично не встигає за джерелом і скидання кадрів йому вже не
	// допомагає.
	viewerOverflowStreakMax = 3
	// viewerOverflowWindow — переповнення, розділені більшим проміжком, НЕ
	// вважаються «поспіль»: лічильник починається наново. Без цього вікна нога,
	// яка моргнула тричі за годину, помирала б як безнадійна.
	viewerOverflowWindow = 10 * time.Second
)

// viewerLeg — ОДИН глядач ноди. Стан під ns.mu, окрім sent (атомарний лічильник).
type viewerLeg struct {
	pc     *webrtc.PeerConnection
	trk    *webrtc.TrackLocalStaticRTP
	userID string // user_id з claims тікета — для runtime-revoke за user

	// ready — ця нога у PeerConnectionStateConnected; live — ready І є publisher
	// ноди (єдине місце запису live — recomputeBinding, як і раніше).
	ready bool
	live  bool

	// hidden — F-39: вкладка ЦЬОГО глядача прихована, він сам про це сказав
	// (POST /viewer/visibility, visibility.go). Прихована нога НЕ отримує
	// медіа (recomputeBinding не робить її live) і не тримає агента в
	// "resume" — але лишається в ns.viewers з усіма своїми правами й
	// session_id. Це і є різниця між «прихований» і «пішов»: пішов — знятий з
	// мапи (removeViewer), прихований — на місці й повертається без нового
	// квитка. Дефолт false: нога, яка нічого не сказала, дивиться. Під ns.mu.
	hidden bool

	// sessionID — секрет ренегоціації ЦІЄЇ ноги (F-11, main.go). Пишеться раз,
	// одразу після addViewer, під ns.mu.
	sessionID string
	// renegotiating — на цій нозі вже йде обмін SDP. Атомарний, бо це єдиний
	// стан ноги, який чіпається БЕЗ ns.mu (з HTTP-хендлера).
	renegotiating atomic.Bool

	// primed — нозі вже віддано кеш GOP (пункт 41), тобто картинка в неї піде
	// без позачергового IDR. Ставить рівно recomputeBinding, під ns.mu.
	primed bool
	// primeSlack — скільки пакетів кешу GOP поклали в чергу при priming; на
	// стільки поріг відставання ноги вищий за viewerQueueDepth, доки pump не
	// розбере чергу нижче за viewerQueueDepth. Під ns.mu.
	primeSlack int

	// H-12: стан drop-to-IDR. discarding — нозі зараз НІЧОГО не кладуть у чергу,
	// доки не прийде ключовий пакет; саме пауза в записі й дає pump-у розібрати
	// накопичене. overflowStreak рахує переповнення ПОСПІЛЬ (розділені менш ніж
	// viewerOverflowWindow), overflowAt — момент останнього. Усе під ns.mu.
	discarding     bool
	overflowStreak int
	overflowAt     time.Time

	// out — черга пакетів цієї ноги; НІКОЛИ не закривається (у неї пишуть під
	// ns.mu, поки нога є в ns.viewers). Зупинку pump-а сигналить done, який
	// закриває рівно той виклик removeViewer, що зняв ногу з мапи.
	out  chan *rtp.Packet
	done chan struct{}

	// Останній RR цієї ноги — вхід правила "найгіршого" (worstViewerRR).
	loss   float64
	jitter uint32
	lastRR time.Time

	// RTT цієї ноги (виведений з LSR/DLSR її ж RR, див. rtt.go). Керує не rtt,
	// а rtt мінус БАЗА: базова лінія у кожного глядача своя, і порівнювати їх
	// між собою безглуздо.
	rtt time.Duration

	// Дві корзини ковзного мінімуму — саме вони і є база (див. observeRTT):
	// мінімум поточного вікна, мінімум попереднього і початок поточного.
	minRTTCur  time.Duration
	minRTTPrev time.Duration
	minRTTAt   time.Time

	// NACK_recovered_ratio цієї ноги (nack.go): поточне вікно рахунку і момент,
	// до якого нога вже переведена на PLI. Під ns.mu.
	nackReq   uint64
	nackHit   uint64
	nackWinAt time.Time
	pliUntil  time.Time

	// B4: сигнали затору до ретрансмісії (legCongestion). nackSeen — унікальні
	// seq із NACK за поточний інтервал між RR (nackPrev — за попередній, щоб
	// повторний NACK того самого seq через межу інтервалу не рахувався двічі),
	// sentAtRR — vl.sent на початку інтервалу, pliCnt — PLI від глядача за
	// інтервал. preLoss/plis — підсумок останнього закритого інтервалу. Під ns.mu.
	nackSeen map[uint16]struct{}
	nackPrev map[uint16]struct{}
	sentAtRR uint64
	pliCnt   int
	preLoss  float64
	plis     int

	sent    uint64 // скільки пакетів реально пішло в трек (атомарно)
	lastSeq uint32 // останній seq, реально записаний у трек (атомарно, uint16 у uint32)

	// audioSent — скільки кадрів пішло в аудіо-доріжку цієї ноги (audio.go,
	// атомарно). Нуль назавжди, поки OO_SCREEN_AUDIO не заданий.
	audioSent uint64

	// audioOut — черга звуку від агента для ЦІЄЇ ноги; nil без прапорця (тоді
	// forwardAudioToViewers ногу просто пропускає). Не закривається ніколи, з
	// тієї ж причини, що й out: пишуть у неї під ns.mu, поки нога є в мапі.
	audioOut chan []byte

	// audioDropped — кадри, викинуті через переповнену чергу (атомарно).
	audioDropped uint64

	// tilesOut — черга текстових тайлів (tiles.go); nil, поки глядач не
	// відкрив канал "oosc-tiles" (і завжди без OO_SCREEN_TILES). Пишеться
	// під ns.mu, не закривається. tilesSent/tilesDropped — атомарно.
	tilesOut     chan []byte
	tilesSent    uint64
	tilesDropped uint64

	// born — момент створення ноги: старт відліку time-to-first-frame
	// (/metrics, metrics.go). Пишеться раз, до pump.
	born time.Time
	// tilesLossy — з останнього TypeKeep глядач втратив хоч один тайл: його
	// сховище може не мати тайлів, які агент вважає утриманими (tiles.go).
	tilesLossy atomic.Bool
}

// addViewer реєструє нову viewer-ногу ноди й піднімає її pump. Нога ще НЕ live:
// публікація вмикається лише recomputeBinding() після Connected ТА наявного
// publisher-а (fail-closed — той самий інваріант, що й на одному глядачеві).
func addViewer(ns *nodeSession, pc *webrtc.PeerConnection, trk *webrtc.TrackLocalStaticRTP, userID string) *viewerLeg {
	return addViewerLimit(ns, pc, trk, userID, 0)
}

// addViewerLimit — addViewer зі стелею: перевірка len(ns.viewers) і вставка
// йдуть під одним ns.mu, тож паралельні /offer (ICE-gathering між раннім
// viewerCapReached і реєстрацією триває довго) стелю не перескочать.
// limit<=0 — без стелі. nil — стелю досягнуто, нічого не зареєстровано.
func addViewerLimit(ns *nodeSession, pc *webrtc.PeerConnection, trk *webrtc.TrackLocalStaticRTP, userID string, limit int) *viewerLeg {
	vl := &viewerLeg{
		pc:     pc,
		trk:    trk,
		userID: userID,
		out:    make(chan *rtp.Packet, viewerQueueDepth+gopMaxPackets), // + місце під кеш GOP
		done:   make(chan struct{}),
		born:   time.Now(),
	}
	// Черга звуку існує ЛИШЕ під прапорцем: без нього нога має бути бітово
	// такою, як до появи звуку (nil-канал forwardAudioToViewers пропускає).
	// Виставляємо ДО вставки в мапу — після неї в поле вже можуть писати.
	if audioEnabled {
		vl.audioOut = make(chan []byte, audioQueueDepth)
	}
	ns.mu.Lock()
	if limit > 0 && len(ns.viewers) >= limit {
		ns.mu.Unlock()
		return nil
	}
	if ns.viewers == nil {
		ns.viewers = make(map[*webrtc.PeerConnection]*viewerLeg)
	}
	ns.viewers[pc] = vl
	n := len(ns.viewers)
	ns.viewerCount.Store(int32(n))
	ns.mu.Unlock()
	log.Printf("viewer leg added [node=%s]: %d viewer(s)", ns.nodeID, n)

	go vl.pump(ns)
	// Стелю читаємо ТУТ, синхронно, а не в горутині сторожа: інакше читання
	// глобалі відкладалось до планування горутини й гонило з її зміною
	// (тести підміняють sessionCap; race detector ловив це під навантаженням).
	go watchSessionCap(ns, vl, sessionCap)
	return vl
}

// sessionCap — скільки живе ОДНА сесія перегляду, після чого хаб рве її сам.
//
// 🔴 Навіщо: 31.08 забута відкрита вкладка тримала сесію ПʼЯТЬ ГОДИН. Хаб
// чесно ганяв відео нікому (846 пакетів за останні 10 хвилин), агент увесь той
// час КОДУВАВ — тобто процесор чужого робочого ПК був зайнятий, — а запис
// доріс до 503 МБ. Жодна зі сторін не помилилась: браузер тримав зʼєднання,
// хаб бачив живого глядача.
//
// Ознака «людина пішла» нам недоступна: рух миші й натискання йдуть повз хаб,
// а RTCP від забутої вкладки приходить справно. Тому засувка не розумна, а
// груба — стеля часу. Рівно те саме правило вже діє для тунелю MeshCentral
// (remote_access.session_cap_minutes = 120 хв), і розбіжність між двома
// транспортами була б гіршою за саму стелю.
//
// ponytail: стеля — сесія рветься на рівному місці, якщо хтось справді дивиться
// понад дві години. Апгрейд, коли таке трапиться: продовжувати відлік від
// останньої ДІЇ людини (консоль уже шле події вводу — там і буде ознака).
var sessionCap = envDuration("OO_SCREEN_SESSION_CAP", 120*time.Minute)

// watchSessionCap рве ногу глядача, коли її час вичерпано. Нуль або відʼємне
// значення вимикає стелю зовсім — для налагодження, коли сесію треба тримати
// довго свідомо.
func watchSessionCap(ns *nodeSession, vl *viewerLeg, limit time.Duration) {
	if limit <= 0 {
		return
	}
	t := time.NewTimer(limit)
	defer t.Stop()
	select {
	case <-vl.done:
		// Нога знята раніше — звичайний шлях, нічого робити.
	case <-t.C:
		log.Printf("viewer leg [node=%s]: стеля сесії %s вичерпана — рву", ns.nodeID, limit)
		dropViewer(ns, vl, "стеля сесії "+limit.String())
	}
}

// removeViewer знімає ногу з ноди. Повертає true, якщо саме цей виклик її зняв.
// Мапа під ns.mu — єдина точка синхронізації: хто видалив, той і закриває done,
// тож close(done) стається рівно раз, а send у vl.out ніколи не йде в ногу, якої
// вже немає в мапі (сам канал не закривається — інакше був би send на закритому).
func removeViewer(ns *nodeSession, vl *viewerLeg) bool {
	ns.mu.Lock()
	_, ok := ns.viewers[vl.pc]
	if ok {
		delete(ns.viewers, vl.pc)
	}
	vl.ready, vl.live = false, false
	left := len(ns.viewers)
	ns.viewerCount.Store(int32(left))
	if !hasLiveViewerLocked(ns) {
		ns.gop.reset() // B3: див. recomputeBinding — кеш без глядачів застаріває
	}
	ns.mu.Unlock()
	if !ok {
		return false
	}
	close(vl.done)
	if left == 0 {
		scheduleRecordClose(ns)
	}
	return true
}

// dropViewer рве ОДНУ viewer-ногу (повільний глядач, помилка запису, розрив):
// решта глядачів ноди й agent-нога не зачіпаються. Close() — поза ns.mu (він
// смикає колбеки pion).
//
// H-10: Close() ще й ПОЗА самим dropViewer, в окремій горутині. Найчастіший
// шлях сюди — OnConnectionStateChange цієї ж PeerConnection (main.go, гілка
// Failed/Closed), а закриття звідти і реентрантне, і блокує обробку STUN: pion
// чекає на завершення колбека, колбек чекає на Close() — нога зависає намертво,
// і разом з нею вся черга подій цього з'єднання. Точно так само вже закривається
// agent-нога (main.go, «закриття звідси і реентрантне»).
//
// Видалення з мапи ноди (removeViewer) лишається СИНХРОННИМ: саме воно робить
// ногу невидимою для forwardToViewers, і відкладати його не можна — інакше
// між колбеком і горутиною лишається вікно, у яке пакети йдуть у мертву ногу.
func dropViewer(ns *nodeSession, vl *viewerLeg, reason string) {
	if !removeViewer(ns, vl) {
		return
	}
	log.Printf("viewer leg dropped [node=%s]: %s", ns.nodeID, reason)
	go func() { _ = vl.pc.Close() }()
	sendGate(ns) // міг бути останнім глядачем -> агенту "pause"
}

// markViewerReady позначає ногу Connected. true — якщо це ПЕРШИЙ глядач ноди,
// якому реально піде картинка (перехід 0->1 за hasVisibleViewerLocked): саме на
// ньому має сенс скидати стелю бітрейту. Приховані ноги (F-39) тут не рахуються
// — вони нічого не приймають, тож і адаптацію бітрейту не ведуть.
func markViewerReady(ns *nodeSession, vl *viewerLeg) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	first := !hasVisibleViewerLocked(ns)
	vl.ready = true
	return first
}

// viewerPrimed — чи поїхав цій нозі кеш GOP (пункт 41). Читання під ns.mu, бо
// пише його recomputeBinding під тим самим локом.
func viewerPrimed(ns *nodeSession, vl *viewerLeg) bool {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return vl.primed
}

// markViewerNotReady — нога у Disconnected: транзієнтний стан, ногу з ноди НЕ
// знімаємо (повернеться Connected — поновиться), лише перестаємо форвардити.
func markViewerNotReady(ns *nodeSession, vl *viewerLeg) {
	ns.mu.Lock()
	vl.ready, vl.live, vl.primed = false, false, false
	vl.primeSlack = 0
	ns.mu.Unlock()
}

// hasReadyViewerLocked — чи є у ноди ХОЧА Б ОДИН Connected глядач, БЕЗ огляду
// на те, дивиться він зараз чи згорнув вкладку. Це відповідь на питання «чи
// хтось іще тут», а НЕ предикат гейтингу (ним із F-39 став
// hasVisibleViewerLocked). Кликати під ns.mu.
func hasReadyViewerLocked(ns *nodeSession) bool {
	for _, vl := range ns.viewers {
		if vl.ready {
			return true
		}
	}
	return false
}

// hasVisibleViewerLocked — чи є у ноди ХОЧА Б ОДИН глядач, який реально
// дивиться: Connected І не сховався (F-39). Це і є предикат гейтингу — "pause"
// лише коли таких НУЛЬ, "resume" від ПЕРШОГО, хто повернувся. Кликати під ns.mu.
//
// Свідомо окремо від hasReadyViewerLocked: сплутати ці два предикати означало б
// або тримати агента в кодуванні заради згорнутих вкладок (нічого не змінилось
// би), або вважати згорнутого глядача таким, що пішов, — і рвати йому сесію.
func hasVisibleViewerLocked(ns *nodeSession) bool {
	for _, vl := range ns.viewers {
		if vl.ready && !vl.hidden {
			return true
		}
	}
	return false
}

// hasLiveViewerLocked — чи є кому форвардити (Connected глядач + живий
// publisher). Кликати під ns.mu.
func hasLiveViewerLocked(ns *nodeSession) bool {
	for _, vl := range ns.viewers {
		if vl.live {
			return true
		}
	}
	return false
}

// viewerPCsLocked — знімок PeerConnection усіх ніг ноди (щоб закривати їх поза
// локом). Кликати під ns.mu.
func viewerPCsLocked(ns *nodeSession) []*webrtc.PeerConnection {
	out := make([]*webrtc.PeerConnection, 0, len(ns.viewers))
	for pc := range ns.viewers {
		out = append(out, pc)
	}
	return out
}

// observeRTT кладе новий семпл RTT ноги й веде БАЗУ, над якою рахується приріст:
// мінімум за ковзне вікно minRTTWindow, а НЕ за всю сесію. Кликати під ns.mu.
//
// ЧОМУ ВІКНО, А НЕ ЗА СЕСІЮ. Мінімум за сесію лише защіпається вниз і ніколи не
// відпускається вгору. Якщо базова затримка до глядача виросла надовго й
// ЗАКОННО (переїхав на інший канал, переклався маршрут, пішов у роумінг), база
// назавжди лишається від старого, кращого шляху; приріст стабільно завищений, і
// контролер тримає бітрейт заниженим НАЗАВЖДИ на цілком здоровому зʼєднанні.
//
// ЧОМУ ВІКНО, А НЕ ПОВІЛЬНЕ ЕКСПОНЕНЦІЙНЕ ВІДПУСКАННЯ ВГОРУ. Експонента повзе
// БЕЗПЕРЕРВНО, зокрема поки черга наливається — а «затор» і «нова база» на вхід
// подають РІВНО ОДНЕ Й ТЕ САМЕ (RTT вище бази). Відрізняє їх лише ТРИВАЛІСТЬ,
// тож вікно — це чесне формулювання тієї самої ідеї, до того ж із твердою
// гарантією: поки вікно не минуло, база прибита до фактичного мінімуму, і
// приріст під час затору лишається БІТ-У-БІТ таким, як був до цієї правки.
//
// ДВІ КОРЗИНИ — найдешевший ковзний мінімум: мінімум поточного вікна плюс
// мінімум попереднього, база — менший із двох. O(1) стану, без кільцевого
// буфера й без нових залежностей. Ціна — фактичне вікно не рівно minRTTWindow, а
// від одного до двох: те, що впало в попередню корзину, лишається в грі, поки
// вона не викотиться. Для нас це в потрібний бік — довше, а не коротше.
//
// ponytail: стеля — база старіє лише коли ПРИХОДЯТЬ семпли; нога, чиї RR зникли
// на два вікна, повернеться зі старою базою ще на одне вікно. Свідомо не
// лікуємо: розрив такої довжини — це вже інша PeerConnection і новий viewerLeg
// з нульовим станом, а нога, що мовчить довше за viewerRRStale, все одно нікого
// не веде.
func (vl *viewerLeg) observeRTT(rtt time.Duration, now time.Time) {
	vl.rtt = rtt
	switch {
	case vl.minRTTAt.IsZero(): // перший семпл ноги — він же й ставить вікно
		vl.minRTTCur, vl.minRTTAt = rtt, now
	case now.Sub(vl.minRTTAt) >= minRTTWindow: // вікно минуло — корзини зсуваються
		vl.minRTTPrev, vl.minRTTCur, vl.minRTTAt = vl.minRTTCur, rtt, now
	case rtt < vl.minRTTCur:
		vl.minRTTCur = rtt
	}
}

// minRTT — база приросту: менший із мінімумів поточного й попереднього вікна.
// Нуль = семплів ще не було. Кликати під ns.mu.
func (vl *viewerLeg) minRTT() time.Duration {
	if vl.minRTTPrev > 0 && vl.minRTTPrev < vl.minRTTCur {
		return vl.minRTTPrev
	}
	return vl.minRTTCur
}

// rttExcess — приріст RTT ноги над її базою. Нуль = семпла ще немає (нога не
// дала RR із LSR) АБО нога сидить на своєму мінімумі; для контролера це одне й
// те саме «затору немає». Кликати під ns.mu.
func (vl *viewerLeg) rttExcess() time.Duration {
	base := vl.minRTT()
	if vl.rtt == 0 || base == 0 {
		return 0
	}
	return vl.rtt - base
}

// worstViewerRR — правило адаптації бітрейту при КІЛЬКОХ глядачах, явно: потік
// один на всіх, тож контролер (bitrate.go) веде НАЙГІРША нога — максимум втрат
// серед готових ніг. Свідомо НЕ середнє: середнє розмиває одного проблемного
// глядача рештою, і саме він лишається без картинки. Максимум покомпонентний:
//   - loss — сигнал керування, головний;
//   - rttExcess — сигнал керування, другий: приріст, а не абсолютний RTT, саме
//     щоб далекий глядач не тягнув усіх униз просто фактом відстані;
//   - jitter береться теж по максимуму, але вже НЕ як сигнал керування (він із
//     рішення виведений, див. bitrate.go) — лише щоб у телеметрії стояв найгірший.
//
// rtt <= 0 означає «семпла в цьому RR немає» (LSR ще нуль або значення не
// пройшло санітарну межу) — тоді попередній RTT ноги лишається як був.
// Ноги, чий RR давніший за viewerRRStale, у розрахунок не входять.
func worstViewerRR(ns *nodeSession, vl *viewerLeg, loss float64, jitter uint32, rtt time.Duration, now time.Time) (float64, uint32, time.Duration) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	vl.loss, vl.jitter, vl.lastRR = loss, jitter, now
	if rtt > 0 {
		vl.observeRTT(rtt, now)
	}

	worstLoss, worstJitter, worstExcess := loss, jitter, vl.rttExcess()
	for _, o := range ns.viewers {
		if o == vl || !o.ready || o.lastRR.IsZero() || now.Sub(o.lastRR) > viewerRRStale {
			continue
		}
		if o.loss > worstLoss {
			worstLoss = o.loss
		}
		if o.jitter > worstJitter {
			worstJitter = o.jitter
		}
		if e := o.rttExcess(); e > worstExcess {
			worstExcess = e
		}
	}
	return worstLoss, worstJitter, worstExcess
}

// legMinSent — найменше відправлених за інтервал, з якого preLoss має сенс:
// на 1-2 пакетах keepalive один NACK дав би 50%.
const legMinSent = 20

// noteNackSeqs кладе запитані seq у множину поточного інтервалу. Кликати під ns.mu.
func (vl *viewerLeg) noteNackSeqs(seqs []uint16) {
	if vl.nackSeen == nil {
		vl.nackSeen = make(map[uint16]struct{}, len(seqs))
	}
	for _, s := range seqs {
		if _, dup := vl.nackPrev[s]; dup {
			continue
		}
		vl.nackSeen[s] = struct{}{}
	}
}

// notePLI рахує PLI від глядача цієї ноги (keyframe-request rate, B4).
func notePLI(ns *nodeSession, vl *viewerLeg) {
	ns.mu.Lock()
	vl.pliCnt++
	ns.mu.Unlock()
}

// legCongestion — B4: закриває інтервал ЦІЄЇ ноги (кликати на її RR) і віддає
// НАЙГІРШІ preLoss/plis серед свіжих ніг — те саме правило, що worstViewerRR.
// preLoss = унікальні NACK-нуті seq / відправлені за інтервал: втрати ДО
// ретрансмісії, яких RR FractionLost не показує (див. bitrate.go, B4).
func legCongestion(ns *nodeSession, vl *viewerLeg, now time.Time) congSignals {
	sent := atomic.LoadUint64(&vl.sent)
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if d := sent - vl.sentAtRR; d >= legMinSent {
		vl.preLoss = float64(len(vl.nackSeen)) / float64(d)
		if vl.preLoss > 1 {
			vl.preLoss = 1
		}
		vl.plis = vl.pliCnt
		vl.nackPrev, vl.nackSeen = vl.nackSeen, nil
		vl.sentAtRR, vl.pliCnt = sent, 0
	}
	worst := congSignals{preLoss: vl.preLoss, plis: vl.plis}
	for _, o := range ns.viewers {
		if o == vl || !o.ready || o.lastRR.IsZero() || now.Sub(o.lastRR) > viewerRRStale {
			continue
		}
		if o.preLoss > worst.preLoss {
			worst.preLoss = o.preLoss
		}
		if o.plis > worst.plis {
			worst.plis = o.plis
		}
	}
	return worst
}

// pump — власний писар ноги: єдине місце, де пакет іде у трек глядача. Живе
// окремою горутиною, щоб WriteRTP (SRTP + interceptor-ланцюг цієї ноги) не
// стояв у циклі форвардингу. Помилка запису = ця нога мертва: рвемо ЇЇ, джерело
// й інші глядачі не зачіпаються.
func (vl *viewerLeg) pump(ns *nodeSession) {
	for {
		select {
		case <-vl.done:
			return
		case pkt := <-vl.out:
			if err := vl.trk.WriteRTP(pkt); err != nil {
				dropViewer(ns, vl, "WriteRTP: "+err.Error())
				return
			}
			// lastSeq — найновіший seq у буфері ретрансмісії ЦІЄЇ ноги: рівно
			// те, що pion назве highestAdded. Пишемо тут, а не в
			// forwardToViewers: між чергою і треком стоїть до
			// viewerQueueDepth пакетів, і на цю різницю поїхала б оцінка
			// NACK_recovered_ratio (nack.go).
			atomic.StoreUint32(&vl.lastSeq, uint32(pkt.SequenceNumber))
			if atomic.AddUint64(&vl.sent, 1) == 1 && !vl.born.IsZero() {
				metricsTTFF(ns, time.Since(vl.born))
			}
		}
	}
}
