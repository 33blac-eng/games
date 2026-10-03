package main

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/organicoils/oo-screen/internal/control"
)

const (
	// disconnectGrace — скільки терпіти PeerConnectionStateDisconnected, перш
	// ніж вважати сесію втраченою. Стан ТРАНЗІЄНТНИЙ (зміна маршруту, коротко
	// втрачений шлях ICE) і часто повертається в connected сам. Число і правило
	// не вигадані тут: те саме вже стоїть у глядача —
	// total-erp-app/resources/js/remote/desktop-oo-webrtc.js,
	// createDisconnectGrace (DEFAULT_DISCONNECT_GRACE_MS = 4000).
	disconnectGrace = 4 * time.Second

	// Витримка реконекту. Стеля потрібна не агенту, а ХАБУ: коли він лежить,
	// увесь парк ломиться в нього одночасно, і без верхньої межі агент стає
	// молотаркою рівно на тому підйомі, який мав дочекатись.
	reconnectBackoffMin = 1 * time.Second
	reconnectBackoffMax = 30 * time.Second

	// reacquireBackoffMin — старт бек-офу повторного захоплення екрана (A-02):
	// стеля в циклі = keepaliveAfter, бо довша пауза лишила б сторож без кадру.
	reacquireBackoffMin = 200 * time.Millisecond

	// iceGatherTimeout — дедлайн збору ICE-кандидатів у dial (A-29).
	iceGatherTimeout = 5 * time.Second
	// idrDebounce — не частіше одного IDR за запитом (A-31).
	idrDebounce = 300 * time.Millisecond
	// mouseOnlyGap — мінімальний інтервал між кадрами «лише курсор» (A-08).
	mouseOnlyGap = 66 * time.Millisecond
)

// nextBackoff — наступна витримка: подвоєння зі стелею.
func nextBackoff(cur time.Duration) time.Duration {
	if next := cur * 2; next < reconnectBackoffMax {
		return next
	}
	return reconnectBackoffMax
}

// jitterBackoff — ±20% розкид. Без нього три ПК, що впали від ОДНОГО
// перезапуску хаба, стукають у нього синхронно і далі: їхні витримки однакові
// за побудовою, тож вони так і йдуть строєм.
func jitterBackoff(d time.Duration) time.Duration {
	spread := int64(d) / 5
	if spread <= 0 {
		return d
	}
	return d + time.Duration(rand.Int64N(2*spread+1)-spread)
}

// reconnectDecider вирішує, КОЛИ рвати сесію й перепідключатись — за станом
// PeerConnection, а не за помилкою надсилання.
//
// 🚨 Чому не за помилкою надсилання: без глядача агент стоїть на паузі
// (on-demand гейтинг, gatePaused у main) і НЕ шле нічого. Немає надсилання →
// немає помилки → немає реконекту. Живий випадок 30.08 01:11: перезапуск хаба,
// у лозі агента "webrtc ICE: failed / webrtc PC state: failed" — і далі тиша.
// Процес живий, задача Running, ПК мовчки випав із парку; вартовий з
// -MultipleInstances IgnoreNew теж мовчить, бо процес живий.
//
// Правило — те саме, що вже доведене в глядача (createDisconnectGrace):
//   - failed/closed → негайно (стан остаточний);
//   - disconnected  → лише якщо за disconnectGrace не відновилось;
//   - connected     → скасовує відкладене рішення.
//
// Одноразовий НАВМИСНО: за рішенням іде close() транспорту, а той сам породжує
// "closed" — і без цього прапорця власне закриття рахувалося б новою причиною
// реконекту. Кожен dial робить СВІЙ decider, тож наступна сесія починає чисто.
type reconnectDecider struct {
	grace time.Duration
	fire  func(reason string)

	mu    sync.Mutex
	timer *time.Timer
	gen   uint64 // покоління grace-таймера: скасований таймер не має права спрацювати
	fired bool
}

func newReconnectDecider(grace time.Duration, fire func(reason string)) *reconnectDecider {
	return &reconnectDecider{grace: grace, fire: fire}
}

// note приймає webrtc.PeerConnectionState.String(). Рядком, а не типом pion,
// щоб правило перевірялось тестом без живого PeerConnection.
func (d *reconnectDecider) note(state string) {
	d.mu.Lock()
	if d.fired {
		d.mu.Unlock()
		return
	}
	switch state {
	case "failed", "closed":
		d.cancelLocked()
		d.fired = true
		d.mu.Unlock()
		d.fire("pc-" + state)
	case "disconnected":
		if d.timer != nil {
			d.mu.Unlock()
			return // grace уже йде — не перезапускаємо його новим тим самим станом
		}
		d.gen++
		myGen := d.gen
		d.timer = time.AfterFunc(d.grace, func() {
			d.mu.Lock()
			if d.fired || d.gen != myGen { // скасовано (connected) або вже вирішено
				d.mu.Unlock()
				return
			}
			d.timer, d.fired = nil, true
			d.mu.Unlock()
			d.fire("pc-disconnected-grace")
		})
		d.mu.Unlock()
	case "connected":
		d.cancelLocked()
		d.mu.Unlock()
	default:
		d.mu.Unlock()
	}
}

// cancelLocked гасить grace-таймер. gen++ обовʼязковий: Stop не чекає колбек,
// який уже почав виконуватись і стоїть на mu, тож саме розбіжність поколінь
// не дає йому вистрелити після скасування.
func (d *reconnectDecider) cancelLocked() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.gen++
}

// pending — чи відкладене рішення зараз тікає.
func (d *reconnectDecider) pending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.timer != nil
}

// ---------------------------------------------------------------------------

// hubLiveness — ДРУГИЙ сторож, незалежний від PeerConnectionState.
//
// 🔴 Навіщо, якщо є reconnectDecider. Consent freshness (RFC 7675) у pion
// рахується від ВЛАСНОЇ транспортної активності: таймер заводять надіслані
// STUN binding indications. У нас on-demand гейтинг — без глядача агент не
// кодує і не шле НІЧОГО, тож таймер не заводиться, і стан PeerConnection
// просто ЗАМЕРЗАЄ на "connected". reconnectDecider читає рівно цей стан, отже
// за побудовою не побачить нічого.
//
// Живий випадок 30.08: агент підключився о 08:41:51, о 08:44 хаб
// перезапустили — і агент ВІСІМ ХВИЛИН вважав себе підключеним, не написавши в
// лог жодного рядка, поки хаб давно зняв публікатора. Вартовий задачі теж
// мовчить: він піднімає МЕРТВИЙ процес, а цей живий і просто заклинив.
//
// ОЗНАКА ЖИТТЯ — будь-яке повідомлення в control-каналі "oosc-ctl", під яке хаб
// б'є рівний пульс (control.Heartbeat кожні control.HeartbeatInterval)
// НЕЗАЛЕЖНО від присутності глядача. Ні RTCP, ні успішна відправка на цю роль
// не годяться: на паузі агент не шле медіа, зустрічного RTCP може не бути
// взагалі, і тиша в медіа-тракті на простої — це НОРМА. Плутати її з обривом
// не можна, тому ознака мусить існувати саме на паузі.
//
// ⚠️ РОЗЗБРОЄНИЙ, поки не прийшло перше повідомлення від хаба. Це не
// оптимізація, а захист від найдорожчої помилки: новий агент проти СТАРОГО
// хаба, який пульсу не шле, інакше рвав би справне з'єднання кожні
// HeartbeatTimeout на кожному простоюючому ПК парку.
type hubLiveness struct {
	timeout time.Duration

	mu    sync.Mutex
	armed bool
	last  time.Time
}

// hubLive — сторож цього процесу. Один на бінар, як outputs у main.go: агент
// тримає рівно одне з'єднання з хабом, і другого сторожа тут нема для кого.
var hubLive = hubLiveness{timeout: control.HeartbeatTimeout}

// beat — від хаба щось прийшло, отже він живий. Заразом ОЗБРОЮЄ сторожа:
// перше повідомлення і є доказом, що цей хаб уміє пульсувати.
func (l *hubLiveness) beat(now time.Time) {
	l.mu.Lock()
	l.armed, l.last = true, now
	l.mu.Unlock()
}

// expire — хаб САМ сказав, що знімає ногу (control.Shutdown). Чекати повного
// порогу нема сенсу: відсуваємо останній удар рівно на поріг, і рішення визріє
// на найближчому оберті кадрового циклу.
func (l *hubLiveness) expire(now time.Time) {
	l.mu.Lock()
	l.armed, l.last = true, now.Add(-l.timeout)
	l.mu.Unlock()
}

// silent — чи час рвати. Час передається, а не береться з time.Now(), щоб
// правило перевірялось тестом на керованому часі, без жодного сну.
//
// ОДНОРАЗОВО: рішення знімає зброю, і наступне має право визріти лише в НОВІЙ
// сесії, коли звідти прийде перший удар. Без цього мертвий хаб давав би
// рішення на КОЖНОМУ оберті циклу — тобто реконект поверх реконекту.
func (l *hubLiveness) silent(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.armed || now.Sub(l.last) < l.timeout {
		return false
	}
	l.armed = false
	return true
}

// disarm — нова сесія починає з чистого аркуша: доказ, що ЦЕЙ хаб пульсує, ще
// не пред'явлено. Кличеться на початку кожного dial, бо реконект буває й з
// іншої причини (помилка відправки, стан PeerConnection), і тоді сторож
// лишався б озброєним зі СТАРИМ останнім ударом — тобто міг би вирішити рвати
// щойно підняту сесію ще до її першого удару.
func (l *hubLiveness) disarm() {
	l.mu.Lock()
	l.armed = false
	l.mu.Unlock()
}

// noteHubMessage — єдина точка, куди заходить УСЕ прийняте з "oosc-ctl", і то
// лише заради живості: розбір команд лишається в handleCtlMessage. Виділено
// окремо, щоб сторож перевірявся без живого PeerConnection.
//
// Кличеться з колбека pion, тому тут не сміє бути нічого блокуючого: колбеки
// виконуються на горутині ICE, і затримка в них зупиняє обробку STUN.
func noteHubMessage(l *hubLiveness, now time.Time, data []byte) {
	l.beat(now)
	if m, ok := parseCtlJSON(data); ok && m.Type == control.TypeShutdown {
		l.expire(now)
	}
}

// ---------------------------------------------------------------------------
// A-28: пауза захоплення на час реконекту.

// reconnectGate тримає гейт піднятим, поки агент дозвонюється до хаба, і
// повертає його на місце після дозвону.
//
// 🚨 Навіщо. Реконект триває стільки, скільки лежить хаб — тобто хвилинами.
// Кадровий цикл увесь цей час стоїть у dial, але капчер лишається відкритим, а
// звукова горутина живе окремо й шле пакети в мертвий транспорт. Відкрита
// DXGI-дублікація в простої — це рівно та регресія 01.09, від якої рятує гейт:
// MeshCentral на тому ж ПК не може захопити екран і рве свою desktop-сесію.
//
// paused — той самий gatePaused, яким хаб керує відео і звуком. seen — «хаб
// уже сказав своє слово про гейт»; його ставить onGate на БУДЬ-ЯКИЙ сигнал.
type reconnectGate struct {
	paused, seen *atomic.Bool
	wasPaused    bool
}

// beginReconnectGate ставить паузу і запамʼятовує, що було до неї.
func beginReconnectGate(paused, seen *atomic.Bool) reconnectGate {
	g := reconnectGate{paused: paused, seen: seen}
	g.wasPaused = paused.Swap(true)
	// Слово хаба рахуємо з ЦЬОГО моменту: сигнал попередньої (мертвої) сесії
	// про нову нічого не каже.
	seen.Store(false)
	return g
}

// restore знімає паузу — але лише ту, яку поставили ми, і лише якщо новий хаб
// за час дозвону про гейт не висловився.
//
// Затерти свіжий pause означало б захоплювати екран без глядача (та сама
// боротьба з Mesh), затерти resume — стояти на паузі при живому глядачі. Тому
// обидва «ні»: були на паузі до реконекту — лишаємось; хаб уже сказав — його
// слово головніше.
func (g reconnectGate) restore() {
	if g.wasPaused || g.seen.Load() {
		return
	}
	g.paused.CompareAndSwap(true, false)
}
