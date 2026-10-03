//go:build windows

// Windows-only: offerReq is declared in main.go, which is windows-only.
package main

import (
	"strings"
	"testing"
)

// TestAudioFlagOffChangesNothing — ГОЛОВНИЙ тест кроку на стороні агента: без
// OO_SCREEN_AUDIO агент поводиться рівно як до появи звуку. Це захист робочого
// проду, а не фічі.
//
// Три половини правди в одному місці, бо вимкнена фіча мусить бути невидимою
// цілком, а не «майже»: доріжки немає (nil -> sendAudio нікуди не пише і
// runAudio не стартує), m=audio в offer немає, PCMU у MediaEngine не
// зареєстрований (інакше він проліз би в SDP при першому ж чужому трансивері).
func TestAudioFlagOffChangesNothing(t *testing.T) {
	withAudioFlag(t, false)

	sdp, atrk := agentOffer(t)
	if atrk != nil {
		t.Fatal("addAudioTrack віддав доріжку при вимкненому прапорці")
	}
	if n := countMediaLines(sdp, "audio"); n != 0 {
		t.Fatalf("m=audio у offer агента: %d, want 0:\n%s", n, sdp)
	}
	if n := countMediaLines(sdp, "video"); n != 1 {
		t.Fatalf("m=video у offer агента: %d, want 1:\n%s", n, sdp)
	}
	if strings.Contains(strings.ToUpper(sdp), "PCMU") {
		t.Fatalf("PCMU у offer при вимкненому прапорці:\n%s", sdp)
	}
	// Поле offer-а теж мусить лишитись відсутнім: інакше хаб на тій нозі
	// чекав би звук, якого нема, і глушив би запасний тон.
	if req := (offerReq{Audio: atrk != nil}); req.Audio {
		t.Fatal("offerReq.Audio=true при вимкненому прапорці")
	}
}
