// Package control реалізує версійований line-delimited JSON control-протокол.
// Ним ходить прод-нога кандидата A — DataChannel "oosc-ctl" agent<->hub
// (heartbeat, keyframe_request, bitrate_target, max_fps, select_output,
// fallback_reason, shutdown) — і обидві ноги легасі-кандидата B: agent<->hub
// (raw QUIC, перший стрім з'єднання) і browser<->hub (WebTransport
// bidi-стрім). Hub — авторитетний транслятор між ними: control-повідомлення
// самі по собі МІЖ агентом і браузером напряму не ходять.
//
// Framing: один JSON-об'єкт на рядок, термінований '\n'. Кожне повідомлення
// несе "v" (версія протоколу) і "type". Невідома версія — фатальна помилка
// (несумісний протокол). Невідомий "type" — НЕ помилка: повідомлення
// ігнорується з логом, з'єднання лишається живим (форвардна сумісність).
package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"
)

// Version — поточна версія протоколу.
const Version = 1

// Пульс і поріг тиші — ОДНЕ число на обидві сторони, тут, а не по копії в
// кожному бінарі: агент рахує таймаут рівно від того інтервалу, з яким б'є
// хаб, і роз'їхатись їм нема як.
//
// HeartbeatInterval — з якою частотою жива сторона шле control.Heartbeat.
// 5 с — те, з чим уже живе нога кандидата B (hub-wt, corpus-player-wt).
const HeartbeatInterval = 5 * time.Second

// HeartbeatTimeout — скільки тиші означає «той бік мертвий». П'ять пропусків.
//
// Низ межі: сторож НЕ сміє рвати живе з'єднання на брижах мережі. Канал
// надійний (SCTP ordered/reliable), тож удар не губиться, а ЗАТРИМУЄТЬСЯ
// ретрансмісією — запас у кілька ударів тут не розкіш.
//
// Верх межі: перезапуск хаба мусить лікуватись за ДЕСЯТКИ секунд, не хвилини.
// 25 с виявлення + 10 с на рукостискання нового дзвінка = 35 с, тобто менше за
// ті ~45 с, за які 30.08 поверталися агенти, чий PeerConnection розрив таки
// побачив. Сторож не стає найповільнішою ланкою.
const HeartbeatTimeout = 5 * HeartbeatInterval

// Типи control-повідомлень.
const (
	TypeHello           = "hello"
	TypeDecoderReady    = "decoder_ready"
	TypeKeyframeRequest = "keyframe_request"
	TypeEpochChange     = "epoch_change"
	TypeHeartbeat       = "heartbeat"
	TypeBitrateTarget   = "bitrate_target"
	TypeSelectOutput    = "select_output"
	TypeMaxFps          = "max_fps"
	TypeFallbackReason  = "fallback_reason"
	TypeShutdown        = "shutdown"
	TypeAck             = "ack"
	// TypeContentMode — агент -> hub: режим вмісту (contentmode: "video",
	// "text", "normal"). У "video" hub може підняти ціль бітрейту, але лише в
	// межах власної стелі ноди і поки мережа чиста (hub/cmd/hub-webrtc).
	TypeContentMode = "content_mode"
	// TypeEncStats — агент -> hub (C2, OO_SCREEN_ENC_TELEMETRY): ім'я MFT,
	// звіт CODECAPI-властивостей (EncCaps, ParseEncCaps) і QP кадрів з потоку
	// за вікно (QPLast/QPMin/QPMax; 0 — невідомо). Лише телеметрія.
	TypeEncStats = "enc_stats"
)

var knownTypes = map[string]bool{
	TypeHello:           true,
	TypeDecoderReady:    true,
	TypeKeyframeRequest: true,
	TypeEpochChange:     true,
	TypeHeartbeat:       true,
	TypeBitrateTarget:   true,
	TypeSelectOutput:    true,
	TypeMaxFps:          true,
	TypeFallbackReason:  true,
	TypeShutdown:        true,
	TypeAck:             true,
	TypeContentMode:     true,
	TypeEncStats:        true,
}

// IsKnownType повідомляє, чи цей пакет розпізнає даний тип повідомлення.
func IsKnownType(t string) bool { return knownTypes[t] }

// Msg — єдиний конверт для всіх типів control-повідомлень. Поля, не потрібні
// конкретному типу, лишаються нульовими і не серіалізуються (omitempty).
type Msg struct {
	V          int    `json:"v"`
	Type       string `json:"type"`
	Seq        uint64 `json:"seq"`
	Token      string `json:"token,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Epoch      uint64 `json:"epoch,omitempty"`
	BitrateBps uint64 `json:"bitrate_bps,omitempty"`
	// Output — індекс DXGI-виходу для select_output.
	//
	// omitempty тут не губить сенсу, хоч вихід 0 і законний: відсутнє поле
	// розбирається назад у 0, тобто рівно в той самий «основний монітор». Це
	// свідомо, а не випадково — окремого «не задано» цьому типу не потрібно:
	// сам type і є наміром, а індекс без значення = основний вихід.
	Output int `json:"output,omitempty"`
	// Fps — стеля кадрів/с для max_fps (1..60). Нуля хаб не шле: «зняти стелю»
	// — це Fps = верхній межі, агент однаково бере min(свій -fps, Fps).
	Fps int `json:"fps,omitempty"`
	// Mode — режим вмісту для content_mode ("video" | "text" | "normal").
	Mode string `json:"mode,omitempty"`

	// enc_stats (C2).
	Encoder  string `json:"encoder,omitempty"`
	Software bool   `json:"software,omitempty"`
	EncCaps  string `json:"enc_caps,omitempty"`
	QPLast   int    `json:"qp_last,omitempty"`
	QPMin    int    `json:"qp_min,omitempty"`
	QPMax    int    `json:"qp_max,omitempty"`
	QPFrames int    `json:"qp_frames,omitempty"`
}

// --- Конструктори по одному на тип повідомлення ---

func Hello(token string, seq uint64) Msg {
	return Msg{V: Version, Type: TypeHello, Seq: seq, Token: token}
}

func DecoderReady(seq uint64) Msg {
	return Msg{V: Version, Type: TypeDecoderReady, Seq: seq}
}

func KeyframeRequest(seq uint64) Msg {
	return Msg{V: Version, Type: TypeKeyframeRequest, Seq: seq}
}

func EpochChange(seq, epoch uint64) Msg {
	return Msg{V: Version, Type: TypeEpochChange, Seq: seq, Epoch: epoch}
}

func Heartbeat(seq uint64) Msg {
	return Msg{V: Version, Type: TypeHeartbeat, Seq: seq}
}

func BitrateTarget(seq, bitrateBps uint64) Msg {
	return Msg{V: Version, Type: TypeBitrateTarget, Seq: seq, BitrateBps: bitrateBps}
}

// SelectOutput — «перемкни захоплення на вихід output». Іде тим самим
// control-каналом, що й bitrate_target: hub -> агент. Зворотної відповіді немає
// навмисно — успіх видно глядачеві по самій картинці (нова геометрія + IDR), а
// відмову (неіснуючий індекс) агент лише пише в лог і лишається на поточному
// моніторі, бо рвати живий потік через невдалий клік гірше, ніж його не змінити.
func SelectOutput(seq uint64, output int) Msg {
	return Msg{V: Version, Type: TypeSelectOutput, Seq: seq, Output: output}
}

// MaxFps — «не захоплюй/не кодуй частіше за fps кадрів/с» (стеля з тулбара
// глядача, контракт C1). hub -> агент тим самим "oosc-ctl". Старий агент такого
// типу не знає й ігнорує — форвардна сумісність, як і для всього іншого тут.
func MaxFps(seq uint64, fps int) Msg {
	return Msg{V: Version, Type: TypeMaxFps, Seq: seq, Fps: fps}
}

func FallbackReason(seq uint64, reason string) Msg {
	return Msg{V: Version, Type: TypeFallbackReason, Seq: seq, Reason: reason}
}

func Shutdown(seq uint64, reason string) Msg {
	return Msg{V: Version, Type: TypeShutdown, Seq: seq, Reason: reason}
}

// ContentMode — агент повідомляє hub про зміну режиму вмісту (-video-mode).
func ContentMode(seq uint64, mode string) Msg {
	return Msg{V: Version, Type: TypeContentMode, Seq: seq, Mode: mode}
}

func Ack(seq uint64) Msg {
	return Msg{V: Version, Type: TypeAck, Seq: seq}
}

// ErrUnsupportedVersion — фатальна помилка протоколу: "v" відсутня або не
// збігається з Version цієї збірки. На відміну від невідомого "type", це
// НЕ ігнорується — виклик повинен закрити з'єднання.
type ErrUnsupportedVersion struct{ Got int }

func (e *ErrUnsupportedVersion) Error() string {
	return fmt.Sprintf("control: unsupported protocol version %d (expected %d)", e.Got, Version)
}

// Write серіалізує m у JSON, дописує framing-термінатор '\n' і пише в w
// одним викликом Write (важливо для QUIC-стрімів: часткові Write можуть
// інтерлівитись між конкурентними writer'ами того самого стріму).
func Write(w io.Writer, m Msg) error {
	if m.V == 0 {
		m.V = Version
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("control: marshal: %w", err)
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// MaxLine — стеля одного control-рядка. Найдовше живе повідомлення — shutdown
// з причиною, сотні байтів; 64 КБ — запас на порядки.
const MaxLine = 64 << 10

// Read читає рівно один рядок з br, розбирає JSON і валідує версію.
// Невідома версія повертається як *ErrUnsupportedVersion (фатально для
// виклику). Невідомий "type" НЕ є помилкою тут — Msg повертається як є;
// рішення ігнорувати його — за ReadKnown або за самим викликом.
func Read(br *bufio.Reader) (Msg, error) {
	// Рядок читається зі стелею MaxLine: ReadString росте без меж, і пірінг,
	// що не шле '\n', тримав би в пам'яті скільки завгодно байтів.
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxLine {
			return Msg{}, fmt.Errorf("control: line longer than %d bytes", MaxLine)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			// EOF (or any read error) без завершального '\n' — запис неповний,
			// навіть якщо те, що встигло прийти, саме по собі є валідним JSON.
			// Приймати такий "хвіст" небезпечно: пірінг міг обірватись
			// посередині наступного байта. Відкидаємо як помилку читання.
			return Msg{}, err
		}
		break
	}
	var m Msg
	if uerr := json.Unmarshal([]byte(line), &m); uerr != nil {
		return Msg{}, fmt.Errorf("control: bad json: %w", uerr)
	}
	if m.V != Version {
		return Msg{}, &ErrUnsupportedVersion{Got: m.V}
	}
	return m, nil
}

// ReadKnown читає повідомлення через Read і прозоро пропускає невідомі типи
// (логуючи кожен пропуск через logf, або log.Printf якщо logf == nil), доки
// не прийде відоме повідомлення або не станеться помилка/EOF.
func ReadKnown(br *bufio.Reader, logf func(format string, args ...any)) (Msg, error) {
	if logf == nil {
		logf = log.Printf
	}
	for {
		m, err := Read(br)
		if err != nil {
			return Msg{}, err
		}
		if !IsKnownType(m.Type) {
			logf("control: unknown message type %q (seq=%d), ignoring", m.Type, m.Seq)
			continue
		}
		return m, nil
	}
}
