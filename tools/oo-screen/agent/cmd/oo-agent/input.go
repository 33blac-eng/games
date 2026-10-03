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
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/organicoils/oo-screen/agent/input"
)

// inputChannelLabel — та сама мітка, що в хабі. Один канал, одна назва.
const inputChannelLabel = "oosc-input"

// inputEnabled — той самий прапорець, що в хабі. Змінна, а не os.Getenv на
// місці: тест перемикає її напряму, як audioEnabled.
var inputEnabled = os.Getenv("OO_SCREEN_INPUT") == "1"

// eventInjector — рівно те, що від інʼєктора потрібно каналу. Інтерфейс тут не
// абстракція «на майбутнє», а єдиний спосіб перевірити розбір повідомлення
// тестом: справжній Injector стріляє в живу мишу тієї машини, де йде тест.
type eventInjector interface {
	Inject(input.Event) error
}

// handleInputMessage — одне повідомлення каналу вводу. Помилку повертає, а не
// логує: викликач глушить лог від флуду (logInputProblem), і робити це двічі
// не треба.
func handleInputMessage(data []byte, inj eventInjector) error {
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
