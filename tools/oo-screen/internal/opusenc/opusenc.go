// Package opusenc — спільне для агента й хаба: Opus 48 кГц стерео (F1,
// TZ-GENERAL.md §4), вибір кодека звуку і запасний PCMU.
//
// ЕНКОДЕР — github.com/thesyncim/gopus: ЧИСТИЙ Go, без cgo і без жодної
// транзитивної залежності. Ціна, через яку раніше обрали μ-law
// (internal/pcmu: «libopus на Windows-машині збірки немає»), зникла:
// GOOS=windows go build ./... збирається так само, як і раніше, без mingw.
//
// ВИБІР КОДЕКА: OO_SCREEN_AUDIO_CODEC. Типово (порожньо) — "opus". Значення
// "pcmu" вмикає старий G.711 8 кГц моно як запасний варіант — РІВНО той
// тракт, що був до F1. Сам звук і далі вмикається лише OO_SCREEN_AUDIO=1,
// тобто без нього нічого з цього пакета не виконується.
package opusenc

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/thesyncim/gopus"

	"github.com/organicoils/oo-screen/internal/pcmu"
)

const (
	// Rate — частота Opus в RTP (RFC 7587: завжди 48000).
	Rate = 48000
	// Channels — стерео.
	Channels = 2
	// FrameSamples — семплів НА КАНАЛ у кадрі 20 мс.
	FrameSamples = Rate / 50
	// FrameDuration — тривалість одного кадру.
	FrameDuration = 20 * time.Millisecond
	// DefaultBitrate — 128 кбіт/с на стерео: прозоро для музики в CELT.
	DefaultBitrate = 128000
	// PayloadType — динамічний PT, той самий, що в Chrome за замовчуванням.
	PayloadType = 111
	// Fmtp — stereo=1/sprop-stereo=1: шлемо й хочемо стерео; useinbandfec=1 —
	// LBRR (працює лише в SILK/Hybrid, CELT його ігнорує).
	Fmtp = "minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1"
)

// Codec — який кодек звуку в цьому процесі.
type Codec string

const (
	CodecOpus Codec = "opus"
	CodecPCMU Codec = "pcmu"
)

// FromEnv читає OO_SCREEN_AUDIO_CODEC. Невідоме значення — помилка (разом з
// opus): оператор, що помилився в назві, мусить про це дізнатись з логу.
func FromEnv() (Codec, error) { return Parse(os.Getenv("OO_SCREEN_AUDIO_CODEC")) }

// Parse — див. FromEnv.
func Parse(s string) (Codec, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "opus":
		return CodecOpus, nil
	case "pcmu", "g711", "ulaw":
		return CodecPCMU, nil
	}
	return CodecOpus, fmt.Errorf("OO_SCREEN_AUDIO_CODEC=%q: невідомий кодек (opus|pcmu), беру opus", s)
}

// Capability — опис доріжки для pion.
func (c Codec) Capability() webrtc.RTPCodecCapability {
	if c == CodecPCMU {
		return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: pcmu.Rate, Channels: 1}
	}
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: Rate, Channels: Channels, SDPFmtpLine: Fmtp}
}

// Parameters — те, що реєструється в MediaEngine.
func (c Codec) Parameters() webrtc.RTPCodecParameters {
	pt := webrtc.PayloadType(PayloadType)
	if c == CodecPCMU {
		pt = 0 // статичне призначення RFC 3551
	}
	return webrtc.RTPCodecParameters{RTPCodecCapability: c.Capability(), PayloadType: pt}
}

// Matches — чи цей MimeType належить кодеку.
func (c Codec) Matches(mime string) bool {
	return strings.EqualFold(mime, c.Capability().MimeType)
}

// FrameDur — тривалість кадру цього кодека.
func (c Codec) FrameDur(frame []byte) time.Duration {
	if c == CodecPCMU {
		return pcmu.Duration(len(frame))
	}
	if d := PacketDuration(frame); d > 0 {
		return d
	}
	return FrameDuration
}

// Encoder — Opus 48 кГц стерео, кадри по 20 мс, float32 interleaved.
type Encoder struct {
	enc *gopus.Encoder
	buf []byte
}

// NewEncoder створює енкодер. bitrate<=0 — DefaultBitrate.
func NewEncoder(bitrate int) (*Encoder, error) {
	e, err := gopus.NewEncoder(gopus.EncoderConfig{SampleRate: Rate, Channels: Channels, Application: gopus.ApplicationAudio})
	if err != nil {
		return nil, err
	}
	if bitrate <= 0 {
		bitrate = DefaultBitrate
	}
	if err := e.SetBitrate(bitrate); err != nil {
		return nil, err
	}
	// Складність 5: заміряна ціна — bench у opusenc_test.go і рядок F1 у
	// TZ-GENERAL.md; 10 дає ледь чутну різницю ціною помітно більшого CPU.
	if err := e.SetComplexity(5); err != nil {
		return nil, err
	}
	e.SetFEC(true)
	_ = e.SetPacketLoss(5)
	return &Encoder{enc: e, buf: make([]byte, 1500)}, nil
}

// Encode кодує рівно один кадр (FrameSamples*Channels float32). Повертає
// НОВИЙ зріз — викликач може його тримати.
func (e *Encoder) Encode(pcm []float32) ([]byte, error) {
	if len(pcm) != FrameSamples*Channels {
		return nil, fmt.Errorf("opusenc: кадр %d семплів, треба %d", len(pcm), FrameSamples*Channels)
	}
	n, err := e.enc.Encode(pcm, e.buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), e.buf[:n]...), nil
}

// Lookahead — затримка енкодера в семплах 48 кГц (pre-skip для OpusHead).
func (e *Encoder) Lookahead() int { return e.enc.Lookahead() }

// Decoder — для тестів і бенча (round-trip); у бойовому тракті декодує браузер.
type Decoder struct{ dec *gopus.Decoder }

// NewDecoder — 48 кГц стерео.
func NewDecoder() (*Decoder, error) {
	d, err := gopus.NewDecoder(gopus.DefaultDecoderConfig(Rate, Channels))
	if err != nil {
		return nil, err
	}
	return &Decoder{dec: d}, nil
}

// Decode — у pcm має вистачити місця на 120 мс стерео (5760*2). Повертає
// семплів на канал.
func (d *Decoder) Decode(pkt []byte, pcm []float32) (int, error) { return d.dec.Decode(pkt, pcm) }

// PacketDuration рахує тривалість пакета з TOC (RFC 6716 §3.1). 0 — пакет
// порожній або битий.
func PacketDuration(p []byte) time.Duration {
	if len(p) == 0 {
		return 0
	}
	toc := p[0]
	cfg := toc >> 3
	var per time.Duration
	switch {
	case cfg < 12: // SILK: 10/20/40/60
		per = [4]time.Duration{10, 20, 40, 60}[cfg&3] * time.Millisecond
	case cfg < 16: // Hybrid: 10/20
		per = [2]time.Duration{10, 20}[cfg&1] * time.Millisecond
	default: // CELT: 2.5/5/10/20
		per = [4]time.Duration{2500, 5000, 10000, 20000}[cfg&3] * time.Microsecond
	}
	n := 1
	switch toc & 3 {
	case 1, 2:
		n = 2
	case 3:
		if len(p) < 2 {
			return 0
		}
		n = int(p[1] & 0x3f)
	}
	return per * time.Duration(n)
}

// OpusHead — CodecPrivate для Matroska A_OPUS (RFC 7845 §5.1).
func OpusHead(preSkip int) []byte {
	h := []byte("OpusHead")
	h = append(h, 1, Channels, byte(preSkip), byte(preSkip>>8))
	h = append(h, byte(Rate&0xff), byte(Rate>>8&0xff), byte(Rate>>16&0xff), 0)
	h = append(h, 0, 0, 0) // output gain 0, mapping family 0
	return h
}
