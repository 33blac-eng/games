package cursorproto

import (
	"bytes"
	"errors"
	"image/png"
	"testing"
	"time"
)

func TestPosRoundTrip(t *testing.T) {
	for _, p := range []Pos{
		{},
		{Visible: true, ShapeID: 0xdeadbeef, X: 1919, Y: 1079, FrameW: 1920, FrameH: 1080},
		{Visible: false, ShapeID: 1, X: -5, Y: -7, FrameW: 65535, FrameH: 1},
	} {
		b := EncodePos(p)
		if len(b) != PosSize || Kind(b) != KindPos {
			t.Fatalf("bad encode %v", b)
		}
		got, err := DecodePos(b)
		if err != nil || got != p {
			t.Fatalf("round trip %+v -> %+v, %v", p, got, err)
		}
		if Validate(b) != nil {
			t.Fatal("validate rejected good pos")
		}
	}
}

func TestPosRejects(t *testing.T) {
	good := EncodePos(Pos{Visible: true, X: 1})
	cases := map[string]struct {
		b   []byte
		err error
	}{
		"short": {good[:19], ErrShort},
		"long":  {append(append([]byte{}, good...), 0), ErrTooLarge},
		"magic": {append([]byte{'X'}, good[1:]...), ErrMagic},
		"kind":  {append([]byte{'C', 9}, good[2:]...), ErrKind},
	}
	for name, c := range cases {
		if _, err := DecodePos(c.b); !errors.Is(err, c.err) {
			t.Errorf("%s: got %v want %v", name, err, c.err)
		}
	}
}

func rgba(w, h int, fill byte) []byte {
	b := make([]byte, w*h*4)
	for i := range b {
		b[i] = fill
	}
	return b
}

func TestShapeRoundTrip(t *testing.T) {
	px := rgba(32, 32, 0x80)
	s := Shape{ID: ShapeID(32, 32, 3, 4, px), Format: FormatRGBA, W: 32, H: 32, HotX: 3, HotY: 4, Data: px}
	b, err := EncodeShape(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeShape(b)
	if err != nil || got.ID != s.ID || got.W != 32 || got.HotY != 4 || !bytes.Equal(got.Data, px) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if Validate(b) != nil {
		t.Fatal("validate rejected good shape")
	}
}

func TestShapeLimits(t *testing.T) {
	px := rgba(1, 1, 1)
	bad := []struct {
		s   Shape
		err error
	}{
		{Shape{ID: 0, W: 1, H: 1, Data: px}, ErrID},
		{Shape{ID: 1, W: 0, H: 1, Data: px}, ErrDims},
		{Shape{ID: 1, W: 257, H: 1, Data: rgba(257, 1, 0)}, ErrDims},
		{Shape{ID: 1, W: 1, H: 257, Data: rgba(1, 257, 0)}, ErrDims},
		{Shape{ID: 1, W: 1, H: 1, HotX: 1, Data: px}, ErrHotspot},
		{Shape{ID: 1, W: 2, H: 1, Data: px}, ErrDataLen},
		{Shape{ID: 1, W: 1, H: 1, Format: 7, Data: px}, ErrFormat},
		// 256x256 raw RGBA is 256 KiB: over MaxMessage.
		{Shape{ID: 1, W: 256, H: 256, Data: rgba(256, 256, 0)}, ErrTooLarge},
	}
	for i, c := range bad {
		if _, err := EncodeShape(c.s); !errors.Is(err, c.err) {
			t.Errorf("case %d: got %v want %v", i, err, c.err)
		}
	}
	if _, err := DecodeShape(make([]byte, MaxMessage+1)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversize decode: %v", err)
	}
	if Validate([]byte{1}) == nil || Validate([]byte("Cz")) == nil {
		t.Error("validate accepted junk")
	}
}

func TestBuildShapeFitsAndIsStable(t *testing.T) {
	// Worst case: 256x256 incompressible noise must still fit one message.
	px := make([]byte, 256*256*4)
	seed := uint32(1)
	for i := range px {
		seed = seed*1664525 + 1013904223
		px[i] = byte(seed >> 24)
	}
	s, err := BuildShape(px, 256, 256, 200, 100)
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeShape(s)
	if err != nil || len(b) > MaxMessage {
		t.Fatalf("does not fit: %d %v", len(b), err)
	}
	if s.HotX >= s.W || s.HotY >= s.H {
		t.Fatal("hotspot outside after downscale")
	}
	s2, _ := BuildShape(px, 256, 256, 200, 100)
	if s2.ID != s.ID {
		t.Fatal("id not stable")
	}
	// A typical arrow compresses to PNG.
	small, err := BuildShape(rgba(32, 32, 0), 32, 32, 0, 0)
	if err != nil || small.Format != FormatPNG {
		t.Fatalf("small shape: %v fmt=%d", err, small.Format)
	}
	img, err := png.Decode(bytes.NewReader(small.Data))
	if err != nil || img.Bounds().Dx() != 32 {
		t.Fatalf("png: %v", err)
	}
	// Oversize input (300x300) is cropped to MaxDim.
	big, err := BuildShape(rgba(300, 300, 0), 300, 300, 299, 10)
	if err != nil || big.W != MaxDim || big.HotX != MaxDim-1 {
		t.Fatalf("crop: %+v %v", big, err)
	}
}

func TestFromDXGIMonochrome(t *testing.T) {
	// 8x2 visible, pitch 1: AND rows then XOR rows.
	data := []byte{
		0b11110000, 0b00000000, // AND
		0b10100000, 0b00001111, // XOR
	}
	out, h, ok := FromDXGI(DXGIMonochrome, 8, 4, 1, data)
	if !ok || h != 2 || len(out) != 8*2*4 {
		t.Fatalf("ok=%v h=%d", ok, h)
	}
	px := func(x, y int) [4]byte { o := (y*8 + x) * 4; return [4]byte(out[o : o+4]) }
	if px(0, 0) != [4]byte{0, 0, 0, 255} { // AND1 XOR1 -> invert -> black
		t.Errorf("invert: %v", px(0, 0))
	}
	if px(1, 0)[3] != 0 { // AND1 XOR0 -> transparent
		t.Errorf("transparent: %v", px(1, 0))
	}
	if px(4, 0) != [4]byte{0, 0, 0, 255} { // AND0 XOR0 -> black
		t.Errorf("black: %v", px(4, 0))
	}
	if px(4, 1) != [4]byte{255, 255, 255, 255} { // AND0 XOR1 -> white
		t.Errorf("white: %v", px(4, 1))
	}
	if _, _, ok := FromDXGI(DXGIMonochrome, 8, 4, 1, data[:3]); ok {
		t.Error("accepted short buffer")
	}
}

func TestFromDXGIColor(t *testing.T) {
	// 1x1 BGRA with pitch 8 (padding).
	out, h, ok := FromDXGI(DXGIColor, 1, 1, 8, []byte{1, 2, 3, 4})
	if !ok || h != 1 || !bytes.Equal(out, []byte{3, 2, 1, 4}) {
		t.Fatalf("color: %v %v", out, ok)
	}
	m, _, ok := FromDXGI(DXGIMaskedColor, 2, 1, 8, []byte{1, 2, 3, 0, 0, 0, 0, 0xff})
	if !ok || !bytes.Equal(m, []byte{3, 2, 1, 255, 0, 0, 0, 0}) {
		t.Fatalf("masked: %v", m)
	}
	if _, _, ok := FromDXGI(9, 1, 1, 4, []byte{0, 0, 0, 0}); ok {
		t.Error("accepted unknown kind")
	}
}

func TestCoalescer(t *testing.T) {
	var c Coalescer
	t0 := time.Unix(0, 0)
	p := Pos{Visible: true, X: 1}
	c.Offer(p)
	if _, ok := c.Due(t0); !ok {
		t.Fatal("first sample not sent")
	}
	c.Offer(p)
	if _, ok := c.Due(t0.Add(time.Second)); ok {
		t.Fatal("unchanged sample sent")
	}
	c.Offer(Pos{Visible: true, X: 2})
	if _, ok := c.Due(t0.Add(time.Millisecond)); ok {
		t.Fatal("sent inside coalesce window")
	}
	c.Offer(Pos{Visible: true, X: 3})
	got, ok := c.Due(t0.Add(CoalesceInterval))
	if !ok || got.X != 3 {
		t.Fatalf("latest not flushed: %+v %v", got, ok)
	}
	c.Reset()
	c.Offer(Pos{Visible: true, X: 3})
	if _, ok := c.Due(t0.Add(CoalesceInterval)); !ok {
		t.Fatal("reset did not resend")
	}
}
