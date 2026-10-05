//go:build windows

// oo-agent — Т2: живий агент. DXGI-захоплення (agent/capture) → апаратний
// MFT-енкодер (agent/encode) → один із двох транспортів (обидва кандидати в
// одному бінарі, вибір прапорцем -transport). Не дублює agent/capture чи
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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/quic-go/quic-go"

	"github.com/organicoils/oo-screen/agent/capture"
	"github.com/organicoils/oo-screen/agent/encode"
	"github.com/organicoils/oo-screen/internal/agentcred"
	"github.com/organicoils/oo-screen/internal/consent"
	"github.com/organicoils/oo-screen/internal/contentmode"
	"github.com/organicoils/oo-screen/internal/control"
	"github.com/organicoils/oo-screen/internal/cursorproto"
	"github.com/organicoils/oo-screen/internal/envelope"
	"github.com/organicoils/oo-screen/internal/pacer"
	"github.com/organicoils/oo-screen/internal/refine"
	"github.com/organicoils/oo-screen/internal/swlimit"
	"github.com/organicoils/oo-screen/internal/textmode"
)

var httpClient = &http.Client{Timeout: dialTimeout}

// nodeID — mesh node_id цього ПК, задається прапорцем -node у main(). Порожній
// = старий T1/бенч-режим (hub бере node з env). Читається лише з sender-шляху
// dialWebRTC, який стартує після main() встановив значення.
// multimonParentPinned — F6: батько -multimon запустив дочірні потоки, тож
// select_output основного потоку ігнорується (рев'ю: дубль монітора, A-36).
var multimonParentPinned atomic.Bool

var nodeID string

// cliToken — токен агента, обраний у main() з -token-file / OO_AGENT_TOKEN /
// -token / OO_SCREEN_T1_TOKEN (internal/agentcred, SEC #33). -token лишено для
// сумісності, але він видно в командному рядку schtask будь-якому локальному
// користувачу — тому -token-file.
var cliToken string

// wtCertPin / wtInsecure — TLS легасі-транспорту wt (SEC #32), з прапорців
// -wt-cert-sha256 / -wt-insecure.
var (
	wtCertPin  []byte
	wtInsecure bool
)

// startBitrateBps — фактичне значення -bitrate, з яким підняли агента. Їде в
// offer, щоб hub рахував стелю bitrate_target від нього, а не від свого
// дефолту. Читається лише з dialWebRTC, який стартує після main() присвоїв.
var startBitrateBps int

func authToken() string {
	if cliToken != "" {
		return cliToken
	}
	tok, _, _ := agentcred.ResolveToken("", "", os.Getenv)
	return tok
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

// ---- WebTransport (кандидат B): envelope-кадри по QUIC-стріму -------------

type wtTransport struct {
	conn     *quic.Conn
	videoStr *quic.Stream
}

func dialWT(hubAddr string, onKeyframeRequest func(), onBitrateTarget func(uint64), onSelectOutput func(int)) (*wtTransport, error) {
	// SEC #32: перевірка сертифіката за замовчуванням; самопідписаний hub-wt —
	// через пінінг -wt-cert-sha256 (його CERT_HASH=), -wt-insecure — лише стенд.
	tlsConf := agentcred.WTTLSConfig(hubAddr, []string{agentALPN}, wtCertPin, wtInsecure)
	dialCtx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	conn, err := quic.DialAddr(dialCtx, hubAddr, tlsConf, &quic.Config{})
	if err != nil {
		return nil, fmt.Errorf("dial hub-wt %s: %w", hubAddr, err)
	}

	ctrlStr, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		conn.CloseWithError(0, "")
		return nil, fmt.Errorf("open control stream: %w", err)
	}
	if err := control.Write(ctrlStr, control.Hello(authToken(), 1)); err != nil {
		conn.CloseWithError(0, "")
		return nil, fmt.Errorf("send hello: %w", err)
	}
	// heartbeat раз/5с, поки конект живий (control-протокол §5.4)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		var seq uint64 = 1
		for range t.C {
			seq++
			if err := control.Write(ctrlStr, control.Heartbeat(seq)); err != nil {
				return
			}
		}
	}()

	// Control-стрім двонаправлений: hub шле keyframe_request сюди ж, поки
	// агент лише пише (heartbeat) і ніколи не читає — запити зависають у
	// буфері й ForceIDR ніколи не викликається. Читаємо персистентно й на
	// keyframe_request віддаємо колбек у main() (§5.4/§5.5).
	go func() {
		br := bufio.NewReader(ctrlStr)
		for {
			m, err := control.ReadKnown(br, nil)
			if err != nil {
				return // конект/стрім мертвий — reconnect-логіка в main() це побачить через send-помилки
			}
			if m.Type == control.TypeKeyframeRequest && onKeyframeRequest != nil {
				onKeyframeRequest()
			}
			if m.Type == control.TypeBitrateTarget && onBitrateTarget != nil {
				onBitrateTarget(m.BitrateBps)
			}
			// Дзеркало WebRTC-гілки (handleCtlMessage): вибір монітора мусить
			// працювати обома ногами, інакше «перемкни екран» тихо не діяло б
			// саме на тому транспорті, яким знімають бенчі.
			if m.Type == control.TypeSelectOutput && onSelectOutput != nil {
				onSelectOutput(m.Output)
			}
		}
	}()

	videoStr, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		conn.CloseWithError(0, "")
		return nil, fmt.Errorf("open video stream: %w", err)
	}

	return &wtTransport{conn: conn, videoStr: videoStr}, nil
}

func (t *wtTransport) send(au encode.AU, seq uint64) error {
	flags := uint8(0)
	if au.Keyframe {
		flags |= envelope.FlagKeyframe
		// Енкодер (agent/encode) вставляє SPS/PPS у кожен IDR — контракт §5.2.
		flags |= envelope.FlagConfigured
	}
	f := &envelope.Frame{
		Flags: flags,
		// Епоха БІЛЬШЕ НЕ КОНСТАНТА: SwitchOutput зсуває її, бо інший монітор —
		// інша геометрія, тобто інший SPS. Глядач мусить побачити зсув, інакше
		// нова геометрія прийде посеред старого потоку (див. output.go).
		ConfigEpoch: currentEpoch(),
		FrameSeq:    seq,
		PTS:         uint64(au.PTS.Microseconds()),
		Payload:     bytes.Clone(au.Data),
	}
	buf, err := f.Marshal()
	if err != nil {
		return fmt.Errorf("marshal frame seq=%d: %w", seq, err)
	}
	// 🚨 A-33. Без дедлайну Write на QUIC-стрімі блокується НАЗАВЖДИ, щойно
	// вікно flow control закрилось: хаб перестав вичитувати (завис, а не впав),
	// вікно не рухається — і єдиний ordered sender стоїть у цьому виклику. А
	// поки він стоїть, у txErrCh нічого не приходить, тобто реконект, який мав
	// би це полагодити, не запускається взагалі. Дедлайн перетворює зависання
	// на звичайну помилку відправки, а її кадровий цикл уже вміє лікувати.
	if err := t.videoStr.SetWriteDeadline(time.Now().Add(wtWriteTimeout)); err != nil {
		return fmt.Errorf("set write deadline seq=%d: %w", seq, err)
	}
	if _, err := t.videoStr.Write(buf); err != nil {
		return fmt.Errorf("write frame seq=%d: %w", seq, err)
	}
	return nil
}

// sendAudio: нога WT — бенчова, доріжок у ній немає взагалі (envelope возить
// самі AU відео). Звук туди не їде і ніколи не їхав; runAudio для цього
// транспорту й не стартує (main: гілка лише для webrtc).
func (t *wtTransport) sendAudio([]byte, time.Duration) error { return nil }

func (t *wtTransport) close() {
	if t.videoStr != nil {
		_ = t.videoStr.Close()
	}
	if t.conn != nil {
		_ = t.conn.CloseWithError(0, "")
	}
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
	api, err := newWebRTCAPI()
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
	// SEC #37: інʼєктор створюється ДО control-каналу, щоб пауза (жодного
	// видимого глядача) відпускала затиснуті ним клавіші й кнопки миші.
	inj := newInputInjector()
	onGate = releaseOnPause(inj, onGate)
	var ctlDC *webrtc.DataChannel
	if onGate != nil || onBitrateTarget != nil || onSelectOutput != nil {
		ctl, dcErr := pc.CreateDataChannel("oosc-ctl", nil)
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
		ctlDC = ctl
	}
	// Канал вводу (input.go) — ДРУГИЙ DataChannel того самого зʼєднання, у тому
	// ж стилі, що oosc-ctl: створює його агент (він тут offerer), хаб ловить
	// через OnDataChannel. Окремий від oosc-ctl навмисно: control — це накази
	// хаба про сам потік, а це — потік подій людини, і мішати їх в один конверт
	// означало б дописати вісім полів у control.Msg заради чужого протоколу.
	// nil-інʼєктор (вимкнений прапорець, не-Windows, недосяжний SendInput) =
	// каналу немає взагалі, і в SDP нічого не змінюється.
	if inj != nil {
		in, dcErr := pc.CreateDataChannel(inputChannelLabel, nil)
		if dcErr != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("create input datachannel: %w", dcErr)
		}
		in.OnMessage(func(msg webrtc.DataChannelMessage) {
			if err := handleInputMessage(msg.Data, inj); err != nil {
				logInputProblem(time.Now(), err)
			}
		})
		// SEC #37: канал закрився (сесія впала/хаб пішов) — відпустити все.
		in.OnClose(func() { releaseHeldInput(inj, "input channel closed") })
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
	// Шар курсора (cursor.go) — ТРЕТІЙ DataChannel, у тому ж стилі: створює
	// агент до offer-а, хаб ловить через OnDataChannel і ретранслює глядачам.
	// Надійний і впорядкований (обґрунтування — internal/cursorproto).
	if cursorLayerEnabled {
		cur, dcErr := pc.CreateDataChannel(cursorproto.ChannelLabel, nil)
		if dcErr != nil {
			_ = pc.Close()
			return nil, fmt.Errorf("create cursor datachannel: %w", dcErr)
		}
		cur.OnOpen(func() { cursorPub.SetSink(cur) })
		cur.OnClose(func() { cursorPub.ClearSink(cur) })
	}
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType:    webrtc.MimeTypeH264,
		ClockRate:   90000,
		SDPFmtpLine: h264Fmtp(),
	}, "video", "oo-screen-agent")
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("new track: %w", err)
	}
	// Пейсер (internal/pacer): pion і далі пакетизує, але RTP виходить
	// leaky-bucket-ом на ~2x цілі, а не пачкою в 100+ пакетів на IDR.
	var paced *pacer.Track
	var local webrtc.TrackLocal = track
	if pacerEnabled() {
		paced = pacer.NewTrack(track, pacer.Config{})
		local = paced
	}
	sender, err := pc.AddTrack(local)
	if err != nil {
		if paced != nil {
			paced.Close()
		}
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
	// Канал текстових тайлів (tiles.go) — лише під -text-tiles, до offer-а.
	if err := addTilesChannel(pc); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("create tiles datachannel: %w", err)
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
	// Хаб без OO_SCREEN_AUDIO відхиляє m=audio (порт 0). Opus-доріжку треба
	// зняти ДО SetRemoteDescription, інакше pion валить усе зʼєднання (а з ним
	// і відео) помилкою «codec is not supported by remote» — див. audio.go.
	if atrk != nil && audioRejected(ans.SDP) {
		if err := detachAudioTrack(pc, atrk); err != nil {
			log.Printf("oo-agent: хаб відхилив звук, зняти доріжку не вдалось: %v", err)
		}
		atrk = nil
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("set remote description: %w", err)
	}

	if err := waitConnected(pc, connected, 10*time.Second); err != nil {
		_ = pc.Close()
		return nil, err
	}
	return &webrtcTransport{pc: pc, track: track, paced: paced, atrk: atrk, frameInterval: frameInterval, ctl: ctlDC}, nil
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
	s := media.Sample{Data: au.Data, Duration: t.sampleDuration(au.PTS)}
	if t.paced != nil {
		t.syncPaceTarget()
		return t.paced.WriteSample(s, au.Keyframe)
	}
	return t.track.WriteSample(s)
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
	gopSeconds    int // ТЗ 1.4: інтервал IDR у секундах (GOP = gopSeconds*fps)
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
		Width: reqW, Height: reqH, FPS: s.fps, BitrateBps: bps, GOP: gopFrames(s.gopSeconds, s.fps),
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
			Width: w, Height: h, FPS: s.fps, BitrateBps: bps, GOP: gopFrames(s.gopSeconds, s.fps),
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

// gopFrames — GOP у кадрах для encode.Config; <=0 секунд -> 0 (дефолт
// енкодера, 2*FPS).
func gopFrames(seconds, fps int) int {
	if seconds <= 0 {
		return 0
	}
	return seconds * fps
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
	transportKind := flag.String("transport", "webrtc", "транспорт: webrtc|wt (wt — легасі T1-стенд)")
	hubAddr := flag.String("hub", "", "адреса hub-а (wt: host:port QUIC; webrtc: http://host:port/offer/agent)")
	fps := flag.Int("fps", 30, "цільовий FPS енкодера/GOP (60 — лише для стенда: подвійний CPU без видимої різниці на робочому столі)")
	bitrate := flag.Int("bitrate", 0, "бітрейт, біт/с (CBR); 0 = порахувати за пікселями кадру (defaultBitrate)")
	width := flag.Int("width", 0, "ширина вихідного кадру; 0 = рідна роздільність виводу (енкодер масштабує з нативної)")
	height := flag.Int("height", 0, "висота вихідного кадру; 0 = рідна роздільність виводу")
	output := flag.Int("output", 0, "індекс DXGI-виводу (монітора) на старті; неіснуючий клампиться до 0")
	hubStandby := flag.String("hub-standby", os.Getenv("OO_HUB_STANDBY"), "O2: резервні hub-и через кому (webrtc, той самий формат що -hub). Порожньо = OFF (дефолт). Після -failover-after невдалих dial агент перевіряє GET /healthz резерву і переходить на здоровий. UNVERIFIED на реальних ПК")
	failoverAfter := flag.Int("failover-after", defaultFailoverAfter, "O2: скільки невдалих dial поспіль до перевірки резервного hub-а")
	node := flag.String("node", "", "mesh node_id цього ПК (webrtc): hub реєструє publisher-а під ним і маршрутизує viewer-ів сюди; порожнє = старий T1-режим (node з env на hub)")
	forceSoftware := flag.Bool("force-software", false, "пропустити апаратний енум і взяти софтверний Microsoft H264 MFT (CPU NV12 sync-шлях) — для відтворення софт-шляху на машині з hw-енкодером")
	tokenFlag := flag.String("token", "", "ЗАСТАРІЛО (видно в командному рядку): hub-токен агента; замість нього -token-file або env OO_AGENT_TOKEN")
	wtPin := flag.String("wt-cert-sha256", "", "wt: SHA-256 сертифіката hub-wt (hex або base64 з його CERT_HASH=) — пінінг самопідписаного сертифіката")
	wtInsecureFlag := flag.Bool("wt-insecure", false, "wt: НЕ перевіряти сертифікат hub-wt (лише стенд; MITM)")
	tokenFile := flag.String("token-file", "", "файл із hub-токеном агента (ACL: лише SYSTEM/Administrators); перекриває OO_AGENT_TOKEN і -token")
	logPath := flag.String("log", "", "шлях до файлу логу; якщо задано — увесь вивід іде туди (GUI-режим -H windowsgui без консолі, stdout нема)")
	audioFlag := flag.Bool("audio", false, "передавати звук ПК (перекриває env OO_SCREEN_AUDIO=1)")
	inputFlag := flag.Bool("input", false, "приймати клавіатуру й мишу від глядача (перекриває env OO_SCREEN_INPUT=1)")
	cursorLayerFlag := flag.Bool("cursor-layer", false, "шар курсора: НЕ вмальовувати вказівник у кадр, а слати форму+позицію каналом oosc-cursor (рух миші не коштує кадру); потрібен плеєр з config.cursorLayer")
	refineFlag := flag.Bool("refine", true, "дошліфування нерухомого екрана (ТЗ P4): через 200 мс без нових кадрів 1–2 рази перекодувати останній кадр із нижчим QP; false — вимкнути")
	textTilesFlag := flag.Bool("text-tiles", false, "текстові тайли (STAGE3-444 B): на нерухомому дошліфованому екрані один раз слати lossless PNG-тайли кольорового тексту каналом oosc-tiles (потрібен OO_SCREEN_TILES=1 на хабі і config.textTiles у плеєрі)")
	textFPS := flag.Int("text-fps", 15, "стеля FPS у текстовому режимі (gap #2: набір/читання — дрібні dirty rects); 0 = не обмежувати. Вихід із режиму (рух) знімає стелю миттєво")
	videoModeFlag := flag.Bool("video-mode", false, "режим «Відео» (internal/contentmode): тривалий рух великої площі (відео, прокрутка) -> до -video-fps на апаратному енкодері, що встигає, і прохання до hub підняти бітрейт у межах стелі; поза ним кадри вмісту не частіше -fps. UNVERIFIED на Windows")
	videoFPS := flag.Int("video-fps", 60, "частота в режимі «Відео» (лише з -video-mode)")
	lowMotionCapFlag := flag.Bool("lowmotion-cap", false, "R3: стеля бітрейту для малорухомого вмісту (internal/contentmode.Capper): рух < 25 % екрана -> 25 % цілі (у режимі Video — 50 %), не нижче 1 Мбіт/с; великий рух знімає стелю миттєво. Типово вимкнено. Лише симуляція (bench/quality/lowmotion_run.py), UNVERIFIED на реальному ПК")
	multimonFlag := flag.Bool("multimon", false, "F6: публікувати КОЖЕН монітор окремим потоком одночасно (дочірній процес на монітор, node_id <node>#m<i>; потрібен OO_SCREEN_MULTIMON=1 на хабі). Типово вимкнено; з одним монітором нічого не міняє. UNVERIFIED на реальних ПК")
	multimonMax := flag.Int("multimon-max", multimonMaxDefault, "F6: стеля одночасних потоків разом з основним (кожен = апаратна сесія енкодера)")
	multimonChild := flag.Int("multimon-child", 0, "F6, службовий: цей процес — потік монітора N, запущений батьком -multimon (без звуку/вводу, select_output ігнорує)")
	gopSeconds := flag.Int("gop-seconds", 10, "інтервал періодичного IDR, с (ТЗ 1.4). Довгий GOP = менше важких IDR (див. bench/quality/RESULTS-workloads.md); новий глядач отримує кадр із GOP-кешу хаба (OO_SCREEN_GOP_SPAN, дефолт 12s ≥ GOP, макс 30s) або IDR на keyframe_request/PLI. >11 вимагає на хабі більшого OO_SCREEN_GOP_SPAN")
	consentFlag := flag.String("consent-policy", os.Getenv("OO_SCREEN_CONSENT"), "S3: згода користувача ПК: off (дефолт) | always-ask | ask-if-user-logged-in (питати, якщо сесія не заблокована) | unattended-allowed-by-admin (без запиту, з індикатором). Не-off: агент стартує в паузі й не віддає кадри/ввід до згоди; поки глядач є — topmost-плашка з кнопкою «Завершити сесію»")
	consentTimeout := flag.Duration("consent-timeout", 30*time.Second, "S3: скільки чекати відповіді на запит згоди; мовчання = відмова")
	autoUpdateURL := flag.String("auto-update-url", "", "S6: URL підписаного (ed25519) маніфесту оновлень; порожньо = автооновлення вимкнено (дефолт). Потрібен ключ, зашитий при збірці (-X main.updatePubKey)")
	autoUpdateEvery := flag.Duration("auto-update-interval", 6*time.Hour, "S6: як часто перевіряти маніфест")
	autoUpdateHealth := flag.Duration("auto-update-health-window", 2*time.Minute, "S6: за скільки новий бінарь мусить достукатись до хаба, інакше автоматичний відкат")
	autoUpdateReport := flag.String("auto-update-report-url", "", "O4: куди POST-ити вердикт здоров'я нової версії (ok/fail/inconclusive) для поетапної викатки (oo-rollout serve); порожньо = не звітувати. Токен — env OO_ROLLOUT_REPORT_TOKEN")
	flag.Parse()

	// Прапорці перекривають env з тієї ж причини, що й -token вище: агента
	// запускає Register-ScheduledTask, яка передає ЛИШЕ аргументи, а змінних
	// середовища у задачі нема взагалі. Без цього обидві фічі неможливо
	// увімкнути на бойовому ПК — код був би написаний і недосяжний.
	// Тільки в один бік (прапорець вмикає, не вимикає): -audio=false не мусить
	// гасити те, що людина свідомо ввімкнула через середовище.
	applyFeatureFlags(*audioFlag, *inputFlag)
	if *multimonChild > 0 {
		// F6-дитина: звук, ввід і шар курсора несе лише основний потік.
		audioEnabled, inputEnabled = false, false
		*cursorLayerFlag = false
	}
	textTilesEnabled = *textTilesFlag
	// Шар курсора (cursor.go): лише прапорцем, типово вимкнено. Ставиться ДО
	// першого capture.New — перемикач читається при кожному відкритті капчера.
	if *cursorLayerFlag {
		cursorLayerEnabled = true
		capture.SetCursorLayer(true)
		go runCursorPoller(nil)
	}

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

	// SEC #33: токен — з файлу/env, -token лише для сумісності.
	tok, tokSrc, tokErr := agentcred.ResolveToken(*tokenFlag, *tokenFile, os.Getenv)
	if tokErr != nil {
		log.Fatalf("oo-agent: %v", tokErr)
	}
	if tokSrc == agentcred.SourceFlag {
		log.Printf("oo-agent: WARNING — токен переданий -token і видно в командному рядку процесу; перейди на -token-file (див. README)")
	}
	log.Printf("oo-agent: токен агента з %s", tokSrc)
	cliToken = tok

	if *wtPin != "" {
		pin, perr := agentcred.ParsePin(*wtPin)
		if perr != nil {
			log.Fatalf("oo-agent: %v", perr)
		}
		wtCertPin = pin
	}
	wtInsecure = *wtInsecureFlag
	if wtInsecure && wtCertPin == nil && *transportKind == "wt" {
		log.Printf("oo-agent: WARNING — -wt-insecure: сертифікат hub-wt не перевіряється (лише стенд)")
	}

	nodeID = *node

	if *hubAddr == "" {
		switch *transportKind {
		case "wt":
			*hubAddr = "localhost:4460"
		case "webrtc":
			*hubAddr = "http://127.0.0.1:4470/offer/agent"
		}
	}

	// Реєструється РАНІШЕ за defer release(), отже виконується ПІСЛЯ нього:
	// новий процес не наткнеться на ще зайнятий м'ютекс.
	defer func() {
		if relaunchAfterExit.Load() {
			relaunchSelf()
		}
	}()

	// A-36: другий екземпляр на тому ж ПК рве DXGI-дублікацію першого.
	release, dup := acquireSingleInstance()
	if *multimonChild > 0 {
		release, dup = acquireNamedInstance(`Global\oo-screen-agent-m` + itoa(*multimonChild))
	}
	if dup {
		log.Printf("oo-agent: інший агент уже працює на цьому ПК — виходжу")
		return
	}
	defer release()

	standbyList := *hubStandby
	if *transportKind != "webrtc" && standbyList != "" {
		log.Printf("oo-agent: -hub-standby підтримано лише для webrtc — ігнорую")
		standbyList = ""
	}
	hubSel := newHubSelector(*hubAddr, standbyList, *failoverAfter, httpHealth)

	// A-04: під -H windowsgui os.Stderr не існує — уся капчер-діагностика
	// (recreate failed, access lost) губилась навіть із -log. Один сток.
	logger := slog.New(slog.NewTextHandler(log.Writer(), nil))

	// encode.New нормалізує FPS<=0 до 30 усередині (encode_windows.go:120),
	// але це приватне — зовнішній код (PTS-ділення нижче) про цю нормалізацію
	// не знає. Нормалізуємо прапорець тут, ОДИН раз, і використовуємо
	// effectiveFPS всюди далі (encode.Config і розрахунок PTS), щоб
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
	if *multimonChild > 0 {
		go watchParentStdin(os.Stdin, stop) // F6: батько помер -> виходимо
	}

	// A-39: logoff/shutdown гасять ctx тим самим шляхом, що Ctrl+C; lock/unlock
	// читає кадровий цикл. nil = вікно не піднялось, поведінка як до A-39.
	session := watchSession(stop)

	// S6: щойно встановлене оновлення перевіряємо ПІСЛЯ м'ютекса одного
	// екземпляра і паралельно з роботою агента (health = агент підключився).
	if *multimonChild == 0 {
		go autoUpdateStartup(ctx, *hubAddr, *autoUpdateHealth, stop, *autoUpdateReport, *node)
	}

	if *autoUpdateURL != "" && *multimonChild == 0 {
		if u, uerr := newUpdater(*autoUpdateURL, nodeID); uerr != nil {
			log.Printf("oo-agent: autoupdate вимкнено: %v", uerr)
		} else {
			log.Printf("oo-agent: autoupdate: версія %s, маніфест %s кожні %s", agentVersion, *autoUpdateURL, *autoUpdateEvery)
			go runAutoUpdate(ctx, u, *autoUpdateEvery, stop)
		}
	}

	// A-27: замість Fatalf — чекаємо з бек-офом (див. retryUntil). Але
	// заблокований/захищений робочий стіл (E_ACCESSDENIED на DuplicateOutput)
	// — не привід не йти на хаб: без агента на хабі ПК «зникає з пульта»
	// на весь час локу (Maria, 05.09). Тоді стартуємо БЕЗ капчера: кадровий
	// цикл підніме його через reacquireCapture, щойно зʼявиться глядач.
	var cap_ *capture.Capturer
	if c, cerr := capture.NewWithOptions(outIdx, capture.Options{Logger: logger}); cerr == nil {
		cap_ = c
	} else if errors.Is(cerr, capture.ErrNotAvailable) || errors.Is(cerr, capture.ErrAccessLost) {
		log.Printf("oo-agent: capture.New: %v — стартую без захоплення, підніму при появі глядача", cerr)
	} else {
		var rerr error
		cap_, rerr = retryUntil(ctx, "capture.New", func() (*capture.Capturer, error) {
			return capture.NewWithOptions(outIdx, capture.Options{Logger: logger})
		})
		if rerr != nil {
			return
		}
	}
	// Не чіпаємо readback ДО того, як дізнаємось тип енкодера. Раніше тут стояв
	// SetCPUReadback(false), а після encode.New — умовний SetCPUReadback(true)
	// для софт-шляху. Той toggle false→true (перемикання РЕЖИМУ капчера ПІСЛЯ
	// ініціалізації, ще й у два кроки) — саме «живий стик», де софт-шлях падав.
	// Тепер режим виставляється РІВНО ОДИН раз нижче (stream.applyReadback),
	// коли вже відомо hw vs sw. Капчер за замовчуванням у readback-режимі
	// (NewWithOptions readback=true), тож для софт-шляху це взагалі no-op.

	srcW, srcH := nativeSize(cap_, outIdx)
	// Геометрію беремо ВІД ВИВОДУ, а не з зашитих 1920x1080: на моніторі
	// 2560x1440 енкодер масштабував униз, і текст у таблицях виходив мильнішим,
	// ніж у транспорті, який ми замінюємо. Прапорці лишаються шляхом для
	// слабкого каналу — requestedSize віддає їм пріоритет.
	//
	// У stream кладемо СИРІ прапорці, а не цей результат: після select_output
	// монітор інший, і його рідний розмір має порахуватись заново (openEncoder).
	reqW, reqH := requestedSize(*width, *height, srcW, srcH)
	// Бітрейт мусить іти за пікселями: рідна роздільність при старих 8 Мбіт/с —
	// та сама мильна картинка, лише з іншого боку (на 1440p пікселів у 1.78
	// раза більше при тому самому потоці).
	bitrateBps := defaultBitrate(*bitrate, reqW, reqH)
	log.Printf("oo-agent: capture opened, output %d, native %dx%d -> want %dx%d, bitrate %d bps",
		outIdx, srcW, srcH, reqW, reqH, bitrateBps)

	// Стеля для bitrate_target: hub бере її з поля bitrate в offer (ceilingBps).
	// Ставимо ДО dial — offerReq читає цю змінну.
	//
	// ponytail: при відступі на 1920x1080 (fallbackSize) бітрейт лишається
	// порахованим під рідну — тобто щедрішим, ніж треба. Так СВІДОМО: CBR уже
	// зашитий в енкодер при відкритті, а SetBitrate до першого кадру MFT
	// приймає ненадійно (див. bitrateTarget), і перерахунок лише тут розійшовся
	// б зі стелею, яку ми оголосили хабу. Зайві біти зріже регулятор хаба.
	startBitrateBps = bitrateBps

	s := &stream{
		reqW: *width, reqH: *height, fps: effectiveFPS, gopSeconds: *gopSeconds,
		forceSoftware: *forceSoftware, logger: logger,
		cap: cap_, output: outIdx,
	}
	s.bitrateBps.Store(int64(bitrateBps))
	paceTargetBps.Store(uint64(bitrateBps))
	// nil-guard: на паузі капчер звільнено (releaseCapture), тож на виході з
	// агента, що стався у простої, s.cap уже nil — Close на nil впав би.
	defer func() {
		if s.cap != nil {
			s.cap.Close()
		}
	}() // closure: закриває ПОТОЧНИЙ капчер (SwitchOutput/releaseCapture його міняють)

	if cap_ != nil {
		type encOpen struct {
			enc  *encode.Encoder
			w, h int
			sw   bool
		}
		eo, err := retryUntil(ctx, "encode.New", func() (encOpen, error) {
			e, w, h, sw, err := s.openEncoder(cap_.Device(), cap_.Generation(), srcW, srcH)
			return encOpen{e, w, h, sw}, err
		})
		if err != nil {
			return
		}
		s.enc.Store(eo.enc)
		s.encW, s.encH, s.software = eo.w, eo.h, eo.sw
	}
	defer func() {
		if e := s.encoder(); e != nil {
			e.Close()
		}
	}() // closure: закриває ПОТОЧНИЙ енкодер (nil на паузі)
	if s.cap != nil {
		s.applyReadback()
	}

	// Список моніторів іде в offer (див. offerReq.Outputs) — консолі більше
	// нізвідки його взяти. Оновлюється тут і на кожному перемиканні.
	publishOutputs(outIdx)
	if l := outputs.Load(); l != nil {
		log.Printf("oo-agent: outputs=%d active=%d %+v", len(l.Outputs), l.Active, l.Outputs)
		// F6: решта моніторів — дочірніми потоками. Лише батько, лише webrtc
		// і лише з node_id (без нього хабу нема з чим звʼязати потоки).
		if (*multimonFlag || multimonEnvOn()) && *multimonChild == 0 {
			switch {
			case *transportKind != "webrtc" || nodeID == "":
				log.Printf("oo-agent: multimon потребує -transport=webrtc і -node — вимкнено")
			default:
				kids := multimonChildren(len(l.Outputs), outIdx, *multimonMax)
				if len(kids) > 0 {
					// Рев'ю F6: з дітьми основний потік теж закріплений —
					// інакше select_output перевів би його на монітор, який
					// уже захоплює дитина (дубль + конфлікт DXGI, A-36).
					multimonParentPinned.Store(true)
				}
				startMultimonChildren(ctx, kids, nodeID, *logPath, cliToken)
			}
		}
	}
	// logFirstSoftFrame: одноразове діагностичне логування геометрії CPU-кадру.
	// Краш на Computer (Intel, native 1920x1200, encode 1920x1080) не
	// відтворюється на NVIDIA-боксі, тож коли агент піде на той ПК — ці цифри
	// (розміри, страйди, чи Y/UV не nil) покажуть, ЩО саме приходить у submit,
	// замість голого access violation. Друкуємо рівно раз, щоб не спамити лог.
	var logFirstSoftFrame sync.Once

	// onKeyframeRequest — спільний колбек для обох транспортів: WT читає
	// keyframe_request з control-стріму hub-а, WebRTC отримує RTCP PLI.
	// В обох випадках реакція та сама — примусовий IDR (§5.5).
	// A-13/A-31: колбеки pion не беруть мʼютекс енкодера (Encode тримає його до
	// 500 мс) і не форсують IDR самі — лише піднімають прапорець, який кадровий
	// цикл застосовує з дебаунсом. Шторм PLI від глядачів = один IDR на 300 мс,
	// а не IDR на кожен кадр.
	onKeyframeRequest := func() {
		s.wantIDR.Store(true)
	}

	// on-demand гейтинг: дефолт — НЕ пауза (безпечний фолбек = стара always-on
	// поведінка, якщо hub не шле сигналів). Hub шле "pause" щойно відкриється
	// control-канал і глядача нема, тож без глядача агент іде в паузу за ~мс.
	var gatePaused atomic.Bool
	// gateSeen — «хаб цієї сесії вже сказав своє слово про гейт». Потрібен
	// РІВНО одному місцю: A-28-паузі на час реконекту, яка мусить відрізнити
	// «прапорець стоїть, бо його поставили ми» від «прапорець стоїть, бо так
	// вирішив новий хаб». Без цього відновлення після дозвону затирало б
	// свіжий pause, що прийшов по щойно відкритому контрол-каналу.
	var gateSeen atomic.Bool
	onGate := func(resume bool) {
		gateSeen.Store(true)
		// Лише перемикаємо прапорець — саме звільнення/підняття капчера робить
		// кадровий цикл (releaseCapture/reacquireCapture), бо капчер не
		// thread-safe і його не можна чіпати з цього колбека (інша горутина).
		if resume {
			if gatePaused.CompareAndSwap(true, false) {
				s.wantIDR.Store(true) // новий глядач має отримати IDR негайно
				log.Printf("oo-agent: viewer present — resuming (capture reacquired in frame loop)")
			}
		} else {
			if gatePaused.CompareAndSwap(false, true) {
				log.Printf("oo-agent: no viewer — pausing (capture released in frame loop)")
			}
		}
	}

	// S3: згода користувача ПК. Gate стоїть МІЖ сигналом хаба і gatePaused:
	// resume від хаба лише ПРОСИТЬ, відчиняє — локальне рішення (діалог або
	// політика адміна з прапорця). Без згоди агент стартує на паузі, а ввід
	// відкидає сам (inputAllowed, input.go) — глядачу нічим це обійти.
	consentPolicy, err := consent.ParsePolicy(*consentFlag)
	if err != nil {
		log.Fatalf("oo-agent: %v", err)
	}
	consentGate = consent.New(consent.Config{
		Policy:      consentPolicy,
		UI:          consent.NativeUI{},
		UserPresent: func() bool { return !session.Locked() },
		Timeout:     *consentTimeout,
		Logf:        log.Printf,
	})
	if consentGate.Required() {
		gatePaused.Store(true)
		log.Printf("oo-agent: consent policy=%s — старт у паузі до згоди", consentPolicy)
	}
	onGate = consentGate.Wrap(onGate)

	// applyBitrate — ЄДИНЕ місце, де ціль реально лягає в енкодер. IDR тут
	// БІЛЬШЕ НЕ ФОРСУЄМО (P0 B4/B5): AVEncCommonMeanBitRate — динамічна
	// властивість MFT, CBR-контроль перераховує QP з наступного кадру, тож
	// «GOP догравається старим квантуванням» не відбувається. А IDR — це
	// найбільший кадр, і саме при ЗНИЖЕННІ цілі (канал вузький) він б'є в
	// чергу вузького місця: стенд показав 22-33 IDR/хв під стелею і фризи від
	// них. Якщо глядачу потрібен IDR, хаб шле keyframe_request окремо.
	var (
		lowCap            = contentmode.NewCapper(contentmode.CapConfig{})
		lowCapEnc         *encode.Encoder
		lowCapApplied     int
		applyLowMotionCap func(contentmode.Mode, float64)
	)
	applyBitrate := func(bps int) {
		e := s.encoder()
		if e == nil {
			return // капчер звільнено на паузі — ціль застосується на reacquire
		}
		if err := e.SetBitrate(bps); err != nil {
			log.Printf("oo-agent: SetBitrate(%d): %v", bps, err)
			return
		}
		// Запамʼятовуємо ЖИВУ ціль: SwitchOutput відкриває новий енкодер саме з
		// нею, інакше перемикання монітора мовчки скасовувало б притискання хаба.
		s.bitrateBps.Store(int64(bps))
		paceTargetBps.Store(uint64(bps))
		lowCapApplied = bps // енкодер тепер на новій цілі; стелю перекладе наступний кадр
		log.Printf("oo-agent: bitrate -> %d bps", bps)
	}

	// applyLowMotionCap (-lowmotion-cap, R3) — стеля поверх ЖИВОЇ цілі хаба
	// (s.bitrateBps не чіпаємо: це ціль хаба, на ній відкривається новий
	// енкодер). Лише з кадрового циклу (A-13). Новий енкодер (SwitchOutput,
	// reacquire) відкривається з цілі хаба — тоді вважаємо застосованою її.
	applyLowMotionCap = func(mode contentmode.Mode, area float64) {
		lowCap.Update(mode, area, time.Now())
		e := s.encoder()
		if e == nil {
			return
		}
		target := int(s.bitrateBps.Load())
		if e != lowCapEnc {
			lowCapEnc, lowCapApplied = e, target
		}
		want := lowCap.Bps(target)
		if want == lowCapApplied {
			return
		}
		if err := e.SetBitrate(want); err != nil {
			log.Printf("oo-agent: lowmotion-cap SetBitrate(%d): %v", want, err)
			return
		}
		lowCapApplied = want
		log.Printf("oo-agent: lowmotion-cap -> %d bps (ціль %d, area≈%.3f, mode=%v)", want, target, area, mode)
	}

	// onBitrateTarget — hub просить іншу CBR-ціль. Крутимо ручку на живому
	// енкодері: переоткриття MFT коштувало б зміни епохи (§5.5).
	var bitrateWanted bitrateTarget
	onBitrateTarget := func(want uint64) {
		bps, ok := clampBitrate(want, bitrateBps)
		if !ok {
			return // без валідного bitrate_bps повідомлення ігноруємо
		}
		// «Не готово» = і пауза, і вікно resume, поки капчер ще піднімається
		// (encoder==nil). Хаб шле resume, а ОДРАЗУ за ним resetBitrate, тож ціль
		// регулярно прилітає саме в це вікно: без перевірки на nil-encoder
		// applyBitrate тихо викидав би її на нульовому енкодері, і потік стартував
		// би з дефолтним бітрейтом замість замовленого (знайшов Codex-рев'ю).
		// Відкладена ціль застосується в кадровому циклі одразу після reacquire.
		// A-13: завжди через кадровий цикл (take() перед Encode), ніколи з
		// колбека — SetBitrate бере той самий мʼютекс, що й Encode.
		bitrateWanted.set(bps, true)
		log.Printf("oo-agent: bitrate -> %d bps (застосує кадровий цикл)", bps)
	}

	// onSelectOutput — hub попросив інший монітор (control §select_output; сам
	// запит приходить із консолі ЕРП). Тут лише КЛАДЕМО намір: перемикання
	// капчера робить кадровий цикл, бо capture.Capturer «NOT safe for concurrent
	// use», а ми в горутині DataChannel/QUIC-стріму. Неіснуючий індекс відсіє
	// SwitchOutput проти живої енумерації — залишимось на поточному моніторі.
	onSelectOutput := func(idx int) {
		if *multimonChild > 0 {
			log.Printf("oo-agent: select_output -> %d проігноровано: потік закріплений за монітором %d (F6)", idx, *multimonChild)
			return
		}
		if multimonParentPinned.Load() {
			log.Printf("oo-agent: select_output -> %d проігноровано: -multimon активний, монітори вже публікуються окремими потоками (F6)", idx)
			return
		}
		log.Printf("oo-agent: select_output -> %d (застосує кадровий цикл)", idx)
		s.requestOutput(idx)
	}

	// frameInterval рахуємо ДО dial: webrtcTransport бере його як тривалість
	// першого AU (sampleDuration), і кожен реконект створює транспорт заново.
	frameInterval := time.Second / time.Duration(effectiveFPS)
	// tickFPS/tickInterval — годинник PTS (videomode.go): з -video-mode тікає
	// 1/video-fps, щоб 60 к/с мали власні мітки; інакше = frameInterval.
	tickFPS := videoTickFPS(*videoModeFlag, effectiveFPS, *videoFPS)
	tickInterval := time.Second / time.Duration(tickFPS)

	// pcDown — друга (і головна) причина реконекту поряд із txErrCh: стан
	// PeerConnection. Буфер 1 + неблокуючий запис: причина потрібна одна, а
	// обробник стану pion блокувати не можна.
	pcDown := make(chan string, 1)
	onDown := func(reason string) {
		select {
		case pcDown <- reason:
		default:
		}
	}

	tp, err := retryUntil(ctx, "initial dial "+*hubAddr, func() (transport, error) {
		t, err := dial(*transportKind, hubSel.current(), frameInterval, onKeyframeRequest, onGate, onBitrateTarget, onSelectOutput, onDown)
		if err != nil {
			hubSel.failed(ctx)
			return nil, err
		}
		hubSel.ok()
		*hubAddr = hubSel.current()
		return t, nil
	})
	if err != nil {
		return
	}
	log.Printf("oo-agent: connected via %s to %s", *transportKind, *hubAddr)
	markAgentConnected()

	type sendJob struct {
		au  encode.AU
		seq uint64
	}

	var (
		seq        uint64       // envelope/output frame sequence (wt only; monotonic per AU sent)
		captureSeq uint64       // input frame counter, drives encoder PTS independent of drops/output seq
		queued     atomic.Int64 // # AUs enqueued but not yet sent — admission signal (replaces old per-AU inFlight bool)
		dropped    atomic.Int64
		sent       atomic.Int64
		lastLog    = time.Now()
		keepalives int // скільки разів переслали останній кадр (нерухомий екран)
		refines    int // скільки refine-кадрів закодовано (ТЗ P4)
		// refiner — автомат refine (internal/refine): коли рух стих, ще раз
		// кодуємо останній кадр із нижчим QP. Лише апаратний D3D-шлях.
		refiner = refine.New(refine.Config{MinGap: frameInterval})
		// Gap #2: сигнал «текстовий режим» з площі dirty/move rects. Споживач —
		// стеля FPS (-text-fps, textfps.go): у текстовому режимі кодуємо не
		// частіше за textModeGap; затриманий кадр дошлемо, щойно щілина
		// відкриється (textPending).
		// Один автомат Text / Normal / Video (internal/contentmode); без
		// -video-mode це рівно колишній textmode-детектор.
		contentDet      = contentmode.New(contentmode.Config{NoVideo: !*videoModeFlag})
		contentMode     contentmode.Mode
		contentCtlAt    time.Time
		contentCtlSeq   uint64
		encSecEWMA      float64
		textOn          bool
		textPending     bool
		textThrottled   int
		noChangeSkipped int64
		refineErrLog    sync.Once
		throttled       int // скільки кадрів викинув бюджет CPU софт-енкодера
		// ТЗ P8: адаптивна стеля FPS софт-енкодера за ЗАМІРЯНИМ часом Encode
		// (internal/swlimit). Статичний softwareFrameGap — апріорна оцінка
		// за ядрами; swPol доганяє реальність конкретного ПК. Перебудовується
		// на кожен новий енкодер (зміна виводу/геометрії).
		swPol    *swlimit.Policy
		swPolEnc *encode.Encoder
		// s.lastFrame — останній захоплений кадр, джерело keepalive. Тримаємо
		// саме вказівник капчера, без копії: у zero-copy режимі це його
		// персистентна Blt-текстура, у CPU-режимі — його ж scratch-буфери
		// (capture_windows.go: "overwritten by the next NextFrame" / "the
		// returned frame aliases them"). Поки нового кадру нема, там лежить
		// рівно останній — тобто те, що нам і треба переслати. Живе в stream, а
		// не тут, бо перемикання монітора ЗОБОВʼЯЗАНЕ його скинути разом із
		// капчером, чиї буфери він аліасить.
		// lastSeqAt — стінний час кадру, від якого рахуємо зсув PTS. Не час
		// виклику NextFrame: між кадрами ще є кодування й відправка. Ставимо
		// перед самим циклом — рукостискання транспорту до потоку не належить.
		lastSeqAt time.Time
		// lastAdmitAt — стінний час ОСТАННЬОГО кадру, який admission пропустив
		// далі. Окремо від lastSeqAt: той рухається і на дропнутих кадрах (PTS
		// іде за стінним годинником), а межу паузи треба міряти саме по тому,
		// що дійсно поїхало в транспорт.
		lastAdmitAt time.Time
		txErrCh     = make(chan error, 1)
		tpMu        sync.Mutex // guards tp across reconnects; the single ordered sender goroutine reads it under this lock
		sendQueue   = make(chan sendJob, 8)
	)

	// onContentMode — новий стан автомата Text / Normal / Video. textOn —
	// рівно «режим Text» (у Video текстова стеля не діє ніколи); з
	// -video-mode зміна режиму (і повтор у Video) іде hub-у content_mode.
	onContentMode := func(mode contentmode.Mode, flipped bool) {
		contentMode = mode
		textOn = mode == contentmode.Text
		if flipped {
			c, m := contentDet.TextDetector().Smoothed()
			log.Printf("oo-agent: content mode=%v (changed≈%.3f moved≈%.3f, no-op skipped=%d, text-throttled=%d, enc≈%.1fms)",
				mode, c, m, noChangeSkipped, textThrottled, encSecEWMA*1e3)
		}
		if !videoCtlDue(*videoModeFlag, flipped, mode, time.Since(contentCtlAt)) {
			return
		}
		contentCtlAt = time.Now()
		contentCtlSeq++
		tpMu.Lock()
		cur := tp
		tpMu.Unlock()
		if err := sendContentMode(cur, contentCtlSeq, mode); err != nil && flipped {
			log.Printf("oo-agent: content_mode %v not sent: %v", mode, err)
		}
	}

	// Один ordered sender: усі AU (у т.ч. кілька з одного enc.Encode виклику,
	// напр. IDR+trailing delta AU з тієї самої кодованої картинки) ідуть через
	// ЦЕЙ канал і відправляються СТРОГО послідовно однією горутиною. Раніше
	// кожен AU спамив власну goroutine.send — конкурентні send() на той самий
	// стрім могли інтерлівитись/переставлятись місцями, і кожна goroutine
	// незалежно скидала спільний inFlight, ламаючи admission-контроль.
	go func() {
		for job := range sendQueue {
			tpMu.Lock()
			cur := tp
			tpMu.Unlock()
			if err := cur.send(job.au, job.seq); err != nil {
				select {
				case txErrCh <- err:
				default:
				}
			} else {
				sent.Add(1)
			}
			queued.Add(-1)
		}
	}()

	// Звук — ОКРЕМА горутина, бо джерело в нього своє (WASAPI, ~10мс пакети) і
	// зупиняти через нього кадровий цикл нема за що. Транспорт бере ту саму
	// змінну під tpMu, що й відео-sender вище: після реконекту звук піде в
	// НОВУ доріжку без жодного власного механізму перепідключення.
	// Гейт — той самий gatePaused, що керує відео (див. runAudio).
	if audioEnabled && *transportKind == "webrtc" {
		go runAudio(ctx, &gatePaused, func(data []byte, dur time.Duration) error {
			tpMu.Lock()
			cur := tp
			tpMu.Unlock()
			return cur.sendAudio(data, dur)
		})
	}

	sendAsync := func(au encode.AU) {
		mySeq := seq
		seq++
		queued.Add(1)
		sendQueue <- sendJob{au: au, seq: mySeq}
	}

	// A-03: поки екрана нема (лок-скрін, UAC, капчер відновлює дублікацію),
	// шлемо повтор останнього keepalive-AU — «нічого не змінилось» P-кадру.
	// Декодер глядача копіює референс, сторож у браузері бачить свіжий кадр і
	// не рве сесію на кожному UAC. AU дійсний лише для ТОГО енкодера, що його
	// видав (інша геометрія/SPS = сміття), тому памʼятаємо й енкодер.
	var (
		lastStillAU     *encode.AU
		lastStillEnc    *encode.Encoder
		lastStillSentAt time.Time
		reacqBackoff    = reacquireBackoffMin
		suspended       bool      // A-17: дублікацію віддано на паузі
		lastIDRAt       time.Time // A-31: дебаунс IDR за запитом
	)
	sendStillKeepalive := func() {
		if lastStillAU == nil || gatePaused.Load() || time.Since(lastStillSentAt) < keepaliveAfter {
			return
		}
		if e := s.encoder(); e != nil && e != lastStillEnc {
			return
		}
		now := time.Now()
		captureSeq += seqAdvance(now.Sub(lastSeqAt), tickInterval)
		lastSeqAt = now
		au := *lastStillAU
		au.PTS = time.Duration(captureSeq) * time.Second / time.Duration(tickFPS)
		tilesStill()
		sendAsync(au)
		lastStillSentAt = now
		keepalives++
	}

	reconnect := func() {
		// 🚨 A-28. Реконект — це НЕ мить: хаб може лежати хвилинами, і всі ці
		// хвилини dial крутиться з бек-офом. До цієї зміни агент увесь той час
		// тримав DXGI-дублікацію відкритою (кадровий цикл стоїть тут, але
		// капчер живий), а звукова горутина — своя, вона не стоїть — кодувала
		// й слала пакети в мертвий транспорт. Дублікація в простої це та сама
		// регресія 01.09, від якої рятує гейт: MeshCentral на цьому ж ПК не
		// може захопити екран і рве свою desktop-сесію.
		//
		// Тому на час дозвону ставимо ТОЙ САМИЙ gatePaused, що й гейт хаба
		// (його читає і звук), і віддаємо дублікацію через Suspend (A-17) —
		// девайс і MFT лишаються, resume коштує один DuplicateOutput.
		//
		// Знімається прапорець лише якщо його поставили МИ і новий хаб за час
		// дозвону нічого про гейт не сказав — див. reconnectGate.restore.
		rg := beginReconnectGate(&gatePaused, &gateSeen)
		// S3: згода не переживає реконект (на тому боці може бути інший
		// глядач), тож і паузу знімає лише новий resume через Gate.
		consentGate.Reset()
		if !consentGate.Required() {
			defer rg.restore()
		}
		if s.cap != nil && !suspended {
			s.cap.Suspend()
			suspended = true
			log.Printf("oo-agent: reconnect — desktop duplication released (екран вільний для Mesh)")
		}
		backoff := reconnectBackoffMin
		for {
			if ctx.Err() != nil {
				return
			}
			tpMu.Lock()
			tp.close()
			tpMu.Unlock()

			newTp, err := dial(*transportKind, hubSel.current(), frameInterval, onKeyframeRequest, onGate, onBitrateTarget, onSelectOutput, onDown)
			if err != nil {
				if hubSel.failed(ctx) {
					backoff = reconnectBackoffMin // новий хаб здоровий — пробуємо одразу з короткою витримкою
				}
				wait := jitterBackoff(backoff)
				log.Printf("oo-agent: reconnect failed, retry in %s: %v", wait.Round(time.Millisecond), err)
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
				backoff = nextBackoff(backoff)
				continue
			}
			hubSel.ok()
			*hubAddr = hubSel.current()
			tpMu.Lock()
			tp = newTp
			tpMu.Unlock()
			// Власне close() старого PeerConnection теж дає "closed", а невдала
			// спроба dial — свій. Ці сигнали вже неактуальні: гасимо, інакше
			// наступний прохід циклу переподключався б поверх щойно піднятої сесії.
			select {
			case <-pcDown:
			default:
			}
			// A-32: у sendQueue лежать P-кадри старої сесії; новий хаб без
			// референсу їх не декодує, а декодер глядача — тим паче.
			for drained := false; !drained; {
				select {
				case <-sendQueue:
					queued.Add(-1)
				default:
					drained = true
				}
			}
			// Reference frames on the other side are gone: force a fresh IDR
			// so the new session decodes from scratch (§5.5). Капчер може бути
			// звільнений (реконект стався на паузі) — тоді IDR дасть reacquire.
			if e := s.encoder(); e != nil {
				if err := e.ForceIDR(); err != nil {
					log.Printf("oo-agent: ForceIDR after reconnect: %v", err)
				}
			}
			log.Printf("oo-agent: reconnected via %s to %s", *transportKind, *hubAddr)
			return
		}
	}

	log.Printf("oo-agent: streaming %s -> %s (fps=%d bitrate=%d)", *transportKind, *hubAddr, effectiveFPS, bitrateBps)
	lastSeqAt = time.Now()
	lastAdmitAt = lastSeqAt

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err := <-txErrCh:
			log.Printf("oo-agent: transport error, reconnecting: %v", err)
			reconnect()
			continue
		case reason := <-pcDown:
			// 🚨 Саме цієї гілки бракувало 30.08 01:11. txErrCh нижче нічого не
			// ловить, поки агент на паузі: без глядача він не шле — отже й не
			// помиляється. Стан PeerConnection видно й на паузі.
			log.Printf("oo-agent: %s, reconnecting", reason)
			reconnect()
			continue
		default:
		}

		// 🚨 Друга (і єдина, що працює на паузі) причина реконекту: хаб
		// замовк. Гілка pcDown вище тут безсила за побудовою — стан
		// PeerConnection замерзає на "connected", бо агент, який нічого не шле,
		// не заводить consent-таймер (див. hubLiveness). Перевірка стоїть саме
		// тут, ДО гейта: на паузі цикл обертається кожні 50 мс, тобто сторож
		// живий рівно тоді, коли він потрібен.
		if hubLive.silent(time.Now()) {
			log.Printf("oo-agent: хаб мовчить довше за %s — рвемо і перепідключаємось", control.HeartbeatTimeout)
			reconnect()
			continue
		}

		// on-demand: без глядача не захоплюємо й не кодуємо — і ЗВІЛЬНЯЄМО
		// капчер, щоб не тримати DXGI-дублікацію виводу (інакше MeshCentral на
		// цьому ж ПК не може захопити екран і рве свою desktop-сесію — регресія
		// 01.09). PeerConnection і RTCP/DataChannel-читачі лишаються живими, тож
		// щойно hub пришле "resume", наступний прохід підніме капчер заново.
		if gatePaused.Load() {
			// A-17: віддаємо лише DXGI-дублікацію (саме вона заважає Mesh), а
			// девайс і MFT живуть далі: resume = один DuplicateOutput, а не
			// повний MFShutdown/MFStartup + новий D3D-девайс на кожен вхід глядача.
			if s.cap != nil && !suspended {
				s.cap.Suspend()
				suspended = true
				log.Printf("oo-agent: no viewer — suspended desktop duplication (екран вільний для Mesh)")
			}
			select {
			case <-ctx.Done():
				break loop
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		// Глядач зʼявився, а капчер було звільнено на паузі — піднімаємо заново
		// (той самий вивід, свіжий енкодер+IDR). Невдача — коротка пауза й
		// повтор, потік не рветься назавжди.
		// A-39: на лок-скріні DuplicateOutput приречений (E_ACCESSDENIED), і
		// кожна спроба — це ще й новий D3D-девайс на порожньому місці. Поки
		// сесія заблокована, не пробуємо взагалі: сесію тримає keepalive, а
		// unlock зніме паузу негайно (нижче).
		if session.Locked() && s.encoder() == nil {
			sendStillKeepalive()
			select {
			case <-ctx.Done():
				break loop
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
		// Розблокували — пробуємо ЗАРАЗ, а не через залишок бек-офу, який міг
		// дорости до keepaliveAfter поки екран був замкнений.
		if session.takeUnlocked() {
			reacqBackoff = reacquireBackoffMin
		}
		if s.encoder() == nil {
			if err := s.reacquireCapture(); err != nil {
				wait := jitterBackoff(reacqBackoff)
				log.Printf("oo-agent: reacquire capture failed: %v (retry in %s)", err, wait.Round(time.Millisecond))
				sendStillKeepalive()
				select {
				case <-ctx.Done():
					break loop
				case <-time.After(wait):
				}
				// Стеля = keepaliveAfter: довша пауза лишила б сторож без кадру.
				if reacqBackoff *= 2; reacqBackoff > keepaliveAfter {
					reacqBackoff = keepaliveAfter
				}
				continue
			}
			reacqBackoff = reacquireBackoffMin
			log.Printf("oo-agent: viewer present — reacquired screen capture")
		}

		suspended = false

		// Ціль, що прийшла на паузі, застосовується тут — на першому кадрі
		// після відновлення, поки в MFT знову йдуть кадри.
		if bps := bitrateWanted.take(); bps != 0 {
			applyBitrate(bps)
		}
		// A-31: IDR за запитом — лише звідси, з дебаунсом.
		if s.wantIDR.Swap(false) && time.Since(lastIDRAt) >= idrDebounce {
			if err := s.encoder().ForceIDR(); err != nil {
				log.Printf("oo-agent: ForceIDR (request): %v", err)
			}
			lastIDRAt = time.Now()
		}

		// Перемикання монітора — РІВНО ТУТ, у кадровому циклі, і ніде більше:
		// капчер не є thread-safe, а між NextFrame і Encode його міняти не
		// можна взагалі (кадр аліасить його буфери). Запит прийшов із іншої
		// горутини через requestOutput; помилка (неіснуючий індекс, зайнятий
		// вихід) лишає нас на поточному моніторі — потік не рветься.
		if idx, ok := s.pending.take(); ok {
			if err := s.SwitchOutput(idx); err != nil {
				log.Printf("oo-agent: switch output -> %d: %v", idx, err)
			}
		}

		// capture.NextFrame сама крутиться на DXGI-таймаутах і на нерухомому
		// екрані не повернеться НІКОЛИ (її ж коментар: "Give it a deadline if
		// you need 'no news' reported back"). Дедлайн — єдиний спосіб дізнатись,
		// що екран стоїть, а не що ми ще чекаємо.
		//
		// Refine (ТЗ P4) лише вкорочує цей дедлайн: якщо рух стих, прокидаємось
		// у мить, коли час refine, а не через повний keepaliveAfter.
		refineOn := *refineFlag && !s.software && !gatePaused.Load() && s.lastFrame != nil
		if !refineOn {
			refiner.Disarm()
		}
		waitFor := refiner.Wait(time.Now(), keepaliveAfter)
		refineWait := waitFor < keepaliveAfter
		vIn := contentmode.FPSInput{BaseFPS: s.fps, VideoFPS: *videoFPS, Hardware: !s.software, EncSec: encSecEWMA}
		textGap := max(textModeGap(*textFPS, s.fps, textOn), videoModeGap(*videoModeFlag, contentMode, vIn))
		waitFor, textWait := textFlushWait(textPending, textGap, time.Since(lastAdmitAt), waitFor)
		if textWait {
			refineWait = false // прокинулись заради дошлення, не заради refine
		}
		waitCtx, cancelWait := context.WithTimeout(ctx, waitFor)
		frame, err := s.cap.NextFrame(waitCtx)
		cancelWait()
		// Шар курсора: позицію/форму DXGI віддає з КОЖНИМ кадром, і саме
		// NoChange-кадри (рух миші без змін картинки) тут найчастіші.
		if err == nil {
			observeCursor(frame, s.cap, s.output)
		}
		// A-01: капчер міг пережити ACCESS_LOST і жити вже на іншому девайсі.
		if dev, gen := s.cap.Device(), s.cap.Generation(); encoderStale(dev, gen, s.encDev, s.encGen) {
			s.lastFrame = nil // аліасив буфери/текстуру старого девайса
			if dev == 0 {
				// Дублікацію втрачено, капчер ще відновлює її (лок/UAC):
				// кадру нема, сесію тримає повтор keepalive (A-03).
				sendStillKeepalive()
				continue
			}
			if serr := s.syncEncoderToCapture(); serr != nil {
				log.Printf("oo-agent: encoder rebuild after device change failed: %v — releasing capture", serr)
				s.releaseCapture()
				continue
			}
		}
		if *videoModeFlag && (err != nil || frame.NoChange) {
			onContentMode(contentDet.Tick(time.Now()))
		}
		still := false
		refineQP := 0
		textFlush := false
		textCF := 1.0 // частка змінених пікселів ЦЬОГО кадру (для textCapApplies)
		switch {
		case (err == nil && frame.NoChange || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() == nil &&
			textFlushDue(textPending, gatePaused.Load(), s.lastFrame != nil, textGap, time.Since(lastAdmitAt)):
			// Текстова стеля затримала зміст: шлемо останній кадр як ЗВИЧАЙНИЙ
			// (не still — це не «нічого не змінилось», його не можна кешувати
			// як keepalive-AU).
			frame = s.lastFrame
			textFlush = true
		case err == nil && frame.NoChange:
			// Gap #2: DXGI віддав кадр без dirty/move rects і без руху курсору —
			// картинка та сама. НЕ кодуємо і НЕ вважаємо рухом: таймер refine
			// має йти далі. lastFrame не чіпаємо (у no-op кадру нема площин).
			// Але такий кадр «з'їв» дедлайн очікування, тож refine/keepalive,
			// що вже назріли, обслуговуємо тут само.
			noChangeSkipped++
			if s.lastFrame == nil || gatePaused.Load() {
				continue
			}
			if qp, due := refiner.Due(time.Now()); due && queued.Load() == 0 {
				frame = s.lastFrame
				still = true
				refineQP = qp
			} else if time.Since(lastAdmitAt) >= keepaliveAfter {
				frame = s.lastFrame
				keepalives++
				still = true
			} else {
				continue
			}
		case err == nil:
			s.lastFrame = frame
			refiner.Motion(time.Now()) // новий кадр = рух: refine, що йшов, перериваємо
			// Текстові тайли: invalidate ДО кодування цього кадру (tiles.go).
			tilesMotion()
			textCF = capture.ChangedFraction(frame)
			mode, flipped := contentDet.Update(
				textCF,
				textmode.Fraction(frame.MoveArea, frame.Width, frame.Height), time.Now())
			onContentMode(mode, flipped)
			if *lowMotionCapFlag {
				applyLowMotionCap(mode, contentmode.Area(textCF,
					textmode.Fraction(frame.MoveArea, frame.Width, frame.Height)))
			}
		case ctx.Err() != nil:
			break loop // зупиняють агента, а не просто екран стоїть
		case refineWait && errors.Is(err, context.DeadlineExceeded):
			// Дедлайн вкоротив refine, а не keepalive: keepalive тут не шлемо.
			qp, due := refiner.Due(time.Now())
			switch refineWakeAction(due, gatePaused.Load(), s.lastFrame != nil, queued.Load(), time.Since(lastAdmitAt)) {
			case refineWakeSkip:
				continue
			case refineWakePostpone:
				// Канал ще не відправив попереднє: refine не має права його
				// топити (бюджет ≤ пікового бітрейту) — відкладаємо на кадр.
				refiner.Postpone(time.Now(), frameInterval)
				continue
			case refineWakePostponeKeepalive:
				// Refine відкладено, але keepalive уже назрів — як на
				// baseline-дедлайні: шлемо його (admission вирішить далі).
				refiner.Postpone(time.Now(), frameInterval)
				frame = s.lastFrame
				keepalives++
				still = true
			case refineWakeKeepalive:
				frame = s.lastFrame
				keepalives++
				still = true
			case refineWakeRefine:
				frame = s.lastFrame
				still = true
				refineQP = qp
			}
		case shouldKeepalive(err, gatePaused.Load(), s.lastFrame != nil):
			// Екран нерухомий: пересилаємо ОСТАННІЙ кадр. Декодер отримує
			// крихітний P-кадр «нічого не змінилось», сторож у браузері бачить
			// свіжий кадр і не рве сесію.
			frame = s.lastFrame
			keepalives++
			still = true
		case shouldRearm(err, gatePaused.Load(), s.lastFrame != nil):
			// Стартували на вже нерухомому екрані (або щойно перемкнули монітор
			// на нерухомий — SwitchOutput скидає lastFrame саме в цей стан):
			// беремо поточний робочий стіл через GDI, бо DXGI на такому екрані
			// не віддасть нічого.
			gdi, gerr := s.cap.GDIFrame()
			if gerr != nil {
				log.Printf("oo-agent: capture.GDIFrame: %v (retrying)", gerr)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			log.Printf("oo-agent: перший кадр знято через GDI (екран нерухомий)")
			frame, s.lastFrame = gdi, gdi
			// Перший кадр — теж «рух»: без цього refine (і тайли за ним) на
			// сесії, що стартувала на нерухомому екрані, чекали б першої зміни.
			refiner.Motion(time.Now())
			tilesMotion() // інша картинка (новий монітор/реакваєр) — тайли застаріли
		case errors.Is(err, context.DeadlineExceeded):
			// Дедлайн був, але слати не можна: глядача нема (пауза). Порожній
			// кадр тут не вигадуємо — декодеру нема з чого будувати картинку,
			// а сторожа ми б обдурили.
			continue
		case errors.Is(err, capture.ErrClosed), errors.Is(err, capture.ErrAccessLost), errors.Is(err, capture.ErrNotAvailable):
			// A-02: капчер закрив себе на OOS_ERROR або здався відновлювати
			// дублікацію (лок-скрін/RDP). Раніше цикл крутив ErrClosed 10/с
			// навічно. Звільняємо; reacquire вище підніме заново з бек-офом, а
			// до того сесію тримає keepalive (A-03).
			log.Printf("oo-agent: capture unavailable: %v — releasing, will reacquire", err)
			s.releaseCapture()
			sendStillKeepalive()
			continue
		default:
			log.Printf("oo-agent: capture.NextFrame: %v (retrying)", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// PTS іде за СТІННИМ годинником, а не за лічильником кадрів: скільки
		// кадр справді чекали, на стільки й зсуваємо. Тут же, до admission —
		// щоб і викинутий кадр не лишав RTP позаду стінного часу.
		now := time.Now()
		captureSeq += seqAdvance(now.Sub(lastSeqAt), tickInterval)
		lastSeqAt = now

		// A-08: рух миші без змін на столі (DXGI: LastPresentTime==0) — не
		// привід кодувати 60 повних кадрів/с. Курсор скомпоновано в кадр, тож
		// зовсім пропускати не можна — тримаємо ≤15 к/с.
		if frame.MouseOnly && !still && now.Sub(lastAdmitAt) < mouseOnlyGap {
			continue
		}

		// Admission (§5.5): якщо відправка ПОПЕРЕДНЬОГО кадру ще блокує
		// транспорт — дропаємо ЩОЙНО ЗАХОПЛЕНИЙ кадр ДО кодування. Референсні
		// кадри енкодера лишаються цілі (ми нічого йому не подавали), тому
		// IDR після дропу не потрібен. Верхню межу паузи, яку ці дропи мають
		// право створити, тримає admissionFloor — інакше дроп зʼїдав і
		// keepalive-кадр, і сесія гинула на сторожі браузера.
		// Бюджет CPU софтверного енкодера: зайві кадри викидаємо ДО кодування,
		// у тому самому місці й з тією ж логікою, що admission нижче — саме
		// тут кадр іще нічого не коштував. -force-software не чіпаємо: це
		// свідомий вибір оператора, а не машина, яка не тягне.
		if s.software && !s.forceSoftware {
			if swPolEnc != s.encoder() {
				swPolEnc = s.encoder()
				swPol = swlimit.New(swlimit.Config{
					MaxFPS: softwareFPSCap(runtime.NumCPU(), s.encW*s.encH, s.fps),
					Cores:  runtime.NumCPU(),
					// Софт-шлях не масштабує: щабель Scale лише «вдавав» би падіння
					// навантаження (ewma×area) і затискав би відновлення FPS.
					Scales: []swlimit.Scale{{Num: 1, Den: 1}},
				})
			}
			gap := softwareFrameGap(runtime.NumCPU(), s.encW*s.encH, s.fps)
			if g := swPol.FrameGap(); g > gap {
				gap = g
			}
			if gap > 0 && now.Sub(lastAdmitAt) < gap {
				throttled++
				continue
			}
		}

		// Gap #2: текстова стеля. Стоїть ПІСЛЯ софт-бюджету — обидва мусять
		// пропустити, тож ефективна стеля = min. still (keepalive/refine) і
		// дошлення затриманого не чіпаємо. Вихід із текстового режиму вже
		// обнулив textGap на цьому ж кадрі (textOn оновлено вище).
		// -video-mode: кадри вмісту не частіше contentmode.FPS(режим) — той
		// самий механізм затримки/дошлення, що й текстова стеля.
		vIn = contentmode.FPSInput{BaseFPS: s.fps, VideoFPS: *videoFPS, Hardware: !s.software, EncSec: encSecEWMA}
		if g := max(textModeGap(*textFPS, s.fps, textCapApplies(textOn, textCF)), videoModeGap(*videoModeFlag, contentMode, vIn)); g > 0 && !still && !textFlush && now.Sub(lastAdmitAt) < g {
			textPending = true
			textThrottled++
			continue
		}

		if !shouldAdmit(queued.Load(), now.Sub(lastAdmitAt)) {
			dropped.Add(1)
			if time.Since(lastLog) >= 5*time.Second {
				log.Printf("oo-agent: dropped %d frames in last %s (transport busy), sent=%d",
					dropped.Load(), time.Since(lastLog).Round(time.Millisecond), sent.Load())
				dropped.Store(0)
				lastLog = time.Now()
			}
			continue
		}
		lastAdmitAt = now
		textPending = false

		encFrame := encode.Frame{
			PTS: time.Duration(captureSeq) * time.Second / time.Duration(tickFPS),
		}
		if s.software {
			encFrame.Y = frame.Y
			encFrame.UV = frame.UV
			encFrame.YStride = frame.YStride
			encFrame.UVStride = frame.UVStride
			logFirstSoftFrame.Do(func() {
				log.Printf("oo-agent: first software frame: cap %dx%d enc %dx%d | Y!=nil=%v len(Y)=%d YStride=%d | UV!=nil=%v len(UV)=%d UVStride=%d | cursor(vis=%v comp=%v shape=%v)",
					frame.Width, frame.Height, s.encW, s.encH,
					frame.Y != nil, len(frame.Y), frame.YStride,
					frame.UV != nil, len(frame.UV), frame.UVStride,
					frame.CursorVisible, frame.CursorComposited, frame.CursorShape)
			})
			// Захист: якщо капчер віддав кадр без CPU-площин (напр. режим
			// readback раптом злетів), НЕ передаємо nil у C — там memcpy(nil)
			// = access violation. Логуємо і пропускаємо кадр замість краху.
			if frame.Y == nil || frame.UV == nil {
				log.Printf("oo-agent: software frame with nil planes (Y!=nil=%v UV!=nil=%v) — skipping (no CPU readback?)",
					frame.Y != nil, frame.UV != nil)
				continue
			}
		} else {
			encFrame.Texture = frame.Texture
			// A-06: без покоління енкодер лишив би закешовану input-view на
			// СТАРІЙ текстурі, якщо перебудований капчер отримав ту саму адресу.
			encFrame.TextureGen = frame.TextureGen
		}
		if refineQP > 0 {
			if rerr := s.encoder().SetRefineQP(refineQP); rerr != nil {
				// MaxQP відмовлено — per-sample QP усе одно стоїть; логуємо раз.
				refineErrLog.Do(func() { log.Printf("oo-agent: refine: %v", rerr) })
			}
		} else if refiner.NeedRestore() {
			if rerr := s.encoder().SetRefineQP(0); rerr != nil {
				refineErrLog.Do(func() { log.Printf("oo-agent: refine restore: %v", rerr) })
			}
		}
		encStart := time.Now()
		aus, err := s.encoder().Encode(encFrame)
		if err == nil && !still {
			encSecEWMA = encEWMA(encSecEWMA, time.Since(encStart))
		}
		if swPol != nil && swPolEnc == s.encoder() && s.software && err == nil {
			if d := swPol.Observe(time.Since(encStart), time.Now()); d.Changed {
				// Роздільність софт-шлях не масштабує (submit_cpu ріже, а не
				// скейлить — encodeGeometry), тож крок Scale поки лише радить.
				log.Printf("oo-agent: swlimit %s -> fps cap %d, advised scale %d/%d (not applied: software path encodes native) enc=%dx%d cores=%d",
					d.Reason, d.FPS, d.Scale.Num, d.Scale.Den, s.encW, s.encH, runtime.NumCPU())
			}
		}
		if refineQP > 0 {
			n := 0
			for _, au := range aus {
				n += len(au.Data)
			}
			// Бюджет: наступний refine — не раніше, ніж канал на піку
			// (MaxBitRate = 1,5 x mean, mft.c peak_bps) проковтне цей.
			refiner.Sent(time.Now(), n, int(s.bitrateBps.Load())/2*3)
			refines++
		}
		if err != nil {
			log.Printf("oo-agent: encode.Encode: %v", err)
			// A-12: «MFT event wait timeout» — енкодер завис; перебудова через
			// той самий шлях, що й для ACCESS_LOST.
			if strings.Contains(err.Error(), "wedged") {
				s.releaseCapture()
			}
			continue
		}
		if still {
			tilesStill() // анонс ДО кадру: плеєр не сховає тайли на цьому повторі
		}
		for _, au := range aus {
			sendAsync(au)
		}
		// Refine-AU не кешуємо як keepalive: його залишок поверх іншого
		// референсу зіпсував би картинку.
		if still && refineQP == 0 && len(aus) == 1 && !aus[0].Keyframe {
			cp := aus[0]
			lastStillAU, lastStillEnc, lastStillSentAt = &cp, s.encoder(), time.Now()
		}
		// Текстові тайли: екран нерухомий і refine уже доведений до кінця
		// (або, без refine — софт-енкодер, -refine=false, — простій
		// tilesIdleAfter) — один readback BGRA на епоху (повтор на keepalive,
		// якщо епізод не вдалося почати; Episodes.Start ідемпотентний у
		// межах епохи). Тайли під вказівником не шлемо.
		if still && tilesReady(time.Now(), refineOn, refiner.Complete()) {
			cv, cx, cy := false, 0, 0
			if lf := s.lastFrame; lf != nil {
				cv, cx, cy = lf.CursorVisible, lf.CursorX, lf.CursorY
			}
			tilesStatic(ctx, time.Now(), s.cap.ReadBGRA, tilesCursorExclude(cv, cx, cy))
		}

		if time.Since(lastLog) >= 5*time.Second {
			log.Printf("oo-agent: sent=%d dropped=%d keepalives=%d refines=%d throttled=%d", sent.Load(), dropped.Load(), keepalives, refines, throttled)
			dropped.Store(0)
			lastLog = time.Now()
		}
	}

	log.Printf("oo-agent: shutting down (sent=%d dropped=%d keepalives=%d throttled=%d)", sent.Load(), dropped.Load(), keepalives, throttled)
	tpMu.Lock()
	tp.close()
	tpMu.Unlock()
}
