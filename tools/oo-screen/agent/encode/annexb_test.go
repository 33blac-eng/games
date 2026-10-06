package encode

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/organicoils/oo-screen/internal/h264"
)

// hasSPS decides whether a keyframe gets the cached SPS/PPS prefixed. A false
// negative doubles the header (harmless); a false positive ships an IDR with no
// SPS, which a fresh viewer cannot decode at all — a grey screen. Both start
// code lengths, a zero_byte run, and a start code cut off at the buffer end
// are the shapes MFTs actually emit.
func TestHasSPS(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
		want bool
	}{
		{"4-byte start code", []byte{0, 0, 0, 1, 0x67, 0x4D, 0, 0x1F}, true},
		{"3-byte start code", []byte{0, 0, 1, 0x67, 0x4D}, true},
		{"SPS after AUD and SEI", []byte{0, 0, 0, 1, 0x09, 0xF0, 0, 0, 1, 0x06, 0x05, 0, 0, 0, 1, 0x67, 0x64}, true},
		{"extra leading zero_byte", []byte{0, 0, 0, 0, 1, 0x67}, true},
		{"SPS with nal_ref_idc bits cleared", []byte{0, 0, 1, 0x07}, true},
		{"IDR only", []byte{0, 0, 0, 1, 0x65, 0x88, 0x84}, false},
		{"PPS only", []byte{0, 0, 0, 1, 0x68, 0xEE}, false},
		{"start code cut at the end", []byte{0, 0, 0, 1, 0x65, 0x88, 0, 0, 1}, false},
		{"4-byte start code cut before the NAL byte", []byte{0x65, 0, 0, 0, 1}, false},
		{"0x67 payload byte without a start code", []byte{0, 0, 2, 0x67, 0x67}, false},
		{"empty", nil, false},
	} {
		if got := hasSPS(tc.b); got != tc.want {
			t.Errorf("%s: hasSPS(% x) = %v, want %v", tc.name, tc.b, got, tc.want)
		}
	}
}

func TestWithHeaders(t *testing.T) {
	headers := []byte{0, 0, 0, 1, 0x67, 0x4D, 0, 0x1F, 0, 0, 0, 1, 0x68, 0xEE}
	idr := []byte{0, 0, 0, 1, 0x65, 0x88}

	got, injected := withHeaders(idr, headers)
	if !injected || !bytes.Equal(got, append(append([]byte(nil), headers...), idr...)) {
		t.Fatalf("IDR without SPS: injected=%v got % x, want headers+IDR", injected, got)
	}
	if !hasSPS(got) {
		t.Fatal("prefixed keyframe still has no SPS")
	}

	inband := append(append([]byte(nil), headers...), idr...)
	if got, injected := withHeaders(inband, headers); injected || !bytes.Equal(got, inband) {
		t.Fatalf("inband SPS was prefixed again: % x", got)
	}
	if got, injected := withHeaders(idr, nil); injected || !bytes.Equal(got, idr) {
		t.Fatalf("no cached headers, yet the AU changed: % x", got)
	}
}

// checkPlanes is the only thing between a short NV12 slice and an
// out-of-bounds memcpy inside mft.c.
func TestCheckPlanes(t *testing.T) {
	const w, h = 64, 35 // odd height: chroma rows round up to 18
	ok := Frame{Y: make([]byte, 64*35), UV: make([]byte, 64*18), YStride: 64, UVStride: 64}
	if err := checkPlanes(ok, w, h); err != nil {
		t.Fatalf("exact-size planes rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		f    Frame
		want string
	}{
		{"Y stride below width", Frame{Y: ok.Y, UV: ok.UV, YStride: 63, UVStride: 64}, "Y stride"},
		{"UV stride below width", Frame{Y: ok.Y, UV: ok.UV, YStride: 64, UVStride: 63}, "UV stride"},
		{"Y one byte short", Frame{Y: ok.Y[:64*35-1], UV: ok.UV, YStride: 64, UVStride: 64}, "Y plane too small"},
		{"UV missing the odd row", Frame{Y: ok.Y, UV: ok.UV[:64*17], YStride: 64, UVStride: 64}, "UV plane too small"},
	} {
		if err := checkPlanes(tc.f, w, h); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// Q-06 (research/QUALITY-AUDIT.md): колірний опис SPS не повинен залежати від
// того, чи MFT позначив AU ключовим. SPS libx264 main 1080p без colour
// description (той самий вектор, що internal/h264 spsVectors "main1080").
func TestPrepareAUColourOnEveryAU(t *testing.T) {
	sps, err := hex.DecodeString("674d4028eca03c0113f2e022000003000200000300781e30632c")
	if err != nil {
		t.Fatal(err)
	}
	sc := []byte{0, 0, 0, 1}
	pps := []byte{0x68, 0xEE, 0x3C, 0x80}
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	colourOf := func(t *testing.T, au []byte) *h264.SPS {
		t.Helper()
		for _, n := range h264.SplitNALs(au) {
			if len(n) > 0 && n[0]&0x1F == h264.NALSPS {
				s, err := h264.ParseSPS(n)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
		}
		t.Fatal("SPS зник з AU")
		return nil
	}
	isBT709Limited := func(s *h264.SPS) bool {
		return s.ColourDescriptionPresent && !s.FullRange &&
			s.ColourPrimaries == 1 && s.TransferCharacteristics == 1 && s.MatrixCoefficients == 1
	}
	fix := h264.RewriteAnnexBSPSColourBT709

	// Негативний контроль: вхідний SPS справді без колірного опису.
	if isBT709Limited(colourOf(t, cat(sc, sps))) {
		t.Fatal("тестовий SPS уже BT.709 — тест нічого не перевіряє")
	}

	// 1) не-ключовий AU з інбенд-SPS (I-кадр без CleanPoint) — переписано.
	nonKey := cat(sc, sps, sc, pps, sc, []byte{0x41, 0x9A, 0x02})
	out, injected := prepareAU(nonKey, nil, false, fix)
	if injected || !isBT709Limited(colourOf(t, out)) {
		t.Fatalf("не-ключовий AU: injected=%v, SPS %+v", injected, colourOf(t, out))
	}
	if !bytes.HasSuffix(out, []byte{0x41, 0x9A, 0x02}) {
		t.Fatal("зріз не-ключового AU пошкоджено")
	}

	// 2) ключовий з інбенд-SPS — переписано, без ін'єкції.
	key := cat(sc, sps, sc, pps, sc, []byte{0x65, 0x88, 0x84})
	out, injected = prepareAU(key, []byte{0, 0, 0, 1, 0x67}, true, fix)
	if injected || !isBT709Limited(colourOf(t, out)) {
		t.Fatalf("ключовий з SPS: injected=%v", injected)
	}

	// 3) ключовий без SPS — кешовані заголовки як є (кешуються вже переписаними).
	cached, err := fix(cat(sc, sps, sc, pps))
	if err != nil {
		t.Fatal(err)
	}
	idr := cat(sc, []byte{0x65, 0x88, 0x84})
	out, injected = prepareAU(idr, cached, true, fix)
	if !injected || !bytes.Equal(out, cat(cached, idr)) {
		t.Fatalf("ключовий без SPS: injected=%v", injected)
	}

	// 4) звичайний P-кадр — той самий зріз (без копії й без змін).
	p := cat(sc, []byte{0x41, 0x9A, 0x00, 0x00, 0x03, 0x01})
	out, injected = prepareAU(p, cached, false, fix)
	if injected || &out[0] != &p[0] {
		t.Fatal("P-кадр без SPS мусить іти як є")
	}

	// 5) помилка переписувача / nil — AU як є, потік не ламаємо.
	bad := func([]byte) ([]byte, error) { return nil, errors.New("boom") }
	if out, _ = prepareAU(nonKey, nil, false, bad); !bytes.Equal(out, nonKey) {
		t.Fatal("помилка colourFix змінила AU")
	}
	if out, _ = prepareAU(nonKey, nil, false, nil); !bytes.Equal(out, nonKey) {
		t.Fatal("nil colourFix змінив AU")
	}
}
