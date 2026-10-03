//go:build windows

package main

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/agent/audio"
	"github.com/organicoils/oo-screen/internal/pcmu"
)

// withAudioFlag ставить прапорець на час тесту і повертає його назад — той
// самий прийом, що в хабі (hub/cmd/hub-webrtc/audio_test.go).
func withAudioFlag(t *testing.T, on bool) {
	t.Helper()
	prev := audioEnabled
	audioEnabled = on
	t.Cleanup(func() { audioEnabled = prev })
}

// agentOffer будує offer агента рівно тим шляхом, що dialWebRTC: той самий
// MediaEngine, та сама відеодоріжка, той самий addAudioTrack. Мережі тут немає —
// перевіряємо саме SDP, тобто те, що агент СКАЖЕ про себе хабу.
func agentOffer(t *testing.T) (string, *webrtc.TrackLocalStaticSample) {
	t.Helper()

	api, err := newWebRTCAPI()
	if err != nil {
		t.Fatalf("newWebRTCAPI: %v", err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("NewPeerConnection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	video, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpLine,
	}, "video", "oo-screen-agent")
	if err != nil {
		t.Fatalf("video track: %v", err)
	}
	if _, err := pc.AddTrack(video); err != nil {
		t.Fatalf("AddTrack(video): %v", err)
	}
	atrk, err := addAudioTrack(pc)
	if err != nil {
		t.Fatalf("addAudioTrack: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	return pc.LocalDescription().SDP, atrk
}

func countMediaLines(sdp, kind string) int {
	n := 0
	for _, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "m="+kind+" ") {
			n++
		}
	}
	return n
}

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

// TestAudioFlagOnPublishesPCMUTrack — під прапорцем агент оголошує другу доріжку
// саме тим кодеком, який приймає браузер (див. internal/pcmu: перевірено живцем
// у Chrome 148 — PCMU/8000 є в RTCRtpReceiver.getCapabilities('audio')).
func TestAudioFlagOnPublishesPCMUTrack(t *testing.T) {
	withAudioFlag(t, true)

	sdp, atrk := agentOffer(t)
	if atrk == nil {
		t.Fatal("addAudioTrack не дав доріжки під прапорцем")
	}
	if n := countMediaLines(sdp, "audio"); n != 1 {
		t.Fatalf("m=audio у offer агента: %d, want 1:\n%s", n, sdp)
	}
	if !strings.Contains(strings.ToUpper(sdp), "PCMU/8000") {
		t.Fatalf("у offer немає PCMU/8000:\n%s", sdp)
	}
}

// float48kStereo — пакет WASAPI у найпоширенішому mix format: 48 кГц, стерео,
// float32. Канали різні навмисно — щоб перевірити саме зведення в моно, а не
// «взяли перший канал і не помітили».
func float48kStereo(frames int, left, right func(i int) float64) ([]byte, audio.Format) {
	f := audio.Format{
		SampleRate: 48000, Channels: 2,
		SampleFormat:  audio.SampleFormatFloat,
		BitsPerSample: 32, ValidBitsPerSample: 32,
		BytesPerFrame: 8,
	}
	data := make([]byte, 0, frames*8)
	for i := 0; i < frames; i++ {
		data = binary.LittleEndian.AppendUint32(data, math.Float32bits(float32(left(i))))
		data = binary.LittleEndian.AppendUint32(data, math.Float32bits(float32(right(i))))
	}
	return data, f
}

// TestEncoderRateAndFraming — арифметика тракту: 48 кГц -> 8 кГц це рівно 6:1, і
// кадр PCMU це рівно 20 мс. Зламана децимація (наприклад 1:1 або «взяли кожен
// шостий, але з іншим кроком») тут одразу дає інше число кадрів.
func TestEncoderRateAndFraming(t *testing.T) {
	var e audioEncoder
	// 1 секунда звуку -> 50 кадрів по 20 мс.
	data, f := float48kStereo(48000, func(int) float64 { return 0.5 }, func(int) float64 { return 0.5 })
	frames := e.encode(data, f)
	if len(frames) != 50 {
		t.Fatalf("із 1с звуку вийшло %d кадрів, want 50", len(frames))
	}
	for i, fr := range frames {
		if len(fr) != pcmu.FrameSamples {
			t.Fatalf("кадр %d має %d байт, want %d", i, len(fr), pcmu.FrameSamples)
		}
	}
	if e.emitted != 50*int64(pcmu.FrameSamples) {
		t.Fatalf("годинник доріжки = %d семплів, want %d", e.emitted, 50*pcmu.FrameSamples)
	}
	// Постійні 0.5 на обох каналах мусять дати постійні ~0.5 і на виході:
	// зведення в моно — середнє, а не сума (сума дала б кліп на 1.0).
	got := pcmu.Decode(frames[10][0])
	wantF := 0.5 * float64(math.MaxInt16)
	if want := int16(wantF); got < want-400 || got > want+400 {
		t.Fatalf("семпл після зведення = %d, want ≈%d (сума замість середнього?)", got, want)
	}
}

// TestSilenceIsRealData — 🚨 ІНВАРІАНТ ЗАВДАННЯ: тиша в loopback це реальні
// нулі, а не обрив. Нулі на вході мусять дати ПОВНОЦІННІ кадри на виході (тишу
// в μ-law), а не «нічого не сталось». Якби кодер тут мовчав, доріжка провалилась
// би в розрив на кожній паузі в звуці, і годинник поїхав би на всю її довжину.
func TestSilenceIsRealData(t *testing.T) {
	var e audioEncoder
	data, f := float48kStereo(4800, func(int) float64 { return 0 }, func(int) float64 { return 0 })
	frames := e.encode(data, f)
	if len(frames) != 5 {
		t.Fatalf("із 100мс тиші вийшло %d кадрів, want 5", len(frames))
	}
	for i, fr := range frames {
		for j, b := range fr {
			if v := pcmu.Decode(b); v != 0 {
				t.Fatalf("кадр %d семпл %d = %d, want 0 (тиша має кодуватись у нуль)", i, j, v)
			}
		}
	}
}

// TestAudioGap — правило єдиного годинника. Мітка кадру (той самий QPC-домен, що
// й у відео) вирішує, чи доріжка відстала; допуск ±40 мс — оголошена межа для
// робочого стола.
func TestAudioGap(t *testing.T) {
	const tol = audioSyncTolerance
	cases := []struct {
		name    string
		elapsed time.Duration
		emitted int64
		want    int64
	}{
		{"рівно в такт", time.Second, pcmu.Rate, 0},
		{"відстали на 20мс — у межах допуску", time.Second, pcmu.Rate - 160, 0},
		{"відстали на 39мс — ще в межах", time.Second, pcmu.Rate - 312, 0},
		{"відстали на 100мс — доливаємо", time.Second, pcmu.Rate - 800, 800},
		{"джерело стало на 2с", 3 * time.Second, pcmu.Rate, 2 * pcmu.Rate},
		{"випередили (кадрів більше за час)", time.Second, pcmu.Rate + 800, 0},
		{"перший кадр", 0, 0, 0},
	}
	for _, c := range cases {
		if got := audioGap(c.elapsed, c.emitted, tol); got != c.want {
			t.Errorf("%s: audioGap(%v, %d) = %d, want %d", c.name, c.elapsed, c.emitted, got, c.want)
		}
	}
}

// TestSilenceFillKeepsClock — доливання тиші мусить рухати ГОДИННИК доріжки, а
// не тільки видавати байти: інакше наступний audioGap побачив би той самий
// розрив і лив би тишу вічно.
func TestSilenceFillKeepsClock(t *testing.T) {
	var e audioEncoder
	frames := e.silence(800) // 100 мс
	if len(frames) != 5 {
		t.Fatalf("100мс тиші дали %d кадрів, want 5", len(frames))
	}
	if e.emitted != 800 {
		t.Fatalf("годинник = %d, want 800", e.emitted)
	}
	if got := audioGap(100*time.Millisecond, e.emitted, audioSyncTolerance); got != 0 {
		t.Fatalf("після доливання розрив = %d, want 0 — тиша лилась би нескінченно", got)
	}
}

// TestUnsupportedFormatsRejected — формат, який ми не розберемо, мусить бути
// ВІДХИЛЕНИЙ явно (audioLayout), а не мовчки перетворитись на тишу: інакше на
// такому ПК «звуку немає» і причини не видно.
func TestUnsupportedFormatsRejected(t *testing.T) {
	base := audio.Format{
		SampleRate: 48000, Channels: 2, SampleFormat: audio.SampleFormatFloat,
		BitsPerSample: 32, ValidBitsPerSample: 32, BytesPerFrame: 8,
	}
	if _, _, ok := audioLayout(base); !ok {
		t.Fatal("48кГц стерео float32 відхилено — це і є типовий mix format WASAPI")
	}

	bad := map[string]func(f *audio.Format){
		"44.1кГц не ділиться на 8000": func(f *audio.Format) { f.SampleRate = 44100 },
		"нуль каналів":                func(f *audio.Format) { f.Channels = 0 },
		"float64":                     func(f *audio.Format) { f.BitsPerSample = 64 },
		"невідомий формат семпла":     func(f *audio.Format) { f.SampleFormat = audio.SampleFormatUnknown },
	}
	for name, mutate := range bad {
		f := base
		mutate(&f)
		if _, _, ok := audioLayout(f); ok {
			t.Errorf("%s: audioLayout прийняв %+v", name, f)
		}
	}

	// PCM 16 біт мусить проходити — це другий за поширеністю mix format.
	f := base
	f.SampleFormat, f.BitsPerSample, f.ValidBitsPerSample, f.BytesPerFrame = audio.SampleFormatPCM, 16, 16, 4
	if _, _, ok := audioLayout(f); !ok {
		t.Fatal("48кГц стерео PCM16 відхилено")
	}
}
