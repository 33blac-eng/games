// Звук ПК на агентській стороні: agent/audio (WASAPI loopback, 48 кГц) ->
// моно -> 8 кГц -> G.711 μ-law -> ДРУГА доріжка того самого WebRTC-зʼєднання,
// що везе відео. Жодного паралельного механізму: доріжка додається в тому ж
// dialWebRTC, живе рівно стільки, скільки транспорт, і вмирає разом із ним.
//
// 🔴 ПРАПОРЕЦЬ OO_SCREEN_AUDIO=1, ТИПОВО ВИМКНЕНО — рівно як у хабі (hub/cmd/
// hub-webrtc/audio.go). Без нього: кодек не реєструється в MediaEngine, доріжка
// не додається, offer бітово той самий, горутина не стартує, WASAPI не
// відкривається взагалі.
//
// ЧОМУ μ-law, А НЕ OPUS — див. пакет internal/pcmu: там і жива перевірка
// можливостей браузера, і ціна кожного варіанта.
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"log"
	"math"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/organicoils/oo-screen/agent/audio"
	"github.com/organicoils/oo-screen/internal/pcmu"
)

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

// registerAudioCodec додає PCMU у MediaEngine — ЛИШЕ під прапорцем. Payload
// type 0 не наш вибір, а статичне призначення RFC 3551 для PCMU/8000.
func registerAudioCodec(m *webrtc.MediaEngine) error {
	if !audioEnabled {
		return nil
	}
	return m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypePCMU,
			ClockRate: pcmu.Rate,
			Channels:  1,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio)
}

// addAudioTrack вішає звукову доріжку на зʼєднання агента. nil, nil без
// прапорця — і саме на цьому nil тримається вся інваріантність вимкненого
// режиму: немає доріжки -> немає m=audio в offer -> хаб бачить старого агента.
//
// TrackLocalStaticSample, а не StaticRTP: пакетизувати μ-law нема з чого —
// джерело віддає семпли, а не RTP, тож цю роботу робить pion.
func addAudioTrack(pc *webrtc.PeerConnection) (*webrtc.TrackLocalStaticSample, error) {
	if !audioEnabled {
		return nil, nil
	}
	trk, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:  webrtc.MimeTypePCMU,
		ClockRate: pcmu.Rate,
		Channels:  1,
	}, "audio", "oo-screen-agent")
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
	factor, step, ok := audioLayout(f)
	if !ok {
		return nil
	}
	var out [][]byte
	for off := 0; off+step <= len(data); off += step {
		mono := 0.0
		for ch := 0; ch < f.Channels; ch++ {
			mono += audioSample(data[off+ch*(f.BitsPerSample/8):], f)
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
func audioLayout(f audio.Format) (factor, step int, ok bool) {
	if f.Channels <= 0 || f.BitsPerSample <= 0 || f.BitsPerSample%8 != 0 {
		return 0, 0, false
	}
	if f.SampleRate <= 0 || f.SampleRate%pcmu.Rate != 0 {
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
	return f.SampleRate / pcmu.Rate, step, true
}

// audioSample читає один семпл як значення в [-1, 1]. Дзеркалить приватний
// pcmSample з agent/audio — той пакет готовий і його не чіпаємо, а розбирати
// байти все одно комусь треба.
func audioSample(b []byte, f audio.Format) float64 {
	bits := f.BitsPerSample
	if len(b) < bits/8 {
		return 0
	}
	if f.SampleFormat == audio.SampleFormatFloat {
		if bits != 32 {
			return 0 // 64-бітний float у mix format WASAPI не зустрічається
		}
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
	}
	if f.SampleFormat != audio.SampleFormatPCM {
		return 0
	}

	valid := f.ValidBitsPerSample
	if valid <= 0 || valid > bits {
		valid = bits
	}
	var raw int64
	switch bits {
	case 8:
		return float64(int(b[0])-128) / 128
	case 16:
		raw = int64(int16(binary.LittleEndian.Uint16(b)))
	case 24:
		raw = int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16
		if raw&0x800000 != 0 {
			raw |= ^int64(0xffffff)
		}
	case 32:
		raw = int64(int32(binary.LittleEndian.Uint32(b)))
	default:
		return 0
	}
	if shift := bits - valid; shift > 0 {
		raw >>= shift
	}
	return float64(raw) / math.Ldexp(1, valid-1)
}

// audioGap — скільки семплів 8 кГц треба долити тишею, щоб доріжка наздогнала
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
func audioGap(elapsed time.Duration, emitted int64, tol time.Duration) int64 {
	if elapsed <= 0 {
		return 0
	}
	gap := int64(elapsed*pcmu.Rate/time.Second) - emitted
	if gap < int64(tol*pcmu.Rate/time.Second) {
		return 0
	}
	return gap
}

// ---------------------------------------------------------------------------

// audioSend — як віддати готовий кадр у транспорт.
type audioSend func(data []byte, dur time.Duration) error

// runAudio — увесь життєвий цикл звуку агента. Кличеться однією горутиною з
// main() і повертається лише разом із ctx.
//
// 🚨 ГЕЙТИНГ ОКРЕМО ВІД ВІДЕО, АЛЕ ЗА ТИМ САМИМ СИГНАЛОМ. paused — той самий
// gatePaused, яким хаб керує відео. Поки глядача немає, пристрій НЕ ВІДКРИТИЙ
// зовсім: не «читаємо й викидаємо», а не захоплюємо. RMS у гейтингу не бере
// участі НІ В ЯКОМУ ВИГЛЯДІ — тиша в loopback це справжні нулі, і сплутати її з
// «нема глядача» означало б глушити звук щоразу, коли на тому ПК просто нічого
// не грає.
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

func runAudio(ctx context.Context, paused *atomic.Bool, send audioSend) {
	fails := 0
	for {
		if !audioWait(ctx, paused) {
			return
		}
		c, err := audioOpen()
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
		log.Printf("oo-agent: аудіо-захоплення почалось (%+v -> PCMU 8кГц моно)", c.Format())
		audioCapture(ctx, paused, c, send)
		_ = c.Close()
		log.Printf("oo-agent: аудіо-захоплення зупинено (глядача немає або джерело впало)")
	}
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
	var (
		enc     audioEncoder
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

		if _, _, ok := audioLayout(f.Format); !ok {
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

		gap := audioGap(f.Timestamp.Sub(first), enc.emitted, audioSyncTolerance)
		gaps += gap
		out := enc.silence(gap)
		out = append(out, enc.encode(f.Data, f.Format)...)
		for _, fr := range out {
			if err := send(fr, pcmu.Duration(len(fr))); err != nil {
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
