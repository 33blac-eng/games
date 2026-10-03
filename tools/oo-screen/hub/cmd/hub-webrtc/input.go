// Ввід глядача: браузер -> хаб -> агент ВЛАСНИМ каналом, замість паралельного
// MeshCentral.
//
// 🔴 ПРАПОРЕЦЬ OO_SCREEN_INPUT=1, ТИПОВО ВИМКНЕНО — рівно як OO_SCREEN_AUDIO.
// Без нього хаб мусить поводитись бітово так, як до появи цього файла: канал
// вводу не приймається ні з боку глядача, ні з боку агента, обробників немає,
// у SDP нічого не змінюється. Прод працює; Mesh лишається тим самим шляхом
// вводу, яким і був, і ЖОДЕН його рядок звідси не чіпається.
//
// СТИЛЬ — той самий, що в oosc-ctl: іменований DataChannel того самого
// PeerConnection, який уже везе медіа. Не друге зʼєднання, не другий порт, не
// другий сигналінг. Хаб тут — той самий авторитетний транслятор, що й для
// control: повідомлення глядача НЕ йде до агента без перевірки.
//
// 🔴 ЦЕ КАНАЛ ПОВНОГО КОНТРОЛЮ НАД ЧУЖИМ ПК. Тому три засувки, і всі три —
// на боці хаба, а не браузера:
//
//  1. ТІКЕТ. Кожне повідомлення несе той самий одноразовий ERP-квиток, яким
//     відкрилась ця ж viewer-нога (offerReq.Ticket). Порівняння —
//     constant-time, і не збіглось = рвемо ВСЮ сесію глядача (dropViewer), а
//     не лише канал вводу: якщо на цьому каналі опинився хтось чужий, дивитись
//     йому теж більше нема чого.
//  2. ЧАСТОТА. ~200 подій/с на клієнта. Коалесинг рухів робить браузер, тож
//     чесний глядач у стелю не впирається ніколи; впирається лише той, хто
//     жене потік навмисно.
//  3. РОЗМІР. Одна подія — це десятки байтів; кілобайт тут не буває, і
//     приймати його нема причини.
//
// Відкликання на льоту вже є і НЕ дублюється: revoke.go рве ноги, канал вводу
// живе рівно стільки, скільки PeerConnection, і вмирає разом з ним.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
	"golang.org/x/time/rate"
)

// inputChannelLabel — мітка каналу вводу на ОБОХ ногах: глядач відкриває його
// до хаба, агент — до хаба. Одна мітка, бо це один і той самий потік подій.
const inputChannelLabel = "oosc-input"

// grantControl — єдине значення grant, яке відчиняє канал вводу. Рядок, а не
// bool: у квитку вже є поле довільного дозволу, і чесніше звірятися з ним, ніж
// заводити другий, паралельний спосіб сказати те саме.
const grantControl = "control"

const (
	// inputRatePerSec — стеля подій на КЛІЄНТА. Орієнтир ~200/с: миша з
	// коалесингом дає ~80/с (12 мс), клавіатура на автоповторі — ще десятки,
	// кнопки й колесо — одиниці. Тобто вдвічі-втричі більше за найгучнішу
	// чесну роботу і на порядок менше за те, чим канал можна залити.
	inputRatePerSec = 200

	// inputBurst — миттєвий сплеск. Події їдуть пачками (натиснув-відпустив,
	// прокрутив), і рівний token-bucket без запасу різав би саме їх. 40 ≈
	// пʼята частка секунди роботи.
	inputBurst = 40

	// inputMaxBytes — стеля одного повідомлення. Найдовша реальна подія —
	// mouse_move з двома float64 і тікетом, це <200 байтів.
	inputMaxBytes = 1024
)

// inputEnabled — прапорець фічі. Змінна, а не os.Getenv на місці: тести
// перемикають її напряму (і повертають назад), як audioEnabled.
var inputEnabled = os.Getenv("OO_SCREEN_INPUT") == "1"

// viewerInputMsg — конверт від браузера. Тікет ЗОВНІ події навмисно: агенту
// він не потрібен і не поїде — до агента йде рівно Event, байт у байт.
type viewerInputMsg struct {
	Ticket string          `json:"ticket"`
	Event  json.RawMessage `json:"event"`
}

// inputVerdict — що робити з одним повідомленням.
type inputVerdict int

const (
	// inputAccept — переслати агенту.
	inputAccept inputVerdict = iota
	// inputDrop — викинути мовчки (крім лога): битий кадр, перебір частоти,
	// завеликий розмір. Сесію НЕ чіпаємо: це або шум, або новіший браузер із
	// полем, якого ми ще не знаємо — те саме правило форвардної сумісності,
	// що в internal/control ("невідомий type ігнорується").
	inputDrop
	// inputKill — рвати ВСЮ сесію глядача. Рівно один привід: тікет не той.
	inputKill
)

// judgeInput вирішує долю одного повідомлення каналу вводу. Чиста функція —
// саме тому три засувки перевіряються тестом без живого PeerConnection.
//
// Порядок навмисний: спершу «хто ти», потім «скільки тобі можна». Флуд із
// чужим тікетом має вбити сесію на ПЕРШОМУ ж повідомленні, а не витратити на
// себе квоту й піти в лог як «перевищення частоти».
func judgeInput(data []byte, ticket, grant string, lim *rate.Limiter, now time.Time) (inputVerdict, json.RawMessage, string) {
	if ticket == "" {
		// Нога без тікета (T1 static-token режим) не має права на ввід узагалі.
		return inputKill, nil, "нога без тікета не має права на ввід"
	}
	// 🔴 Рівень дозволу вирішує ЕРП і кладе у квиток; хаб лише виконує. Тікет
	// на перегляд не дає керувати чужою клавіатурою — і саме тому ЕРП більше
	// не приймає grant полем запиту (ScreenEngineController::ticket): інакше
	// ця перевірка була б декоративною, бо браузер вписував би сюди "control"
	// сам собі.
	if grant != grantControl {
		return inputKill, nil, "квиток без дозволу на керування (grant=" + grant + ")"
	}
	if len(data) > inputMaxBytes {
		return inputDrop, nil, "повідомлення завелике"
	}
	var m viewerInputMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return inputDrop, nil, "битий JSON"
	}
	if subtle.ConstantTimeCompare([]byte(m.Ticket), []byte(ticket)) != 1 {
		return inputKill, nil, "чужий або відкликаний тікет"
	}
	// Поля немає взагалі, або воно null: агент таке однаково відкине, тож
	// хопа до нього ця подія не варта.
	if len(m.Event) == 0 || string(m.Event) == "null" {
		return inputDrop, nil, "порожня подія"
	}
	if !lim.AllowN(now, 1) {
		return inputDrop, nil, "перевищено стелю подій/с"
	}
	return inputAccept, m.Event, ""
}

// viewerInputHandler — обробник каналу вводу ноги. Кличеться ЛИШЕ під
// прапорцем і ЛИШЕ для ноги з тікетом; setupViewerLeg кличе його
// зі спільного OnDataChannel (pion тримає лише один такий колбек на PC).
func viewerInputHandler(ns *nodeSession, vl *viewerLeg, ticket, grant string) func(*webrtc.DataChannel) {
	lim := rate.NewLimiter(inputRatePerSec, inputBurst)
	// Окремий обмежувач САМОГО ЛОГА: відкинуті події — це рівно той випадок,
	// коли їх багато, і writeln на кожну перетворив би захист від флуду на
	// власний флуд у журнал.
	logLim := rate.NewLimiter(1, 1)
	// Окремий, ДУЖЕ рідкий лічильник для «агенту нема куди слати»: стан не
	// миттєвий, а тривалий — поки ПК без прапорця, кожна подія падає в нікуди,
	// і навіть один рядок на секунду залив би журнал. Раз на 30 с достатньо,
	// щоб причина знайшлась із першого `journalctl | grep input`.
	noChanLim := rate.NewLimiter(rate.Every(30*time.Second), 1)

	return func(dc *webrtc.DataChannel) {
		log.Printf("input: viewer channel open [node=%s]", ns.nodeID)
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			now := time.Now()
			verdict, ev, why := judgeInput(msg.Data, ticket, grant, lim, now)
			switch verdict {
			case inputAccept:
				// F-39: людина клацає — отже, дивиться. Знімаємо прихованість,
				// навіть якщо її POST /viewer/visibility загубився або прийшов
				// не в тому порядку. Це страховка в бік «слати», а не «різати».
				unhideViewer(ns, vl)
				if !sendInputToAgent(ns, ev) && noChanLim.AllowN(now, 1) {
					log.Printf("input: агент цієї ноди НЕ має каналу вводу [node=%s] — "+
						"події глядача летять у нікуди (ПК розкочено без -input?)", ns.nodeID)
				}
			case inputDrop:
				if logLim.AllowN(now, 1) {
					log.Printf("input: подію відкинуто [node=%s]: %s", ns.nodeID, why)
				}
			case inputKill:
				log.Printf("input: РВУ СЕСІЮ ГЛЯДАЧА [node=%s]: %s", ns.nodeID, why)
				dropViewer(ns, vl, "input: "+why)
			}
		})
	}
}

// sendInputToAgent пише подію в канал вводу агента ЦІЄЇ ноди. Немає каналу
// (старий агент, прапорець у нього вимкнений, агент саме перепідключається) —
// подія просто зникає: ввід не варто чергувати, бо застаріле натискання гірше
// за ненатиснуте.
//
// Повертає, чи БУЛО КУДИ слати. Не «доставлено»: доставку по каналу підтвердить
// хіба що сам агент. Розрізняти ці два стани довелось 31.08: миша на «Computer»
// не працювала, глядач відкривав канал справно, і хаб мовчав — бо цей самий
// return був беззвучний. Пошук причини зайняв двадцять хвилин рівно тому, що
// єдиний, хто знав правду, нічого про неї не сказав.
func sendInputToAgent(ns *nodeSession, ev []byte) bool {
	ns.mu.Lock()
	dc := ns.agentInput
	ns.mu.Unlock()
	// H-09: закритий канал (агент саме перепідключається) — теж «нема куди».
	if dc == nil {
		return false
	}
	if st := dc.ReadyState(); st == webrtc.DataChannelStateClosing || st == webrtc.DataChannelStateClosed {
		return false
	}
	if err := dc.Send(ev); err != nil && inputSendErrLim.Allow() {
		log.Printf("input: не доставлено агенту [node=%s]: %v", ns.nodeID, err)
	}
	return true
}

// inputSendErrLim — тротлінг логу невдалих Send: одна подія миші = один рядок
// давало 534 записи за 72 год на кожному реконекті агента (H-09).
var inputSendErrLim = rate.NewLimiter(rate.Every(5*time.Second), 1)
