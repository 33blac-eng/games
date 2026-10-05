// Звук ПК на агентській стороні: agent/audio (WASAPI loopback, 48 кГц) ->
// Opus 48 кГц стерео (або запасний μ-law 8 кГц моно) -> ДРУГА доріжка того самого WebRTC-зʼєднання,
// що везе відео. Жодного паралельного механізму: доріжка додається в тому ж
// dialWebRTC, живе рівно стільки, скільки транспорт, і вмирає разом із ним.
//
// 🔴 ПРАПОРЕЦЬ OO_SCREEN_AUDIO=1, ТИПОВО ВИМКНЕНО — рівно як у хабі (hub/cmd/
// hub-webrtc/audio.go). Без нього: кодек не реєструється в MediaEngine, доріжка
// не додається, offer бітово той самий, горутина не стартує, WASAPI не
// відкривається взагалі.
//
// F1: КОДЕК — Opus 48 кГц стерео (internal/opusenc, чистий Go, без cgo).
// Старий μ-law 8 кГц моно лишився запасним: OO_SCREEN_AUDIO_CODEC=pcmu. Хаб
// мусить мати ТОЙ САМИЙ кодек (той самий env), інакше він звук агента не
// форвардить і пише про це в лог.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/organicoils/oo-screen/agent/audio"
	"github.com/organicoils/oo-screen/internal/opusenc"
	"github.com/organicoils/oo-screen/internal/pcmu"
)

// audioCodec — кодек звукової доріжки (OO_SCREEN_AUDIO_CODEC, типово opus).
// Змінна: тести перемикають її напряму.
var audioCodec = func() opusenc.Codec {
	c, err := opusenc.FromEnv()
	if err != nil {
		log.Printf("oo-agent: %v", err)
	}
	return c
}()

// audioEnabled — той самий прапорець, що в хабі. Змінна, а не os.Getenv на
// місці: тести перемикають її напряму.
var audioEnabled = os.Getenv("OO_SCREEN_AUDIO") == "1"

const (
	// audioSyncTolerance — наскільки звуковій доріжці дозволено відстати від
	// мітки кадру, перш ніж ми доллємо тишу. ±40 мс — межа, оголошена для
	// робочого стола; менший поріг ганяв би вставки на кожному джиттері
	// WASAPI-пакета (вони приходять не строго по 10 мс).
	audioSyncTolerance = 40 * time.Millisecond

	// audioPollTimeout — з яким дедлайном чекаємо пакет. Потрібен НЕ для
	// «звуку немає» (тиша в loopback — це реальні нулі, і вони приходять
	// пакетами), а щоб гейт міг зупинити захоплення, поки NextFrame стоїть на
	// джерелі, яке справді завмерло.
	audioPollTimeout = 250 * time.Millisecond

	// audioReopenAfter — пауза перед повторною спробою відкрити пристрій.
	// Відмова не назавжди: людина могла висмикнути навушники саме в цю мить,
	// і глушити звук до перезапуску агента через це не можна.
	audioReopenAfter = 5 * time.Second
)

// registerAudioCodec додає кодек звуку (audioCodec) у MediaEngine — ЛИШЕ під
// прапорцем. PT: 111 для Opus (як у Chrome), 0 для PCMU (RFC 3551).
func registerAudioCodec(m *webrtc.MediaEngine) error {
	if !audioEnabled {
		return nil
	}
	return m.RegisterCodec(audioCodec.Parameters(), webrtc.RTPCodecTypeAudio)
}

// addAudioTrack вішає звукову доріжку на зʼєднання агента. nil, nil без
// прапорця — і саме на цьому nil тримається вся інваріантність вимкненого
// режиму: немає доріжки -> немає m=audio в offer -> хаб бачить старого агента.
//
// TrackLocalStaticSample, а не StaticRTP: і μ-law, і Opus-кадр 20 мс — це
// рівно один RTP-пакет, тож пакетизацію робить pion.
func addAudioTrack(pc *webrtc.PeerConnection) (*webrtc.TrackLocalStaticSample, error) {
	if !audioEnabled {
		return nil, nil
	}
	trk, err := webrtc.NewTrackLocalStaticSample(audioCodec.Capability(), "audio", "oo-screen-agent")
	if err != nil {
		return nil, err
	}
	sender, err := pc.AddTrack(trk)
	if err != nil {
		return nil, err
	}
	// Обовʼязковий RTCP read-loop, як і у відеодоріжки: без нього interceptor-и
	// цього sender-а мертві.
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	return trk, nil
}

// audioRejected — чи хаб відхилив m=audio (порт 0). Так відповідає хаб із
// вимкненим OO_SCREEN_AUDIO. Рядок формату в такому m-рядку pion пише "0", а це
// статичний PT PCMU: з μ-law-доріжкою це випадково «збігалось», з Opus —
// SetRemoteDescription падає, тому відхилену доріжку знімаємо самі.
func audioRejected(sdp string) bool {
	for _, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "m=audio 0 ") {
			return true
		}
	}
	return false
}

// detachAudioTrack знімає звукову доріжку з ще не домовленого зʼєднання.
func detachAudioTrack(pc *webrtc.PeerConnection, trk webrtc.TrackLocal) error {
	for _, tr := range pc.GetTransceivers() {
		if s := tr.Sender(); s != nil && s.Track() == trk {
			return pc.RemoveTrack(s)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// audioEncoder — 48 кГц N каналів -> 8 кГц моно μ-law, кадрами по 20 мс.
//
// Стан переживає межі WASAPI-пакетів навмисно: пакет приходить довільної
// довжини (~10 мс, але не гарантовано), а віддавати треба рівні 20 мс — інакше
// тривалість семпла в media.Sample стрибала б, і разом з нею RTP-годинник.
// ---------------------------------------------------------------------------
type audioEncoder struct {
	acc float64 // сума семплів поточної групи децимації
	n   int     // скільки семплів у групі
	out []byte  // недобраний кадр μ-law

	// emitted — скільки семплів 8 кГц доріжка вже віддала. Це і є її годинник:
	// з нього рахується розрив (audioGap), а не зі стінного часу.
	emitted int64
}

// push приймає один моно-семпл вихідної частоти й, коли набереться кадр,
// повертає його.
//
// ponytail: децимація — box-фільтр на factor семплів (просте середнє). Його
// нулі стоять РІВНО на кратних 8 кГц, тобто там, куди складається найгірше
// дзеркало; смуги між ними він давить слабо, і на різкому шумі це чути як
// легкий призвук. Апгрейд робиться на місці: замінити середнє на FIR
// (наприклад 48-точковий півсмуговий), решта тракту не змінюється.
func (e *audioEncoder) push(sample float64, factor int) []byte {
	e.acc += sample
	e.n++
	if e.n < factor {
		return nil
	}
	v := e.acc / float64(factor)
	e.acc, e.n = 0, 0

	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	e.out = append(e.out, pcmu.Encode(int16(v*math.MaxInt16)))
	return e.take()
}

// silence доливає n семплів 8 кГц тиші й повертає всі кадри, що з цього
// склались. Викликається лише з audioGap — тобто коли доріжка ФАКТИЧНО відстала
// від спільного годинника, а не коли в loopback тихо.
func (e *audioEncoder) silence(n int64) [][]byte {
	var out [][]byte
	for i := int64(0); i < n; i++ {
		e.out = append(e.out, pcmu.Silence)
		if f := e.take(); f != nil {
			out = append(out, f)
		}
	}
	return out
}

// take віддає кадр, щойно він набрався.
func (e *audioEncoder) take() []byte {
	if len(e.out) < pcmu.FrameSamples {
		return nil
	}
	frame := e.out[:pcmu.FrameSamples]
	e.out = append([]byte(nil), e.out[pcmu.FrameSamples:]...)
	e.emitted += pcmu.FrameSamples
	return frame
}

// encode перетворює один WASAPI-пакет на готові кадри PCMU. Порожній результат
// — нормальний стан: у пакеті просто менше 20 мс звуку.
func (e *audioEncoder) encode(data []byte, f audio.Format) [][]byte {
	factor, step, ok := audioLayout(f, pcmu.Rate)
	if !ok {
		return nil
	}
	var out [][]byte
	for off := 0; off+step <= len(data); off += step {
		mono := 0.0
		for ch := 0; ch < f.Channels; ch++ {
			mono += audio.Sample(data[off+ch*(f.BitsPerSample/8):], f)
		}
		if frame := e.push(mono/float64(f.Channels), factor); frame != nil {
			out = append(out, frame)
		}
	}
	return out
}

// audioLayout розкладає формат на «у скільки разів проріджуємо» і «скільки
// байтів займає один кадр усіх каналів». ok=false — формат, у якому ми не
// беремось за звук; викликач мусить його ЗГАСИТИ, а не подати сміття далі.
func audioLayout(f audio.Format, outRate int) (factor, step int, ok bool) {
	if f.Channels <= 0 || f.BitsPerSample <= 0 || f.BitsPerSample%8 != 0 {
		return 0, 0, false
	}
	if outRate <= 0 || f.SampleRate <= 0 || f.SampleRate%outRate != 0 {
		return 0, 0, false // 44.1 кГц дробовим кроком не проріджується
	}
	switch {
	case f.SampleFormat == audio.SampleFormatFloat && f.BitsPerSample == 32:
	case f.SampleFormat == audio.SampleFormatPCM &&
		(f.BitsPerSample == 8 || f.BitsPerSample == 16 || f.BitsPerSample == 24 || f.BitsPerSample == 32):
	default:
		return 0, 0, false
	}
	step = f.BytesPerFrame
	if step <= 0 {
		step = f.Channels * f.BitsPerSample / 8
	}
	if step < f.Channels*f.BitsPerSample/8 {
		return 0, 0, false
	}
	return f.SampleRate / outRate, step, true
}

// audioGap — скільки семплів (частоти rate) треба долити тишею, щоб доріжка наздогнала
// спільний годинник.
//
// 🚨 ЄДИНИЙ МОНОТОННИЙ ГОДИННИК. elapsed — це різниця audio.Frame.Timestamp
// цього пакета й ПЕРШОГО пакета сесії. agent/audio ставить цю мітку з того
// самого QPC-домену, з якого йде capture.NV12Frame.Captured, тобто звук і відео
// міряють час одним приладом. Стінного time.Now() тут немає навмисно: він
// стрибає від NTP і зсунув би звук відносно відео рівно на величину корекції.
//
// Розрив у loopback — це не тиша (тиша приходить нулями), а факт: пристрій
// перезапустили, потік застряг, ми стояли на паузі. Без доливання доріжка
// назавжди лишилась би на ту паузу позаду відео.
func audioGap(elapsed time.Duration, emitted int64, tol time.Duration, rate int) int64 {
	if elapsed <= 0 || rate <= 0 {
		return 0
	}
	r := time.Duration(rate)
	gap := int64(elapsed*r/time.Second) - emitted
	if gap < int64(tol*r/time.Second) {
		return 0
	}
	return gap
}

// ---------------------------------------------------------------------------

// audioSend — як віддати готовий кадр у транспорт.
type audioSend func(data []byte, dur time.Duration) error

// audioCapturer — рівно те, що runAudio просить у джерела. Інтерфейс, а не
// *audio.Capturer, лише заради шва audioOpen: WASAPI в тесті не піднімеш, а
// інваріант «send впав -> звук вертається в НОВУ доріжку» перевірити треба.
type audioCapturer interface {
	Format() audio.Format
	NextFrame(ctx context.Context) (*audio.Frame, error)
	Close() error
}

// audioOpen — шов для тесту. У бою це рівно audio.New.
var audioOpen = func() (audioCapturer, error) { return audio.New() }

// runAudio — увесь життєвий цикл звуку агента. Кличеться однією горутиною з
// main() і повертається лише разом із ctx.
//
// 🚨 ГЕЙТИНГ ОКРЕМО ВІД ВІДЕО, АЛЕ ЗА ТИМ САМИМ СИГНАЛОМ. paused — той самий
// gatePaused, яким хаб керує відео. Поки глядача немає, пристрій НЕ ВІДКРИТИЙ
// зовсім: не «читаємо й викидаємо», а не захоплюємо. RMS у гейтингу не бере
// участі НІ В ЯКОМУ ВИГЛЯДІ — тиша в loopback це справжні нулі, і сплутати її з
// «нема глядача» означало б глушити звук щоразу, коли на тому ПК просто нічого
// не грає.
func runAudio(ctx context.Context, paused *atomic.Bool, send audioSend) {
	fails := 0
	for {
		if !audioWait(ctx, paused) {
			return
		}
		c, err := audioOpen()
		// Непідтримуваний mix format — та сама «відмова відкрити», що й помилка
		// audio.New: з паузою audioReopenAfter. Без цього audioCapture виходив на
		// першому ж пакеті, і цикл open/close крутився гарячим, по три рядки логу
		// на пакет.
		if err == nil {
			if _, _, ok := audioLayout(c.Format(), audioClockRate()); !ok {
				err = fmt.Errorf("формат звуку не підтримано (%+v) — доріжка лишиться тихою", c.Format())
				_ = c.Close()
			}
		}
		if err != nil {
			fails++
			// Логуємо першу відмову й далі рідко: ПК без звукової карти не
			// повинен засипати лог, але й замовкнути назовсім не можна —
			// пристрій міг просто на мить зникнути.
			if fails == 1 || fails%12 == 0 {
				log.Printf("oo-agent: audio.New: %v (спроба %d, звук поки без джерела)", err, fails)
			}
			if !audioSleep(ctx, audioReopenAfter) {
				return
			}
			continue
		}
		if fails > 0 {
			log.Printf("oo-agent: аудіо-джерело відкрито після %d невдалих спроб", fails)
			fails = 0
		}
		log.Printf("oo-agent: аудіо-захоплення почалось (%+v -> %s)", c.Format(), audioCodecLabel())
		audioCapture(ctx, paused, c, send)
		_ = c.Close()
		log.Printf("oo-agent: аудіо-захоплення зупинено (глядача немає або джерело впало)")
	}
}

// audioClockRate — частота годинника доріжки поточного кодека (до створення
// енкодера: перевірка mix format при відкритті джерела).
func audioClockRate() int {
	if audioCodec == opusenc.CodecPCMU {
		return pcmu.Rate
	}
	return opusenc.Rate
}

// audioWait тримає горутину, поки немає глядача. false = час іти.
func audioWait(ctx context.Context, paused *atomic.Bool) bool {
	for paused.Load() {
		if !audioSleep(ctx, 50*time.Millisecond) {
			return false
		}
	}
	return ctx.Err() == nil
}

func audioSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// audioCapture — цикл одного відкритого пристрою. Повертається, коли зник
// глядач, помер ctx або джерело віддало помилку.
func audioCapture(ctx context.Context, paused *atomic.Bool, c audioCapturer, send audioSend) {
	enc, err := newFrameEncoder(audioCodec)
	if err != nil {
		log.Printf("oo-agent: енкодер звуку (%s): %v — доріжка лишиться тихою", audioCodec, err)
		return
	}
	var (
		first   time.Time
		lastLog = time.Now()
		peak    float64
		frames  int
		gaps    int64
	)
	for {
		if ctx.Err() != nil || paused.Load() {
			return
		}

		waitCtx, cancel := context.WithTimeout(ctx, audioPollTimeout)
		f, err := c.NextFrame(waitCtx)
		cancel()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				continue // джерело мовчить — вертаємось перевірити гейт
			}
			if ctx.Err() == nil {
				log.Printf("oo-agent: audio.NextFrame: %v", err)
			}
			return
		}

		if _, _, ok := audioLayout(f.Format, enc.rate()); !ok {
			// Мовчки крутити цей цикл далі означало б «звуку немає і невідомо
			// чому». Пристрій із таким mix format ми не обслуговуємо — кажемо
			// це вголос і йдемо; відео не зачеплене.
			log.Printf("oo-agent: формат звуку не підтримано (%+v) — доріжка лишиться тихою", f.Format)
			return
		}
		if first.IsZero() {
			first = f.Timestamp
		}
		if f.RMS > peak {
			peak = f.RMS
		}

		gap := audioGap(f.Timestamp.Sub(first), enc.clock(), audioSyncTolerance, enc.rate())
		gaps += gap
		out := enc.silence(gap)
		out = append(out, enc.encode(f.Data, f.Format)...)
		for _, fr := range out {
			if err := send(fr, audioCodec.FrameDur(fr)); err != nil {
				// Мертва аудіо-доріжка НЕ привід рвати сесію: розрив помітить
				// відеошлях (txErrCh/pcDown) і підніме транспорт заново, а
				// наступний send уже піде в нову доріжку.
				log.Printf("oo-agent: audio send: %v", err)
				return
			}
			frames++
		}

		if time.Since(lastLog) >= 30*time.Second {
			// RMS — саме тут і ТІЛЬКИ тут: показати оператору різницю між
			// «звук іде, але на ПК тихо» і «звуку немає взагалі».
			log.Printf("oo-agent: audio frames=%d peakRMS=%.4f gapSamples=%d", frames, peak, gaps)
			lastLog, peak, gaps = time.Now(), 0, 0
		}
	}
}

// audioTrackSample — обгортка WriteSample для webrtcTransport. Винесена, щоб
// nil-доріжка (прапорець вимкнено, нога WT) була одним місцем, а не перевіркою
// на кожному виклику.
func audioTrackSample(trk *webrtc.TrackLocalStaticSample, data []byte, dur time.Duration) error {
	if trk == nil {
		return nil
	}
	return trk.WriteSample(media.Sample{Data: data, Duration: dur})
}
