package main

import (
	"errors"
	"testing"

	"github.com/organicoils/oo-screen/agent/input"
)

type recordingInjector struct {
	got []input.Event
	err error
}

func (r *recordingInjector) Inject(ev input.Event) error {
	r.got = append(r.got, ev)
	return r.err
}

// 1. З ВИМКНЕНИМ прапорцем інʼєктора немає, а отже й каналу вводу немає зовсім:
// dialWebRTC створює "oosc-input" рівно тоді, коли newInputInjector() != nil.
// Червоніє, якщо прибрати перевірку inputEnabled.
func TestNoInjectorWithoutFlag(t *testing.T) {
	prev := inputEnabled
	defer func() { inputEnabled = prev }()

	inputEnabled = false
	if inj := newInputInjector(); inj != nil {
		t.Fatalf("без OO_SCREEN_INPUT інʼєктора не має бути, отримано %#v", inj)
	}
}

// 2. Клавіша їде СКАН-КОДОМ і доходить до інʼєктора саме такою. Це не
// косметика: віртуальний код на ПК з українською розкладкою дав би українську
// літеру замість набраної.
func TestHandleInputMessageKeyKeepsScancode(t *testing.T) {
	inj := &recordingInjector{}
	// KeyA -> скан-код 0x1E; unicode 'a' іде поруч, але фізична клавіша
	// головніша (див. agent/input: Unicode читається лише при Scancode == 0).
	raw := []byte(`{"v":1,"type":"key","down":true,"scancode":30,"unicode":97}`)
	if err := handleInputMessage(raw, inj); err != nil {
		t.Fatalf("валідна подія мала пройти: %v", err)
	}
	if len(inj.got) != 1 {
		t.Fatalf("до інʼєктора мала дійти рівно одна подія, дійшло %d", len(inj.got))
	}
	ev := inj.got[0]
	if ev.Kind != input.KindKey || ev.Scancode != 30 || !ev.Down || ev.Unicode != 'a' {
		t.Fatalf("подію спотворено по дорозі: %+v", ev)
	}
}

// 3. Миша: нормалізовані 0..1 доходять як є (перерахунок у пікселі — робота
// agent/input, не наша).
func TestHandleInputMessageMouseMove(t *testing.T) {
	inj := &recordingInjector{}
	if err := handleInputMessage([]byte(`{"v":1,"type":"mouse_move","x":0.25,"y":0.75}`), inj); err != nil {
		t.Fatalf("валідний рух мав пройти: %v", err)
	}
	ev := inj.got[0]
	if ev.X == nil || ev.Y == nil || *ev.X != 0.25 || *ev.Y != 0.75 {
		t.Fatalf("координати спотворено: %+v", ev)
	}
}

// 4. Усе, що не пройшло input.Validate, до SendInput НЕ доходить. Канал —
// межа довіри, і за нею чужий ПК.
func TestHandleInputMessageRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"не json":           `{`,
		"чужа версія":       `{"v":9,"type":"mouse_move","x":0.5,"y":0.5}`,
		"координата поза 1": `{"v":1,"type":"mouse_move","x":1.5,"y":0.5}`,
		"невідомий тип":     `{"v":1,"type":"reboot"}`,
		"клавіша ні з чим":  `{"v":1,"type":"key","down":true}`,
		"колесо в нікуди":   `{"v":1,"type":"mouse_wheel","wheel_x":0,"wheel_y":0}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			inj := &recordingInjector{}
			err := handleInputMessage([]byte(raw), inj)
			if err == nil {
				t.Fatalf("подія мала бути відкинута")
			}
			if !errors.Is(err, input.ErrInvalidEvent) {
				t.Fatalf("очікував ErrInvalidEvent, отримав %v", err)
			}
			if len(inj.got) != 0 {
				t.Fatalf("до SendInput не сміло дійти нічого, дійшло %d", len(inj.got))
			}
		})
	}
}
