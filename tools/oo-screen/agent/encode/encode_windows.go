//go:build windows

// Package encode is the agent-side H.264 encoder: a hardware Media Foundation
// MFT in low-latency CBR mode (Ф0, plan §5.1), emitting one full Annex-B access
// unit per frame per the canonical contract in §5.2.
//
// The Go layer owns policy — the SPS/PPS repeat rule, PTS units, the flush and
// restart sequence — while mft.c owns the COM objects. Nothing here touches COM.
package encode

/*
#cgo CFLAGS: -O2 -Wall
#cgo LDFLAGS: -lmfplat -lmfuuid -lmf -lmfreadwrite -lole32 -luuid -ld3d11 -ldxguid -loleaut32

#include <stdlib.h>
#include "mft.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/organicoils/oo-screen/internal/h264"
)

// Typed errors. Callers switch on these, never on strings.
var (
	// ErrNoHardware means there is no usable H.264 MFT for this capture on the
	// path New was asked for. mft.c already drops to the software Microsoft
	// H.264 MFT by itself when no hardware MFT exists or all of them sit on
	// another GPU (A-18); it returns this when even that enumeration is empty,
	// or when the hardware MFT it picked is not D3D11-aware (A-19). The agent
	// answers the latter by retrying with Config.ForceSoftware.
	ErrNoHardware = errors.New("encode: no hardware H.264 MFT on this machine")

	// ErrWedged means the MFT stopped raising events mid-submit (A-12: driver
	// reset, device removed, NVENC session lost). Nothing but a rebuild of the
	// encoder recovers it; it is a status code from mft.c, not a message match.
	ErrWedged = errors.New("encode: encoder wedged, rebuild required")

	// ErrClosed is returned after Close.
	ErrClosed = errors.New("encode: closed")
)

// Config describes one encoding epoch. Width/Height/FPS/BitrateBps are required.
type Config struct {
	Width      int
	Height     int
	FPS        int
	BitrateBps int

	// GOP is the IDR interval in frames. Zero means 2*FPS (plan §5.1: IDR
	// every 2s plus on request).
	GOP int

	// D3DDevice is an ID3D11Device* shared with the capture pipeline. When set,
	// Encode accepts an NV12 texture handle and nothing is read back to the CPU
	// (the zero-copy path of Додаток C). Zero means the CPU byte path.
	D3DDevice uintptr

	// SrcWidth/SrcHeight describe the incoming NV12 textures when they are
	// bigger than the encoded frame. The encoder then rescales NV12->NV12 with
	// a D3D11 VideoProcessor — still entirely on the GPU. Zero means "same".
	SrcWidth  int
	SrcHeight int

	// ForceSoftware skips the hardware MFTEnumEx and opens the built-in
	// software Microsoft H264 Video Encoder MFT directly. Used by the gate to
	// exercise the software sync path on a box that also has hardware. When
	// false the encoder tries hardware first and only falls back to software
	// automatically. The software path is CPU-only, so D3DDevice is ignored.
	ForceSoftware bool
}

// Frame is one NV12 input. Either the CPU planes or Texture must be set; when
// both are present Texture wins, because that is the path that skips the copy.
type Frame struct {
	Y  []byte
	UV []byte

	YStride  int
	UVStride int

	// Texture is an NV12 ID3D11Texture2D* on the same device as Config.D3DDevice.
	Texture uintptr
	// TextureGen is the producer's pipeline generation for Texture (A-06):
	// capture.NV12Frame.TextureGen. The encoder caches a GPU input view per
	// source texture, and a rebuilt capture pipeline can hand back the same
	// address for a different texture — so the address alone is not identity.
	TextureGen uint64

	// PTS is monotonic presentation time from the start of the epoch (§5.2).
	PTS time.Duration
}

// AU is one complete Annex-B access unit (plan §5.2). Data is owned by the
// caller: Encode copies out of the encoder's buffer before returning.
type AU struct {
	Data     []byte
	Keyframe bool
	PTS      time.Duration
}

// Encoder wraps one H.264 MFT — hardware on the capture's adapter when there is
// one, the software Microsoft MFT otherwise (see ErrNoHardware). It is safe
// for concurrent use only in the sense that the mutex serialises callers; the
// MFT itself is driven from whichever goroutine calls in.
//
// Threading model. mft.c keeps a dedicated anchor thread that holds
// CoInitializeEx(COINIT_MULTITHREADED) + MFStartup for as long as any Encoder
// is open, and owns the matching CoUninitialize/MFShutdown. Because that MTA
// exists, every Go thread that enters cgo here joins it implicitly, so no
// runtime.LockOSThread is needed: an MF_TRANSFORM_ASYNC hardware MFT is
// free-threaded by contract (Media Foundation's own work-queue threads call it
// concurrently with ours), IMFMediaEventGenerator and IMFDXGIDeviceManager are
// free-threaded too, and the shared ID3D11Device runs with
// ID3D10Multithread::SetMultithreadProtected(TRUE). The mutex below is what
// keeps OUR calls serialised; goroutine-to-thread migration between calls is
// harmless because none of these objects has thread affinity.
// The accessors Name/Async/ZeroCopy/Level read values cached at New, so they
// need no lock and cannot race Close.
type Encoder struct {
	mu     sync.Mutex
	e      *C.oos_enc
	cfg    Config
	closed bool

	// Immutable facts about the MFT, read once in New. They used to be read
	// from the C handle on every call, racing Close (which nils e.e) without
	// the mutex; caching removes both the race and the cgo round-trip.
	name     string
	async    bool
	hardware bool
	zeroCopy bool
	level    int
	profile  int

	headers []byte // cached SPS/PPS, Annex-B

	// headersInjected counts IDR AUs that did NOT carry an inband SPS and had
	// the cached one prefixed by us. Zero means the MFT repeats headers itself.
	headersInjected int
	keyframes       int

	lastWait    time.Duration
	lastProcess time.Duration
}

// LastSubmitTiming splits the cost of the most recent Encode: how long it
// waited for the MFT to ask for input (back-pressure — the encoder telling us
// it is saturated) and how long ProcessInput itself took (the cost the capture
// loop actually pays per frame).
func (e *Encoder) LastSubmitTiming() (wait, process time.Duration) {
	return e.lastWait, e.lastProcess
}

// New opens an H.264 MFT — the hardware one on the capture's adapter, else the
// software one (always with Config.ForceSoftware) — and configures it for
// low-latency CBR.
func New(cfg Config) (*Encoder, error) {
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("encode: bad frame size %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.FPS <= 0 {
		cfg.FPS = 30
	}
	if cfg.BitrateBps <= 0 {
		cfg.BitrateBps = 8_000_000
	}
	if cfg.GOP <= 0 {
		cfg.GOP = cfg.FPS * 2
	}

	var c C.oos_enc_cfg
	c.width = C.int32_t(cfg.Width)
	c.height = C.int32_t(cfg.Height)
	c.fps = C.int32_t(cfg.FPS)
	c.bitrate_bps = C.int32_t(cfg.BitrateBps)
	c.gop = C.int32_t(cfg.GOP)
	c.d3d_device = C.uintptr_t(cfg.D3DDevice)
	c.src_width = C.int32_t(cfg.SrcWidth)
	c.src_height = C.int32_t(cfg.SrcHeight)
	if cfg.ForceSoftware {
		c.force_software = 1
	}

	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))

	var handle *C.oos_enc
	switch st := C.oos_enc_open(&c, &handle, buf, 256); st {
	case C.OOS_ENC_OK:
	case C.OOS_ENC_NOHW:
		return nil, fmt.Errorf("%w: %s", ErrNoHardware, C.GoString(buf))
	default:
		return nil, fmt.Errorf("encode: open: %s", C.GoString(buf))
	}

	enc := &Encoder{
		e:        handle,
		cfg:      cfg,
		name:     C.GoString(C.oos_enc_name(handle)),
		async:    C.oos_enc_is_async(handle) != 0,
		hardware: C.oos_enc_is_hardware(handle) != 0,
		zeroCopy: C.oos_enc_is_d3d(handle) != 0,
		level:    int(C.oos_enc_level(handle)),
		profile:  int(C.oos_enc_profile(handle)),
	}
	var hp *C.uint8_t
	var hl C.int32_t
	C.oos_enc_headers(handle, &hp, &hl)
	if hl > 0 {
		enc.headers = C.GoBytes(unsafe.Pointer(hp), C.int(hl))
	}
	// A-14: ICodecAPI knobs are best effort; say which ones this GPU refused,
	// otherwise "LowLatency/CBR/B=0" is a belief, not a fact.
	if r := C.GoString(C.oos_enc_cfg_report(handle)); r != "" {
		log.Printf("encode: %s refused ICodecAPI knobs: %s", enc.name, r)
	}
	runtime.SetFinalizer(enc, func(x *Encoder) { x.Close() })
	return enc, nil
}

// Name is the MFT's friendly name, e.g. "NVIDIA H.264 Encoder MFT".
// Cached at New, so it stays valid (and lock-free) after Close.
func (e *Encoder) Name() string { return e.name }

// Async reports whether the MFT is an asynchronous (hardware) transform.
func (e *Encoder) Async() bool { return e.async }

// Hardware reports whether a hardware MFT was selected (MFTEnumEx HARDWARE).
// False means the software Microsoft H264 Video Encoder MFT fallback is in use,
// which is synchronous and CPU-only — the caller must feed CPU NV12 planes
// (SetCPUReadback(true) on the capturer), never a texture.
func (e *Encoder) Hardware() bool { return e.hardware }

// ZeroCopy reports whether the DXGI surface path is live: IMFDXGIDeviceManager
// set on the MFT and NV12 handed over as an ID3D11Texture2D.
func (e *Encoder) ZeroCopy() bool { return e.zeroCopy }

// Level is the H.264 level_idc that was pinned on the output type; -1 means the
// MFT picked one (4.2 is not legal above 1080p, so a bigger desktop lands here).
func (e *Encoder) Level() int { return e.level }

// Profile is the H.264 profile_idc the MFT actually accepted: 77 = Main (what
// browsers announce), 100 = High (the fallback rung, which browsers refuse).
// Reported so a fallback is visible in the log instead of silently producing a
// stream no viewer will negotiate.
func (e *Encoder) Profile() int { return e.profile }

// Headers is the cached SPS/PPS in Annex-B form, or nil. Under e.mu: the
// drain loop replaces e.headers when the MFT renegotiates mid-stream.
func (e *Encoder) Headers() []byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.headers
}

// HeaderStats reports how many keyframes were produced and how many of them
// needed the cached SPS/PPS prefixed manually because the MFT did not repeat
// headers inband (plan §5.2 requires SPS/PPS with every IDR).
func (e *Encoder) HeaderStats() (keyframes, injected int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.keyframes, e.headersInjected
}

// Encode submits one frame and returns every access unit the MFT had ready.
//
// A hardware MFT is pipelined: the AUs returned for a given call usually belong
// to earlier frames. Each AU carries its own PTS, so callers must use that and
// never assume one-in-one-out.
func (e *Encoder) Encode(f Frame) ([]AU, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}

	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))

	pts := C.int64_t(f.PTS.Nanoseconds() / 100) // MF units are 100ns

	var st C.int
	switch {
	case f.Texture != 0:
		st = C.oos_enc_submit_texture(e.e, C.uintptr_t(f.Texture),
			C.uint64_t(f.TextureGen), pts, buf, 256)
	case len(f.Y) > 0 && len(f.UV) > 0:
		if err := checkPlanes(f, e.cfg.Width, e.cfg.Height); err != nil {
			return nil, err
		}
		st = C.oos_enc_submit_cpu(e.e,
			(*C.uint8_t)(unsafe.Pointer(&f.Y[0])), C.int32_t(f.YStride),
			(*C.uint8_t)(unsafe.Pointer(&f.UV[0])), C.int32_t(f.UVStride),
			pts, buf, 256)
	default:
		return nil, errors.New("encode: frame has neither planes nor a texture")
	}
	switch st {
	case C.OOS_ENC_OK, C.OOS_ENC_AGAIN:
		// AGAIN means MF_E_NOTACCEPTING: the frame was not taken, but whatever
		// output was pending is still worth draining below.
	default:
		return nil, submitErr(int(st), C.GoString(buf))
	}

	var waitUS, procUS C.int64_t
	C.oos_enc_last_timing(e.e, &waitUS, &procUS)
	e.lastWait = time.Duration(waitUS) * time.Microsecond
	e.lastProcess = time.Duration(procUS) * time.Microsecond

	return e.drainLocked(buf, 0)
}

// statusWedged is OOS_ENC_WEDGED as a Go constant: _test.go files cannot use cgo.
const statusWedged = int(C.OOS_ENC_WEDGED)

// submitErr turns a failed submit status into an error. OOS_ENC_WEDGED becomes
// ErrWedged so callers can errors.Is it; the C message is kept for the log only.
func submitErr(st int, msg string) error {
	if st == statusWedged {
		return fmt.Errorf("encode: submit: %w: %s", ErrWedged, msg)
	}
	return fmt.Errorf("encode: submit: %s", msg)
}

// drainLocked pops everything the MFT has ready. timeoutMS>0 waits that long
// for the first AU.
func (e *Encoder) drainLocked(buf *C.char, timeoutMS int) ([]AU, error) {
	var out []AU
	for {
		var au C.oos_enc_au
		st := C.oos_enc_poll(e.e, &au, C.uint32_t(timeoutMS), buf, 256)
		timeoutMS = 0
		if st == C.OOS_ENC_AGAIN || st == C.OOS_ENC_EOS {
			return out, nil
		}
		if st != C.OOS_ENC_OK {
			return out, fmt.Errorf("encode: poll: %s", C.GoString(buf))
		}
		data := C.GoBytes(unsafe.Pointer(au.data), C.int(au.len))
		C.oos_enc_release_au(e.e)

		key := au.keyframe != 0
		if key {
			e.keyframes++
			// §5.2: SPS/PPS must accompany every IDR. Most hardware MFTs do
			// repeat them inband once MF_LOW_LATENCY is set; when one does not,
			// prefix the cached sequence header ourselves.
			// The MFT may have renegotiated its output type mid-stream, in
			// which case mft.c re-cached a new SPS/PPS. Re-read before
			// injecting so we never prefix a header from a dead stream.
			var hp *C.uint8_t
			var hl C.int32_t
			C.oos_enc_headers(e.e, &hp, &hl)
			if hl > 0 {
				e.headers = C.GoBytes(unsafe.Pointer(hp), C.int(hl))
				// ТЗ 1.3 / P3 — UNVERIFIED на залізі: явний BT.709 limited у
				// VUI кешованого SPS. Профіль/рівень не змінюються.
				if rw, err := h264.RewriteAnnexBSPSColourBT709(e.headers); err == nil {
					e.headers = rw
				}
			}
			var injected bool
			if data, injected = withHeaders(data, e.headers); injected {
				e.headersInjected++
			} else if hasSPS(data) {
				// Інбенд-SPS від MFT — той самий перепис (UNVERIFIED на залізі).
				// Помилка розбору -> AU іде як є, потік не ламаємо.
				if rw, err := h264.RewriteAnnexBSPSColourBT709(data); err == nil {
					data = rw
				}
			}
		}
		out = append(out, AU{
			Data:     data,
			Keyframe: key,
			PTS:      time.Duration(int64(au.pts_100ns) * 100),
		})
	}
}

// ForceIDR asks for an IDR on the next submitted frame. The hub uses this for
// keyframe-request (plan §5.4).
func (e *Encoder) ForceIDR() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))
	if C.oos_enc_force_idr(e.e, buf, 256) != C.OOS_ENC_OK {
		return fmt.Errorf("encode: force idr: %s", C.GoString(buf))
	}
	return nil
}

// SetBitrate retargets the live encoder's CBR bitrate. The hub asks for this
// (control bitrate_target) when the link degrades; reopening the MFT would cost
// an epoch change, so the knob is turned in place. Callers clamp — the encoder
// only rejects a non-positive value.
func (e *Encoder) SetBitrate(bps int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))
	if C.oos_enc_set_bitrate(e.e, C.int32_t(bps), buf, 256) != C.OOS_ENC_OK {
		return fmt.Errorf("encode: set bitrate %d: %s", bps, C.GoString(buf))
	}
	e.cfg.BitrateBps = bps
	return nil
}

// SetRefineQP switches the following Encode calls to static-screen refine
// (ТЗ P4): qp>0 asks the MFT for that per-frame QP and caps MaxQP at it; 0
// restores normal rate control. The per-sample QP is set even when the MFT
// refuses MaxQP — that refusal is still returned, so the caller can log it.
func (e *Encoder) SetRefineQP(qp int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))
	if C.oos_enc_set_refine_qp(e.e, C.int32_t(qp), buf, 256) != C.OOS_ENC_OK {
		return fmt.Errorf("encode: set refine qp %d: %s", qp, C.GoString(buf))
	}
	return nil
}

// Flush is the reset path of plan §5.5: drain what the MFT holds, discard the
// reference state (MFT_MESSAGE_COMMAND_FLUSH), restart streaming and force an
// IDR. Everything still buffered is dropped — the video epoch is over, so no
// delta may reference across this point.
func (e *Encoder) Flush() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	buf := (*C.char)(C.calloc(256, 1))
	defer C.free(unsafe.Pointer(buf))
	if C.oos_enc_flush(e.e, buf, 256) != C.OOS_ENC_OK {
		return fmt.Errorf("encode: flush: %s", C.GoString(buf))
	}
	return nil
}

// Close releases the MFT, the DXGI device manager and the texture pool.
// Idempotent.
func (e *Encoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.e != nil {
		C.oos_enc_close(e.e)
		e.e = nil
	}
	runtime.SetFinalizer(e, nil)
	return nil
}
