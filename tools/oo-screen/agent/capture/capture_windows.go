//go:build windows

// Package capture is the agent-side screen source: DXGI Desktop Duplication
// feeding a persistent D3D11 BGRA->NV12 pipeline (Ф0, plan §4).
//
// The Go layer owns policy — timeouts, the recreate state machine, backoff —
// while dxgi.c owns the COM objects. Nothing here touches COM directly.
package capture

/*
#cgo CFLAGS: -O2 -Wall
#cgo LDFLAGS: -ld3d11 -ldxgi -lole32 -lgdi32 -luser32

#include <stdlib.h>
#include "dxgi.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Windows hands a process that never declared DPI awareness a VIRTUALISED
// desktop: GetDC(NULL) then reports the desktop in logical (scaled) pixels
// while DXGI keeps reporting physical ones. oos_gdi_next blits DXGI's physical
// rectangle out of that DC, so on a scaled display — a laptop at 125-150% is
// the normal case, not the exotic one — the first frame would come out cropped
// and magnified. A Go binary carries no application manifest, so the awareness
// is declared with the runtime call instead; it has to happen before any DC
// exists, hence init() and not New.
//
// Desktop Duplication is unaffected either way: DesktopCoordinates and the
// duplication surface are always physical pixels, so the DXGI path keeps
// producing exactly what it produced before.
func init() {
	user32 := syscall.NewLazyDLL("user32.dll")
	// PER_MONITOR_AWARE_V2 = (DPI_AWARENESS_CONTEXT)-4, Windows 10 1703+.
	// A failure here means awareness is already declared (a manifest, or a
	// host process that got there first) — that declaration wins, ours is a
	// no-op, and there is nothing useful to do about it this early.
	if p := user32.NewProc("SetProcessDpiAwarenessContext"); p.Find() == nil {
		p.Call(^uintptr(3))
		return
	}
	// ponytail: before 1703 this is system-DPI awareness — 1:1 on the primary
	// monitor, which is the single-screen laptop case; true per-monitor there
	// would need a manifest resource in the build.
	if p := user32.NewProc("SetProcessDPIAware"); p.Find() == nil {
		p.Call()
	}
}

// Typed errors. Callers switch on these, never on strings.
var (
	// ErrAccessLost means the duplication object is gone and must be
	// recreated: DXGI_ERROR_ACCESS_LOST, DEVICE_REMOVED, DEVICE_RESET, a
	// desktop switch, a resolution/rotation change, or a GPU reset.
	// Capturer recovers from this on its own; it only escapes NextFrame when
	// recovery kept failing past the retry budget.
	ErrAccessLost = errors.New("capture: duplication access lost")

	// ErrNotAvailable means Desktop Duplication cannot run here at all —
	// an RDP session, the logon/secure desktop, or a headless adapter.
	// Retrying does not help until the session changes. Secure desktop is Ф3.
	ErrNotAvailable = errors.New("capture: desktop duplication unavailable in this session")

	// ErrClosed is returned by NextFrame after Close.
	ErrClosed = errors.New("capture: closed")

	// ErrInvalidCall is DXGI_ERROR_INVALID_CALL (A-07). It used to be reported
	// as ErrAccessLost, which reads as "Windows took the desktop away" when it
	// actually means we called DXGI wrong — a frame acquired twice without a
	// release, or a duplication used after it was invalidated. Recovery is the
	// same (recreate), the diagnosis is not.
	ErrInvalidCall = errors.New("capture: DXGI invalid call")
)

// NV12Frame is one captured desktop frame, already converted on the GPU.
//
// Y and UV are copies owned by the caller — the underlying staging map is
// released before NextFrame returns.
//
// TODO(zero-copy): this CPU readback exists only because Ф0 has no encoder yet.
// The real path hands the NV12 ID3D11Texture2D straight to the hardware MFT
// (MF_SA_D3D11_AWARE + IMFDXGIDeviceManager + MFCreateDXGISurfaceBuffer), and
// then Y/UV here become nil and a texture handle takes their place.
type NV12Frame struct {
	Width  int
	Height int

	Y  []byte // luma plane, YStride bytes per row, Height rows
	UV []byte // interleaved chroma, UVStride bytes per row, Height/2 rows

	YStride  int
	UVStride int

	// Texture is the NV12 ID3D11Texture2D* when CPU readback is off
	// (SetCPUReadback(false)); Y/UV are nil then. It is the capturer's
	// persistent Blt target and is overwritten by the next NextFrame.
	Texture uintptr
	// TextureGen is Capturer.Generation() at the moment this frame was taken
	// (A-06). A consumer that caches anything per-texture must compare BOTH
	// Texture and TextureGen: after a recreate the new texture can land on the
	// old address.
	TextureGen uint64

	// CursorVisible is DXGI's PointerPosition.Visible for this output.
	CursorVisible bool
	// CursorComposited is true when the pointer was actually blended into the
	// BGRA texture before conversion. False when the pointer is off-screen or
	// no shape has arrived yet.
	CursorComposited bool
	// CursorShape is the DXGI shape kind of the cached pointer.
	CursorShape CursorShapeType
	CursorX     int
	CursorY     int

	// MouseOnly is true when DXGI reported LastPresentTime == 0: the desktop
	// image did not change, only the pointer moved. It is still a new frame for
	// us, because the pointer is composited in (plan §4 / Codex R2#14).
	MouseOnly bool
	// AccumulatedFrames is how many presents DXGI coalesced into this one.
	AccumulatedFrames uint32

	// Gap #2 (RESEARCH-leaders.md): DXGI dirty/move-rect metadata.
	// RectsValid=false means DXGI gave no metadata for a real present, so the
	// whole output counts as dirty (DirtyArea is then the full area). Areas are
	// in pixels, clipped to the output; MoveArea counts destination rects.
	RectsValid bool
	DirtyRects int
	MoveRects  int
	DirtyArea  int64
	MoveArea   int64
	// NoChange: zero dirty and move rects AND the pointer did not move or
	// change shape — the composited image is identical to the previous one.
	// Such a frame carries no planes and no texture; skip it (and do not
	// treat it as motion).
	NoChange bool

	// Captured is when NextFrame started the acquire that produced this frame.
	Captured time.Time
	// AcquireConvert is AcquireNextFrame + cursor composite + Blt + readback.
	AcquireConvert time.Duration
}

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

// Options tunes the capture loop. The zero value is usable.
type Options struct {
	// FrameBudget is the AcquireNextFrame timeout — one frame's worth of time,
	// not a millisecond poll. Default 33ms (~30fps).
	FrameBudget time.Duration
	// MinBackoff/MaxBackoff bound the reinit loop after ACCESS_LOST.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// MaxReinit is how many consecutive failed recreations NextFrame tolerates
	// before giving up and returning ErrAccessLost. Default 5.
	MaxReinit int
	// Logger, optional.
	Logger *slog.Logger
}

func (o *Options) withDefaults() {
	if o.FrameBudget <= 0 {
		o.FrameBudget = 33 * time.Millisecond
	}
	if o.MinBackoff <= 0 {
		o.MinBackoff = 10 * time.Millisecond
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 200 * time.Millisecond
	}
	if o.MaxReinit <= 0 {
		o.MaxReinit = 5
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Capturer owns one duplicated output and its persistent conversion pipeline.
//
// It is NOT safe for concurrent use: all D3D11 work happens on the goroutine
// that calls NextFrame, and Close must not race with it.
type Capturer struct {
	mu     sync.Mutex
	c      *C.oos_cap
	opts   Options
	output int
	closed bool

	width, height int

	// gen counts successful opens: 1 after New, +1 on every reinit (A-06).
	// Everything downstream that caches something derived from the pipeline —
	// the encoder's input view, above all — must key that cache on (device,
	// gen) and not on a pointer. A rebuilt pipeline releases its textures and
	// the very next allocation on the same device can hand back the SAME
	// address, so "the pointer did not change" is not evidence that the
	// texture did not change.
	gen uint64

	// readback mirrors oos_set_readback so a reinit restores the choice.
	readback bool

	// scratch reused across frames so a steady capture loop allocates nothing.
	y  []byte
	uv []byte
}

// New opens output outputIdx (index within adapter 0) and builds the pipeline.
func New(outputIdx int) (*Capturer, error) { return NewWithOptions(outputIdx, Options{}) }

// NewWithOptions is New with explicit tuning.
func NewWithOptions(outputIdx int, opts Options) (*Capturer, error) {
	opts.withDefaults()
	cap_ := &Capturer{opts: opts, output: outputIdx, readback: true}
	if err := cap_.open(); err != nil {
		return nil, err
	}
	runtime.SetFinalizer(cap_, func(x *Capturer) { x.Close() })
	return cap_, nil
}

// OutputCount reports how many outputs adapter 0 exposes.
func OutputCount() (int, error) {
	n := int(C.oos_output_count())
	if n < 0 {
		return 0, errors.New("capture: cannot enumerate DXGI outputs")
	}
	return n, nil
}

// OutputInfo describes one DXGI output for a monitor list (Index is exactly the
// index New/NewWithOptions takes). Width/Height are desktop coordinates — what
// the person sees, rotation included; the duplication's own ModeDesc, which is
// what the encoder is sized from, comes from Capturer.Size.
type OutputInfo struct {
	Index   int  `json:"index"`
	Width   int  `json:"width"`
	Height  int  `json:"height"`
	Primary bool `json:"primary"`
}

// Outputs enumerates adapter 0's outputs. Nothing is duplicated and no D3D11
// device is created, so this is cheap enough to call per offer.
func Outputs() ([]OutputInfo, error) {
	n, err := OutputCount()
	if err != nil {
		return nil, err
	}
	out := make([]OutputInfo, 0, n)
	for i := 0; i < n; i++ {
		var w, h, primary C.int32_t
		if C.oos_output_info(C.int32_t(i), &w, &h, &primary) != C.OOS_OK {
			return nil, fmt.Errorf("capture: output %d: cannot read DXGI_OUTPUT_DESC", i)
		}
		out = append(out, OutputInfo{
			Index:   i,
			Width:   int(w),
			Height:  int(h),
			Primary: primary != 0,
		})
	}
	return out, nil
}

func (c *Capturer) open() error {
	var handle *C.oos_cap
	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))

	st := C.oos_open(C.int32_t(c.output), &handle, buf, 256)
	if st != C.OOS_OK {
		msg := C.GoString(buf)
		switch st {
		case C.OOS_ACCESS_LOST:
			// DuplicateOutput failing with E_ACCESSDENIED at open time is the
			// RDP / secure-desktop case, not a transient loss.
			return fmt.Errorf("%w (output %d): %s", ErrNotAvailable, c.output, msg)
		case C.OOS_INVALID_CALL:
			// A-07: our call was wrong, the session is fine. Say so.
			return fmt.Errorf("%w (output %d): %s", ErrInvalidCall, c.output, msg)
		default:
			return fmt.Errorf("capture: open output %d: %s", c.output, msg)
		}
	}
	c.c = handle
	if !c.readback {
		C.oos_set_readback(handle, 0)
	}
	c.width = int(C.oos_width(handle))
	c.height = int(C.oos_height(handle))
	c.gen++ // A-06
	return nil
}

// Generation is how many times this capturer's pipeline has been built (A-06).
// It changes on every recreate, whether or not Device() changed, so it is the
// safe key for any cache built on top of NV12Texture().
func (c *Capturer) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// Suspend releases only the desktop duplication (A-17): the D3D device and the
// NV12 pipeline stay, so an encoder bound to Device() remains valid and the
// next NextFrame re-duplicates on the same device. Used while no viewer is
// present, where the duplication itself is what starves MeshCentral.
func (c *Capturer) Suspend() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c != nil && !c.closed {
		C.oos_suspend(c.c)
	}
}

// SetCPUReadback turns the NV12 staging copy+Map on or off. With it off,
// NextFrame leaves Y/UV nil and only NV12Texture is meaningful: that is the
// zero-copy path into agent/encode. Defaults to on.
func (c *Capturer) SetCPUReadback(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := C.int32_t(0)
	if on {
		v = 1
	}
	if c.c != nil {
		C.oos_set_readback(c.c, v)
	}
	c.readback = on
}

// Device is the ID3D11Device* behind this capturer, for an encoder that wants
// to share it via IMFDXGIDeviceManager. Zero after Close or during a reinit.
func (c *Capturer) Device() uintptr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c == nil {
		return 0
	}
	return uintptr(C.oos_device(c.c))
}

// NV12Texture is the persistent ID3D11Texture2D* that VideoProcessorBlt writes
// into. It is overwritten by the next NextFrame, so a consumer must copy it
// GPU-side before returning for more.
func (c *Capturer) NV12Texture() uintptr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c == nil {
		return 0
	}
	return uintptr(C.oos_nv12_texture(c.c))
}

// CPUMaps counts how many times the NV12 surface was mapped for CPU reading.
// The zero-copy gate of Додаток C wants this at zero.
func (c *Capturer) CPUMaps() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c == nil {
		return 0
	}
	return int64(C.oos_cpu_maps(c.c))
}

// Size returns the duplicated output's dimensions.
func (c *Capturer) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.width, c.height
}

// reinit tears the pipeline down and rebuilds it, with backoff. This is the
// ACCESS_LOST / rotation / hotplug / lock-unlock / GPU-reset state machine.
//
// A-05: this loop is the SHORT one on purpose, and the defaults above say so —
// 5 attempts, 10ms doubling to 200ms, ~0.5s of wall time in total.
//
// There are two reacquire loops in this agent, and they are not redundant:
// this one covers the transient loss (a mode switch, a UAC prompt, a GPU reset)
// where the duplication comes back within a frame or three, and it is the only
// one that can do so without dropping the encoder. The OUTER loop, in the frame
// loop of cmd/oo-agent, covers the long outage — a lock screen lasts minutes,
// not milliseconds — and it is the only one that can keep the session alive
// while it waits, because it sends the keepalive AU the viewer's watchdog needs.
//
// So the budget here must stay SMALL. Sizing it for a lock screen (12 attempts
// backing off to 500ms was ~3.6s) does not make the lock screen recoverable —
// nothing here can duplicate a secure desktop — it just blocks NextFrame for
// seconds while the outer loop, which would have sent keepalives, waits on it.
// Give up early, return ErrAccessLost, and let the owner of the keepalive
// handle the long wait.
func (c *Capturer) reinit(ctx context.Context) error {
	backoff := c.opts.MinBackoff
	var last error
	for attempt := 1; attempt <= c.opts.MaxReinit; attempt++ {
		if c.c != nil {
			C.oos_close(c.c)
			c.c = nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		err := c.open()
		if err == nil {
			c.opts.Logger.Info("capture: duplication recreated",
				"output", c.output, "attempt", attempt,
				"width", c.width, "height", c.height)
			// Dimensions may have changed (rotation, resolution switch); drop
			// the scratch so the next frame sizes it afresh.
			c.y, c.uv = nil, nil
			return nil
		}
		last = err
		c.opts.Logger.Warn("capture: recreate failed",
			"output", c.output, "attempt", attempt, "err", err)
		backoff *= 2
		if backoff > c.opts.MaxBackoff {
			backoff = c.opts.MaxBackoff
		}
	}
	return fmt.Errorf("%w: gave up after %d attempts: %v",
		ErrAccessLost, c.opts.MaxReinit, last)
}

// GDIFrame grabs the CURRENT desktop with GDI instead of waiting for it to
// change, and returns it through the same BGRA->NV12 pipeline as NextFrame —
// same scratch buffers, same NV12 texture, so an encoder bound to
// Device()/NV12Texture() is unaffected.
//
// It exists because Desktop Duplication reports nothing at all while the screen
// is still: AcquireNextFrame only ever returns WAIT_TIMEOUT, so a session that
// starts on a static desktop never gets a first frame and the viewer times out
// waiting for one. Measured on a genuinely idle desktop here: NextFrame timed
// out 6 times out of 6 (and so did re-creating the duplication, and so did
// RedrawWindow on the whole desktop), while the GDI grab returned the real
// picture every time in ~50ms at 2560x1440.
//
// The frame has no pointer in it — BitBlt does not draw the cursor and DXGI has
// not handed us a shape yet. Use it for the first frame, not for the stream.
func (c *Capturer) GDIFrame() (*NV12Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.c == nil {
		return nil, fmt.Errorf("%w: pipeline not open", ErrAccessLost)
	}

	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))

	var f C.oos_frame
	start := time.Now()
	if st := C.oos_gdi_next(c.c, &f, buf, 256); st != C.OOS_OK {
		return nil, fmt.Errorf("capture: output %d: GDI grab: %s", c.output, C.GoString(buf))
	}
	frame := c.copyOut(&f, start)
	C.oos_release(c.c)
	return frame, nil
}

// ReadBGRA copies the current desktop image (the one last converted, pointer
// included) into a fresh BGRA buffer: w*4 bytes per row, h rows. One GPU
// staging copy + Map; meant for the text-tile pass on a static screen, not
// per frame. Must be called from the capture loop (like NextFrame).
func (c *Capturer) ReadBGRA() (pix []byte, w, h int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, 0, 0, ErrClosed
	}
	if c.c == nil {
		return nil, 0, 0, fmt.Errorf("%w: pipeline not open", ErrAccessLost)
	}
	w, h = int(C.oos_width(c.c)), int(C.oos_height(c.c))
	if w <= 0 || h <= 0 {
		return nil, 0, 0, fmt.Errorf("capture: read_bgra: bad size %dx%d", w, h)
	}
	pix = make([]byte, w*4*h)
	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))
	if st := C.oos_read_bgra(c.c, (*C.uint8_t)(unsafe.Pointer(&pix[0])), C.int32_t(w*4), buf, 256); st != C.OOS_OK {
		return nil, 0, 0, fmt.Errorf("capture: output %d: %s", c.output, C.GoString(buf))
	}
	return pix, w, h, nil
}

// NextFrame blocks until a frame is available, ctx is done, or capture fails.
//
// A DXGI_ERROR_WAIT_TIMEOUT is neither a frame nor an error: NextFrame simply
// loops, so a perfectly static desktop makes this block until ctx expires. Give
// it a deadline if you need "no news" reported back.
func (c *Capturer) NextFrame(ctx context.Context) (*NV12Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}

	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c.c == nil {
			if err := c.reinit(ctx); err != nil {
				return nil, err
			}
		}

		var f C.oos_frame
		start := time.Now()
		st := C.oos_next(c.c, C.uint32_t(c.opts.FrameBudget.Milliseconds()),
			&f, buf, 256)

		switch st {
		case C.OOS_TIMEOUT:
			// Static screen. Not a frame, not an error — go round again.
			continue

		case C.OOS_ACCESS_LOST:
			c.opts.Logger.Warn("capture: access lost, recreating",
				"output", c.output, "detail", C.GoString(buf))
			C.oos_close(c.c)
			c.c = nil
			continue

		case C.OOS_INVALID_CALL:
			// A-07: recovered the same way, logged as what it is — a bug on our
			// side of the DXGI contract, not the desktop going away.
			c.opts.Logger.Error("capture: DXGI invalid call, recreating (this is our bug, not a desktop switch)",
				"output", c.output, "detail", C.GoString(buf))
			C.oos_close(c.c)
			c.c = nil
			continue

		case C.OOS_OK:
			// Resolution may have changed while suspended (A-17): the staging
			// textures were sized at open, so treat it exactly like ACCESS_LOST
			// — rebuild the pipeline (and, upstream, the encoder).
			if int(C.oos_width(c.c)) != c.width || int(C.oos_height(c.c)) != c.height {
				c.opts.Logger.Warn("capture: output size changed while suspended, recreating",
					"output", c.output, "was", fmt.Sprintf("%dx%d", c.width, c.height),
					"now", fmt.Sprintf("%dx%d", int(C.oos_width(c.c)), int(C.oos_height(c.c))))
				C.oos_close(c.c)
				c.c = nil
				continue
			}
			frame := c.copyOut(&f, start)
			C.oos_release(c.c)
			return frame, nil

		default:
			msg := C.GoString(buf)
			C.oos_close(c.c)
			c.c = nil
			c.closed = true
			return nil, fmt.Errorf("capture: output %d: %s", c.output, msg)
		}
	}
}

// copyOut turns the mapped staging planes into Go bytes. The scratch buffers
// are reused, so the returned frame aliases them: consume a frame before asking
// for the next one.
func (c *Capturer) copyOut(f *C.oos_frame, start time.Time) *NV12Frame {
	w, h := int(f.width), int(f.height)
	yPitch, uvPitch := int(f.y_pitch), int(f.uv_pitch)
	// NV12 texture height is rounded up to even; chroma has half the rows.
	alignedH := (h + 1) &^ 1
	chromaRows := alignedH / 2

	if f.no_change != 0 {
		// Nothing to show: the NV12 texture still holds the previous image.
		fr := &NV12Frame{Width: w, Height: h, TextureGen: c.gen}
		fillMeta(fr, f, start)
		return fr
	}

	if f.y == nil {
		// Zero-copy mode: nothing was read back, the NV12 lives on the GPU.
		fr := &NV12Frame{
			Width:      w,
			Height:     h,
			Texture:    uintptr(C.oos_nv12_texture(c.c)),
			TextureGen: c.gen,
		}
		fillMeta(fr, f, start)
		return fr
	}

	ySize := yPitch * h
	uvSize := uvPitch * chromaRows
	if cap(c.y) < ySize {
		c.y = make([]byte, ySize)
	}
	if cap(c.uv) < uvSize {
		c.uv = make([]byte, uvSize)
	}
	c.y = c.y[:ySize]
	c.uv = c.uv[:uvSize]

	copy(c.y, unsafe.Slice((*byte)(unsafe.Pointer(f.y)), ySize))
	copy(c.uv, unsafe.Slice((*byte)(unsafe.Pointer(f.uv)), uvSize))

	fr := &NV12Frame{
		Width:    w,
		Height:   h,
		Y:        c.y,
		UV:       c.uv,
		YStride:  yPitch,
		UVStride: uvPitch,
	}
	fillMeta(fr, f, start)
	return fr
}

// fillMeta copies the cursor/DXGI metadata shared by every copyOut branch.
func fillMeta(fr *NV12Frame, f *C.oos_frame, start time.Time) {
	fr.CursorVisible = f.cursor_visible != 0
	fr.CursorComposited = f.cursor_composited != 0
	fr.CursorShape = CursorShapeType(f.cursor_shape_type)
	fr.CursorX = int(f.cursor_x)
	fr.CursorY = int(f.cursor_y)
	fr.MouseOnly = f.mouse_only != 0
	fr.AccumulatedFrames = uint32(f.accumulated_frames)
	fr.RectsValid = f.rects_valid != 0
	fr.DirtyRects = int(f.dirty_count)
	fr.MoveRects = int(f.move_count)
	fr.DirtyArea = int64(f.dirty_area)
	fr.MoveArea = int64(f.move_area)
	fr.NoChange = f.no_change != 0
	fr.Captured = start
	fr.AcquireConvert = time.Since(start)
}

// Close releases the duplication, the pipeline and the D3D11 device.
// Idempotent.
func (c *Capturer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed && c.c == nil {
		return nil
	}
	c.closed = true
	if c.c != nil {
		C.oos_close(c.c)
		c.c = nil
	}
	runtime.SetFinalizer(c, nil)
	return nil
}
