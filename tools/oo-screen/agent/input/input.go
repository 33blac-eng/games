// Package input injects a remote operator's keyboard and mouse into the local
// Windows input queue through SendInput.
//
// # Position, not motion
//
// Mouse events carry a position normalized to 0..1 over the captured surface —
// the browser divides by the size of the video element — never pixels and never
// deltas. Absolute positions are what let the layer reading the channel coalesce
// a burst of moves down to the newest one: dropping intermediate deltas would
// move the pointer to the wrong place, dropping intermediate positions cannot.
// Coalescing therefore belongs above this package, and this package is built so
// it is legal.
//
// # Physical keys, not characters
//
// Keys travel as PS/2 set 1 scan codes and are injected with
// KEYEVENTF_SCANCODE, which is a correctness requirement rather than a detail.
// A virtual-key code is interpreted through the layout active on the machine
// being controlled, so an operator on a US layout pressing A would produce Ф on
// a host whose layout is Ukrainian. A scan code names the physical key and lands
// as the same key the operator actually pressed. Event.Unicode is the escape
// hatch for input that has no physical key behind it (composed text, an on-screen
// keyboard, a pasted character); it is injected with KEYEVENTF_UNICODE, which
// bypasses the layout entirely.
//
// # What will not work, and is not a bug on our side
//
// Windows refuses injected input wherever it protects the user, and no amount of
// work in this package changes that:
//
//   - UAC elevation prompts. User Interface Privilege Isolation (UIPI) discards
//     input sent from a lower integrity level to a higher one. Elevating the
//     agent does not fix it either, because the prompt is drawn on a separate
//     secure desktop that this process is not attached to.
//   - The lock screen, the credential provider, and everything else on the
//     Winlogon secure desktop.
//   - Ctrl+Alt+Del, which is the Secure Attention Sequence and is recognised
//     below the level any software can inject at, and Win+L, which the shell
//     consumes before injected input can reach it.
//
// Only a kernel-mode filter driver would change any of this, and we deliberately
// ship none: MeshCentral stays as the fallback path for exactly these cases.
// Inject reports ErrBlocked when Windows swallows an event, so a caller can log
// it and carry on rather than treating it as a broken session.
package input

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

// Version is the wire version of the input protocol. A mismatch is fatal to the
// event, not to the session; the caller decides which.
const Version = 1

var (
	// ErrNotAvailable means input injection is not possible on this build or
	// this desktop. Callers may use errors.Is to fall back to MeshCentral.
	ErrNotAvailable = errors.New("input: injection unavailable")

	// ErrInvalidEvent means the event was rejected before it reached Windows.
	// Everything arriving over the channel is untrusted, so this is the normal
	// outcome for a malformed or out-of-range message.
	ErrInvalidEvent = errors.New("input: invalid event")

	// ErrBlocked means Windows accepted fewer events than were submitted. In
	// practice this is UIPI or the secure desktop, i.e. one of the documented
	// limitations above, not a defect.
	ErrBlocked = errors.New("input: injection blocked by Windows")
)

// Kind is the event discriminator on the wire.
type Kind string

const (
	KindMouseMove   Kind = "mouse_move"
	KindMouseButton Kind = "mouse_button"
	KindMouseWheel  Kind = "mouse_wheel"
	KindKey         Kind = "key"
	// KindReleaseAll releases every key and button this injector still holds
	// down. The hub sends it when a controlling viewer's leg goes away: a
	// viewer that vanished mid-drag or with Ctrl held never sends the key-up,
	// and the key would stay stuck on someone else's PC until they pressed it.
	KindReleaseAll Kind = "release_all"
)

// Button names a physical mouse button. x1 and x2 are the side buttons.
type Button string

const (
	ButtonLeft   Button = "left"
	ButtonRight  Button = "right"
	ButtonMiddle Button = "middle"
	ButtonX1     Button = "x1"
	ButtonX2     Button = "x2"
)

// Event is one input event as it arrives from the browser, one JSON object per
// message.
type Event struct {
	V    int  `json:"v"`
	Kind Kind `json:"type"`

	// X and Y are the pointer position normalized to 0..1 over the captured
	// surface, with 0,0 at its top-left corner. They are pointers because 0 is
	// a perfectly legal coordinate: with a plain float64 an absent field would
	// silently decode to the top-left corner and yank the pointer there.
	// Required for mouse_move; optional for mouse_button and mouse_wheel, where
	// omitting them means "act wherever the pointer already is".
	X *float64 `json:"x,omitempty"`
	Y *float64 `json:"y,omitempty"`

	// Button is required for mouse_button and ignored otherwise.
	Button Button `json:"button,omitempty"`

	// Down distinguishes press from release, for both mouse_button and key.
	Down bool `json:"down,omitempty"`

	// WheelX and WheelY are wheel notches, positive meaning right and away from
	// the user. Fractional values are allowed for trackpads; they are scaled by
	// WHEEL_DELTA and rounded.
	WheelX float64 `json:"wheel_x,omitempty"`
	WheelY float64 `json:"wheel_y,omitempty"`

	// Scancode is a PS/2 set 1 make code. Either the bare make code with
	// Extended set separately, or the 0xE0-prefixed form, is accepted.
	Scancode uint16 `json:"scancode,omitempty"`
	Extended bool   `json:"extended,omitempty"`

	// Unicode is the code point to inject when the key has no scan code. It is
	// used only when Scancode is 0: a sender that fills both is asking for the
	// physical key, and the physical key is what it gets.
	Unicode rune `json:"unicode,omitempty"`
}

// ParseEvent decodes and validates one JSON message from the input channel.
// Unknown fields are ignored so a newer browser can add some without breaking
// an older agent, the same forward-compatibility rule internal/control follows.
func ParseEvent(message []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(message, &ev); err != nil {
		return Event{}, fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	if err := ev.Validate(); err != nil {
		return Event{}, err
	}
	return ev, nil
}

// Validate rejects anything that must not reach SendInput. Inject calls it, so
// a hand-built Event is checked as strictly as one off the wire.
func (e Event) Validate() error {
	if e.V != Version {
		return fmt.Errorf("%w: version %d, want %d", ErrInvalidEvent, e.V, Version)
	}
	if err := checkNormalized("x", e.X); err != nil {
		return err
	}
	if err := checkNormalized("y", e.Y); err != nil {
		return err
	}
	if (e.X == nil) != (e.Y == nil) {
		return fmt.Errorf("%w: x and y must be given together", ErrInvalidEvent)
	}

	switch e.Kind {
	case KindMouseMove:
		if e.X == nil {
			return fmt.Errorf("%w: %s without a position", ErrInvalidEvent, e.Kind)
		}
	case KindMouseButton:
		if _, ok := buttons[e.Button]; !ok {
			return fmt.Errorf("%w: unknown button %q", ErrInvalidEvent, e.Button)
		}
	case KindMouseWheel:
		if !finite(e.WheelX) || !finite(e.WheelY) {
			return fmt.Errorf("%w: wheel deltas %v,%v are not finite", ErrInvalidEvent, e.WheelX, e.WheelY)
		}
		if e.WheelX == 0 && e.WheelY == 0 {
			return fmt.Errorf("%w: wheel event scrolls nothing", ErrInvalidEvent)
		}
	case KindReleaseAll:
		// No fields: it names no key, it undoes all of them.
	case KindKey:
		if e.Scancode == 0 && e.Unicode == 0 {
			return fmt.Errorf("%w: key with neither scancode nor unicode", ErrInvalidEvent)
		}
		if e.Scancode == 0 && !utf8.ValidRune(e.Unicode) {
			return fmt.Errorf("%w: unicode %#x is not a code point", ErrInvalidEvent, e.Unicode)
		}
	default:
		return fmt.Errorf("%w: unknown type %q", ErrInvalidEvent, e.Kind)
	}
	return nil
}

func checkNormalized(name string, v *float64) error {
	if v == nil {
		return nil
	}
	if math.IsNaN(*v) || *v < 0 || *v > 1 {
		return fmt.Errorf("%w: %s = %v is outside 0..1", ErrInvalidEvent, name, *v)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Bounds is a rectangle in virtual-screen pixels. Left and Top are signed on
// purpose: a monitor placed left of or above the primary one starts at a
// negative coordinate, and that is the case this whole file exists to get right.
type Bounds struct {
	Left, Top, Width, Height int32
}

func (b Bounds) valid() bool { return b.Width > 0 && b.Height > 0 }

// pixel maps a normalized point onto a virtual-screen pixel inside b. The
// browser computes x as offsetX/clientWidth with offsetX in [0,width), so
// floor(x*Width) inverts that exactly; x == 1 is folded onto the last pixel
// rather than one past the edge.
func (b Bounds) pixel(x, y float64) (int32, int32) {
	return b.Left + axisPixel(x, b.Width), b.Top + axisPixel(y, b.Height)
}

func axisPixel(v float64, size int32) int32 {
	p := int64(math.Floor(v * float64(size)))
	if p < 0 {
		p = 0
	}
	if p > int64(size)-1 {
		p = int64(size) - 1
	}
	return int32(p)
}

// absolute converts a virtual-screen pixel into the 0..65535 pair SendInput
// wants alongside MOUSEEVENTF_ABSOLUTE|MOUSEEVENTF_VIRTUALDESK, where b is the
// bounding rectangle of every monitor.
//
// The origin is not 0,0. With a second monitor left of or above the primary one
// the virtual screen starts at a negative Left/Top, so the pixel is made
// relative to that corner before it is scaled.
//
// The scale itself was measured rather than assumed, because the two plausible
// readings of "normalized to 0..65535" disagree by a pixel and the wrong one is
// invisible in the middle of a screen. Injecting the values where the two
// predictions differ and reading the pointer back showed Windows computing
//
//	pixel = Left + (value * Width) >> 16
//
// on this hardware, 5 samples to 0 against the round(value*(Width-1)/65535)
// reading. So one pixel is not one value but a whole interval of them, and the
// exact inverse is the middle of that interval: any value in
// [offset*65536/Width, (offset+1)*65536/Width) lands on the pixel, and aiming at
// its centre leaves the most room on both sides.
//
// A desktop wider than 65536 pixels would have fewer values than pixels and
// some columns would become unreachable. That is a limit of the Win32 API, not
// of this code, and no such desktop exists.
func (b Bounds) absolute(px, py int32) (int32, int32) {
	return absAxis(px-b.Left, b.Width), absAxis(py-b.Top, b.Height)
}

func absAxis(offset, size int32) int32 {
	if size <= 1 {
		return 0
	}
	v := math.Floor((float64(offset) + 0.5) * absoluteScale / float64(size))
	if v < 0 {
		return 0
	}
	if v > absoluteMax {
		return absoluteMax
	}
	return int32(v)
}

// Win32 numbers. They live in the portable file rather than the Windows one so
// that the tables built out of them, and the event to INPUT translation, stay
// testable on any OS.
const (
	absoluteMax    = 65535 // the largest value SendInput accepts
	absoluteScale  = 65536 // the divisor Windows scales by, measured; see Bounds.absolute
	wheelDeltaUnit = 120   // WHEEL_DELTA

	inputMouse    = 0
	inputKeyboard = 1

	mouseMove       = 0x0001
	mouseLeftDown   = 0x0002
	mouseLeftUp     = 0x0004
	mouseRightDown  = 0x0008
	mouseRightUp    = 0x0010
	mouseMiddleDown = 0x0020
	mouseMiddleUp   = 0x0040
	mouseXDown      = 0x0080
	mouseXUp        = 0x0100
	mouseWheel      = 0x0800
	mouseHWheel     = 0x1000
	mouseVirtualDsk = 0x4000
	mouseAbsolute   = 0x8000

	xbutton1 = 0x0001
	xbutton2 = 0x0002

	keyExtended = 0x0001
	keyUp       = 0x0002
	keyUnicode  = 0x0004
	keyScancode = 0x0008
)

type buttonCodes struct {
	down, up uint32
	data     uint32
}

var buttons = map[Button]buttonCodes{
	ButtonLeft:   {mouseLeftDown, mouseLeftUp, 0},
	ButtonRight:  {mouseRightDown, mouseRightUp, 0},
	ButtonMiddle: {mouseMiddleDown, mouseMiddleUp, 0},
	ButtonX1:     {mouseXDown, mouseXUp, xbutton1},
	ButtonX2:     {mouseXDown, mouseXUp, xbutton2},
}

// rawMouseInput mirrors Win32 MOUSEINPUT, the largest member of the INPUT union.
type rawMouseInput struct {
	dx, dy    int32
	mouseData uint32
	flags     uint32
	time      uint32
	extraInfo uintptr
}

// rawKeybdInput mirrors Win32 KEYBDINPUT. It shares its storage with
// rawMouseInput, exactly as the C union does, and has the same alignment, so
// overlaying it on rawInput.mi reproduces the C layout on every Windows
// architecture rather than only on amd64.
type rawKeybdInput struct {
	vk        uint16
	scan      uint16
	flags     uint32
	time      uint32
	extraInfo uintptr
}

// rawInput mirrors Win32 INPUT. Declaring the union as its largest member lets
// Go insert the same padding after the type tag that C does.
type rawInput struct {
	typ uint32
	mi  rawMouseInput
}

func mouseEvent(dx, dy int32, data, flags uint32) rawInput {
	return rawInput{typ: inputMouse, mi: rawMouseInput{dx: dx, dy: dy, mouseData: data, flags: flags}}
}

func keyEvent(scan uint16, flags uint32) rawInput {
	in := rawInput{typ: inputKeyboard}
	ki := (*rawKeybdInput)(unsafe.Pointer(&in.mi))
	ki.scan = scan
	ki.flags = flags
	return in
}

// eventInputs translates one validated event into the INPUT array to submit.
// surface is the part of the desktop the operator is looking at and virtual is
// the bounding rectangle of every monitor; the two are equal only when the whole
// desktop is being streamed.
//
// A positioned click becomes a move plus a button in a single array, not two
// calls: SendInput submits an array atomically with respect to other threads'
// input, so nothing can slip between the move and the click and make the click
// land somewhere else.
func eventInputs(ev Event, surface, virtual Bounds) []rawInput {
	var inputs []rawInput
	if ev.Kind != KindKey && ev.X != nil && ev.Y != nil {
		px, py := surface.pixel(*ev.X, *ev.Y)
		ax, ay := virtual.absolute(px, py)
		// MOUSEEVENTF_VIRTUALDESK is not optional. Without it Windows maps the
		// 0..65535 range onto the primary monitor alone, and every position on
		// a second monitor silently lands on the first one.
		inputs = append(inputs, mouseEvent(ax, ay, 0, mouseMove|mouseAbsolute|mouseVirtualDsk))
	}

	switch ev.Kind {
	case KindMouseMove:
		// The move above is the whole event.
	case KindMouseButton:
		codes := buttons[ev.Button]
		flags := codes.up
		if ev.Down {
			flags = codes.down
		}
		inputs = append(inputs, mouseEvent(0, 0, codes.data, flags))
	case KindMouseWheel:
		if ev.WheelY != 0 {
			inputs = append(inputs, mouseEvent(0, 0, wheelData(ev.WheelY), mouseWheel))
		}
		if ev.WheelX != 0 {
			inputs = append(inputs, mouseEvent(0, 0, wheelData(ev.WheelX), mouseHWheel))
		}
	case KindKey:
		inputs = append(inputs, keyInputs(ev)...)
	}
	return inputs
}

func keyInputs(ev Event) []rawInput {
	flags := uint32(0)
	if !ev.Down {
		flags |= keyUp
	}

	if ev.Scancode != 0 {
		scan, extended := normalizeScancode(ev.Scancode, ev.Extended)
		flags |= keyScancode
		if extended {
			flags |= keyExtended
		}
		return []rawInput{keyEvent(scan, flags)}
	}

	// KEYEVENTF_UNICODE carries UTF-16 code units, so anything outside the BMP
	// is two events that have to travel in one array to arrive as one character.
	flags |= keyUnicode
	units := utf16.Encode([]rune{ev.Unicode})
	inputs := make([]rawInput, 0, len(units))
	for _, unit := range units {
		inputs = append(inputs, keyEvent(unit, flags))
	}
	return inputs
}

// normalizeScancode accepts either form a sender might use — a bare make code
// with the extended flag alongside it, or the 0xE0-prefixed code browsers hand
// out for the arrow block, right Ctrl/Alt and the numpad Enter — and returns
// what SendInput actually wants: the low byte in wScan plus
// KEYEVENTF_EXTENDEDKEY. Passing 0xE04D straight through happens to work on
// current Windows but is nowhere documented to.
//
// The 0xE1 prefix (Pause alone uses it) is left as-is: it is a three-code
// sequence rather than a prefix and is not worth special-casing here.
func normalizeScancode(scan uint16, extended bool) (uint16, bool) {
	if scan>>8 == 0xE0 {
		return scan & 0xFF, true
	}
	return scan, extended
}

// wheelMaxNotches caps one wheel event, see wheelData.
const wheelMaxNotches = 50

// wheelData converts notches to the WHEEL_DELTA units mouseData carries,
// clamped to ±wheelMaxNotches. The hub's limiter counts events, not their
// size: without the clamp a single wheel_y:1e6 would scroll millions of lines.
// 50 notches is more than the fastest trackpad flick produces in one event.
func wheelData(notches float64) uint32 {
	if math.IsNaN(notches) {
		return 0
	}
	notches = max(-wheelMaxNotches, min(notches, wheelMaxNotches))
	return uint32(int32(math.Round(notches * wheelDeltaUnit)))
}

// Injector feeds validated events into the local input queue. It is safe for
// concurrent use, though a session should inject from one goroutine anyway:
// order is part of the meaning of key-down/key-up.
type Injector struct {
	mu      sync.Mutex
	surface Bounds
	held    heldSet
}

// heldKey identifies one physical key or button, whatever form the sender used
// to name it (0xE04D and 0x4D+extended are the same arrow key).
type heldKey struct {
	kind     Kind
	button   Button
	scancode uint16
	extended bool
	unicode  rune
}

// heldSet tracks what is currently pressed through this injector, so that
// ReleaseAll can undo it. Pure bookkeeping, no Win32: tested on any OS.
type heldSet map[heldKey]Event

func keyOf(ev Event) heldKey {
	k := heldKey{kind: ev.Kind, button: ev.Button}
	if ev.Kind == KindKey {
		if ev.Scancode != 0 {
			k.scancode, k.extended = normalizeScancode(ev.Scancode, ev.Extended)
		} else {
			k.unicode = ev.Unicode
		}
	}
	return k
}

// note records a press and forgets a release. Moves and wheels hold nothing.
func (h *heldSet) note(ev Event) {
	if ev.Kind != KindKey && ev.Kind != KindMouseButton {
		return
	}
	k := keyOf(ev)
	if !ev.Down {
		delete(*h, k)
		return
	}
	if *h == nil {
		*h = heldSet{}
	}
	up := Event{V: Version, Kind: ev.Kind, Button: ev.Button, Scancode: ev.Scancode, Extended: ev.Extended, Unicode: ev.Unicode}
	(*h)[k] = up
}

// releases returns the release event for everything held and forgets it all.
func (h *heldSet) releases() []Event {
	out := make([]Event, 0, len(*h))
	for _, up := range *h {
		out = append(out, up)
	}
	*h = nil
	return out
}

// New reports whether input injection is possible before the first event
// arrives, so a caller can disable the feature cleanly instead of failing per
// event.
func New() (*Injector, error) {
	if err := checkAvailable(); err != nil {
		return nil, err
	}
	return &Injector{}, nil
}

// SetSurface declares which rectangle of the virtual desktop the operator's
// video is showing, in the same pixel space VirtualScreen reports. Until it is
// called the whole virtual desktop is assumed, which is right for a
// single-monitor host and wrong the moment a second monitor appears. The agent
// does not call it yet: capture.OutputInfo carries no Left/Top to call it with
// (see the ponytail in oo-agent dialWebRTC), so multi-monitor input stays off.
//
// Both rectangles must come from a process with the same DPI awareness — a mix
// of physical DXGI bounds and DPI-virtualised metrics puts the pointer in the
// wrong place on a scaled display. The agent satisfies this by declaring
// per-monitor awareness in agent/capture's init.
func (in *Injector) SetSurface(b Bounds) error {
	if !b.valid() {
		return fmt.Errorf("%w: surface %+v has no area", ErrInvalidEvent, b)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	in.surface = b
	return nil
}

func (in *Injector) surfaceOr(fallback Bounds) Bounds {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.surface.valid() {
		return fallback
	}
	return in.surface
}

// Inject validates ev and submits it to Windows. A rejected event yields
// ErrInvalidEvent and nothing is sent; an event Windows refuses yields
// ErrBlocked, which for the cases listed in the package comment is expected
// rather than broken.
func (in *Injector) Inject(ev Event) error {
	if err := ev.Validate(); err != nil {
		return err
	}
	if ev.Kind == KindReleaseAll {
		return in.ReleaseAll()
	}
	err := in.inject(ev)
	// A press Windows refused is not held; a release is forgotten either way,
	// or ReleaseAll would keep retrying a key that is already up.
	if err == nil || !ev.Down {
		in.mu.Lock()
		in.held.note(ev)
		in.mu.Unlock()
	}
	return err
}

// ReleaseAll sends a release for every key and button still held through this
// injector. The agent calls it when the input channel closes; the hub asks for
// it (KindReleaseAll) when a controlling viewer leaves.
func (in *Injector) ReleaseAll() error {
	in.mu.Lock()
	ups := in.held.releases()
	in.mu.Unlock()
	var first error
	for _, up := range ups {
		if err := in.inject(up); err != nil && first == nil {
			first = err
		}
	}
	return first
}
