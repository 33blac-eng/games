package h264

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// SPS-вектори згенеровано libx264 (ffmpeg -c:v libx264 ...), поля звірено
// з ffprobe. Усі без стартового коду.
var spsVectors = []struct {
	name                 string
	hex                  string
	profile, level, w, h int
	vui, colour, full    bool
	prim, trc, mat       int
}{
	// -profile:v main 1920x1080, без колірного опису; є emulation prevention, cropping 1088->1080
	{"main1080", "674d4028eca03c0113f2e022000003000200000300781e30632c", 77, 40, 1920, 1080, true, false, false, 2, 2, 2},
	// -profile:v high 1280x720 bt709 + color_range pc
	{"high720full", "6764001facd9405005bb016e02020280000003008000001e078c18cb", 100, 31, 1280, 720, true, true, true, 1, 1, 1},
	// -profile:v baseline 640x360 bt470bg/smpte170m, tv range
	{"base360_601", "6742c01ed900a02ff97016a0a0c0a8000003000800000301e078b17240", 66, 30, 640, 360, true, true, false, 5, 6, 5},
	// -profile:v high 1366x768 (cropping по ширині: 1376->1366)
	{"high1366", "67640020acd94056061e6f0110000003001000000303c0f1831960", 100, 32, 1366, 768, true, false, false, 2, 2, 2},
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseSPSVectors(t *testing.T) {
	for _, v := range spsVectors {
		s, err := ParseSPS(mustHex(t, v.hex))
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if int(s.ProfileIDC) != v.profile || int(s.LevelIDC) != v.level || s.Width != v.w || s.Height != v.h {
			t.Errorf("%s: got %d/%d %dx%d", v.name, s.ProfileIDC, s.LevelIDC, s.Width, s.Height)
		}
		if s.VUIPresent != v.vui || s.ColourDescriptionPresent != v.colour || s.FullRange != v.full ||
			s.ColourPrimaries != v.prim || s.TransferCharacteristics != v.trc || s.MatrixCoefficients != v.mat {
			t.Errorf("%s: VUI %+v", v.name, s)
		}
	}
}

// Мінімальний SPS без VUI, закодований вручну: Baseline(66) level 3.0,
// sps_id=0, log2_max_frame_num-4=0, poc_type=2, refs=1, gaps=0,
// 20x15 MB (320x240), frame_mbs_only=1, direct8x8=1, crop=0, vui=0, stop.
// Біти: ue(0)=1 ue(0)=1 ue(2)=011 ue(1)=010 0 ue(19)=000010100 ue(14)=0001111
// 1 1 0 0 1 -> 1 1 011 010 0 000010100 0001111 1 1 0 0 | 1 (stop) + align.
func handSPSNoVUI(t *testing.T) []byte {
	w := &bitWriter{}
	for _, f := range []struct{ v, n int }{{66, 8}, {0, 8}, {30, 8},
		{1, 1}, {1, 1}, {3, 3}, {2, 3}, {0, 1}, {0x14, 9}, {0x0F, 7}, {1, 1}, {1, 1}, {0, 1}, {0, 1}, {1, 1}} {
		w.put(f.v, f.n)
	}
	for w.n%8 != 0 {
		w.put(0, 1)
	}
	return append([]byte{0x67}, escapeRBSP(w.b)...)
}

func TestParseHandSPSNoVUI(t *testing.T) {
	s, err := ParseSPS(handSPSNoVUI(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.ProfileIDC != 66 || s.LevelIDC != 30 || s.Width != 320 || s.Height != 240 || s.VUIPresent {
		t.Fatalf("got %+v", s)
	}
}

func checkRewrite(t *testing.T, name string, in []byte) {
	t.Helper()
	orig, err := ParseSPS(in)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out, err := RewriteSPSColourBT709(in)
	if err != nil {
		t.Fatalf("%s: rewrite: %v", name, err)
	}
	// у виході не має бути неекранованих 00 00 0x (x<=3)
	if bytes.Contains(out, []byte{0, 0, 0}) || bytes.Contains(out, []byte{0, 0, 1}) || bytes.Contains(out, []byte{0, 0, 2}) {
		t.Fatalf("%s: start-code emulation in output %x", name, out)
	}
	got, err := ParseSPS(out)
	if err != nil {
		t.Fatalf("%s: reparse: %v", name, err)
	}
	if !got.VUIPresent || !got.VideoSignalTypePresent || !got.ColourDescriptionPresent || got.FullRange ||
		got.ColourPrimaries != 1 || got.TransferCharacteristics != 1 || got.MatrixCoefficients != 1 {
		t.Fatalf("%s: colour not BT.709 limited: %+v", name, got)
	}
	if got.ProfileIDC != orig.ProfileIDC || got.ConstraintFlags != orig.ConstraintFlags || got.LevelIDC != orig.LevelIDC ||
		got.Width != orig.Width || got.Height != orig.Height || got.vuiFlagPos != orig.vuiFlagPos {
		t.Fatalf("%s: non-VUI fields changed: %+v vs %+v", name, got, orig)
	}
	if orig.VideoSignalTypePresent && got.VideoFormat != orig.VideoFormat {
		t.Fatalf("%s: video_format changed", name)
	}
	// усі біти до vui-прапорця — біт-у-біт
	a, b := unescapeRBSP(in[1:]), unescapeRBSP(out[1:])
	ra, rb := &bitReader{b: a}, &bitReader{b: b}
	for i := 0; i < orig.vuiFlagPos; i++ {
		if ra.bits(1) != rb.bits(1) {
			t.Fatalf("%s: bit %d before VUI differs", name, i)
		}
	}
	if out[0] != in[0] {
		t.Fatalf("%s: NAL header changed", name)
	}
	// ідемпотентність
	out2, err := RewriteSPSColourBT709(out)
	if err != nil || !bytes.Equal(out, out2) {
		t.Fatalf("%s: not idempotent: %x vs %x (%v)", name, out, out2, err)
	}
}

func TestRewriteSPSColour(t *testing.T) {
	for _, v := range spsVectors {
		checkRewrite(t, v.name, mustHex(t, v.hex))
	}
	checkRewrite(t, "hand-noVUI", handSPSNoVUI(t))
}

// Хвіст VUI (timing_info тощо) зберігається: для main1080 перевіряємо, що
// біти після video_signal_type у старому й новому SPS однакові.
func TestRewritePreservesVUITail(t *testing.T) {
	in := mustHex(t, spsVectors[0].hex) // VUI без video_signal_type
	out, err := RewriteSPSColourBT709(in)
	if err != nil {
		t.Fatal(err)
	}
	a, b := unescapeRBSP(in[1:]), unescapeRBSP(out[1:])
	s, _ := ParseSPS(in)
	// старий: ... vuiflag aspect(1[+8/40]) overscan(1[+1]) vst=0 | tail
	// новий: те саме, але vst займає 1+3+1+1+24 = 30 біт замість 1
	ra := &bitReader{b: a, pos: s.vuiFlagPos + 1}
	if ra.bits(1) == 1 {
		if ra.bits(8) == 255 {
			ra.bits(32)
		}
	}
	if ra.bits(1) == 1 {
		ra.bits(1)
	}
	ra.bits(1) // vst flag = 0
	tailA := ra.pos
	tailB := tailA + 29
	sa, sb := lastOneBit(a), lastOneBit(b)
	if sa-tailA != sb-tailB {
		t.Fatalf("tail length differs: %d vs %d", sa-tailA, sb-tailB)
	}
	rb := &bitReader{b: b, pos: tailB}
	for i := tailA; i < sa; i++ {
		if ra.bits(1) != rb.bits(1) {
			t.Fatalf("tail bit %d differs", i)
		}
	}
}

func TestRewriteAnnexB(t *testing.T) {
	sps := mustHex(t, spsVectors[0].hex)
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	idr := []byte{0x65, 0x88, 0x84, 0x00}
	var in []byte
	for _, n := range [][]byte{sps, pps, idr} {
		in = append(in, 0, 0, 0, 1)
		in = append(in, n...)
	}
	out, err := RewriteAnnexBSPSColourBT709(in)
	if err != nil {
		t.Fatal(err)
	}
	nals := SplitNALs(out)
	if len(nals) != 3 || !bytes.Equal(nals[1], pps) || !bytes.Equal(nals[2], idr) {
		t.Fatalf("non-SPS NALs changed: %x", out)
	}
	s, _ := ParseSPS(nals[0])
	if !s.ColourDescriptionPresent || s.MatrixCoefficients != 1 {
		t.Fatal("SPS not rewritten")
	}
	noSPS := in[len(sps)+4:]
	if o, _ := RewriteAnnexBSPSColourBT709(noSPS); &o[0] != &noSPS[0] {
		t.Fatal("buffer without SPS must pass through untouched")
	}
}
