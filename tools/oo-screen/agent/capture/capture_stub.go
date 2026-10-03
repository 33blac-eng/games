//go:build !windows

// Package capture — non-Windows stub so `go build ./...` and `go vet ./...`
// stay green on Linux CI. DXGI Desktop Duplication is Windows-only; there is no
// intent to port this file (a Linux agent would be PipeWire, a separate source).
package capture

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

var (
	ErrAccessLost   = errors.New("capture: duplication access lost")
	ErrNotAvailable = errors.New("capture: DXGI desktop duplication requires Windows")
	ErrClosed       = errors.New("capture: closed")
)

type CursorShapeType int32

const (
	CursorNone        CursorShapeType = 0
	CursorMonochrome  CursorShapeType = 1
	CursorColor       CursorShapeType = 2
	CursorMaskedColor CursorShapeType = 4
)

func (t CursorShapeType) String() string {
	switch t {
	case CursorMonochrome:
		return "monochrome"
	case CursorColor:
		return "color"
	case CursorMaskedColor:
		return "masked-color"
	default:
		return "none"
	}
}

// NV12Frame mirrors the Windows type so callers compile everywhere.
type NV12Frame struct {
	Width, Height     int
	Y, UV             []byte
	YStride, UVStride int

	CursorVisible     bool
	CursorComposited  bool
	CursorShape       CursorShapeType
	CursorX, CursorY  int
	MouseOnly         bool
	AccumulatedFrames uint32

	Captured       time.Time
	AcquireConvert time.Duration
}

type Options struct {
	FrameBudget time.Duration
	MinBackoff  time.Duration
	MaxBackoff  time.Duration
	MaxReinit   int
	Logger      *slog.Logger
}

type Capturer struct{}

func New(outputIdx int) (*Capturer, error) { return nil, ErrNotAvailable }

func NewWithOptions(outputIdx int, opts Options) (*Capturer, error) {
	return nil, ErrNotAvailable
}

func OutputCount() (int, error) { return 0, ErrNotAvailable }

// OutputInfo mirrors the Windows type so callers compile everywhere.
type OutputInfo struct {
	Index   int  `json:"index"`
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	Primary bool `json:"primary"`
}

func Outputs() ([]OutputInfo, error) { return nil, ErrNotAvailable }

func (c *Capturer) Size() (int, int) { return 0, 0 }

func (c *Capturer) NextFrame(ctx context.Context) (*NV12Frame, error) {
	return nil, ErrNotAvailable
}

func (c *Capturer) Close() error { return nil }
