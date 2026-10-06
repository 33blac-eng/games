// Звук: РЕТРАНСЛЯЦІЯ звуку ПК від агента до браузера, із запасним тоном.
//
// Крок 1 доводив саму ТРУБУ — що друга доріжка доходить від хаба до браузера
// тим самим WebRTC-зʼєднанням, що й відео; джерелом тоді був вбудований тон.
// Тепер джерело справжнє: агент знімає WASAPI loopback (agent/audio), кодує
// G.711 μ-law і публікує другою доріжкою свого ж offer-а. Хаб її форвардить.
//
// Тон ЛИШИВСЯ запасним джерелом під тим самим прапорцем: нода, чий агент звуку
// не публікує (старий агент, вимкнений у нього прапорець, ПК без звукової
// карти), досі отримує тон — і це ж лишається перевіркою тракту в один клац.
//
// 🔴 ПРАПОРЕЦЬ OO_SCREEN_AUDIO=1, ТИПОВО ВИМКНЕНО. Без нього хаб мусить
// поводитись рівно як до появи цього файлу: кодек не реєструється в
// MediaEngine, доріжка не додається, у SDP-відповіді нічого зайвого, аудіо-
// трансивера в агентській нозі немає. Прод працює — ламати його заради
// недоробленої фічі не можна.
//
// F1: КОДЕК — Opus 48 кГц стерео (internal/opusenc, чистий Go). μ-law 8 кГц
// моно лишився запасним: OO_SCREEN_AUDIO_CODEC=pcmu — ТОЙ САМИЙ env, що в
// агента. Хаб реєструє ОБИДВА кодеки, щоб прийняти будь-якого агента, але
// глядачу віддає лише свій (hubAudioCodec()); звук агента з іншим кодеком не
// транскодується, а відкидається з рядком у лозі (readAgentAudio).
//
// Стиль — той самий, що у fanout.go: доріжка НАЛЕЖИТЬ viewerLeg, живе рівно
// стільки, скільки нога, і зупиняється тим самим vl.done. Жодного паралельного
// механізму зі своїм життєвим циклом.
package main

import (
	"log"
	"math"
	"os"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/organicoils/oo-screen/internal/opusenc"
	"github.com/organicoils/oo-screen/internal/pcmu"
)

const (
	// toneHz/toneAmp — запасний тон: 440 Гц на чверть шкали. Синтезуємо на
	// місці, бо в μ-law це дешевше за будь-який файл: 8 рядків проти embed +
	// Ogg-парсера. Якщо колись повернеться Opus, старий артефакт робиться
	// одним рядком:
	//   ffmpeg -f lavfi -i "sine=f=440:d=2" -c:a libopus -page_duration 20000 testtone.ogg
	// (-page_duration 20000 обовʼязковий: без нього на сторінці лежить пачка
	// кадрів, а pion віддав би її одним RTP-пакетом, який декодер не розбере).
	toneHz  = 440.0
	toneAmp = 0.25 * math.MaxInt16

	// audioQueueDepth — черга звуку ОДНІЄЇ viewer-ноги, у кадрах по 20 мс.
	// 25 = півсекунди. Переповнення тут НЕ рве ногу (на відміну від відео,
	// viewerQueueDepth): загублені 20 мс звуку — це клац, а не втрачена сесія,
	// і платити за нього чорним екраном безглуздо.
	audioQueueDepth = 25

	// audioIdleTick — як часто pump прокидається, поки агент мовчить (гейт на
	// паузі, реконект). Не тайм-аут джерела: мовчання агента — нормальний стан,
	// а не аварія, і тон замість нього ми НЕ вмикаємо (див. audioPump).
	audioIdleTick = 200 * time.Millisecond
)

// audioEnabled — прапорець фічі. Змінна, а не виклик os.Getenv на місці:
// тести перемикають її напряму (і повертають назад), інакше довелось би
// перезапускати процес заради однієї гілки.
var audioEnabled = os.Getenv("OO_SCREEN_AUDIO") == "1"

// audioCap — те, чим і доріжка, і кодек описуються в обох напрямках. Одна
// змінна на всі чотири місця (MediaEngine, доріжка глядача, трансивер агента,
// тест) — щоб codec mismatch не міг зʼявитись через розбіжність літералів.
func audioCap() webrtc.RTPCodecCapability { return hubAudioCodec().Capability() }

// hubAudioCodec — кодек звуку цього хаба (OO_SCREEN_AUDIO_CODEC, типово opus).
// atomic.Value, бо тести міняють його (withHubAudioCodec), поки горутини ніг
// попередніх тестів ще дожовують свої кадри.
var hubCodecV atomic.Value

func init() {
	c, err := opusenc.FromEnv()
	if err != nil {
		log.Printf("hub: %v", err)
	}
	hubCodecV.Store(c)
}

func hubAudioCodec() opusenc.Codec { return hubCodecV.Load().(opusenc.Codec) }

// registerAudioCodec додає кодеки звуку в MediaEngine — ЛИШЕ під прапорцем. Це
// перша з двох засувок: без зареєстрованого кодека хаб не зміг би відповісти
// на аудіо-m-рядок, навіть якби доріжку хтось додав. PT: 111 Opus, 0 PCMU
// (статичне призначення RFC 3551).
func registerAudioCodec(m *webrtc.MediaEngine) error {
	if !audioEnabled {
		return nil
	}
	// Свій кодек першим — його pion і обере для глядача; другий лише щоб
	// агент з іншим кодеком не ламав переговори (його звук відкидається).
	other := opusenc.CodecPCMU
	if hubAudioCodec() == opusenc.CodecPCMU {
		other = opusenc.CodecOpus
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: audioCap(), PayloadType: hubAudioCodec().Parameters().PayloadType}, webrtc.RTPCodecTypeAudio); err != nil {
		return err
	}
	return m.RegisterCodec(other.Parameters(), webrtc.RTPCodecTypeAudio)
}

// addAudioTrack вішає аудіо-доріжку на viewer-ногу. Той самий stream id, що й у
// відео ("oo-screen-hub"): браузер збирає обидві доріжки в ОДИН MediaStream, і
// приймальний бік нічого не мусить зшивати руками.
//
// TrackLocalStaticSample, а не StaticRTP як у відео, і це не розбіжність
// стилю, а різні задачі. Відео ретранслюється пакет-у-пакет, бо H.264-кадр
// фрагментований по кількох RTP і склеїти його заново означало б переписати
// пакетизатор. Кадр μ-law — це рівно один пакет і рівно 160 байт, тож
// «розпакувати й віддати семпл» тут не втрата, а спрощення: pion сам веде
// seq/ts однією монотонною шкалою на всю ногу, і перемикання джерела
// (агент <-> тон) не робить у ній жодного розриву.
func addAudioTrack(pc *webrtc.PeerConnection) (*webrtc.TrackLocalStaticSample, error) {
	trk, err := webrtc.NewTrackLocalStaticSample(audioCap(), "audio", "oo-screen-hub")
	if err != nil {
		return nil, err
	}
	sender, err := pc.AddTrack(trk)
	if err != nil {
		return nil, err
	}
	// Обовʼязковий RTCP read-loop, як і в решти ніг: без нього interceptor-и
	// цього sender-а мертві.
	go drainRTCP(sender.Read)
	return trk, nil
}

// forwardAudioToViewers розкладає один кадр μ-law від агента ноди по чергах її
// глядачів. Брат forwardToViewers: той самий неблокуючий send, та сама вимога
// «повільний глядач гальмує тільки себе».
//
// Різниця одна й свідома: переповнення черги тут НЕ рве ногу. Кадр — це 20 мс
// звуку; викинути його дешевше, ніж вимкнути людині екран.
//
// payload віддається всім ногам одним і тим самим зрізом — його ніхто не
// мутує; копію робить читач треку рівно один раз (readAgentAudio).
func forwardAudioToViewers(ns *nodeSession, payload []byte) {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	for _, vl := range ns.viewers {
		if !vl.audioLive || vl.audioOut == nil {
			continue
		}
		select {
		case vl.audioOut <- payload:
		default:
			atomic.AddUint64(&vl.audioDropped, 1)
		}
	}
}

// audioTone — запасне джерело: 440 Гц, фаза неперервна між кадрами (розрив
// фази чути як клац на кожні 20 мс). Кодек — hubAudioCodec(); для Opus енкодер
// створюється ліниво, по одному на ногу (стан Opus-енкодера не ділиться).
type audioTone struct {
	phase float64
	enc   *opusenc.Encoder
	pcm   []float32
}

func (t *audioTone) next() []byte {
	if hubAudioCodec() == opusenc.CodecOpus {
		return t.nextOpus()
	}
	const step = 2 * math.Pi * toneHz / pcmu.Rate
	buf := make([]byte, pcmu.FrameSamples)
	for i := range buf {
		buf[i] = pcmu.Encode(int16(math.Sin(t.phase) * toneAmp))
		if t.phase += step; t.phase > 2*math.Pi {
			t.phase -= 2 * math.Pi
		}
	}
	return buf
}

func (t *audioTone) nextOpus() []byte {
	if t.enc == nil {
		enc, err := opusenc.NewEncoder(64000)
		if err != nil {
			return nil
		}
		t.enc = enc
		t.pcm = make([]float32, opusenc.FrameSamples*opusenc.Channels)
	}
	const step = 2 * math.Pi * toneHz / opusenc.Rate
	for i := 0; i < opusenc.FrameSamples; i++ {
		v := float32(math.Sin(t.phase) * toneAmp / math.MaxInt16)
		t.pcm[2*i], t.pcm[2*i+1] = v, v
		if t.phase += step; t.phase > 2*math.Pi {
			t.phase -= 2 * math.Pi
		}
	}
	pkt, err := t.enc.Encode(t.pcm)
	if err != nil {
		return nil
	}
	return pkt
}

// audioPump — писар аудіо-доріжки ОДНІЄЇ ноги, брат-близнюк vl.pump з fanout.go:
// власна горутина, зупинка по тому самому vl.done.
//
// 🚨 ВИБІР ДЖЕРЕЛА — ЗА ОГОЛОШЕННЯМ АГЕНТА (offerReq.Audio), А НЕ ЗА ТИМ, ЧИ
// ЗАРАЗ ІДУТЬ ПАКЕТИ. Агент із гейтингом мовчить, поки немає глядача, а глядач
// зʼявляється саме зараз — тобто «пакетів ще немає» це НОРМАЛЬНИЙ стан перших
// сотень мілісекунд кожного підключення. Вмикати на них тон означало б пищати
// 440 Гц людині у вухо на кожному вході в сесію. Тому: агент сказав, що звук
// у нього є -> чекаємо його звук скільки треба; сказав, що немає -> тон.
func (vl *viewerLeg) audioPump(ns *nodeSession, trk *webrtc.TrackLocalStaticSample) {
	// Запис сесії (record.go): аудіо-доріжку у файлі годує РІВНО ОДНА нога —
	// інакше N глядачів написали б у неї N копій того самого звуку. Слот
	// забирається В ЦИКЛІ, а не один раз на старті: рекордер живе разом з
	// agent-ногою, тобто може зʼявитись пізніше за цей pump (агент з
	// on-demand гейтингом не шле медіа, поки глядача немає) або змінитись,
	// коли агента витіснив новий.
	var rec *recorder
	defer func() {
		rec.releaseAudio()
		// Викинуті кадри — єдиний слід того, що глядач не встигав за звуком.
		// Мовчки їх губити означало б лишити «іноді тріщить» без жодної цифри.
		if n := atomic.LoadUint64(&vl.audioDropped); n > 0 {
			log.Printf("audio [node=%s]: викинуто %d кадрів (черга глядача переповнювалась), віддано %d",
				ns.nodeID, n, atomic.LoadUint64(&vl.audioSent))
		}
	}()

	var tone audioTone
	// H-33: один таймер на весь pump замість time.After у циклі. time.After
	// створює НОВИЙ таймер на кожному оберті, а обертів тут 50 на секунду на
	// КОЖНОГО глядача, і жоден із них не збирається раніше за свій строк —
	// тобто рівний потік сміття рівно там, де ми його не бачимо. Один
	// перезаряджуваний таймер дає ту саму семантику за нуль алокацій.
	//
	// Зливати канал перед Reset НЕ треба і НЕ МОЖНА: модуль на go 1.26, а з
	// Go 1.23 канал таймера синхронний — після Stop/Reset у ньому не лишається
	// значення від попереднього строку, тож старий рецепт `if !Stop() { <-C }`
	// тут просто заблокував би pump назавжди.
	tick := time.NewTimer(time.Hour)
	tick.Stop()
	defer tick.Stop()
	armTick := func(d time.Duration) {
		tick.Stop()
		tick.Reset(d)
	}
	for {
		select {
		case <-vl.done:
			return
		default:
		}

		if ns.hasAgentAudio() {
			armTick(audioIdleTick)
			select {
			case <-vl.done:
				return
			case data := <-vl.audioOut:
				if !vl.writeAudio(ns, trk, &rec, data) {
					return
				}
			case <-tick.C:
				// Агент на паузі або переподключається. Нічого не пишемо:
				// пропуск у RTP браузер загладжує сам, а підсунуте сюди
				// «щось» він відтворив би як звук.
				//
				// Слот запису віддаємо: або звуку нема ні в кого (тоді
				// байдуже), або його не шлють саме нам (сховали вкладку без
				// audio) — і тоді файл мусить годувати той, хто чує.
				rec.releaseAudio()
				rec = nil
			}
			continue
		}

		// Тон сам собі метроном: джерела, яке б задавало темп, тут немає.
		if !vl.writeAudio(ns, trk, &rec, tone.next()) {
			return
		}
		armTick(opusenc.FrameDuration) // і μ-law, і Opus-кадр тону — 20 мс
		select {
		case <-vl.done:
			return
		case <-tick.C:
		}
	}
}

// writeAudio віддає один кадр у доріжку глядача і в запис сесії. false =
// доріжка мертва, pump має піти.
func (vl *viewerLeg) writeAudio(ns *nodeSession, trk *webrtc.TrackLocalStaticSample, rec **recorder, data []byte) bool {
	if len(data) == 0 {
		return true
	}
	dur := hubAudioCodec().FrameDur(data)
	if err := trk.WriteSample(media.Sample{Data: data, Duration: dur}); err != nil {
		return false
	}
	atomic.AddUint64(&vl.audioSent, 1)

	if cur := ns.rec.Load(); cur != *rec {
		(*rec).releaseAudio()
		*rec = nil
		if cur.claimAudio() {
			*rec = cur
		}
	}
	(*rec).offerAudio(data, dur)
	return true
}

// readAgentAudio — читач звукової доріжки агента ноди. Дзеркало відео-читача в
// setupAgentLeg, тільки коротше: у μ-law один RTP-пакет = один цілий кадр, тож
// ні депакетизації, ні збирання AU тут немає.
//
// ponytail: гейта по ns.generation, як у відео, тут навмисно немає. Заміна
// агента закриває його PeerConnection цілком (setupAgentLeg), а це рве й цей
// ReadRTP; найгірше, що дає вікно між закриттям і виходом із циклу, — один
// зайвий кадр у 20 мс. Для відео така ж дрібниця означала б склейку двох
// потоків в одному GOP, тому там гейт і потрібен.
func readAgentAudio(ns *nodeSession, track *webrtc.TrackRemote) {
	if mime := track.Codec().MimeType; !hubAudioCodec().Matches(mime) {
		// Транскодування немає навмисно: це CPU хаба на кожну ноду. Агент і
		// хаб мусять мати однаковий OO_SCREEN_AUDIO_CODEC.
		log.Printf("agent audio [node=%s]: кодек агента %s != кодек хаба %s — звук агента відкидається (вирівняйте OO_SCREEN_AUDIO_CODEC)",
			ns.nodeID, mime, hubAudioCodec())
		for {
			if _, _, err := track.ReadRTP(); err != nil {
				return
			}
		}
	}
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			log.Printf("agent audio leg closed [node=%s]: %v", ns.nodeID, err)
			return
		}
		if len(pkt.Payload) == 0 {
			continue
		}
		// Копія рівно тут, ОДИН раз на кадр: далі той самий зріз читають N
		// черг глядачів, а буфер під pkt.Payload належить pion і піде під
		// наступний пакет.
		forwardAudioToViewers(ns, append([]byte(nil), pkt.Payload...))
	}
}
