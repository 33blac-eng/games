//go:build windows

package main

import (
	"testing"
	"time"
)

// Дефект: на ПЕРШОМУ AU різниці PTS ще нема, і замість тривалості кадру в pion
// їхав АБСОЛЮТНИЙ PTS. WriteSample крутить RTP-годинник на
// Duration.Seconds()*clockRate, тож одна така відправка зсувала шкалу на весь
// час, який сесія прожила. Перший AU буває не лише на старті: реконект робить
// транспорт заново (havePrev=false), а captureSeq рахує далі — тобто що довше
// жила сесія до обриву, то більший стрибок. Після 2147afe9 PTS іде за стінним
// годинником, тому «час життя» тут — справжні хвилини, а не хвилини/fps.
func TestSampleDurationFirstAU(t *testing.T) {
	const (
		fps       = 30
		clockRate = 90000 // H.264 RTP clock, media engine у newWebRTCAPI
	)
	frameInterval := time.Second / fps

	// 42 хвилини сесії до реконекту.
	const lived = 42 * time.Minute
	tr := &webrtcTransport{frameInterval: frameInterval}

	got := tr.sampleDuration(lived)
	if got != frameInterval {
		t.Fatalf("перший AU (сесія жила %v): тривалість %v, хочемо кадровий інтервал %v — RTP-годинник поїхав би на %.0f тіків (%v) уперед",
			lived, got, frameInterval,
			(got-frameInterval).Seconds()*clockRate, got-frameInterval)
	}
	// Нуль тут теж неправильний: наступний кадр дістав би ту саму RTP-мітку,
	// а це для депакетизатора ОДИН момент дискретизації (RFC 6184 §5.1).
	if got <= 0 {
		t.Fatalf("перший AU: тривалість %v — наступний кадр дістане ту саму RTP-мітку", got)
	}

	// Другий AU: різниця вже є, і саме вона має їхати в pion (стінний час, а
	// не фіксований 1/fps) — це те, що доведено живим прогоном 2147afe9.
	const waited = 700 * time.Millisecond
	if got := tr.sampleDuration(lived + waited); got != waited {
		t.Fatalf("другий AU через %v: тривалість %v, хочемо реальну різницю PTS %v", waited, got, waited)
	}

	// AU з тим самим PTS (кілька AU з однієї кодованої картинки, напр.
	// IDR + trailing delta): різниця нульова — фолбек знову кадровий інтервал,
	// а не абсолютний PTS.
	if got := tr.sampleDuration(lived + waited); got != frameInterval {
		t.Fatalf("AU з незмінним PTS: тривалість %v, хочемо кадровий інтервал %v", got, frameInterval)
	}
}
