// Package cursorproto is the binary wire format of the cursor layer: the
// remote mouse pointer travels as its own shape + position stream instead of
// being composited into the video (agent -cursor-layer, player
// config.cursorLayer). Same idea as Parsec / Chrome Remote Desktop / RDP: the
// pointer moving must not cost an encoded frame, and pointer latency must not
// be video latency.
//
// CHANNEL CHOICE. One DataChannel, label ChannelLabel, RELIABLE + ORDERED.
// A split (unreliable/unordered for positions, reliable for shapes) was
// rejected because:
//   - a position references a shape id; on one ordered channel the shape is
//     guaranteed to arrive before the first position that uses it, with no
//     "unknown id" state on the player;
//   - positions are coalesced by the agent to at most one per
//     CoalesceInterval and only on change (20 bytes each, <=125/s), and the
//     agent drops a position whenever the channel's buffered amount is above
//     a small threshold, so a retransmit can delay at most one pending sample
//     — every sample carries absolute state, never a delta;
//   - the hub forwards ONE label generically (no per-message reliability
//     policy to keep in sync across two channels).
//
// All integers are little endian. Every message starts with Magic and a Kind.
//
//	Position (KindPos, PosSize bytes):
//	  0  u8  Magic 'C'
//	  1  u8  KindPos
//	  2  u8  flags (bit0 = visible)
//	  3  u8  reserved (0)
//	  4  u32 shape id (0 = none yet)
//	  8  i32 x   pointer HOTSPOT position in FRAME pixels (DXGI output space)
//	  12 i32 y
//	  16 u16 frame width   (so the player can map without the video size)
//	  18 u16 frame height
//
//	Shape (KindShape, ShapeHeader + len(data) bytes):
//	  0  u8  Magic 'C'
//	  1  u8  KindShape
//	  2  u8  format (FormatRGBA | FormatPNG)
//	  3  u8  reserved (0)
//	  4  u32 shape id (FNV-1a of w,h,hotspot,RGBA; never 0)
//	  8  u16 width   1..MaxDim
//	  10 u16 height  1..MaxDim
//	  12 u16 hotspot x (< width)
//	  14 u16 hotspot y (< height)
//	  16 ... pixel data (RGBA: exactly w*h*4 bytes, straight alpha; PNG: a PNG)
package cursorproto

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"time"
)

const (
	// ChannelLabel is the DataChannel label on both legs (agent->hub, hub->viewer).
	ChannelLabel      = "oosc-cursor"
	Magic        byte = 'C'
	KindPos      byte = 1
	KindShape    byte = 2
	// KindMode is the hub->agent grant (3 bytes: Magic,
	// KindMode, 0|1). 1 = every viewer of the agent leg draws the cursor
	// itself, so the agent may stop compositing it; 0 = composite again.
	// Never relayed to viewers (Validate rejects it).
	KindMode byte = 3
	ModeSize      = 3

	FormatRGBA byte = 0
	FormatPNG  byte = 1

	FlagVisible byte = 1

	PosSize     = 20
	ShapeHeader = 16
	// MaxDim is the largest shape side accepted (DXGI pointers are <=256).
	MaxDim = 256
	// MaxMessage keeps every message under pion's default SCTP max message
	// size (65536).
	MaxMessage = 65535

	// CoalesceInterval is the minimum spacing of position messages.
	CoalesceInterval = 8 * time.Millisecond
)

var (
	ErrShort    = errors.New("cursorproto: message too short")
	ErrMagic    = errors.New("cursorproto: bad magic")
	ErrKind     = errors.New("cursorproto: unknown kind")
	ErrTooLarge = errors.New("cursorproto: message too large")
	ErrDims     = errors.New("cursorproto: bad shape dimensions")
	ErrHotspot  = errors.New("cursorproto: hotspot outside shape")
	ErrFormat   = errors.New("cursorproto: bad shape format")
	ErrDataLen  = errors.New("cursorproto: pixel data length mismatch")
	ErrID       = errors.New("cursorproto: shape id 0")
)

// Pos is a pointer position sample.
type Pos struct {
	Visible        bool
	ShapeID        uint32
	X, Y           int32
	FrameW, FrameH uint16
}

// Shape is one pointer image.
type Shape struct {
	ID         uint32
	Format     byte
	W, H       int
	HotX, HotY int
	Data       []byte
}

// Kind returns the message kind, or 0 if b is not a cursorproto message.
func Kind(b []byte) byte {
	if len(b) < 2 || b[0] != Magic {
		return 0
	}
	return b[1]
}

// EncodePos returns the PosSize-byte wire form of p.
func EncodePos(p Pos) []byte {
	b := make([]byte, PosSize)
	b[0], b[1] = Magic, KindPos
	if p.Visible {
		b[2] = FlagVisible
	}
	binary.LittleEndian.PutUint32(b[4:], p.ShapeID)
	binary.LittleEndian.PutUint32(b[8:], uint32(p.X))
	binary.LittleEndian.PutUint32(b[12:], uint32(p.Y))
	binary.LittleEndian.PutUint16(b[16:], p.FrameW)
	binary.LittleEndian.PutUint16(b[18:], p.FrameH)
	return b
}

// DecodePos parses a position message.
func DecodePos(b []byte) (Pos, error) {
	if len(b) < PosSize {
		return Pos{}, ErrShort
	}
	if b[0] != Magic {
		return Pos{}, ErrMagic
	}
	if b[1] != KindPos {
		return Pos{}, ErrKind
	}
	if len(b) != PosSize {
		return Pos{}, ErrTooLarge
	}
	return Pos{
		Visible: b[2]&FlagVisible != 0,
		ShapeID: binary.LittleEndian.Uint32(b[4:]),
		X:       int32(binary.LittleEndian.Uint32(b[8:])),
		Y:       int32(binary.LittleEndian.Uint32(b[12:])),
		FrameW:  binary.LittleEndian.Uint16(b[16:]),
		FrameH:  binary.LittleEndian.Uint16(b[18:]),
	}, nil
}

func checkShape(format byte, w, h, hx, hy, dataLen int, id uint32) error {
	if id == 0 {
		return ErrID
	}
	if w < 1 || h < 1 || w > MaxDim || h > MaxDim {
		return ErrDims
	}
	if hx < 0 || hy < 0 || hx >= w || hy >= h {
		return ErrHotspot
	}
	switch format {
	case FormatRGBA:
		if dataLen != w*h*4 {
			return ErrDataLen
		}
	case FormatPNG:
		if dataLen < pngMinLen {
			return ErrDataLen
		}
	default:
		return ErrFormat
	}
	if ShapeHeader+dataLen > MaxMessage {
		return ErrTooLarge
	}
	return nil
}

// EncodeShape validates s and returns its wire form.
func EncodeShape(s Shape) ([]byte, error) {
	if err := checkShape(s.Format, s.W, s.H, s.HotX, s.HotY, len(s.Data), s.ID); err != nil {
		return nil, err
	}
	if s.Format == FormatPNG {
		if err := checkPNGHeader(s.Data, s.W, s.H); err != nil {
			return nil, err
		}
	}
	b := make([]byte, ShapeHeader+len(s.Data))
	b[0], b[1], b[2] = Magic, KindShape, s.Format
	binary.LittleEndian.PutUint32(b[4:], s.ID)
	binary.LittleEndian.PutUint16(b[8:], uint16(s.W))
	binary.LittleEndian.PutUint16(b[10:], uint16(s.H))
	binary.LittleEndian.PutUint16(b[12:], uint16(s.HotX))
	binary.LittleEndian.PutUint16(b[14:], uint16(s.HotY))
	copy(b[ShapeHeader:], s.Data)
	return b, nil
}

// DecodeShape parses and validates a shape message. Data aliases b.
func DecodeShape(b []byte) (Shape, error) {
	if len(b) > MaxMessage {
		return Shape{}, ErrTooLarge
	}
	if len(b) < ShapeHeader {
		return Shape{}, ErrShort
	}
	if b[0] != Magic {
		return Shape{}, ErrMagic
	}
	if b[1] != KindShape {
		return Shape{}, ErrKind
	}
	s := Shape{
		Format: b[2],
		ID:     binary.LittleEndian.Uint32(b[4:]),
		W:      int(binary.LittleEndian.Uint16(b[8:])),
		H:      int(binary.LittleEndian.Uint16(b[10:])),
		HotX:   int(binary.LittleEndian.Uint16(b[12:])),
		HotY:   int(binary.LittleEndian.Uint16(b[14:])),
		Data:   b[ShapeHeader:],
	}
	if err := checkShape(s.Format, s.W, s.H, s.HotX, s.HotY, len(s.Data), s.ID); err != nil {
		return Shape{}, err
	}
	if s.Format == FormatPNG {
		if err := checkPNGHeader(s.Data, s.W, s.H); err != nil {
			return Shape{}, err
		}
	}
	return s, nil
}

// pngMinLen is the PNG signature plus a complete IHDR chunk header + body
// (8 + 4 len + 4 type + 13 data); fewer bytes cannot be a usable PNG.
const pngMinLen = 8 + 8 + 13

var pngSig = [8]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// checkPNGHeader requires the PNG's own IHDR dimensions to equal the declared
// shape size. Without it a small message could declare 16x16 while its IHDR
// says 65535x65535, and the player's image decoder would allocate for the
// IHDR size (decompression bomb) before anything else looked at it.
func checkPNGHeader(d []byte, w, h int) error {
	if len(d) < pngMinLen || [8]byte(d[:8]) != pngSig ||
		binary.BigEndian.Uint32(d[8:]) != 13 || string(d[12:16]) != "IHDR" {
		return ErrFormat
	}
	if binary.BigEndian.Uint32(d[16:]) != uint32(w) || binary.BigEndian.Uint32(d[20:]) != uint32(h) {
		return ErrDims
	}
	return nil
}

// Validate checks any cursorproto message (the hub's size/shape cap before
// fanning out).
func Validate(b []byte) error {
	if len(b) > MaxMessage {
		return ErrTooLarge
	}
	switch Kind(b) {
	case KindPos:
		_, err := DecodePos(b)
		return err
	case KindShape:
		_, err := DecodeShape(b)
		return err
	case 0:
		if len(b) < 2 {
			return ErrShort
		}
		return ErrMagic
	default:
		return ErrKind
	}
}

// ShapeID is the content hash used as the shape cache key. Never 0.
func ShapeID(w, h, hx, hy int, rgba []byte) uint32 {
	f := fnv.New32a()
	var hdr [8]byte
	binary.LittleEndian.PutUint16(hdr[0:], uint16(w))
	binary.LittleEndian.PutUint16(hdr[2:], uint16(h))
	binary.LittleEndian.PutUint16(hdr[4:], uint16(hx))
	binary.LittleEndian.PutUint16(hdr[6:], uint16(hy))
	_, _ = f.Write(hdr[:])
	_, _ = f.Write(rgba)
	id := f.Sum32()
	if id == 0 {
		id = 1
	}
	return id
}

// EncodeMode builds the hub->agent cursor-layer grant.
func EncodeMode(on bool) []byte {
	b := []byte{Magic, KindMode, 0}
	if on {
		b[2] = 1
	}
	return b
}

// DecodeMode parses a grant; ok=false for anything that is not exactly one.
func DecodeMode(b []byte) (on, ok bool) {
	if len(b) != ModeSize || b[0] != Magic || b[1] != KindMode || b[2] > 1 {
		return false, false
	}
	return b[2] == 1, true
}
