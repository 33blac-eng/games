//go:build !windows

// Package encode — non-Windows stub so `go build ./...` and `go vet ./...` stay
// green on Linux CI. Media Foundation is Windows-only; a Linux agent would use
// VA-API/NVENC directly, a separate implementation.
package encode

import (
	"errors"
	"time"
)

var (
	ErrNoHardware = errors.New("encode: hardware H.264 MFT requires Windows")
	ErrClosed     = errors.New("encode: closed")
	ErrWedged     = errors.New("encode: encoder wedged, rebuild required")
)

type Config struct {
	Width         int
	Height        int
	FPS           int
	BitrateBps    int
	GOP           int
	D3DDevice     uintptr
	SrcWidth      int
	SrcHeight     int
	ForceSoftware bool
	IntraRefresh  int
}

type Frame struct {
	Y        []byte
	UV       []byte
	YStride  int
	UVStride int
	Texture  uintptr
	PTS      time.Duration
}

type AU struct {
	Data     []byte
	Keyframe bool
	PTS      time.Duration
}

type Encoder struct{}

func New(Config) (*Encoder, error) { return nil, ErrNoHardware }

func (e *Encoder) Name() string               { return "" }
func (e *Encoder) Async() bool                { return false }
func (e *Encoder) Hardware() bool             { return false }
func (e *Encoder) ZeroCopy() bool             { return false }
func (e *Encoder) Level() int                 { return 0 }
func (e *Encoder) Profile() int               { return 0 }
func (e *Encoder) Headers() []byte            { return nil }
func (e *Encoder) HeaderStats() (int, int)    { return 0, 0 }
func (e *Encoder) Encode(Frame) ([]AU, error) { return nil, ErrNoHardware }
func (e *Encoder) ForceIDR() error            { return ErrNoHardware }
func (e *Encoder) SetBitrate(int) error       { return ErrNoHardware }
func (e *Encoder) SetRefineQP(int) error      { return ErrNoHardware }
func (e *Encoder) Flush() error               { return ErrNoHardware }
func (e *Encoder) SetQPBounds(int, int) error { return ErrNoHardware }
func (e *Encoder) IntraRefresh() bool         { return false }
func (e *Encoder) CodecAPICaps() string       { return "" }
func (e *Encoder) Close() error               { return nil }
