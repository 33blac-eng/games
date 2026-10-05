//go:build windows

// oo-agent — Т2: живий агент. DXGI-захоплення (agent/capture) → апаратний
// MFT-енкодер (agent/encode) → транспорт WebRTC (легасі-бенч WT — лише в
// збірці з -tags wt, transport_wt.go; прапорець -transport). Не дублює agent/capture чи
// agent/encode — тільки склеює їх і §5.5 admission-політику.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"github.com/organicoils/oo-screen/agent/capture"
	"github.com/organicoils/oo-screen/agent/encode"
	"github.com/organicoils/oo-screen/internal/control"
	"github.com/organicoils/oo-screen/internal/h264"
)

const (
	// minBitrateBps — нижня межа для bitrate_target: під нею 1080p-екран уже
	// нечитабельний, тож просідання каналу краще ловити втратою кадрів, ніж
	// кашею з макроблоків.
	minBitrateBps = 300_000

	// h264FmtpLine — фолбек, коли рівень енкодера ще невідомий (dial до
	// відкриття MFT) або MFT його не назвав. Живий рядок дає h264Fmtp().
	// Main 3.1 (4d001f) — рівно те, що Chrome оголошує в
	// getCapabilities('video'); High (64xx) там не з'являється ЖОДНОГО разу,
	// тож фолбек-рядок мусить бути таким, який приймач узагалі здатен узяти.
	h264FmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f"
	dialTimeout  = 10 * time.Second // dial/HTTP таймаут: реконект/shutdown не мають зависати назавжди

)

var httpClient = &http.Client{Timeout: dialTimeout}

// encProfileMain — profile_idc профілю Main (eAVEncH264VProfile_Main у mft.c).
// Значення в MF_MT_MPEG2_PROFILE збігається з profile_idc у SPS.
const encProfileMain = 77

// encPLID — profile-level-id (6 hex-цифр), прочитаний із SPS ЖИВОГО енкодера.
// Пишеться при кожному відкритті енкодера, читається на dial. Порожньо =
// енкодера ще нема (dial до відкриття MFT) або SPS не розібрався.
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

// nodeID — mesh node_id цього ПК, задається прапорцем -node у main(). Порожній
// = старий T1/бенч-режим (hub бере node з env). Читається лише з sender-шляху
// dialWebRTC, який стартує після main() встановив значення.
var nodeID string

// cliToken — токен, переданий прапорцем -token (перекриває env). Дозволяє
// запускати агента напряму зі schtask без .cmd-обгортки, що ставить env — а
// саме та обгортка відкривала видиме вікно cmd.exe на екрані працівника.
var cliToken string

// startBitrateBps — фактичне значення -bitrate, з яким підняли агента. Їде в
// offer, щоб hub рахував стелю bitrate_target від нього, а не від свого
// дефолту. Читається лише з dialWebRTC, який стартує після main() присвоїв.
var startBitrateBps int

func authToken() string {
	if cliToken != "" {
		return cliToken
	}
	if v := os.Getenv("OO_SCREEN_T1_TOKEN"); v != "" {
		return v
	}
	return "t1-dev-token"
}

// ---------------------------------------------------------------------------
// transport: спільний інтерфейс над двома кандидатами транспорту. send
// викликається ЛИШЕ з sender-горутини одного кадру за раз (серіалізовано
// через inFlight у main) — тому реалізаціям не треба власного мʼютекса на
// запис.
// ---------------------------------------------------------------------------

type transport interface {
	send(au encode.AU, seq uint64) error
	// sendAudio віддає готовий кадр PCMU у звукову доріжку. Кличеться з
	// ОКРЕМОЇ горутини (runAudio) — це безпечно, бо доріжка інша, ніж у
	// відео, і pion серіалізує запис у кожну доріжку сам.
	sendAudio(data []byte, dur time.Duration) error
	close()
}

// ---- WebRTC (кандидат A): TrackLocalStaticSample, Pion пакетизує в RTP ----

type webrtcTransport struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	// atrk — доріжка звуку цього ж зʼєднання. nil без OO_SCREEN_AUDIO, і
	// тоді sendAudio — порожній виклик (audio.go: audioTrackSample).
	atrk     *webrtc.TrackLocalStaticSample
	lastPTS  time.Duration
	havePrev bool
	// frameInterval — 1/fps: тривалість, яку віддаємо pion, поки різниці PTS
	// сусідніх AU ще нема (перший AU сесії). Див. sampleDuration.
	frameInterval time.Duration
	// ctl — той самий "oosc-ctl"; агент у нього пише лише fallback_reason
	// (reportAvailability). nil = канал не створювався.
	ctl *webrtc.DataChannel
	// fmtp — рядок, з яким піднято ЦЮ ногу (offer). Див. fmtpStale.
	fmtp string
}

// fmtpStale — енкодер відкрився ПІСЛЯ dial з іншим profile-level-id, ніж той,
// що ця нога оголосила. Типовий шлях: старт на заблокованому ПК (енкодера ще
// нема -> фолбек 4d001f), перший глядач -> MFT на 2560x1440 з рівнем 5.1.
// Хаб і глядач домовились про одне, кодується інше — рівно той режим, з якого
// почався A-26. Лікується лише новим offer-ом, тобто реконектом.
func (t *webrtcTransport) fmtpStale() bool {
	return t.fmtp != h264Fmtp()
}

// sendFallbackReason — сказати хабу, чому картинки зараз не буде ("" = знову
// буде). false = канал ще не відкритий, повторити пізніше.
func (t *webrtcTransport) sendFallbackReason(seq uint64, reason string) bool {
	if t.ctl == nil || t.ctl.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	var b bytes.Buffer
	if err := control.Write(&b, control.FallbackReason(seq, reason)); err != nil {
		return false
	}
	return t.ctl.SendText(b.String()) == nil
}

func newWebRTCAPI(fmtp string) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: fmtp,
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
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i), webrtc.WithSettingEngine(se)), nil
}

type offerReq struct {
	SDP   string `json:"sdp"`
	Token string `json:"token"`
	// Node — mesh node_id цього ПК; hub реєструє publisher-а під ним і
	// маршрутизує viewer-ів саме сюди. Порожнє = старий T1/бенч-режим (hub
	// бере node з env OO_SCREEN_AGENT_NODE_ID).
	Node string `json:"node,omitempty"`
	// Bitrate — фактичний стартовий бітрейт агента (біт/с, прапорець -bitrate).
	// Hub інакше не знає, з чим агента підняли, і бере стелю для bitrate_target
	// із власного дефолту — на агенті з іншим -bitrate та стеля брехлива.
	Bitrate int `json:"bitrate,omitempty"`
	// Outputs/ActiveOutput — монітори цього ПК і той, що зараз у потоці. Їдуть
	// тим самим шляхом, що Bitrate, і з тієї ж причини: знає їх лише агент, а
	// консолі згодом треба буде чим наповнити список вибору монітора. Hub, який
	// цих полів ще не знає, просто їх проігнорує (encoding/json без
	// DisallowUnknownFields).
	Outputs      []capture.OutputInfo `json:"outputs,omitempty"`
	ActiveOutput int                  `json:"active_output"`
	// Audio — цей агент публікує другу доріжку зі звуком ПК. Хабу це треба
	// знати ОКРЕМО від SDP: агент із гейтингом не шле медіа, поки немає
	// глядача, тож «чи є в агента звук» не можна вивести з факту приходу
	// RTP — той самий глухий кут, що вже був із publisher-ом (див. hub
	// main.go, «publisher up»). Відсутнє поле = старий агент без звуку.
	Audio bool `json:"audio,omitempty"`
}

type answerResp struct {
	SDP string `json:"sdp"`
}

// outputList — знімок «які монітори є і який активний». Пишеться при старті й
// на кожному успішному перемиканні, читається з dialWebRTC (offer) і зі
// switch-ендпоінта, тобто з ІНШИХ горутин — тому міняється цілим знімком через
// atomic.Pointer, без замка.
type outputList struct {
	Outputs []capture.OutputInfo `json:"outputs"`
	Active  int                  `json:"active"`
}

var outputs atomic.Pointer[outputList]

// maxFpsWanted — остання стеля кадрів/с від хаба (control max_fps, C1); 0 = без
// стелі. Глобальна, а не ще один колбек крізь dial*: її пише DataChannel-
// горутина, читає лише кадровий цикл (maxFpsGap), і стан цей — рівно одне число.
// Скидається на початку кожного dialWebRTC: стеля належить сесії хаба, а хаб
// повторює її на oosc-ctl OnOpen лише тоді, коли вона в нього є. Після рестарту
// хаба (кожна заливка) її в нього нема — і старе число інакше тримало б fps
// для всіх наступних глядачів.
var maxFpsWanted atomic.Int32

// publishOutputs перечитує список виходів і запам'ятовує активний. Помилка
// енумерації не фатальна: без списку агент стрімить як стрімив, просто консоль
// не побачить, з чого вибирати.
func publishOutputs(active int) {
	list, err := capture.Outputs()
	if err != nil {
		log.Printf("oo-agent: capture.Outputs: %v (список моніторів не піде в offer)", err)
	}
	outputs.Store(&outputList{Outputs: list, Active: active})
}

// onDown — сесію треба піднімати заново. Кличеться зі стану PeerConnection
// (див. reconnectDecider), тобто й тоді, коли агент на паузі й нічого не шле.
func dialWebRTC(hubURL string, frameInterval time.Duration, onKeyframeRequest func(), onGate func(bool), onBitrateTarget func(uint64), onSelectOutput func(int), onDown func(string)) (*webrtcTransport, error) {
	// Сторож живості починає нову сесію роззброєним: тримати озброєним із
	// останнім ударом ПОПЕРЕДНЬОЇ означало б вирішити рвати щойно підняте.
	hubLive.disarm()
	setMaxFps(0) // нова сесія хаба — стеля, якщо є, приїде з його OnOpen
	// Один знімок на всю ногу: MediaEngine і трек мусять оголосити ОДНЕ й те
	// саме, навіть якщо енкодер відкриється посеред dial.
	fmtp := h264Fmtp()
	api, err := newWebRTCAPI(fmtp)
	if err != nil {
		return nil, fmt.Errorf("new api: %w", err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}
	// Control-DataChannel: hub шле сюди типізований control.Msg (JSON —
	// bitrate_target, keyframe_request) і, для зворотної сумісності, старі
	// текстові "resume"/"pause" за присутністю глядача. Створюємо ДО offer-а,
	// щоб канал потрапив у SDP; hub (answerer) ловить його через OnDataChannel.
	var ctlChan *webrtc.DataChannel
	if onGate != nil || onBitrateTarget != nil || onSelectOutput != nil {
		ctl, dcErr := pc.CreateDataChannel("oosc-ctl", nil)
		ctlChan = ctl
		if dcErr != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("create control datachannel: %w", dcErr)
		}
		ctl.OnMessage(func(msg webrtc.DataChannelMessage) {
			// Живість — ПЕРШИМ рядком і без жодного блокування: цей колбек
			// живе на горутині pion, і затримка в ньому зупиняє обробку STUN.
			// Прикметно, що ознака життя береться з БУДЬ-ЯКОГО повідомлення, а
			// не лише з heartbeat: гейт, bitrate_target і select_output теж
			// доводять, що хаб на тому кінці живий.
			noteHubMessage(&hubLive, time.Now(), msg.Data)
			handleCtlMessage(msg.Data, onKeyframeRequest, onGate, onBitrateTarget, onSelectOutput)
		})
	}
	// Канал вводу (input.go) — ДРУГИЙ DataChannel того самого зʼєднання, у тому
	// ж стилі, що oosc-ctl: створює його агент (він тут offerer), хаб ловить
	// через OnDataChannel. Окремий від oosc-ctl навмисно: control — це накази
	// хаба про сам потік, а це — потік подій людини, і мішати їх в один конверт
	// означало б дописати вісім полів у control.Msg заради чужого протоколу.
	// nil-інʼєктор (вимкнений прапорець, не-Windows, недосяжний SendInput) =
	// каналу немає взагалі, і в SDP нічого не змінюється.
	if inj := newInputInjector(); inj != nil {
		in, dcErr := pc.CreateDataChannel(inputChannelLabel, nil)
		if dcErr != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("create input datachannel: %w", dcErr)
		}
		attachInputChannel(in, inj)
		// Поверхня вводу лишається ВСІМ віртуальним робочим столом (дефолт
		// input.Injector), і це точно лише поки монітор один: DXGI-виходи не
		// віддають свій Left/Top через capture.OutputInfo, тож звузити її нема з
		// чого.
		// ponytail: стеля — на кількох моніторах курсор ляже не туди; апгрейд
		// робиться на місці: додати Left/Top в oos_output_info -> OutputInfo і
		// кликати inj.SetSurface() тут і в SwitchOutput.
		if l := outputs.Load(); l != nil && len(l.Outputs) > 1 {
			log.Printf("oo-agent: УВАГА — моніторів %d, а поверхня вводу поки що весь віртуальний робочий стіл: курсор ляже не туди. Для кількох моніторів лишається Mesh.", len(l.Outputs))
		}
	}
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: fmtp,
	}, "video", "oo-screen-agent")
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("new track: %w", err)
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add track: %w", err)
	}
	// Друга доріжка — звук ПК (audio.go), ЛИШЕ під OO_SCREEN_AUDIO. Додаємо
	// ТУТ, поруч із відео і до offer-а: обидві доріжки мусять потрапити в один
	// SDP, інакше звук поїхав би окремою негоціацією — тобто паралельним
	// механізмом, якого ми якраз уникаємо.
	atrk, err := addAudioTrack(pc)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add audio track: %w", err)
	}
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Printf("oo-agent: webrtc ICE: %s", s)
	})
	// Стан PeerConnection веде ДВІ речі: рукостискання (waitConnected нижче) і
	// рішення про реконект (reconnectDecider). Обидві тут, бо обробник у pion
	// один на зʼєднання — це сетер, а не підписка, і другий виклик затер би
	// перший. Саме так і губився реконект: waitConnected ставив свій обробник,
	// той лише ЛОГУВАВ failed, і більше цей стан ніхто не читав.
	connected := make(chan struct{})
	var connectedOnce sync.Once
	dec := newReconnectDecider(disconnectGrace, func(reason string) {
		log.Printf("oo-agent: peer connection down (%s) — reconnecting", reason)
		if onDown != nil {
			onDown(reason)
		}
	})
	// live: рішення про реконект має сенс ЛИШЕ для сесії, яка відбулась. Поки
	// йде рукостискання, невдалу спробу закриває сам dialWebRTC, і її ж "closed"
	// інакше друкував би в лог "peer connection down" на кожному оберті
	// реконекту — тобто саме той лог, по якому цю аварію й читають.
	var live atomic.Bool
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("oo-agent: webrtc PC state: %s", s)
		if s == webrtc.PeerConnectionStateConnected {
			live.Store(true)
			connectedOnce.Do(func() { close(connected) })
		}
		if live.Load() {
			dec.note(s.String())
		}
	})
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := sender.Read(buf)
			if err != nil {
				return
			}
			pkts, err := rtcp.Unmarshal(buf[:n])
			if err != nil {
				continue
			}
			for _, p := range pkts {
				if _, ok := p.(*rtcp.PictureLossIndication); ok {
					log.Printf("oo-agent: PLI received from hub")
					if onKeyframeRequest != nil {
						onKeyframeRequest()
					}
				}
			}
		}
	}()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("create offer: %w", err)
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("set local description: %w", err)
	}
	// A-29: без дедлайну зламаний мережевий стек = вічне «connecting» у
	// кадровому циклі; хост-кандидати збираються за мілісекунди.
	select {
	case <-gatherComplete:
	case <-time.After(iceGatherTimeout):
		_ = pc.Close()
		return nil, fmt.Errorf("ICE gathering timeout (%s)", iceGatherTimeout)
	}

	req := offerReq{SDP: pc.LocalDescription().SDP, Token: authToken(), Node: nodeID, Bitrate: startBitrateBps, Audio: atrk != nil}
	if l := outputs.Load(); l != nil {
		req.Outputs, req.ActiveOutput = l.Outputs, l.Active
	}
	body, _ := json.Marshal(req)
	resp, err := httpClient.Post(hubURL, "application/json", bytes.NewReader(body))
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("post offer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = pc.Close()
		return nil, fmt.Errorf("hub responded %d", resp.StatusCode)
	}
	var ans answerResp
	if err := json.NewDecoder(resp.Body).Decode(&ans); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("decode answer: %w", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("set remote description: %w", err)
	}

	if err := waitConnected(pc, connected, 10*time.Second); err != nil {
		_ = pc.Close()
		return nil, err
	}
	return &webrtcTransport{pc: pc, track: track, atrk: atrk, frameInterval: frameInterval, ctl: ctlChan, fmtp: fmtp}, nil
}

// waitConnected чекає на connected, який закриває обробник стану з dialWebRTC.
// Власного обробника більше не ставить — див. коментар там.
func waitConnected(pc *webrtc.PeerConnection, connected <-chan struct{}, timeout time.Duration) error {
	select {
	case <-connected:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timed out waiting for PeerConnectionStateConnected (state=%s)", pc.ConnectionState())
	}
}

func (t *webrtcTransport) send(au encode.AU, _ uint64) error {
	// WebRTC не використовує envelope: RTP/Pion самі несуть seq/timestamp.
	return t.track.WriteSample(media.Sample{Data: au.Data, Duration: t.sampleDuration(au.PTS)})
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
}

// ---------------------------------------------------------------------------

// onGate(resume) — hub сигналить присутність глядача через control-DataChannel:
// resume=true коли зʼявився viewer, false коли пішов. Агент кодує лише коли
// resume (on-demand): без глядача капчер+енкодер простоюють, не палячи CPU й
// не ллючи потік на hub. Порожній/нереалізований шлях (WT, старий hub без
// DataChannel) → onGate ніколи не кличеться, агент лишається в дефолтному
// стані (не-пауза) = стара always-on поведінка. Тобто гейтинг — суто виграш
// там, де його підтримують обидві сторони, і безпечний фолбек, де ні.
// onBitrateTarget(bps) — hub просить іншу CBR-ціль (control §5.4). Два входи,
// але формат один: типізований control.Msg (bitrate_target). WT возить його
// control-стрімом, WebRTC — DataChannel-ом "oosc-ctl", тим самим, де для
// сумісності ще ходять текстові "resume"/"pause".
// onDown — тільки WebRTC: у нозі WT (bench) стану PeerConnection немає, там
// розрив і далі видно лише помилкою запису в QUIC-стрім.
func dial(kind, hubAddr string, frameInterval time.Duration, onKeyframeRequest func(), onGate func(bool), onBitrateTarget func(uint64), onSelectOutput func(int), onDown func(string)) (transport, error) {
	switch kind {
	case "wt":
		return dialWT(hubAddr, onKeyframeRequest, onBitrateTarget, onSelectOutput) // WT (bench) без гейтингу
	case "webrtc":
		return dialWebRTC(hubAddr, frameInterval, onKeyframeRequest, onGate, onBitrateTarget, onSelectOutput, onDown)
	default:
		return nil, fmt.Errorf("unknown -transport %q (want wt|webrtc)", kind)
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
		case control.TypeMaxFps:
			setMaxFps(m.Fps)
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

// setMaxFps — стеля з хаба (1..60); усе поза межами = «без стелі», а не
// «0 кадрів/с»: зависла картинка гірша за зайвий CPU.
func setMaxFps(fps int) {
	if fps < 1 || fps > 60 {
		fps = 0
	}
	if int(maxFpsWanted.Swap(int32(fps))) != fps {
		log.Printf("oo-agent: max_fps -> %d (0 = без стелі)", fps)
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

// keepStillAU — чи годиться щойно закодований keepalive-AU на повтор під час
// локу/UAC (sendStillKeepalive). Лише коли той самий кадр УЖЕ був закодований
// перед цим: тоді P-кадр справді «нічого не змінилось». Кадр, який
// max_fps/admission/mouse-only викинули ДО кодування, потрапляє в keepalive
// першим — і його P-кадр несе реальну дельту. Повтор такої дельти поверх
// референсу, де вона вже є, накладає її вдруге.
func keepStillAU(still, sameFrameEncodedBefore bool, aus []encode.AU) bool {
	return still && sameFrameEncodedBefore && len(aus) == 1 && !aus[0].Keyframe
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

// ---------------------------------------------------------------------------
// stream — капчер і енкодер як ОДНЕ ціле.
//
// Тримати їх окремими змінними було можна рівно доти, доки монітор вибирався
// один раз на старті. Перемикання робить їх нерозривними: інший вихід — інша
// геометрія й інший D3D11-девайс, а encode.Config бакає Width/Height/
// SrcWidth/SrcHeight намертво (SetResolution в енкодері немає, є лише
// SetBitrate/ForceIDR/Flush/Close). Тобто новий монітор = НОВИЙ енкодер, і
// напівперемкнутий стан (новий капчер зі старим енкодером) не має існувати
// навіть на мить.
// ---------------------------------------------------------------------------
type stream struct {
	// Незмінне, з прапорців. reqW/reqH — СИРІ значення -width/-height, де
	// 0 означає «рідна роздільність поточного виводу»: розвʼязує їх openEncoder
	// проти живого капчера, бо після select_output монітор уже інший.
	reqW, reqH    int
	fps           int
	forceSoftware bool
	logger        *slog.Logger

	// Стан, який чіпає ЛИШЕ кадровий цикл (і SwitchOutput, який кличе той самий
	// цикл). Власного мʼютекса тут немає НАВМИСНО: capture.Capturer і так «NOT
	// safe for concurrent use» (capture_windows.go), тож замок поверх нього
	// створив би ілюзію, що міняти капчер можна з будь-якої горутини. Запит
	// іззовні кладеться в pending і застосовується циклом.
	cap       *capture.Capturer
	output    int
	encW      int
	encH      int
	software  bool
	lastFrame *capture.NV12Frame

	// enc бере не тільки цикл: control-горутина кличе ForceIDR/SetBitrate.
	// Тому вказівник атомарний, а самі виклики серіалізує ВЛАСНИЙ мʼютекс
	// енкодера (encode.Encoder.mu) — той самий, що вже серіалізував гонку
	// keyframe_request із кадровим циклом. Колбек, який під час перемикання
	// встиг узяти старий енкодер, дістане ErrClosed — помилку в лог, не краш.
	enc atomic.Pointer[encode.Encoder]
	// encDev — D3D-девайс, під який відкрито поточний енкодер (A-01). Капчер
	// після ACCESS_LOST (лок/UAC/зміна роздільності/сон) створює НОВИЙ девайс,
	// а енкодер тримав старий: текстури з чужого девайса давали сміття або
	// помилки до самого виходу глядача. Розбіжність = перебудова енкодера.
	encDev uintptr
	// encGen — покоління капчера, під яке відкрито енкодер (A-06). Самої адреси
	// девайса мало: капчер після перебудови звільняє старий ID3D11Device, і
	// НОВИЙ може лягти рівно на ту саму адресу — тоді encDev збігся б, енкодер
	// лишився б від мертвого девайса, а помилка виглядала б як «сміття в кадрі».
	encGen uint64

	// bitrateBps — ЖИВА ціль CBR: hub міг притиснути її через bitrate_target.
	// Новий енкодер відкриваємо саме з нею, інакше перемикання монітора мовчки
	// повертало б бітрейт до стартового -bitrate.
	bitrateBps atomic.Int64

	pending outputRequest

	// wantIDR — запит IDR від control-горутин (keyframe_request, resume);
	// застосовує кадровий цикл (A-13/A-31).
	wantIDR atomic.Bool
}

func (s *stream) encoder() *encode.Encoder { return s.enc.Load() }

// openEncoder відкриває енкодер під джерело srcW×srcH і повертає геометрію, у
// якій він РЕАЛЬНО кодує. Один шлях для старту і для SwitchOutput: правило
// «софт-MFT не масштабує» (encodeGeometry) мусить діяти в обох, а дублікат
// такого правила рано чи пізно розійдеться з оригіналом.
func (s *stream) openEncoder(device uintptr, gen uint64, srcW, srcH int) (*encode.Encoder, int, int, bool, error) {
	// D3DDevice потрібен ЛИШЕ апаратному (zero-copy) шляху: у C весь D3D-блок
	// під `if (e->is_hardware)` (mft.c). При -force-software ми наперед знаємо,
	// що піде софт, тож чужий девайс у CPU-only енкодер не передаємо зовсім.
	// Після перемикання монітора девайс ІНШИЙ — його дає новий капчер, і саме
	// тому енкодер тут відкривається заново, а не переналаштовується.
	d3d := device
	if s.forceSoftware {
		d3d = 0
	}
	bps := int(s.bitrateBps.Load())
	// Прапорці розвʼязуємо ТУТ, а не один раз на старті: SwitchOutput заходить
	// у це саме місце з іншим монітором, і «рідна роздільність» для нього —
	// роздільність НОВОГО виводу.
	reqW, reqH := requestedSize(s.reqW, s.reqH, srcW, srcH)
	auto := s.reqW <= 0 || s.reqH <= 0
	cfg := encode.Config{
		Width: reqW, Height: reqH, FPS: s.fps, BitrateBps: bps,
		D3DDevice: d3d, SrcWidth: srcW, SrcHeight: srcH, ForceSoftware: s.forceSoftware,
	}
	enc, err := encode.New(cfg)
	// A-19: ErrNoHardware — це не «MFT не взяв цю геометрію», а «апаратного
	// шляху для НАШОГО адаптера нема зовсім» (немає hw-MFT; усі hw-MFT на
	// чужому GPU — A-18; знайдений hw-MFT не D3D11-aware). Вибору між hw і sw
	// тут не існує, тому поріг ядер softwareNativeAffordable — який нижче
	// вирішує «а чи варта рідна роздільність софту» — до цієї гілки не
	// стосується: слабкий софт кращий за ПК, зниклий із пульта.
	if err != nil && !s.forceSoftware && errors.Is(err, encode.ErrNoHardware) {
		swCfg := cfg
		swCfg.ForceSoftware = true
		swCfg.D3DDevice = 0 // CPU-only енкодер чужого девайса не бере
		if enc2, err2 := encode.New(swCfg); err2 == nil {
			log.Printf("oo-agent: апаратного H.264 MFT для цього захоплення нема (%v) — софтверний енкодер, %dx%d, cores=%d",
				err, reqW, reqH, runtime.NumCPU())
			enc, err = enc2, nil
		}
	}
	if err != nil {
		// Не кожен MFT бере рідну роздільність (див. fallbackSize): відступаємо
		// на 1920x1080 замість того, щоб лишити ПК без картинки зовсім.
		// Перш ніж жертвувати роздільністю — пробуємо СОФТВЕРНИЙ енкодер у
		// рідній. Апаратний MFT відмовляє на 2560x1440, софтверний ту саму
		// геометрію бере (level 5.1). Але тільки там, де машина це тягне:
		// див. softwareNativeAffordable, число заміряне, не вгадане.
		if auto && !s.forceSoftware && softwareNativeAffordable(runtime.NumCPU()) {
			swCfg := cfg
			swCfg.ForceSoftware = true
			if enc2, err2 := encode.New(swCfg); err2 == nil {
				log.Printf("oo-agent: hardware refused %dx%d (%v) — software encoder at native (cores=%d)",
					reqW, reqH, err, runtime.NumCPU())
				enc, err = enc2, nil
			}
		}
	}
	if err != nil {
		if fw, fh, ok := fallbackSize(auto, reqW, reqH); ok {
			log.Printf("oo-agent: encoder refused %dx%d (%v) — falling back to %dx%d", reqW, reqH, err, fw, fh)
			reqW, reqH = fw, fh
			cfg.Width, cfg.Height = fw, fh
			enc, err = encode.New(cfg)
		}
	}
	if err != nil {
		return nil, 0, 0, false, err
	}
	software := !enc.Hardware()
	w, h := encodeGeometry(reqW, reqH, srcW, srcH, software)
	if w != reqW || h != reqH {
		log.Printf("oo-agent: software path — re-opening encoder at native %dx%d (was %dx%d): submit_cpu crops, not scales",
			w, h, reqW, reqH)
		enc.Close()
		enc, err = encode.New(encode.Config{
			Width: w, Height: h, FPS: s.fps, BitrateBps: bps,
			D3DDevice: 0, SrcWidth: srcW, SrcHeight: srcH, ForceSoftware: true,
		})
		if err != nil {
			return nil, 0, 0, false, err
		}
	}
	// A-26: fmtp у SDP мусить нести ПРОФІЛЬ І РІВЕНЬ цього MFT, а не константи.
	if plid, perr := encoderPLID(enc.Headers()); perr == nil {
		encPLID.Store(plid)
	} else {
		log.Printf("oo-agent: 🔴 SPS енкодера не розібрано (%v) — fmtp лишається фолбеком %s; якщо MFT кодує щось інше, глядач дістане 415 h264_profile_mismatch",
			perr, h264FmtpLine)
	}
	// Профільна драбина в mft.c має другу сходинку (High) лише щоб MFT, який не
	// бере Main, усе-таки відкрився. Але High браузери не оголошують, тож така
	// машина потребує ручного втручання — і мусить кричати про це в лог, а не
	// тихо віддавати потік, який ніхто не візьме.
	if p := enc.Profile(); p != 0 && p != encProfileMain {
		log.Printf("oo-agent: 🔴 MFT %q відмовив у Main і взяв profile_idc=%d — браузери оголошують лише 42xx/4d/f4, хаб відповість 415 h264_profile_mismatch",
			enc.Name(), p)
	}
	log.Printf("oo-agent: encoder %q async=%v zero-copy=%v profile=%d level=%d encode=%dx%d software=%v fmtp=%s",
		enc.Name(), enc.Async(), enc.ZeroCopy(), enc.Profile(), enc.Level(), w, h, software, h264Fmtp())
	// 🔴 Перевіряємо ФАКТ, а не лише гілку помилки: без апаратного MFT mft.c
	// мовчки бере софтверний усередині encode.New, тож softwareNativeAffordable
	// вище не виконується жодного разу (див. softwareFPSCap). Причина мусить
	// бути в лозі повністю — інакше наступного разу це знову шукатимуть замірами
	// на живому парку.
	if software && !s.forceSoftware {
		if capped := softwareFPSCap(runtime.NumCPU(), w*h, s.fps); capped < s.fps {
			log.Printf("oo-agent: софтверний енкодер на %d ядрах, кадр %dx%d — це ≈%.2f ядра при %d к/с; тримаємо ≤%d к/с (≈%.2f ядра). Геометрію НЕ знижуємо: софт-MFT кропить, а не масштабує",
				runtime.NumCPU(), w, h, softwareCoreCost(w*h, s.fps), s.fps,
				capped, softwareCoreCost(w*h, capped))
		}
	}
	s.encDev = device
	s.encGen = gen
	return enc, w, h, software, nil
}

// nativeSize — рідна геометрія виводу: з капчера, а без нього (стартували на
// заблокованому столі) — з енумерації DXGI; крайній фолбек 1920x1080.
func nativeSize(c *capture.Capturer, outIdx int) (int, int) {
	if c != nil {
		return c.Size()
	}
	if outs, err := capture.Outputs(); err == nil && outIdx >= 0 && outIdx < len(outs) && outs[outIdx].Width > 0 {
		return outs[outIdx].Width, outs[outIdx].Height
	}
	return 1920, 1080
}

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

// syncEncoderToCapture перебудовує енкодер, якщо капчер після ACCESS_LOST
// живе вже на іншому D3D-девайсі (A-01). Лише з кадрового циклу.
func (s *stream) syncEncoderToCapture() error {
	dev, gen := s.cap.Device(), s.cap.Generation()
	if !encoderStale(dev, gen, s.encDev, s.encGen) {
		return nil
	}
	srcW, srcH := s.cap.Size()
	enc, w, h, sw, err := s.openEncoder(dev, gen, srcW, srcH)
	if err != nil {
		return err
	}
	if old := s.enc.Swap(enc); old != nil {
		old.Close()
	}
	s.encW, s.encH, s.software = w, h, sw
	s.applyReadback()
	epoch := bumpEpoch()
	if err := enc.ForceIDR(); err != nil {
		log.Printf("oo-agent: ForceIDR after device change: %v", err)
	}
	log.Printf("oo-agent: capture device changed — encoder rebuilt (native %dx%d, encode %dx%d, epoch=%d)", srcW, srcH, w, h, epoch)
	return nil
}

// applyReadback виставляє режим капчера РІВНО ОДИН раз на його життя, коли вже
// відомо, який енкодер відкрився: апаратний MFT бере D3D-текстуру напряму
// (zero-copy), софтверний текстур не приймає — лише CPU NV12.
func (s *stream) applyReadback() {
	if s.software {
		s.cap.SetCPUReadback(true) // капчер і так стартує в readback — це підтвердження, не toggle
		log.Printf("oo-agent: software MFT path (CPU NV12, higher CPU)")
		return
	}
	s.cap.SetCPUReadback(false)
	log.Printf("oo-agent: hardware H.264 encoder — zero-copy texture path")
}

// requestOutput — просить перемикання на вихід idx. Можна кликати з будь-якої
// горутини: сам перехід зробить кадровий цикл (див. коментар до stream).
func (s *stream) requestOutput(idx int) { s.pending.set(idx) }

// releaseCapture звільняє захоплення екрана, поки глядача нема: закриває капчер
// (а з ним DXGI-дублікацію виводу) і енкодер. Викликати ЛИШЕ з кадрового циклу —
// капчер не thread-safe. Прибирає конкуренцію з MeshCentral за той самий вивід
// у простої: регресія 01.09 — наш агент на «паузі» тримав дублікацію відкритою,
// і desktop-сесії Mesh на тому ж ПК рвались кожні ~19с. Енкодер thread-safe
// (mu+closed), тож колбеки keyframe/bitrate, що прийдуть після Close, повернуть
// ErrClosed, а не впадуть; nil-покажчик колбеки перевіряють окремо.
func (s *stream) releaseCapture() {
	if e := s.enc.Swap(nil); e != nil {
		e.Close()
	}
	if s.cap != nil {
		s.cap.Close()
		s.cap = nil
	}
	// lastFrame аліасив буфери капчера — після Close це висяча памʼять.
	s.lastFrame = nil
}

// reacquireCapture повертає захоплення на ПОТОЧНИЙ вивід, коли зʼявився глядач.
// Дзеркало releaseCapture: новий капчер під той самий output, новий енкодер під
// його геометрію, свіжа епоха та IDR — глядач декодує з нуля. Теж лише з циклу.
func (s *stream) reacquireCapture() error {
	cap_, err := capture.NewWithOptions(s.output, capture.Options{Logger: s.logger})
	if err != nil {
		return err
	}
	srcW, srcH := cap_.Size()
	enc, encW, encH, software, err := s.openEncoder(cap_.Device(), cap_.Generation(), srcW, srcH)
	if err != nil {
		cap_.Close()
		return err
	}
	s.cap = cap_
	s.enc.Store(enc)
	s.encW, s.encH, s.software = encW, encH, software
	s.applyReadback()
	bumpEpoch() // новий енкодер = новий SPS; глядач не має склеїти його зі старим
	if err := enc.ForceIDR(); err != nil {
		log.Printf("oo-agent: ForceIDR after reacquire: %v", err)
	}
	return nil
}

// SwitchOutput перемикає захоплення на вихід idx: новий капчер, новий енкодер
// під його геометрію, нова епоха конфігурації, свіжий IDR.
//
// Викликати ЛИШЕ з кадрового циклу. Порядок такий, що до останнього моменту є
// куди відкотитись: новий капчер і новий енкодер піднімаються ПОВНІСТЮ, і лише
// потім закривається старе. Впасти на півдорозі означає лишитись на старому
// моніторі — потік не переривається взагалі.
func (s *stream) SwitchOutput(idx int) error {
	n, err := capture.OutputCount()
	if err != nil {
		return err
	}
	if err := resolveOutput(idx, n); err != nil {
		return err
	}
	if idx == s.output {
		return nil // вже на ньому: зайвий IDR і зсув епохи глядачеві ні до чого
	}

	newCap, err := capture.NewWithOptions(idx, capture.Options{Logger: s.logger})
	if err != nil {
		return fmt.Errorf("вихід %d: %w", idx, err)
	}
	srcW, srcH := newCap.Size()
	newEnc, encW, encH, software, err := s.openEncoder(newCap.Device(), newCap.Generation(), srcW, srcH)
	if err != nil {
		newCap.Close() // відкат: старий капчер+енкодер цілі, кадровий цикл нічого не помітив
		return fmt.Errorf("вихід %d: encode.New: %w", idx, err)
	}

	oldCap, oldEnc := s.cap, s.enc.Swap(newEnc)
	s.cap, s.output, s.encW, s.encH, s.software = newCap, idx, encW, encH, software
	// 🚨 lastFrame аліасить буфери СТАРОГО капчера (copyOut: «the returned frame
	// aliases them»), а в zero-copy — його текстуру на СТАРОМУ D3D-девайсі.
	// Після Close це висяча памʼять, і найближчий keepalive подав би її в новий
	// MFT. Скидаємо разом із капчером; наступний кадр візьметься з нового
	// виходу, а якщо той нерухомий — через GDI (shouldRearm бачить lastFrame==nil).
	s.lastFrame = nil
	s.applyReadback()
	oldEnc.Close()
	oldCap.Close()

	// 🚨 Зсув епохи — не косметика: нова геометрія несе інший SPS, і глядач, що
	// не побачив зміни епохи, склеїв би його зі старим потоком.
	epoch := bumpEpoch()
	if err := newEnc.ForceIDR(); err != nil {
		log.Printf("oo-agent: ForceIDR after switch: %v", err)
	}
	publishOutputs(idx)
	log.Printf("oo-agent: switched to output %d (native %dx%d, encode %dx%d, epoch=%d)",
		idx, srcW, srcH, encW, encH, epoch)
	return nil
}

// Локального HTTP-перемикача (-switch-addr) тут більше немає: у control-протоколі
// зʼявився select_output, і монітор перемикає консоль через hub тим самим каналом,
// що вже возить bitrate_target. Тримати поруч ДРУГИЙ, нікому не підзвітний вхід у
// той самий stream.requestOutput означало б лишити на ПК працівника відкриту
// ручку керування його екраном повз усю авторизацію ЕРП.

func main() {
	transportKind := flag.String("transport", "webrtc", "транспорт: webrtc|wt (wt — легасі T1-стенд, лише збірка -tags wt)")
	hubAddr := flag.String("hub", "", "адреса hub-а (wt: host:port QUIC; webrtc: http://host:port/offer/agent)")
	fps := flag.Int("fps", 30, "цільовий FPS енкодера/GOP (60 — лише для стенда: подвійний CPU без видимої різниці на робочому столі)")
	bitrate := flag.Int("bitrate", 0, "бітрейт, біт/с (CBR); 0 = порахувати за пікселями кадру (defaultBitrate)")
	width := flag.Int("width", 0, "ширина вихідного кадру; 0 = рідна роздільність виводу (енкодер масштабує з нативної)")
	height := flag.Int("height", 0, "висота вихідного кадру; 0 = рідна роздільність виводу")
	output := flag.Int("output", 0, "індекс DXGI-виводу (монітора) на старті; неіснуючий клампиться до 0")
	node := flag.String("node", "", "mesh node_id цього ПК (webrtc): hub реєструє publisher-а під ним і маршрутизує viewer-ів сюди; порожнє = старий T1-режим (node з env на hub)")
	forceSoftware := flag.Bool("force-software", false, "пропустити апаратний енум і взяти софтверний Microsoft H264 MFT (CPU NV12 sync-шлях) — для відтворення софт-шляху на машині з hw-енкодером")
	tokenFlag := flag.String("token", "", "hub-токен агента (перекриває env OO_SCREEN_T1_TOKEN); дозволяє запуск напряму зі schtask без .cmd-обгортки")
	logPath := flag.String("log", "", "шлях до файлу логу; якщо задано — увесь вивід іде туди (GUI-режим -H windowsgui без консолі, stdout нема)")
	audioFlag := flag.Bool("audio", false, "передавати звук ПК (перекриває env OO_SCREEN_AUDIO=1)")
	inputFlag := flag.Bool("input", false, "приймати клавіатуру й мишу від глядача (перекриває env OO_SCREEN_INPUT=1)")
	flag.Parse()

	// Прапорці перекривають env з тієї ж причини, що й -token вище: агента
	// запускає Register-ScheduledTask, яка передає ЛИШЕ аргументи, а змінних
	// середовища у задачі нема взагалі. Без цього обидві фічі неможливо
	// увімкнути на бойовому ПК — код був би написаний і недосяжний.
	// Тільки в один бік (прапорець вмикає, не вимикає): -audio=false не мусить
	// гасити те, що людина свідомо ввімкнула через середовище.
	applyFeatureFlags(*audioFlag, *inputFlag)

	// GUI-режим (-H windowsgui) не має консолі, тож log за замовчуванням у
	// нікуди. -log перенаправляє його у файл. Ставимо ДО першого log.Printf.
	if *logPath != "" {
		if f, where, ferr := openAgentLog(*logPath); ferr == nil {
			log.SetOutput(f)
			defer f.Close()
			if where != *logPath {
				log.Printf("oo-agent: лог недоступний за %s — пишу у %s", *logPath, where)
			}
		}
	}
	// A-41: геометрію перевіряємо ПІСЛЯ налаштування логу (щоб причина лягла у
	// файл під -H windowsgui, де стандартного виводу нема зовсім) і ДО всього
	// іншого — рвати старт має сенс лише поки нічого не піднято.
	//
	// Fatalf тут доречний саме тому, що це помилка АРГУМЕНТІВ: сама вона не
	// розсмокчеться, а мовчазний старт віддав би оператору не ту геометрію, яку
	// він просив. Планувальник -width/-height не передає (перевірено по всьому
	// дереву), тож бойові ПК цією гілкою не ходять.
	if err := validateSize(*width, *height); err != nil {
		log.Fatalf("oo-agent: %v", err)
	}

	cliToken = *tokenFlag

	nodeID = *node

	// A-36: другий екземпляр на тому ж ПК рве DXGI-дублікацію першого.
	release, dup := acquireSingleInstance()
	if dup {
		log.Printf("oo-agent: інший агент уже працює на цьому ПК — виходжу")
		return
	}
	defer release()

	if *hubAddr == "" {
		switch *transportKind {
		case "wt":
			*hubAddr = "localhost:4460"
		case "webrtc":
			*hubAddr = "http://127.0.0.1:4470/offer/agent"
		}
	}

	// encode.New нормалізує FPS<=0 до 30 усередині (encode_windows.go:120),
	// але це приватне — зовнішній код (PTS-ділення в кадровому циклі) про цю
	// нормалізацію не знає. Нормалізуємо прапорець тут, ОДИН раз, і
	// використовуємо s.fps всюди далі (encode.Config і розрахунок PTS), щоб
	// -fps=0 не привів до ділення на нуль у циклі захоплення.
	effectiveFPS := *fps
	if effectiveFPS <= 0 {
		log.Printf("oo-agent: -fps=%d invalid, normalizing to 30", effectiveFPS)
		effectiveFPS = 30
	}

	// Стартовий індекс КЛАМПИМО (на відміну від перемикання на льоту, де
	// неіснуючий індекс — помилка): агента піднімає планувальник із індексом,
	// збереженим колись, і від'єднаний другий монітор інакше означав би
	// log.Fatalf, тобто зниклий з пульта ПК. Див. clampStartOutput.
	outIdx := *output
	if n, cerr := capture.OutputCount(); cerr == nil {
		if c := clampStartOutput(outIdx, n); c != outIdx {
			log.Printf("oo-agent: -output=%d немає (виходів %d) — стартую з %d", outIdx, n, c)
			outIdx = c
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A-39: logoff/shutdown гасять ctx тим самим шляхом, що Ctrl+C; lock/unlock
	// читає кадровий цикл. nil = вікно не піднялось, поведінка як до A-39.
	session := watchSession(stop)

	s := openStream(ctx, &stream{
		reqW: *width, reqH: *height, fps: effectiveFPS,
		forceSoftware: *forceSoftware,
		// A-04: під -H windowsgui os.Stderr не існує — уся капчер-діагностика
		// (recreate failed, access lost) губилась навіть із -log. Один сток.
		logger: slog.New(slog.NewTextHandler(log.Writer(), nil)),
		output: outIdx,
	}, *bitrate)
	if s == nil {
		return
	}
	defer s.close()

	// Список моніторів іде в offer (див. offerReq.Outputs) — консолі більше
	// нізвідки його взяти. Оновлюється тут і на кожному перемиканні.
	publishOutputs(outIdx)
	if l := outputs.Load(); l != nil {
		log.Printf("oo-agent: outputs=%d active=%d %+v", len(l.Outputs), l.Active, l.Outputs)
	}

	a := newAgent(s, session, *transportKind, *hubAddr)
	tp, err := retryUntil(ctx, "initial dial "+*hubAddr, a.dial)
	if err != nil {
		return
	}
	a.tp = tp
	log.Printf("oo-agent: connected via %s to %s", *transportKind, *hubAddr)

	go a.runSender()
	// Звук — ОКРЕМА горутина, бо джерело в нього своє (WASAPI, ~10мс пакети) і
	// зупиняти через нього кадровий цикл нема за що. Транспорт береться тим
	// самим a.transport(), що й у відео-sender-а: після реконекту звук піде в
	// НОВУ доріжку без жодного власного механізму перепідключення.
	// Гейт — той самий gatePaused, що керує відео (див. runAudio).
	if audioEnabled && *transportKind == "webrtc" {
		go runAudio(ctx, &a.gatePaused, func(data []byte, dur time.Duration) error {
			return a.transport().sendAudio(data, dur)
		})
	}
	go a.watchAvailability(ctx)

	a.run(ctx)
}

// openStream піднімає капчер і енкодер стартового монітора (s.output) і
// рахує стартовий бітрейт. nil — агента зупинили під час бек-офу; тоді все,
// що встигло відкритись, уже закрито.
func openStream(ctx context.Context, s *stream, bitrateFlag int) *stream {
	// A-27: замість Fatalf — чекаємо з бек-офом (див. retryUntil). Але
	// заблокований/захищений робочий стіл (E_ACCESSDENIED на DuplicateOutput)
	// — не привід не йти на хаб: без агента на хабі ПК «зникає з пульта»
	// на весь час локу (Maria, 05.09). Тоді стартуємо БЕЗ капчера: кадровий
	// цикл підніме його через reacquireCapture, щойно зʼявиться глядач.
	open := func() (*capture.Capturer, error) {
		return capture.NewWithOptions(s.output, capture.Options{Logger: s.logger})
	}
	if c, cerr := open(); cerr == nil {
		s.cap = c
	} else if errors.Is(cerr, capture.ErrNotAvailable) || errors.Is(cerr, capture.ErrAccessLost) {
		log.Printf("oo-agent: capture.New: %v — стартую без захоплення, підніму при появі глядача", cerr)
	} else {
		c, rerr := retryUntil(ctx, "capture.New", open)
		if rerr != nil {
			return nil
		}
		s.cap = c
	}
	// Не чіпаємо readback ДО того, як дізнаємось тип енкодера. Раніше тут стояв
	// SetCPUReadback(false), а після encode.New — умовний SetCPUReadback(true)
	// для софт-шляху. Той toggle false→true (перемикання РЕЖИМУ капчера ПІСЛЯ
	// ініціалізації, ще й у два кроки) — саме «живий стик», де софт-шлях падав.
	// Тепер режим виставляється РІВНО ОДИН раз нижче (stream.applyReadback),
	// коли вже відомо hw vs sw. Капчер за замовчуванням у readback-режимі
	// (NewWithOptions readback=true), тож для софт-шляху це взагалі no-op.

	srcW, srcH := nativeSize(s.cap, s.output)
	// Геометрію беремо ВІД ВИВОДУ, а не з зашитих 1920x1080: на моніторі
	// 2560x1440 енкодер масштабував униз, і текст у таблицях виходив мильнішим,
	// ніж у транспорті, який ми замінюємо. Прапорці лишаються шляхом для
	// слабкого каналу — requestedSize віддає їм пріоритет.
	//
	// У stream лежать СИРІ прапорці, а не цей результат: після select_output
	// монітор інший, і його рідний розмір має порахуватись заново (openEncoder).
	reqW, reqH := requestedSize(s.reqW, s.reqH, srcW, srcH)
	// Бітрейт мусить іти за пікселями: рідна роздільність при старих 8 Мбіт/с —
	// та сама мильна картинка, лише з іншого боку (на 1440p пікселів у 1.78
	// раза більше при тому самому потоці).
	bitrateBps := defaultBitrate(bitrateFlag, reqW, reqH)
	log.Printf("oo-agent: capture opened, output %d, native %dx%d -> want %dx%d, bitrate %d bps",
		s.output, srcW, srcH, reqW, reqH, bitrateBps)

	// Стеля для bitrate_target: hub бере її з поля bitrate в offer (ceilingBps).
	// Ставимо ДО dial — offerReq читає цю змінну.
	//
	// ponytail: при відступі на 1920x1080 (fallbackSize) бітрейт лишається
	// порахованим під рідну — тобто щедрішим, ніж треба. Так СВІДОМО: CBR уже
	// зашитий в енкодер при відкритті, а SetBitrate до першого кадру MFT
	// приймає ненадійно (див. bitrateTarget), і перерахунок лише тут розійшовся
	// б зі стелею, яку ми оголосили хабу. Зайві біти зріже регулятор хаба.
	startBitrateBps = bitrateBps
	s.bitrateBps.Store(int64(bitrateBps))

	if s.cap != nil {
		type encOpen struct {
			enc  *encode.Encoder
			w, h int
			sw   bool
		}
		cap_ := s.cap
		eo, err := retryUntil(ctx, "encode.New", func() (encOpen, error) {
			e, w, h, sw, err := s.openEncoder(cap_.Device(), cap_.Generation(), srcW, srcH)
			return encOpen{e, w, h, sw}, err
		})
		if err != nil {
			s.close()
			return nil
		}
		s.enc.Store(eo.enc)
		s.encW, s.encH, s.software = eo.w, eo.h, eo.sw
		s.applyReadback()
	}
	return s
}

// close закриває ПОТОЧНІ енкодер і капчер (SwitchOutput/releaseCapture їх
// міняють; на паузі капчер звільнено й s.cap == nil — Close на nil впав би).
func (s *stream) close() {
	if e := s.encoder(); e != nil {
		e.Close()
	}
	if s.cap != nil {
		s.cap.Close()
	}
}
