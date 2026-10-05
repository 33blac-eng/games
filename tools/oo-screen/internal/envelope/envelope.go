// Package envelope — байтовий конверт кадру для транспорту B (WebTransport).
// Формат заморожено в плані (Додаток D, R3#9). Little-endian, магія 'OOSC'.
//
//	| 0  | 4   | magic 'OOSC'                                  |
//	| 4  | 1   | version=1 (невідома → закрити сесію з кодом)  |
//	| 5  | 1   | flags: bit0=keyframe, bit1=config-змінено     |
//	| 6  | 2   | config_epoch                                  |
//	| 8  | 8   | frame_seq (монотонний)                        |
//	| 16 | 8   | pts, мкс від старту сесії (timebase 1МГц)     |
//	| 24 | 4   | payload_len (max 8МіБ; більше → відхилити)    |
//	| 28 | len | H.264 Annex-B повний AU                       |
package envelope

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Magic          = "OOSC"
	Version        = 1
	HeaderSize     = 28
	MaxPayloadLen  = 8 << 20 // 8 МіБ
	FlagKeyframe   = 1 << 0
	FlagConfigured = 1 << 1 // config-змінено
)

var (
	ErrBadMagic       = errors.New("envelope: bad magic")
	ErrBadVersion     = errors.New("envelope: unknown version")
	ErrPayloadTooBig  = errors.New("envelope: payload exceeds 8MiB")
	ErrShortHeader    = errors.New("envelope: short header")
)

type Frame struct {
	Flags       uint8
	ConfigEpoch uint16
	FrameSeq    uint64
	PTS         uint64 // мкс від старту сесії
	Payload     []byte // повний Annex-B AU
}

func (f *Frame) Keyframe() bool      { return f.Flags&FlagKeyframe != 0 }
func (f *Frame) ConfigChanged() bool { return f.Flags&FlagConfigured != 0 }

// Marshal серіалізує кадр (заголовок + payload) у новий буфер.
func (f *Frame) Marshal() ([]byte, error) {
	if len(f.Payload) > MaxPayloadLen {
		return nil, ErrPayloadTooBig
	}
	buf := make([]byte, HeaderSize+len(f.Payload))
	copy(buf[0:4], Magic)
	buf[4] = Version
	buf[5] = f.Flags
	binary.LittleEndian.PutUint16(buf[6:8], f.ConfigEpoch)
	binary.LittleEndian.PutUint64(buf[8:16], f.FrameSeq)
	binary.LittleEndian.PutUint64(buf[16:24], f.PTS)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(len(f.Payload)))
	copy(buf[HeaderSize:], f.Payload)
	return buf, nil
}

// ReadFrame читає рівно один кадр зі стріму (length-prefixed, один стрім на epoch).
// Помилки формату — фатальні для сесії за контрактом.
func ReadFrame(r io.Reader) (*Frame, error) {
	hdr := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrShortHeader
		}
		return nil, err // io.EOF на межі кадру = чистий кінець стріму
	}
	if string(hdr[0:4]) != Magic {
		return nil, ErrBadMagic
	}
	if hdr[4] != Version {
		return nil, fmt.Errorf("%w: %d", ErrBadVersion, hdr[4])
	}
	plen := binary.LittleEndian.Uint32(hdr[24:28])
	if plen > MaxPayloadLen {
		return nil, ErrPayloadTooBig
	}
	f := &Frame{
		Flags:       hdr[5],
		ConfigEpoch: binary.LittleEndian.Uint16(hdr[6:8]),
		FrameSeq:    binary.LittleEndian.Uint64(hdr[8:16]),
		PTS:         binary.LittleEndian.Uint64(hdr[16:24]),
		Payload:     make([]byte, plen),
	}
	if _, err := io.ReadFull(r, f.Payload); err != nil {
		return nil, fmt.Errorf("envelope: short payload: %w", err)
	}
	return f, nil
}
