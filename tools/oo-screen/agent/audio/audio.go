// Package audio captures the default Windows render endpoint through WASAPI
// loopback and presents its packets at 48 kHz.
package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

var (
	// ErrNoDevice means Windows has no default render endpoint. Audio is an
	// optional feature, so callers may use errors.Is to disable it cleanly.
	ErrNoDevice = errors.New("audio: no default render device")

	// ErrNotAvailable means WASAPI loopback could not be initialized.
	ErrNotAvailable = errors.New("audio: WASAPI loopback unavailable")

	// ErrClosed is returned by NextFrame after Close.
	ErrClosed = errors.New("audio: closed")
)

// SampleFormat identifies the sample representation in Frame.Data.
type SampleFormat uint8

const (
	SampleFormatUnknown SampleFormat = iota
	SampleFormatPCM
	SampleFormatFloat
)

func (f SampleFormat) String() string {
	switch f {
	case SampleFormatPCM:
		return "pcm"
	case SampleFormatFloat:
		return "float"
	default:
		return "unknown"
	}
}

// Format describes the interleaved audio returned in Frame.Data. SampleRate is
// always 48000. The device mix format determines every other field.
type Format struct {
	SampleRate         int
	Channels           int
	SampleFormat       SampleFormat
	BitsPerSample      int
	ValidBitsPerSample int
	ChannelMask        uint32
	BytesPerFrame      int
}

// Frame is one WASAPI packet. Data contains interleaved samples and is owned by
// the caller. Silence is a real frame with zero-filled Data and RMS == 0; a
// stalled source returns no frame and leaves NextFrame blocked.
type Frame struct {
	Data      []byte
	Frames    int
	Format    Format
	Timestamp time.Time
	RMS       float64
}

type nativeFrame struct {
	data           []byte
	frames         int
	format         Format
	qpc100ns       uint64
	timestampValid bool
}

type nativeSource interface {
	read(timeout time.Duration) (nativeFrame, error)
	format() Format
	clock100ns() uint64
	close() error
}

// Capturer owns one WASAPI loopback client. NextFrame calls must be serialized;
// like agent/capture.Capturer, Close must not race with NextFrame.
type Capturer struct {
	mu     sync.Mutex
	source nativeSource
	closed bool

	formatMu sync.RWMutex
	current  Format

	clockAnchor time.Time
	qpcAnchor   uint64
}

// New opens the default render endpoint for WASAPI loopback capture.
func New() (*Capturer, error) {
	return openWith(newNativeSource)
}

func openWith(open func() (nativeSource, error)) (*Capturer, error) {
	source, err := open()
	if err != nil {
		return nil, fmt.Errorf("audio: open loopback: %w", err)
	}

	// WASAPI packet timestamps and Go's monotonic time both use the Windows
	// performance counter. Bracket time.Now so the midpoint maps that QPC
	// domain onto the same monotonic-bearing time.Time used by screen capture.
	qpcBefore := source.clock100ns()
	anchor := time.Now()
	qpcAfter := source.clock100ns()
	qpcMid := qpcBefore + (qpcAfter-qpcBefore)/2

	return &Capturer{
		source:      source,
		current:     source.format(),
		clockAnchor: anchor,
		qpcAnchor:   qpcMid,
	}, nil
}

// Format reports the format most recently negotiated with the default device.
// It may change after a default-render-device notification and recreation.
func (c *Capturer) Format() Format {
	c.formatMu.RLock()
	defer c.formatMu.RUnlock()
	return c.current
}

// NextFrame blocks until a WASAPI packet is available, ctx is done, or the
// source fails. Default-device changes are handled inside the native capture
// thread; the first packet from the recreated client carries its new Format.
func (c *Capturer) NextFrame(ctx context.Context) (*Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}

	const poll = 50 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		native, err := c.source.read(poll)
		if errors.Is(err, errNativeTimeout) {
			continue
		}
		if err != nil {
			return nil, err
		}

		c.formatMu.Lock()
		c.current = native.format
		c.formatMu.Unlock()

		qpc := native.qpc100ns
		if !native.timestampValid {
			qpc = c.source.clock100ns()
		}
		return &Frame{
			Data:      native.data,
			Frames:    native.frames,
			Format:    native.format,
			Timestamp: c.timestamp(qpc),
			RMS:       rmsLevel(native.data, native.format),
		}, nil
	}
}

func (c *Capturer) timestamp(qpc100ns uint64) time.Time {
	delta := int64(qpc100ns - c.qpcAnchor)
	if qpc100ns < c.qpcAnchor {
		delta = -int64(c.qpcAnchor - qpc100ns)
	}
	return c.clockAnchor.Add(time.Duration(delta) * 100 * time.Nanosecond)
}

// Close releases the loopback client and its notification subscription.
// It is idempotent.
func (c *Capturer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.source.close()
}

var errNativeTimeout = errors.New("audio: native read timeout")

func rmsLevel(data []byte, format Format) float64 {
	bytesPerSample := format.BitsPerSample / 8
	if bytesPerSample == 0 || len(data) < bytesPerSample {
		return 0
	}

	var sum float64
	var count int
	for offset := 0; offset+bytesPerSample <= len(data); offset += bytesPerSample {
		var sample float64
		switch format.SampleFormat {
		case SampleFormatPCM:
			sample = pcmSample(data[offset:offset+bytesPerSample], format)
		case SampleFormatFloat:
			switch format.BitsPerSample {
			case 32:
				sample = float64(math.Float32frombits(binary.LittleEndian.Uint32(data[offset:])))
			case 64:
				sample = math.Float64frombits(binary.LittleEndian.Uint64(data[offset:]))
			default:
				return 0
			}
		default:
			return 0
		}
		if math.IsNaN(sample) || math.IsInf(sample, 0) {
			continue
		}
		sum += sample * sample
		count++
	}
	if count == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(count))
}

func pcmSample(sample []byte, format Format) float64 {
	bits := format.BitsPerSample
	valid := format.ValidBitsPerSample
	if valid <= 0 || valid > bits {
		valid = bits
	}
	if bits == 8 {
		return float64(int(sample[0])-128) / 128
	}

	var raw int64
	switch bits {
	case 16:
		raw = int64(int16(binary.LittleEndian.Uint16(sample)))
	case 24:
		raw = int64(sample[0]) | int64(sample[1])<<8 | int64(sample[2])<<16
		if raw&0x800000 != 0 {
			raw |= ^int64(0xffffff)
		}
	case 32:
		raw = int64(int32(binary.LittleEndian.Uint32(sample)))
	default:
		return 0
	}
	if shift := bits - valid; shift > 0 {
		raw >>= shift
	}
	return float64(raw) / math.Ldexp(1, valid-1)
}
