package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"golang.org/x/time/rate"
)

// Ці три тести — і є три засувки каналу повного контролю над чужим ПК. Кожен
// має вміти почервоніти: приберіть перевірку тікета в judgeInput — впаде
// перший, приберіть AllowN — впаде другий, приберіть ticket == "" — третій.

const testTicket = "tkt-Ab12"

func msg(t *testing.T, ticket string, event any) []byte {
	t.Helper()
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	b, err := json.Marshal(viewerInputMsg{Ticket: ticket, Event: raw})
	if err != nil {
		t.Fatalf("marshal msg: %v", err)
	}
	return b
}

func freshLimiter() *rate.Limiter { return rate.NewLimiter(inputRatePerSec, inputBurst) }

// move — подія руху миші рівно в тому вигляді, у якому її шле браузер.
func move(x, y float64) map[string]any {
	return map[string]any{"v": 1, "type": "mouse_move", "x": x, "y": y}
}

// 1. Подія з ВАЛІДНИМ тікетом проходить, і до агента їде рівно вкладена подія —
// без тікета, байт у байт.
func TestJudgeInputAcceptsOwnTicket(t *testing.T) {
	verdict, ev, why := judgeInput(msg(t, testTicket, move(0.5, 0.25)), testTicket, grantControl, freshLimiter(), time.Now())
	if verdict != inputAccept {
		t.Fatalf("свій тікет мав пройти, отримано verdict=%d (%s)", verdict, why)
	}
	var got map[string]any
	if err := json.Unmarshal(ev, &got); err != nil {
		t.Fatalf("до агента поїхав не JSON: %v", err)
	}
	if got["type"] != "mouse_move" || got["x"] != 0.5 || got["y"] != 0.25 {
		t.Fatalf("подія спотворена по дорозі: %v", got)
	}
	if _, leaked := got["ticket"]; leaked {
		t.Fatalf("тікет протік до агента: %v", got)
	}
}

// 2. Подія БЕЗ валідного тікета відкидається — і рве ВСЮ сесію, а не лише канал.
func TestJudgeInputForeignTicketKillsSession(t *testing.T) {
	cases := map[string]string{
		"чужий тікет":     "tkt-foreign",
		"порожній тікет":  "",
		"майже той самий": testTicket + "x",
	}
	for name, sent := range cases {
		t.Run(name, func(t *testing.T) {
			verdict, ev, why := judgeInput(msg(t, sent, move(0.1, 0.1)), testTicket, grantControl, freshLimiter(), time.Now())
			if verdict != inputKill {
				t.Fatalf("мав бути inputKill, отримано verdict=%d (%s)", verdict, why)
			}
			if ev != nil {
				t.Fatalf("подія без валідного тікета не сміє їхати до агента: %s", ev)
			}
		})
	}
}

// 2б. Нога взагалі без тікета (T1 static-token режим) не має права на ввід —
// перше ж повідомлення рве сесію, навіть якщо тікет у конверті «правильний».
func TestJudgeInputLegWithoutTicketNeverAllowed(t *testing.T) {
	verdict, _, why := judgeInput(msg(t, "", move(0.1, 0.1)), "", grantControl, freshLimiter(), time.Now())
	if verdict != inputKill {
		t.Fatalf("нога без тікета мала отримати inputKill, отримано verdict=%d (%s)", verdict, why)
	}
	if !strings.Contains(why, "без тікета") {
		t.Fatalf("причина мала називати відсутній тікет, отримано %q", why)
	}
}

// 3. Перевищення частоти відкидається — але сесію НЕ рве: це шум, а не чужий.
func TestJudgeInputRateLimit(t *testing.T) {
	lim := freshLimiter()
	now := time.Now() // час стоїть -> відро не поповнюється
	data := msg(t, testTicket, move(0.5, 0.5))

	for i := 0; i < inputBurst; i++ {
		if v, _, why := judgeInput(data, testTicket, grantControl, lim, now); v != inputAccept {
			t.Fatalf("подія %d із дозволених %d мала пройти, verdict=%d (%s)", i, inputBurst, v, why)
		}
	}
	v, ev, why := judgeInput(data, testTicket, grantControl, lim, now)
	if v != inputDrop {
		t.Fatalf("подія понад сплеск мала бути відкинута, verdict=%d", v)
	}
	if ev != nil {
		t.Fatalf("відкинута подія не сміє їхати до агента: %s", ev)
	}
	if !strings.Contains(why, "стелю") {
		t.Fatalf("причина мала називати стелю подій/с, отримано %q", why)
	}

	// А через секунду квота відновлюється: обмежувач гальмує флуд, а не роботу.
	if v, _, why := judgeInput(data, testTicket, grantControl, lim, now.Add(time.Second)); v != inputAccept {
		t.Fatalf("через секунду подія мала пройти, verdict=%d (%s)", v, why)
	}
}

// 4. Сміття й завеликі повідомлення просто відкидаються (форвардна сумісність:
// новіший браузер із зайвим полем не має класти сесію).
func TestJudgeInputDropsJunk(t *testing.T) {
	lim := freshLimiter()
	now := time.Now()
	if v, _, _ := judgeInput([]byte("не json"), testTicket, grantControl, lim, now); v != inputDrop {
		t.Fatalf("битий JSON мав бути відкинутий, verdict=%d", v)
	}
	big := make([]byte, inputMaxBytes+1)
	if v, _, _ := judgeInput(big, testTicket, grantControl, lim, now); v != inputDrop {
		t.Fatalf("завелике повідомлення мало бути відкинуте, verdict=%d", v)
	}
	noEvent := []byte(`{"ticket":"` + testTicket + `"}`)
	if v, _, _ := judgeInput(noEvent, testTicket, grantControl, lim, now); v != inputDrop {
		t.Fatalf("конверт без події мав бути відкинутий, verdict=%d", v)
	}
	if v, _, _ := judgeInput(msg(t, testTicket, nil), testTicket, grantControl, lim, now); v != inputDrop {
		t.Fatalf("подія null мала бути відкинута, verdict=%d", v)
	}
}

// 5. Немає каналу до агента — подія просто зникає, без паніки й без черги.
// Але зникає ГУЧНО: функція мусить сказати, що слати не було куди, інакше
// «миша не працює» знову діагностується наосліп (31.08, ПК «Computer»).
func TestSendInputToAgentWithoutChannel(t *testing.T) {
	ns := &nodeSession{nodeID: "n-no-agent"}
	if sendInputToAgent(ns, []byte(`{"v":1,"type":"mouse_move","x":0,"y":0}`)) {
		t.Fatal("без каналу агента функція сказала, що було куди слати")
	}
}

// Дзеркальна половина: канал Є -> функція каже «є куди», і мовчазного рядка в
// журналі не буде. Без неї «фікс» можна було б зробити, повертаючи false завжди.
func TestSendInputToAgentWithChannel(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	defer func() { _ = pc.Close() }()

	dc, err := pc.CreateDataChannel(inputChannelLabel, nil)
	if err != nil {
		t.Fatalf("CreateDataChannel: %v", err)
	}
	ns := &nodeSession{nodeID: "n-with-agent", agentInput: dc}
	if !sendInputToAgent(ns, []byte(`{"v":1,"type":"mouse_move","x":0,"y":0}`)) {
		t.Fatal("канал агента є, а функція сказала, що слати нема куди")
	}
}

// Квиток на ПЕРЕГЛЯД не відчиняє керування. Рве сесію, а не просто ковтає
// подію: тікет із чужим рівнем дозволу — це не помилка мережі, а спроба
// зробити більше, ніж видано.
func TestInputRejectsNonControlGrant(t *testing.T) {
	for _, g := range []string{"view", "", "Control", "control ", "admin"} {
		v, _, why := judgeInput(msg(t, testTicket, move(0.5, 0.5)), testTicket, g, freshLimiter(), time.Now())
		if v != inputKill {
			t.Errorf("grant=%q пустили до керування (вердикт %v, %s)", g, v, why)
		}
	}
}

// Дзеркальна половина: правильний дозвіл ПРОХОДИТЬ. Без цієї перевірки
// засувку можна було б «полагодити», заблокувавши геть усе.
func TestInputAcceptsControlGrant(t *testing.T) {
	v, _, why := judgeInput(msg(t, testTicket, move(0.5, 0.5)), testTicket, grantControl, freshLimiter(), time.Now())
	if v != inputAccept {
		t.Fatalf("правильний дозвіл не пройшов: %v, %s", v, why)
	}
}
