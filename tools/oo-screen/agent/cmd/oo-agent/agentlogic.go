// Platform-independent pieces of oo-agent: control-channel parsing, keepalive/
// admission policy, bitrate clamping, H.264 fmtp and the WebRTC media engine.
// Moved verbatim out of main.go (windows-only) so the tests exercise the real
// code on Linux CI as well.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/organicoils/oo-screen/internal/control"
	"github.com/organicoils/oo-screen/internal/h264"
	"github.com/organicoils/oo-screen/internal/pacer"
)

const (
	// minBitrateBps — нижня межа для bitrate_target: під нею 1080p-екран уже
	// нечитабельний, тож просідання каналу краще ловити втратою кадрів, ніж
	// кашею з макроблоків.
	minBitrateBps = 300_000

	// keepaliveAfter — скільки чекати новий кадр, перш ніж переслати останній.
	// На нерухомому екрані DXGI не віддає кадрів узагалі (dxgi.c: WAIT_TIMEOUT
	// -> OOS_TIMEOUT, capture.NextFrame крутиться далі), а сторож у браузері рве
	// сесію безповоротно, якщо кадру не було 3000 мс (web/desktop-oo.js:
	// fallback() ставить finished=true). Тобто без keepalive людина, яка на три
	// секунди прибрала руку з миші, гарантовано втрачає сесію.
	//
	// 1000 мс дає потрійний запас: два поспіль загублені keepalive ще не валять
	// сесію. Ціна виміряна, а не на око: повторно закодований ІДЕНТИЧНИЙ кадр на
	// NVIDIA H.264 MFT (1920x1080, CBR 8 Мбіт/с) важить 75–374 байти, у
	// середньому 184 — це ≈1,5 кбіт/с, тобто ~0,02% від цільового бітрейту.
	keepaliveAfter = 1000 * time.Millisecond

	// admissionFloor — найдовша пауза, яку має право створити admission-дроп
	// (§5.5). Дроп рахує лише queued і тому викидає й keepalive-кадр — тобто
	// рівно те, що годує сторож у браузері. На насиченому транспорті захист від
	// нерухомого екрана зникав саме тоді, коли він найпотрібніший, і платив за
	// це не якістю, а сесією.
	//
	// Дроп коштує ЦІЛОГО keepaliveAfter, а не одного кадру: після continue
	// NextFrame вичікує дедлайн наново. Тож двох дропів поспіль уже досить, щоб
	// дійти до 3000 мс сторожа.
	//
	// Два keepaliveAfter — та сама межа, з якої виходить
	// TestKeepaliveAfterLeavesRoomForWatchdog: два пропущені keepalive ще в
	// запасі, третій мусить пройти. Окремої константи не заводимо навмисно:
	// поріг зобовʼязаний рухатись разом із keepaliveAfter.
	admissionFloor = 2 * keepaliveAfter

	agentALPN = "oo-screen-agent"
	// h264FmtpLine — фолбек, коли рівень енкодера ще невідомий (dial до
	// відкриття MFT) або MFT його не назвав. Живий рядок дає h264Fmtp().
	// Main 3.1 (4d001f) — рівно те, що Chrome оголошує в
	// getCapabilities('video'); High (64xx) там не з'являється ЖОДНОГО разу,
	// тож фолбек-рядок мусить бути таким, який приймач узагалі здатен узяти.
	h264FmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"
	dialTimeout  = 10 * time.Second // dial/HTTP таймаут: реконект/shutdown не мають зависати назавжди

	// wtWriteTimeout — стеля на ОДИН запис кадру в QUIC-стрім (A-33).
	//
	// Не «скільки не шкода чекати», а «з якої миті чекати вже нема сенсу»:
	// admissionFloor — найдовша пауза, яку ми взагалі дозволяємо потоку (два
	// keepaliveAfter, далі сторож у браузері рве сесію). Запис, що не вклався в
	// неї, вже нічого не рятує — картинка на тому боці однаково прострочена,
	// тож дешевше визнати транспорт мертвим і перепідключитись.
	wtWriteTimeout = admissionFloor
)

// encPLID — profile-level-id (6 hex-цифр), прочитаний із SPS ЖИВОГО енкодера.
// Пишеться при кожному відкритті енкодера, читається на dial. Порожньо =
// енкодера ще нема (dial до відкриття MFT) або SPS не розібрався.
// encProfileMain — profile_idc профілю Main (eAVEncH264VProfile_Main у mft.c).
// Значення в MF_MT_MPEG2_PROFILE збігається з profile_idc у SPS.
const encProfileMain = 77

var encPLID atomic.Value // string

// h264Fmtp — SDP fmtp для нашої доріжки (A-26).
//
// І профіль, і рівень беремо з SPS, який видав САМ енкодер, а не з констант.
// Історія тут довга і кожен її крок коштував сесії: спершу оголошували рівень
// 4.2, поки MFT на 2560x1440 пінив 5.1 — глядач рахував буфер за оголошеним
// рівнем і давився першим великим IDR. Потім рівень почали брати з живого
// енкодера, а профіль лишили константою High — і High виявився профілем, який
// Chrome не оголошує взагалі (getCapabilities дає лише 42xx/4d/f4), тож хаб
// чесно відповідав 415 h264_profile_mismatch.
//
// SPS — єдине місце, де записано те, що РЕАЛЬНО поїде в мережу. Поки fmtp
// походить із нього, розійтись із енкодером він не може за побудовою.
func h264Fmtp() string {
	plid, _ := encPLID.Load().(string)
	if len(plid) != 6 {
		return h264FmtpLine
	}
	return "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + plid
}

// encoderPLID дістає profile-level-id із кешованих SPS/PPS енкодера.
func encoderPLID(headers []byte) (string, error) {
	for _, nal := range h264.SplitNALs(headers) {
		if len(nal) == 0 || nal[0]&0x1F != 7 {
			continue
		}
		sps, err := h264.ParseSPS(nal)
		if err != nil {
			return "", err
		}
		// Нижній регістр навмисно: hub і решта коду порівнюють profile-level-id
		// як рядок ("4d001f"), а ProfileLevelID() друкує великими.
		return strings.ToLower(sps.ProfileLevelID()), nil
	}
	return "", errors.New("у заголовках енкодера немає SPS")
}

// ---- WebRTC (кандидат A): TrackLocalStaticSample, Pion пакетизує в RTP ----

type webrtcTransport struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	// paced — та сама доріжка під пейсером (nil при OO_SCREEN_PACER=0);
	// тоді кадри йдуть через неї, а track лише всередині.
	paced   *pacer.Track
	paceBps uint64
	// atrk — доріжка звуку цього ж зʼєднання. nil без OO_SCREEN_AUDIO, і
	// тоді sendAudio — порожній виклик (audio.go: audioTrackSample).
	atrk     *webrtc.TrackLocalStaticSample
	lastPTS  time.Duration
	havePrev bool
	// frameInterval — 1/fps: тривалість, яку віддаємо pion, поки різниці PTS
	// сусідніх AU ще нема (перший AU сесії). Див. sampleDuration.
	frameInterval time.Duration
}

func newWebRTCAPI() (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: h264Fmtp(),
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "nack"},
				{Type: "nack", Parameter: "pli"},
			},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	// PCMU — лише під OO_SCREEN_AUDIO (audio.go). Без прапорця MediaEngine
	// лишається бітово тим самим, що й до появи звуку.
	if err := registerAudioCodec(m); err != nil {
		return nil, err
	}
	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	// B6: агент медіа не приймає (лише шле RTP; datachannel — SCTP, не SRTP),
	// але отримує SRTCP від хаба (NACK, PLI, RR/REMB). Вікно 1024 — як у
	// хаба і libwebrtc, щоб пізній чи переставлений RTCP не відкидався мовчки.
	se.SetSRTPReplayProtectionWindow(1024)
	se.SetSRTCPReplayProtectionWindow(1024)
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(se)), nil
}

// sendAudio: тривалість тут — РЕАЛЬНА довжина payload-а (8 кГц, байт=семпл), а
// не оцінка. Тому RTP-годинник звуку рухається рівно на стільки, скільки в
// кадрі звуку, і власного дрейфу не має — на відміну від відео, де тривалість
// доводиться виводити з різниці PTS (див. sampleDuration).
func (t *webrtcTransport) sendAudio(data []byte, dur time.Duration) error {
	return audioTrackSample(t.atrk, data, dur)
}

// sampleDuration — що покласти в media.Sample.Duration для AU з цим PTS.
//
// Це НЕ мітка цього кадру, а КРОК ДО НАСТУПНОГО: pion пакетизує кадр із
// поточною RTP-міткою і аж потім зсуває годинник
// (track_local_static.go: tickF = Duration.Seconds()*clockRate ->
// packetizer.Packetize -> p.Timestamp += samples). Тому базово це реальний
// інтервал між PTS сусідніх AU, а не фіксований 1/fps: живий захоплений кадр
// не метрономний (WAIT_TIMEOUT на статичному екрані вже поглинутий
// capture.NextFrame).
//
// Фолбек, коли різниці ще нема, — кадровий інтервал. Різниці нема на ПЕРШОМУ
// AU (старт і КОЖЕН реконект: транспорт новий, havePrev скинуто, а captureSeq
// рахує далі) і на AU з тим самим PTS (кілька AU з однієї кодованої
// картинки). Раніше фолбеком стояв абсолютний au.PTS — тобто весь час життя
// сесії як тривалість одного кадру; після 2147afe9 PTS іде за стінним
// годинником, тож цей стрибок росте разом із сесією і зсуває RTP-годинник на
// десятки секунд уперед — розрив шкали, завмирання, викид буфера у глядача.
//
// Нуль тут не годиться: наступний кадр дістав би ту саму RTP-мітку, що й цей,
// а однакова мітка означає один момент дискретизації — депакетизатор склеїв
// би два різні кадри в один AU (RFC 6184 §5.1).
//
// ponytail: стеля схеми — тривалість тут завжди ПОПЕРЕДНІЙ інтервал, тож на
// ЗМІНІ інтервалу (33 мс -> keepalive 1 с) приймач усе одно бачить сплеск
// jitter. Виміряно живцем на старті після 60 с паузи: 612706 тактів@90k зі
// старим фолбеком проти 10002 з цим — у 61 раз менше.
// Ратчету бітрейту з цього більше НЕ виходить: hub перестав керувати за
// jitter (177a3e56) — у тракті зі свідомо змінним fps (1..60 к/с) цей
// сигнал міряє зміну частоти кадрів, а не затор. Тут він лишається лише
// як телеметрія.
// Прибрати решту можна лише ставлячи RTP-мітку з ВЛАСНОГО PTS кадру:
// TrackLocalStaticRTP із власним пакетизатором замість WriteSample.
func (t *webrtcTransport) sampleDuration(pts time.Duration) time.Duration {
	dur := t.frameInterval
	if t.havePrev {
		if d := pts - t.lastPTS; d > 0 {
			dur = d
		}
	}
	t.lastPTS = pts
	t.havePrev = true
	return dur
}

func (t *webrtcTransport) close() {
	if t.pc != nil {
		_ = t.pc.Close()
	}
	if t.paced != nil {
		t.paced.Close()
	}
}

// paceTargetBps — жива ціль енкодера (стартова й кожна, що лягла в
// SetBitrate); пейсер транспорту підхоплює її на наступному кадрі. Глобальна,
// бо транспорт переживає реконекти окремо від stream.
var paceTargetBps atomic.Uint64

// pacerEnabled — OO_SCREEN_PACER: типово ВИМКНЕНО (весь AU одразу в сокет),
// "1" вмикає. Стенд показав користь лише під стелею 8 Мбіт/с, ціною до 75 мс
// черги; під 4 Мбіт/с різниці немає. Вмикати після перевірки на реальних ПК.
func pacerEnabled() bool { return os.Getenv("OO_SCREEN_PACER") == "1" }

// syncPaceTarget — з кадрового циклу перед WriteSample.
func (t *webrtcTransport) syncPaceTarget() {
	if b := paceTargetBps.Load(); b != t.paceBps {
		t.paced.SetTarget(b)
		t.paceBps = b
	}
}

// parseCtlJSON розбирає одне повідомлення WebRTC-control-каналу як
// control.Msg — тим самим control.Read, що й нога WT, а не власним парсером.
// Биті дані чи невідомий тип -> ok=false і мовчазне ігнорування.
//
// DataChannel message-oriented: кадр приходить цілим, тож термінатор '\n'
// дописуємо, якщо hub його не поставив. Для QUIC-стріму строгість control.Read
// («нема '\n' -> запис неповний») правильна, тут вона різала б валідний кадр.
func parseCtlJSON(data []byte) (control.Msg, bool) {
	if len(data) == 0 || data[0] != '{' {
		return control.Msg{}, false
	}
	if data[len(data)-1] != '\n' {
		data = append(bytes.Clone(data), '\n') // Clone: буфер pion не наш, не мутуємо
	}
	m, err := control.Read(bufio.NewReader(bytes.NewReader(data)))
	if err != nil || !control.IsKnownType(m.Type) {
		return control.Msg{}, false
	}
	return m, true
}

// handleCtlMessage розводить одне повідомлення control-каналу по колбеках.
// Винесено з ctl.OnMessage, щоб протокол перевірявся тестом без живого
// PeerConnection. Нерозпізнане ігнорується мовчки — зʼєднання не рвемо.
func handleCtlMessage(data []byte, onKeyframeRequest func(), onGate func(bool), onBitrateTarget func(uint64), onSelectOutput func(int)) {
	if m, ok := parseCtlJSON(data); ok {
		switch m.Type {
		case control.TypeBitrateTarget:
			if onBitrateTarget != nil {
				onBitrateTarget(m.BitrateBps)
			}
		case control.TypeSelectOutput:
			// Сам перехід робить кадровий цикл (stream.requestOutput):
			// капчер не thread-safe, а це — колбек DataChannel-горутини.
			if onSelectOutput != nil {
				onSelectOutput(m.Output)
			}
		case control.TypeKeyframeRequest:
			// Досі WebRTC-нога реагувала лише на RTCP PLI, тож запит хаба
			// control-каналом нікуди не доходив.
			if onKeyframeRequest != nil {
				onKeyframeRequest()
			}
		}
		return
	}
	switch s := string(data); {
	case s == "resume" && onGate != nil:
		onGate(true)
	case s == "pause" && onGate != nil:
		onGate(false)
	default:
		// ponytail: "bitrate <bps>" лишається терпимим псевдонімом заради
		// сумісності; основний формат — JSON вище.
		if bps, ok := parseBitrateLine(s); ok && onBitrateTarget != nil {
			onBitrateTarget(bps)
		}
	}
}

// parseBitrateLine розбирає ТЕКСТОВИЙ (не основний) рядок "bitrate <bps>".
// ParseUint сам відкидає порожнє, нечислове й знак, тож "bitrate", "bitrate abc"
// і "bitrate -5" дають ok=false і до енкодера не доходять.
func parseBitrateLine(s string) (uint64, bool) {
	rest, ok := strings.CutPrefix(s, "bitrate ")
	if !ok {
		return 0, false
	}
	bps, err := strconv.ParseUint(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return bps, true
}

// shouldKeepalive вирішує, чи пересилати останній кадр після очікування.
// Виділено з циклу захоплення, щоб правило перевірялось тестом без DXGI,
// енкодера й хаба.
//
// waitErr — те, що повернула capture.NextFrame з дедлайном keepaliveAfter:
// nil означає, що кадр прийшов раніше порогу, і пересилати нічого не треба.
func shouldKeepalive(waitErr error, paused, haveLast bool) bool {
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		return false // кадр прийшов вчасно, або це зовсім інша помилка
	}
	if paused {
		return false // глядача нема: keepalive зʼїв би економію, заради якої робився гейтинг
	}
	return haveLast // без жодного захопленого кадру повторювати нема чого
}

// shouldAdmit вирішує, чи пускати щойно захоплений кадр далі — на кодування й
// відправку. Виділено з циклу з тієї ж причини, що й shouldKeepalive: правило
// перевіряється тестом без DXGI, енкодера й хаба.
//
// Базове правило §5.5 лишається: поки транспорт не відправив ПОПЕРЕДНІЙ кадр
// (queued > 0), новий викидаємо ДО кодування — не топимо канал, який і так не
// встигає.
//
// Виняток один, і він не про keepalive окремо: дроп не має права затягнути
// паузу довше за admissionFloor. Гарантуємо не кадр, а ВЕРХНЮ МЕЖУ паузи —
// інакше «пропускати keepalive завжди» просто вимкнуло б admission. Ціна межі:
// не більше одного примусового кадру на admissionFloor, тобто ≤0,5 кадру/с
// поверх того, що транспорт тягне.
//
// Стеля цього рішення: якщо транспорт стоїть намертво, примусові кадри за
// ~16 с заповнять sendQueue (буфер 8) і цикл упреться в sendAsync. Такий стан —
// уже не «канал не встигає», а мертвий транспорт, і лікує його реконект по
// txErrCh, а не admission.
// refineWake — що робити, коли дедлайн NextFrame вкоротив refine (а не
// keepalive). Refine лише вкорочує очікування, тож він не має права з'їсти
// keepalive: якщо refine не відбувся (не на часі або канал зайнятий і його
// відкладено), а з останнього пропущеного кадру минуло keepaliveAfter, —
// keepalive, як на baseline-дедлайні (shouldKeepalive; admission далі сам).
type refineWake int

const (
	refineWakeSkip refineWake = iota
	refineWakeRefine
	refineWakePostpone
	refineWakePostponeKeepalive
	refineWakeKeepalive
)

func refineWakeAction(due, paused, haveLast bool, queued int64, sinceAdmitted time.Duration) refineWake {
	if paused || !haveLast {
		return refineWakeSkip
	}
	keep := sinceAdmitted >= keepaliveAfter
	switch {
	case !due && keep:
		return refineWakeKeepalive
	case !due:
		return refineWakeSkip
	case queued > 0 && keep:
		return refineWakePostponeKeepalive
	case queued > 0:
		return refineWakePostpone
	}
	return refineWakeRefine
}

func shouldAdmit(queued int64, sinceAdmitted time.Duration) bool {
	return queued == 0 || sinceAdmitted >= admissionFloor
}

// shouldRearm вирішує, чи брати кадр в обхід DXGI — через capture.GDIFrame.
//
// Сесія, що стартувала на ВЖЕ нерухомому екрані, інакше не зрушить з місця:
// DXGI на такому екрані не віддає нічого (тільки WAIT_TIMEOUT), keepalive
// повторювати нема чого — жодного кадру ще не було, — і глядач падає на
// first-frame-timeout, так і не побачивши картинки. Живий прогін на цій машині:
// агент до фікса не відправив ЖОДНОГО пакета за 45 с при підключеному глядачі.
//
// Що на справді нерухомому екрані НЕ працює (перевірено, 6 з 6 таймаутів):
// просто чекати далі, пересоздати дуплікацію, пересоздати весь капчер,
// RedrawWindow на весь робочий стіл. Працює лише GDI-знімок. capture-probe
// проблеми не бачить не тому, що знає інший спосіб, а тому, що його запускають
// на екрані, який у ту мить рухається.
//
// paused перевіряємо з тієї ж причини, що й у shouldKeepalive: гейт може стати
// на паузу, поки ми чекали кадр (у живому прогоні саме так і сталось), а
// хапати екран без глядача — це та сама економія, заради якої гейтинг робився.
func shouldRearm(waitErr error, paused, haveLast bool) bool {
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		return false
	}
	return !paused && !haveLast
}

// seqAdvance — на скільки кадрових інтервалів просунути лічильник PTS, якщо від
// попереднього кадру минуло waited. PTS у циклі = captureSeq/fps, а Duration у
// webrtcTransport.send — різниця PTS сусідніх AU; саме вона крутить RTP-годинник
// у pion (WriteSample: tickF = Duration.Seconds()*clockRate).
//
// Просте ++ дало б 1/fps незалежно від того, скільки кадр НАСПРАВДІ чекали. На
// нерухомому екрані кадр приходить через сотні мілісекунд, а PTS зсувався б на
// 16 мс: RTP відстає від стінного годинника, і приймач рахує цю різницю як
// jitter (виміряно диференційно: рух 66 Гц -> jitter 0.2–1.0 мс, перемальовка
// 20 Гц -> 20–24 мс, і hub на цьому скручує бітрейт до підлоги при нульових
// втратах). Те саме стосується keepalive-кадру й кадру, викинутого admission.
//
// Не менше одного інтервалу: PTS мусить строго рости, інакше різниця сусідніх
// PTS нульова, тривалість кадру взятися нізвідки і sampleDuration підставить
// номінальний кадровий інтервал замість виміряного.
func seqAdvance(waited, frameInterval time.Duration) uint64 {
	if n := uint64(waited / frameInterval); n > 1 {
		return n
	}
	return 1
}

// clampBitrate зводить ціль із bitrate_target до розумних меж: не нижче
// minBitrateBps і не вище стартового -bitrate — hub може лише ПРИТИСКАТИ
// потік, а не піднімати його понад те, на що агента налаштували. Нульовий
// (відсутній у JSON) bitrate_bps -> ok=false, повідомлення ігнорується.
func clampBitrate(want uint64, start int) (int, bool) {
	if want == 0 || start <= 0 {
		return 0, false
	}
	lo := minBitrateBps
	if start < lo {
		lo = start // стеля виграє: нижня межа не може бути вищою за неї
	}
	switch {
	case want > uint64(start):
		return start, true
	case want < uint64(lo):
		return lo, true
	default:
		return int(want), true
	}
}

// bitrateTarget передає ціль із control-горутини в цикл захоплення. MFT
// приймає нову AVEncCommonMeanBitRate надійно лише ПОКИ в нього йдуть кадри,
// а пауза без глядача — у нас штатний режим (див. onGate). Тому на паузі
// ціль не смикає енкодер, а чекає першого кадру після відновлення.
type bitrateTarget struct {
	pending atomic.Int64
}

// set повертає bps, який треба застосувати ПРЯМО ЗАРАЗ, або 0 — якщо ціль
// відкладено до відновлення. Нова ціль затирає попередню відкладену: у черзі
// має сенс лише остання.
func (b *bitrateTarget) set(bps int, paused bool) int {
	if paused {
		b.pending.Store(int64(bps))
		return 0
	}
	b.pending.Store(0)
	return bps
}

// take віддає відкладену ціль рівно один раз; 0 = нічого не відкладено.
func (b *bitrateTarget) take() int { return int(b.pending.Swap(0)) }

// encoderStale — чи належить відкритий енкодер ІНШОМУ капчеру, ніж той, що
// віддає кадри. Одна функція на обидва місця, де це питають (кадровий цикл і
// syncEncoderToCapture): два незалежні порівняння рано чи пізно розійшлись би.
//
// A-06: адреси девайса МАЛО. Капчер після перебудови звільняє ID3D11Device, і
// наступний може лягти рівно на ту саму адресу — тоді devEq==true при
// повністю новому пайплайні. Покоління росте завжди, тож воно і є тотожністю.
func encoderStale(dev uintptr, gen uint64, encDev uintptr, encGen uint64) bool {
	return dev != encDev || gen != encGen
}
