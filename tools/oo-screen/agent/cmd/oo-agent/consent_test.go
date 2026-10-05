package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/consent"
)

type yesUI struct{ yes bool }

func (u yesUI) Ask(context.Context, string) bool { return u.yes }
func (yesUI) ShowIndicator(func())               {}
func (yesUI) HideIndicator()                     {}

// S3: без локальної згоди канал вводу НЕ інʼєктує нічого — навіть валідну
// подію з квитком grant=control. Після згоди — інʼєктує; після «Завершити» —
// знову ні.
func TestInputBlockedWithoutConsent(t *testing.T) {
	prev := consentGate
	defer func() { consentGate = prev }()
	raw := []byte(`{"v":1,"type":"mouse_move","x":0.5,"y":0.5}`)

	consentGate = consent.New(consent.Config{Policy: consent.AlwaysAsk, UI: yesUI{false}, Timeout: time.Second})
	paused := true
	sig := consentGate.Wrap(func(r bool) { paused = !r })
	inj := &recordingInjector{}
	sig(true) // хаб каже «є глядач» — UI відповідає «Ні»
	time.Sleep(20 * time.Millisecond)
	if err := handleInputMessage(raw, inj); !errors.Is(err, errNoConsent) || len(inj.got) != 0 || !paused {
		t.Fatalf("denied: err=%v injected=%d paused=%v", err, len(inj.got), paused)
	}

	consentGate = consent.New(consent.Config{Policy: consent.AlwaysAsk, UI: yesUI{true}, Timeout: time.Second})
	sig = consentGate.Wrap(func(r bool) { paused = !r })
	sig(true)
	for i := 0; i < 200 && !consentGate.Allowed(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if err := handleInputMessage(raw, inj); err != nil || len(inj.got) != 1 {
		t.Fatalf("granted: err=%v injected=%d", err, len(inj.got))
	}
	consentGate.End()
	if err := handleInputMessage(raw, inj); !errors.Is(err, errNoConsent) || len(inj.got) != 1 {
		t.Fatalf("after End: err=%v injected=%d", err, len(inj.got))
	}
}

func TestInputUnaffectedWhenConsentOff(t *testing.T) {
	prev := consentGate
	defer func() { consentGate = prev }()
	consentGate = nil
	inj := &recordingInjector{}
	if err := handleInputMessage([]byte(`{"v":1,"type":"mouse_move","x":0.5,"y":0.5}`), inj); err != nil || len(inj.got) != 1 {
		t.Fatalf("policy off must be legacy behaviour: %v", err)
	}
}
