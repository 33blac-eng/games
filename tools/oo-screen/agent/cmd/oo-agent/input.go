// Ввід від глядача на боці агента: канал "oosc-input" ТОГО САМОГО
// WebRTC-зʼєднання, що везе відео й звук -> agent/input -> SendInput.
//
// 🔴 ПРАПОРЕЦЬ OO_SCREEN_INPUT=1, ТИПОВО ВИМКНЕНО — той самий, що в хабі
// (hub/cmd/hub-webrtc/input.go), рівно як OO_SCREEN_AUDIO. Без нього агент
// каналу не відкриває, інʼєктора не створює, і поводиться бітово так, як до
// появи цього файла. Ввід через MeshCentral при цьому лишається недоторканим:
// цей файл про НЬОГО нічого не знає й нічого в ньому не міняє.
//
// Розбір і перевірка події — НЕ тут: усе це вже вирішено й заміряно в
// agent/input (ParseEvent + Validate + нормалізовані 0..1 координати + скан-коди
// замість віртуальних кодів). Тут лише склейка каналу з інʼєктором, і саме тому
// файл без build-тега: він компілюється всюди, а Windows-only частину
// (SendInput) тримає agent/input.
package main

import (
	"errors"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/agent/input"
	"github.com/organicoils/oo-screen/internal/consent"
)

// consentGate — S3 (internal/consent). nil = політика off, ввід як раніше.
// Ставить main до першого dial; читає канал вводу.
var consentGate *consent.Gate

var errNoConsent = errors.New("input dropped: no local consent (S3)")

// inputChannelLabel — та сама мітка, що в хабі. Один канал, одна назва.
const inputChannelLabel = "oosc-input"

// inputMoveChannelLabel — ДРУГИЙ канал вводу хаб -> агент (хвиля 10, затримка
// миші): невпорядкований і без ретрансмісій ({Ordered:false,
// MaxRetransmits:0}). Рух, загублений на нозі хаб-агент, не тримає в черзі
// SCTP наступні (head-of-line у надійному впорядкованому "oosc-input": при
// 1 % втрат кожен загублений пакет — +RTT·k до ВСІХ подальших рухів).
// Приймаємо ЛИШЕ mouse_move: кнопка чи клавіша без ретрансмісії могла б
// лишитися затиснутою. Старий хаб каналу не бере — рух іде "oosc-input".
const inputMoveChannelLabel = "oosc-input-move"

// inputMoveChannelInit — параметри move-каналу (ненадійний, невпорядкований).
func inputMoveChannelInit() *webrtc.DataChannelInit {
	ordered := false
	var retr uint16
	return &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &retr}
}

// handleMoveMessage — повідомлення move-каналу: лише mouse_move, решта —
// відкидається (захист від затиснутих кнопок/клавіш без ретрансмісії).
func handleMoveMessage(data []byte, inj eventInjector) error {
	if !consentGate.Allowed() {
		return errNoConsent
	}
	ev, err := input.ParseEvent(data)
	if err != nil {
		return err
	}
	if ev.Kind != input.KindMouseMove {
		return errMoveOnly
	}
	return inj.Inject(ev)
}

var errMoveOnly = errors.New("input dropped: у " + inputMoveChannelLabel + " дозволено лише mouse_move")

// attachInputMoveChannel — move-канал на той самий інʼєктор.
func attachInputMoveChannel(dc *webrtc.DataChannel, inj eventInjector) {
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if err := handleMoveMessage(msg.Data, inj); err != nil {
			logInputProblem(time.Now(), err)
		}
	})
}

// inputEnabled — той самий прапорець, що в хабі. Змінна, а не os.Getenv на
// місці: тест перемикає її напряму, як audioEnabled.
var inputEnabled = os.Getenv("OO_SCREEN_INPUT") == "1"

// eventInjector — рівно те, що від інʼєктора потрібно каналу. Інтерфейс тут не
// абстракція «на майбутнє», а єдиний спосіб перевірити розбір повідомлення
// тестом: справжній Injector стріляє в живу мишу тієї машини, де йде тест.
type eventInjector interface {
	Inject(input.Event) error
	ReleaseAll() error
}

// attachInputChannel вішає канал вводу на інʼєктор. На закритті каналу
// (реконект, розрив, хаб закрив ногу) — ReleaseAll: key-up, який глядач так і
// не надіслав, уже не прийде цим каналом ніколи, а клавіша на цьому ПК
// лишилась би затиснутою, доки людина за ним не натисне її сама.
func attachInputChannel(dc *webrtc.DataChannel, inj eventInjector) {
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if err := handleInputMessage(msg.Data, inj); err != nil {
			logInputProblem(time.Now(), err)
		}
	})
	dc.OnClose(func() {
		if err := inj.ReleaseAll(); err != nil {
			log.Printf("oo-agent: канал вводу закрито, відпустити клавіші не вдалось: %v", err)
		}
	})
}

// handleInputMessage — одне повідомлення каналу вводу. Помилку повертає, а не
// логує: викликач глушить лог від флуду (logInputProblem), і робити це двічі
// не треба.
func handleInputMessage(data []byte, inj eventInjector) error {
	// S3: без локальної згоди ввід не інʼєктується, що б не прислав хаб.
	if !consentGate.Allowed() {
		return errNoConsent
	}
	ev, err := input.ParseEvent(data)
	if err != nil {
		return err
	}
	return inj.Inject(ev)
}

// newInputInjector повертає інʼєктор або nil, якщо ввід недоступний: вимкнений
// прапорець, не-Windows, або SendInput недосяжний на цьому робочому столі.
// nil означає «каналу не буде», а не «канал буде й мовчатиме»: мовчазний канал
// виглядав би для глядача як живий ввід, який нічого не робить.
func newInputInjector() *input.Injector {
	if !inputEnabled {
		return nil
	}
	inj, err := input.New()
	if err != nil {
		log.Printf("oo-agent: ввід вимкнено — %v", err)
		return nil
	}
	return inj
}

// logInputThrottle — не частіше ніж раз на секунду. Биті події приходять
// пачками (розʼїхались версії, чужий відправник), і рядок на кожну перетворив
// би журнал на смітник рівно тоді, коли він найпотрібніший.
const logInputThrottle = time.Second

var lastInputLog atomic.Int64

func logInputProblem(now time.Time, err error) {
	prev := lastInputLog.Load()
	if now.UnixNano()-prev < int64(logInputThrottle) {
		return
	}
	if !lastInputLog.CompareAndSwap(prev, now.UnixNano()) {
		return
	}
	log.Printf("oo-agent: подію вводу відкинуто: %v", err)
}

// applyFeatureFlags вмикає фічі з аргументів командного рядка.
//
// 🔴 Без цього обидві фічі були б написані й НЕДОСЯЖНІ в бою: на робочі ПК
// агента ставить Register-ScheduledTask, яка передає лише -Argument, а
// змінних середовища у задачі нема взагалі (той самий глухий кут, який колись
// вирішили прапорцем -token).
//
// Тільки вмикає, ніколи не вимикає: -audio=false не мусить гасити те, що
// людина свідомо ввімкнула через OO_SCREEN_AUDIO у своєму середовищі.
func applyFeatureFlags(audio, input bool) {
	if audio {
		audioEnabled = true
	}
	if input {
		inputEnabled = true
	}
}

// releaseHeldInput — SEC #37: відпустити все, що інʼєктор затиснув і не
// відпустив (обрив сесії посеред Ctrl+перетягування). nil — нічого не робить.
func releaseHeldInput(inj *input.Injector, why string) {
	if inj == nil {
		return
	}
	k, b := inj.Held()
	if k == 0 && b == 0 {
		return
	}
	if err := inj.ReleaseAll(); err != nil {
		log.Printf("oo-agent: відпускання затиснутих (%s): %v", why, err)
		return
	}
	log.Printf("oo-agent: відпущено %d клавіш і %d кнопок миші (%s)", k, b, why)
}

// releaseOnPause загортає onGate: пауза (жодного видимого глядача) відпускає
// затиснуте. Канал вводу живе між агентом і хабом і переживає відхід
// глядача, тож одного OnClose каналу мало.
func releaseOnPause(inj *input.Injector, onGate func(bool)) func(bool) {
	if inj == nil || onGate == nil {
		return onGate
	}
	return func(resume bool) {
		if !resume {
			releaseHeldInput(inj, "no viewers")
		}
		onGate(resume)
	}
}
