package input

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unsafe"
)

// windowsPixel is the mapping Windows applies to an absolute coordinate. It was
// measured on real hardware, not taken from the documentation, which only says
// "normalized between 0 and 65,535": values where this formula and the other
// plausible reading of that sentence predict different pixels were injected and
// the pointer read back, and this one won 5 to 0.
//
// Running our conversion back through it is what proves a position lands on the
// pixel the operator actually clicked rather than merely producing a
// plausible-looking number.
func windowsPixel(value, origin, size int32) int32 {
	return origin + int32((int64(value)*int64(size))>>16)
}

func TestNormalizedToAbsolute(t *testing.T) {
	var (
		single = Bounds{Left: 0, Top: 0, Width: 1920, Height: 1080}
		// A second monitor to the LEFT of the primary one: the virtual screen
		// starts at -1920, which is the case that breaks any code assuming the
		// desktop begins at 0,0.
		wideVirtual = Bounds{Left: -1920, Top: 0, Width: 3840, Height: 1080}
		leftMonitor = Bounds{Left: -1920, Top: 0, Width: 1920, Height: 1080}
		// A monitor ABOVE the primary one: negative Top.
		tallVirtual = Bounds{Left: 0, Top: -1080, Width: 1920, Height: 2160}
		topMonitor  = Bounds{Left: 0, Top: -1080, Width: 1920, Height: 1080}
	)

	cases := []struct {
		name             string
		surface, virtual Bounds
		x, y             float64
		wantX, wantY     int32
	}{
		{"single top-left", single, single, 0, 0, 0, 0},
		{"single centre", single, single, 0.5, 0.5, 960, 540},
		{"single bottom-right", single, single, 1, 1, 1919, 1079},

		{"left monitor top-left", leftMonitor, wideVirtual, 0, 0, -1920, 0},
		{"left monitor centre", leftMonitor, wideVirtual, 0.5, 0.5, -960, 540},
		{"left monitor bottom-right", leftMonitor, wideVirtual, 1, 1, -1, 1079},

		{"primary beside a left monitor", single, wideVirtual, 0, 0, 0, 0},
		{"primary beside a left monitor, far corner", single, wideVirtual, 1, 1, 1919, 1079},

		{"monitor above, top-left", topMonitor, tallVirtual, 0, 0, 0, -1080},
		{"monitor above, bottom-right", topMonitor, tallVirtual, 1, 1, 1919, -1},

		// Whole desktop streamed rather than one monitor.
		{"whole wide desktop centre", wideVirtual, wideVirtual, 0.5, 0.5, 0, 540},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			px, py := tc.surface.pixel(tc.x, tc.y)
			if px != tc.wantX || py != tc.wantY {
				t.Fatalf("pixel(%v,%v) = %d,%d, want %d,%d", tc.x, tc.y, px, py, tc.wantX, tc.wantY)
			}

			ax, ay := tc.virtual.absolute(px, py)
			if ax < 0 || ax > absoluteMax || ay < 0 || ay > absoluteMax {
				t.Fatalf("absolute(%d,%d) = %d,%d, outside 0..%d", px, py, ax, ay, absoluteMax)
			}

			gotX := windowsPixel(ax, tc.virtual.Left, tc.virtual.Width)
			gotY := windowsPixel(ay, tc.virtual.Top, tc.virtual.Height)
			if gotX != tc.wantX || gotY != tc.wantY {
				t.Fatalf("Windows maps %d,%d back to %d,%d, want %d,%d", ax, ay, gotX, gotY, tc.wantX, tc.wantY)
			}
		})
	}
}

// TestAbsoluteRoundTripEveryPixel walks a whole axis to show the conversion is
// exact everywhere, not just at the corners a table happens to name.
func TestAbsoluteRoundTripEveryPixel(t *testing.T) {
	virtual := Bounds{Left: -1920, Top: -200, Width: 3840, Height: 1280}
	for px := virtual.Left; px < virtual.Left+virtual.Width; px++ {
		ax, _ := virtual.absolute(px, virtual.Top)
		if got := windowsPixel(ax, virtual.Left, virtual.Width); got != px {
			t.Fatalf("pixel %d -> absolute %d -> pixel %d", px, ax, got)
		}
	}
}

func TestParseEvent(t *testing.T) {
	ev, err := ParseEvent([]byte(`{"v":1,"type":"mouse_move","x":0,"y":0.75}`))
	if err != nil {
		t.Fatalf("ParseEvent() error = %v", err)
	}
	if ev.X == nil || *ev.X != 0 {
		t.Fatalf("x = %v, want a present 0 (an absent field must not decode to the top-left corner)", ev.X)
	}
	if ev.Y == nil || *ev.Y != 0.75 {
		t.Fatalf("y = %v, want 0.75", ev.Y)
	}

	// A field a newer browser adds must not break an older agent.
	if _, err := ParseEvent([]byte(`{"v":1,"type":"key","scancode":30,"down":true,"pressure":0.4}`)); err != nil {
		t.Fatalf("unknown field rejected: %v", err)
	}
}

func TestParseEventRejects(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{"bad json", `{`, "unexpected end"},
		{"wrong version", `{"v":2,"type":"mouse_move","x":0.5,"y":0.5}`, "version 2"},
		{"unknown type", `{"v":1,"type":"touch"}`, `unknown type "touch"`},
		{"move without position", `{"v":1,"type":"mouse_move"}`, "without a position"},
		{"half a position", `{"v":1,"type":"mouse_move","x":0.5}`, "must be given together"},
		{"x above one", `{"v":1,"type":"mouse_move","x":1.5,"y":0.5}`, "outside 0..1"},
		{"y below zero", `{"v":1,"type":"mouse_move","x":0.5,"y":-0.0001}`, "outside 0..1"},
		{"unknown button", `{"v":1,"type":"mouse_button","button":"scroll","down":true}`, "unknown button"},
		{"missing button", `{"v":1,"type":"mouse_button","down":true}`, "unknown button"},
		{"wheel of zero", `{"v":1,"type":"mouse_wheel"}`, "scrolls nothing"},
		{"key with nothing", `{"v":1,"type":"key","down":true}`, "neither scancode nor unicode"},
		{"lone surrogate", `{"v":1,"type":"key","unicode":55296,"down":true}`, "not a code point"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseEvent([]byte(tc.message))
			if !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("errors.Is(%v, ErrInvalidEvent) = false", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// NaN cannot be written as JSON, so the guard against it is checked directly.
func TestValidateRejectsNaN(t *testing.T) {
	nan := 0.0
	nan = nan / nan
	if err := (Event{V: Version, Kind: KindMouseMove, X: &nan, Y: &nan}).Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("NaN position accepted: %v", err)
	}
	if err := (Event{V: Version, Kind: KindMouseWheel, WheelY: nan}).Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("NaN wheel accepted: %v", err)
	}
}

func TestEventInputs(t *testing.T) {
	surface := Bounds{Left: -1920, Top: 0, Width: 1920, Height: 1080}
	virtual := Bounds{Left: -1920, Top: 0, Width: 3840, Height: 1080}
	pos := func(v float64) *float64 { return &v }

	t.Run("positioned click moves and presses in one array", func(t *testing.T) {
		inputs := eventInputs(Event{
			V: Version, Kind: KindMouseButton, Button: ButtonLeft, Down: true,
			X: pos(0), Y: pos(0),
		}, surface, virtual)

		if len(inputs) != 2 {
			t.Fatalf("got %d inputs, want move + button", len(inputs))
		}
		if inputs[0].mi.flags != mouseMove|mouseAbsolute|mouseVirtualDsk {
			t.Fatalf("move flags = %#x, want MOVE|ABSOLUTE|VIRTUALDESK", inputs[0].mi.flags)
		}
		gotX := windowsPixel(inputs[0].mi.dx, virtual.Left, virtual.Width)
		gotY := windowsPixel(inputs[0].mi.dy, virtual.Top, virtual.Height)
		if gotX != -1920 || gotY != 0 {
			t.Fatalf("click at the top-left of the left monitor lands on %d,%d, want -1920,0", gotX, gotY)
		}
		if inputs[1].mi.flags != mouseLeftDown {
			t.Fatalf("button flags = %#x, want LEFTDOWN", inputs[1].mi.flags)
		}
	})

	t.Run("unpositioned release does not move the pointer", func(t *testing.T) {
		inputs := eventInputs(Event{V: Version, Kind: KindMouseButton, Button: ButtonRight}, surface, virtual)
		if len(inputs) != 1 || inputs[0].mi.flags != mouseRightUp {
			t.Fatalf("got %+v, want a single RIGHTUP", inputs)
		}
	})

	t.Run("side button carries its index", func(t *testing.T) {
		inputs := eventInputs(Event{V: Version, Kind: KindMouseButton, Button: ButtonX2, Down: true}, surface, virtual)
		if inputs[0].mi.mouseData != xbutton2 || inputs[0].mi.flags != mouseXDown {
			t.Fatalf("x2 = flags %#x data %#x, want XDOWN/XBUTTON2", inputs[0].mi.flags, inputs[0].mi.mouseData)
		}
	})

	t.Run("wheel scales to WHEEL_DELTA", func(t *testing.T) {
		inputs := eventInputs(Event{V: Version, Kind: KindMouseWheel, WheelY: -2, WheelX: 1}, surface, virtual)
		if len(inputs) != 2 {
			t.Fatalf("got %d inputs, want vertical + horizontal", len(inputs))
		}
		if int32(inputs[0].mi.mouseData) != -240 || inputs[0].mi.flags != mouseWheel {
			t.Fatalf("vertical = flags %#x data %d, want WHEEL/-240", inputs[0].mi.flags, int32(inputs[0].mi.mouseData))
		}
		if int32(inputs[1].mi.mouseData) != 120 || inputs[1].mi.flags != mouseHWheel {
			t.Fatalf("horizontal = flags %#x data %d, want HWHEEL/120", inputs[1].mi.flags, int32(inputs[1].mi.mouseData))
		}
	})

	t.Run("key goes as a scancode, never a virtual key", func(t *testing.T) {
		inputs := eventInputs(Event{V: Version, Kind: KindKey, Scancode: 0x1E, Down: true}, surface, virtual)
		ki := (*rawKeybdInput)(unsafe.Pointer(&inputs[0].mi))
		if inputs[0].typ != inputKeyboard || ki.vk != 0 || ki.scan != 0x1E {
			t.Fatalf("got type %d vk %#x scan %#x, want a keyboard event with vk 0 and scan 0x1E", inputs[0].typ, ki.vk, ki.scan)
		}
		if ki.flags != keyScancode {
			t.Fatalf("flags = %#x, want SCANCODE alone", ki.flags)
		}
	})

	t.Run("0xE0-prefixed key becomes low byte plus EXTENDEDKEY", func(t *testing.T) {
		inputs := eventInputs(Event{V: Version, Kind: KindKey, Scancode: 0xE04D}, surface, virtual)
		ki := (*rawKeybdInput)(unsafe.Pointer(&inputs[0].mi))
		if ki.scan != 0x4D {
			t.Fatalf("scan = %#x, want 0x4D", ki.scan)
		}
		if ki.flags != keyScancode|keyExtended|keyUp {
			t.Fatalf("flags = %#x, want SCANCODE|EXTENDEDKEY|KEYUP", ki.flags)
		}
	})

	t.Run("astral code point becomes a surrogate pair in one array", func(t *testing.T) {
		inputs := eventInputs(Event{V: Version, Kind: KindKey, Unicode: 0x1F600, Down: true}, surface, virtual)
		if len(inputs) != 2 {
			t.Fatalf("got %d inputs, want a surrogate pair", len(inputs))
		}
		first := (*rawKeybdInput)(unsafe.Pointer(&inputs[0].mi))
		second := (*rawKeybdInput)(unsafe.Pointer(&inputs[1].mi))
		if first.scan != 0xD83D || second.scan != 0xDE00 {
			t.Fatalf("units = %#x,%#x, want 0xD83D,0xDE00", first.scan, second.scan)
		}
		if first.flags != keyUnicode || second.flags != keyUnicode {
			t.Fatalf("flags = %#x,%#x, want UNICODE on both", first.flags, second.flags)
		}
	})

	t.Run("scancode wins over unicode so the layout cannot rewrite the key", func(t *testing.T) {
		// Operator on a US layout presses A; a sender that also filled unicode
		// with the Ukrainian letter its own layout produced must not win.
		inputs := eventInputs(Event{V: Version, Kind: KindKey, Scancode: 0x1E, Unicode: 'ф', Down: true}, surface, virtual)
		ki := (*rawKeybdInput)(unsafe.Pointer(&inputs[0].mi))
		if len(inputs) != 1 || ki.scan != 0x1E || ki.flags&keyUnicode != 0 {
			t.Fatalf("got %d inputs, scan %#x flags %#x, want the scancode path", len(inputs), ki.scan, ki.flags)
		}
	})
}

// A hostile wheel_y:1e6 must not become millions of lines of scrolling: the
// hub rate-limits events, not their size. Remove the clamp and this fails.
func TestWheelDataClamps(t *testing.T) {
	limit := int32(wheelMaxNotches * wheelDeltaUnit)
	if got := wheelData(1e6); int32(got) != limit {
		t.Fatalf("wheelData(1e6) = %d, want it clamped to %d", int32(got), limit)
	}
	if got := wheelData(-1e12); int32(got) != -limit {
		t.Fatalf("wheelData(-1e12) = %d, want it clamped to %d", int32(got), -limit)
	}
	if got := wheelData(1.5); int32(got) != 180 {
		t.Fatalf("wheelData(1.5) = %d, want 180", int32(got))
	}
}

// A viewer that disappears with Ctrl and the left button held never sends the
// releases. The injector must remember what it pressed so ReleaseAll can undo
// exactly that - no more (moves, wheels, released keys) and no less.
func TestHeldStateReleasesWhatIsStillDown(t *testing.T) {
	var h heldState
	h.track(Event{V: Version, Kind: KindKey, Scancode: 0x1D, Down: true})
	h.track(Event{V: Version, Kind: KindKey, Scancode: 0xE04D, Down: true})
	h.track(Event{V: Version, Kind: KindMouseButton, Button: ButtonLeft, Down: true})
	h.track(Event{V: Version, Kind: KindMouseWheel, WheelY: 1})
	// The arrow released under its other spelling is the same physical key.
	h.track(Event{V: Version, Kind: KindKey, Scancode: 0x4D, Extended: true})

	if ups := h.releaseInputs(); len(ups) != 2 {
		t.Fatalf("releases = %+v, want Ctrl and the left button", ups)
	}
	if again := h.releaseInputs(); len(again) != 0 {
		t.Fatalf("second ReleaseAll would re-send %+v", again)
	}
}

func TestReleaseAllParses(t *testing.T) {
	ev, err := ParseEvent([]byte(`{"v":1,"type":"release_all"}`))
	if err != nil || ev.Kind != KindReleaseAll {
		t.Fatalf("release_all: %+v %v", ev, err)
	}
}

// TestRawInputLayout guards the struct against Win32 INPUT. A wrong size makes
// SendInput read the array at the wrong stride, which corrupts every event
// after the first without any error to show for it.
func TestRawInputLayout(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout expectations below are the 64-bit ones")
	}
	if got := unsafe.Sizeof(rawInput{}); got != 40 {
		t.Fatalf("sizeof(INPUT) = %d, want 40", got)
	}
	if got := unsafe.Offsetof(rawInput{}.mi); got != 8 {
		t.Fatalf("union offset = %d, want 8", got)
	}
	if got := unsafe.Sizeof(rawMouseInput{}); got != 32 {
		t.Fatalf("sizeof(MOUSEINPUT) = %d, want 32", got)
	}
	if got := unsafe.Sizeof(rawKeybdInput{}); got != 24 {
		t.Fatalf("sizeof(KEYBDINPUT) = %d, want 24", got)
	}
	if unsafe.Sizeof(rawKeybdInput{}) > unsafe.Sizeof(rawMouseInput{}) {
		t.Fatal("KEYBDINPUT overlaid on MOUSEINPUT would write past the union")
	}
}

func TestSetSurfaceRejectsEmpty(t *testing.T) {
	in := &Injector{}
	if err := in.SetSurface(Bounds{Left: 10, Top: 10}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("empty surface accepted: %v", err)
	}
	fallback := Bounds{Left: 0, Top: 0, Width: 800, Height: 600}
	if got := in.surfaceOr(fallback); got != fallback {
		t.Fatalf("surfaceOr() = %+v, want the fallback %+v", got, fallback)
	}
}

// The wire form is what the browser will produce, so it is worth pinning.
func TestEventJSONShape(t *testing.T) {
	x := 0.25
	encoded, err := json.Marshal(Event{V: Version, Kind: KindMouseMove, X: &x, Y: &x})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	const want = `{"v":1,"type":"mouse_move","x":0.25,"y":0.25}`
	if string(encoded) != want {
		t.Fatalf("encoded = %s, want %s", encoded, want)
	}
}
