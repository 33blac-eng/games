package encode

import (
	"bytes"
	"strings"
	"testing"
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
