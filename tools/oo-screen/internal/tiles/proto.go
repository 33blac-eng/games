// Package tiles — lossless text-tile refinement for a static screen
// (bench/quality/STAGE3-444.md, recommendation B).
//
// The video stays 4:2:0 H.264 Main. When the screen has been still long
// enough for the refine pass to finish, the agent reads the BGRA desktop back
// once, picks 64x64 tiles that look like coloured text (where 4:2:0 visibly
// smears chroma), encodes them losslessly (PNG) and sends them on the
// "oosc-tiles" DataChannel. The browser draws them over the <video>. Any
// change of the screen bumps the epoch and sends an invalidate; the browser
// drops every tile of an older epoch.
//
// This file is the wire format, shared by agent, hub and (mirrored in JS)
// the player. Pure Go, no build tags.
package tiles

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ChannelLabel — DataChannel label on both legs (agent->hub, hub->viewer).
const ChannelLabel = "oosc-tiles"

// Wire layout, little-endian, HeaderSize bytes followed by Len payload bytes:
//
//	0  u8  magic0 'O'
//	1  u8  magic1 'T'
//	2  u8  version (1)
//	3  u8  type (TypeTile / TypeInvalidate)
//	4  u32 epoch     — bumps on every screen change; older tiles are stale
//	8  u32 frame     — agent content generation the tile was taken from
//	12 u16 x, 14 u16 y, 16 u16 w, 18 u16 h   — tile rect, source pixels
//	20 u16 srcW, 22 u16 srcH                 — captured desktop size
//	24 u8  format (FormatPNG), 25..27 reserved (0)
//	28 u32 payload length
const (
	HeaderSize = 32
	Version    = 1

	TypeTile       = 1
	TypeInvalidate = 2
	// TypeStill — "the next video frame is a still repeat (keepalive) of the
	// episode's picture". Sent on the tiles channel just BEFORE the agent
	// hands such a frame to the video track; the player counts these as
	// credits and hides the tiles when a presented video frame has no credit
	// (= a changed frame that outran its invalidate). No payload, header as
	// TypeInvalidate. Old players ignore unknown types; old hubs drop it.
	//
	// Why not an RTP timestamp in the header: the agent writes samples via
	// pion TrackLocalStaticSample (random initial RTP timestamp, not exposed)
	// and the hub rebases timestamps per node leg (lastOutTS), so neither end
	// can name "the RTP ts of the frame the tiles were read from" as the
	// browser sees it. Keepalive repeats get fresh timestamps too, so a pure
	// "newer ts => stale" rule would clear tiles every second.
	TypeStill = 3

	FormatNone = 0
	FormatPNG  = 1 // RGB(A) PNG, decoded by createImageBitmap in the browser

	// MaxMessage — hard cap of one DataChannel message (header+payload).
	// 64 KiB is the default max-message-size pion advertises and every
	// browser accepts without fragmentation games.
	MaxMessage = 64 * 1024
	MaxPayload = MaxMessage - HeaderSize

	// MaxTileSide — biggest tile edge accepted on the wire.
	MaxTileSide = 256

	// MaxSrcSide — biggest captured-desktop edge accepted on the wire. The
	// player sizes its overlay canvas to SrcW x SrcH, so without a cap one
	// message could ask for a 65535x65535 canvas (~17 GB RGBA).
	MaxSrcSide = 16384
)

// Msg is one decoded message.
type Msg struct {
	Type       uint8
	Epoch      uint32
	Frame      uint32
	X, Y, W, H uint16
	SrcW, SrcH uint16
	Format     uint8
	Payload    []byte
}

var (
	ErrShort    = errors.New("tiles: message shorter than header")
	ErrMagic    = errors.New("tiles: bad magic/version")
	ErrTooLarge = errors.New("tiles: message exceeds MaxMessage")
	ErrLength   = errors.New("tiles: payload length mismatch")
	ErrInvalid  = errors.New("tiles: invalid field")
)

func (m *Msg) validate() error {
	if len(m.Payload) > MaxPayload {
		return ErrTooLarge
	}
	switch m.Type {
	case TypeInvalidate, TypeStill:
		if len(m.Payload) != 0 || m.Format != FormatNone {
			return fmt.Errorf("%w: invalidate/still carries payload", ErrInvalid)
		}
	case TypeTile:
		if m.Format != FormatPNG || len(m.Payload) == 0 {
			return fmt.Errorf("%w: tile format/payload", ErrInvalid)
		}
		if m.W == 0 || m.H == 0 || m.W > MaxTileSide || m.H > MaxTileSide {
			return fmt.Errorf("%w: tile size %dx%d", ErrInvalid, m.W, m.H)
		}
		if m.SrcW > MaxSrcSide || m.SrcH > MaxSrcSide {
			return fmt.Errorf("%w: source %dx%d", ErrInvalid, m.SrcW, m.SrcH)
		}
		if m.SrcW == 0 || m.SrcH == 0 ||
			uint32(m.X)+uint32(m.W) > uint32(m.SrcW) || uint32(m.Y)+uint32(m.H) > uint32(m.SrcH) {
			return fmt.Errorf("%w: tile %d,%d %dx%d outside %dx%d", ErrInvalid, m.X, m.Y, m.W, m.H, m.SrcW, m.SrcH)
		}
		if err := checkPNGHeader(m.Payload, m.W, m.H); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: type %d", ErrInvalid, m.Type)
	}
	return nil
}

// Encode serialises m. It refuses anything Decode would refuse.
func Encode(m *Msg) ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	b := make([]byte, HeaderSize+len(m.Payload))
	b[0], b[1], b[2], b[3] = 'O', 'T', Version, m.Type
	le := binary.LittleEndian
	le.PutUint32(b[4:], m.Epoch)
	le.PutUint32(b[8:], m.Frame)
	le.PutUint16(b[12:], m.X)
	le.PutUint16(b[14:], m.Y)
	le.PutUint16(b[16:], m.W)
	le.PutUint16(b[18:], m.H)
	le.PutUint16(b[20:], m.SrcW)
	le.PutUint16(b[22:], m.SrcH)
	b[24] = m.Format
	le.PutUint32(b[28:], uint32(len(m.Payload)))
	copy(b[HeaderSize:], m.Payload)
	return b, nil
}

// Invalidate builds the invalidate message for a new epoch.
func Invalidate(epoch, frame uint32) []byte {
	b, _ := Encode(&Msg{Type: TypeInvalidate, Epoch: epoch, Frame: frame})
	return b
}

// Still builds the still-repeat announcement for epoch (see TypeStill).
func Still(epoch, frame uint32) []byte {
	b, _ := Encode(&Msg{Type: TypeStill, Epoch: epoch, Frame: frame})
	return b
}

// Decode parses and validates b. Payload aliases b.
func Decode(b []byte) (*Msg, error) {
	if len(b) > MaxMessage {
		return nil, ErrTooLarge
	}
	if len(b) < HeaderSize {
		return nil, ErrShort
	}
	if b[0] != 'O' || b[1] != 'T' || b[2] != Version {
		return nil, ErrMagic
	}
	le := binary.LittleEndian
	n := le.Uint32(b[28:])
	if uint64(n) != uint64(len(b)-HeaderSize) {
		return nil, ErrLength
	}
	if b[25] != 0 || b[26] != 0 || b[27] != 0 {
		return nil, fmt.Errorf("%w: reserved bytes", ErrInvalid)
	}
	m := &Msg{
		Type: b[3], Epoch: le.Uint32(b[4:]), Frame: le.Uint32(b[8:]),
		X: le.Uint16(b[12:]), Y: le.Uint16(b[14:]), W: le.Uint16(b[16:]), H: le.Uint16(b[18:]),
		SrcW: le.Uint16(b[20:]), SrcH: le.Uint16(b[22:]),
		Format: b[24], Payload: b[HeaderSize:],
	}
	if len(m.Payload) == 0 {
		m.Payload = nil
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// pngMinLen is the PNG signature plus a complete IHDR chunk (8 + 4 len +
// 4 type + 13 data); fewer bytes cannot be a usable PNG.
const pngMinLen = 8 + 8 + 13

var pngSig = [8]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// checkPNGHeader requires the PNG's own IHDR size to equal the header's W x H
// (mirror of cursorproto.checkPNGHeader). Without it a 45-byte message could
// declare a 64x64 tile whose IHDR says 65535x65535, and the browser's
// createImageBitmap would allocate for the IHDR size (decompression bomb).
func checkPNGHeader(d []byte, w, h uint16) error {
	if len(d) < pngMinLen || [8]byte(d[:8]) != pngSig ||
		binary.BigEndian.Uint32(d[8:]) != 13 || string(d[12:16]) != "IHDR" {
		return fmt.Errorf("%w: payload is not a PNG", ErrInvalid)
	}
	if binary.BigEndian.Uint32(d[16:]) != uint32(w) || binary.BigEndian.Uint32(d[20:]) != uint32(h) {
		return fmt.Errorf("%w: PNG IHDR size differs from tile %dx%d", ErrInvalid, w, h)
	}
	return nil
}
