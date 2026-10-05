// Platform-free half of the encoder: the Annex-B and NV12-plane checks that
// sit on the hot path but need neither cgo nor Media Foundation, so they are
// tested on any OS (annexb_test.go).
package encode

import "fmt"

// hasSPS reports whether the Annex-B stream carries an SPS NAL (type 7).
// Deliberately local rather than importing internal/h264: the encoder must not
// depend on the parser it is validated against. Only the 3-byte start code is
// matched: the 4-byte one (00 00 00 01) ends in it, so it is found one byte on.
func hasSPS(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 && b[i+3]&0x1F == 7 {
			return true
		}
	}
	return false
}

// withHeaders prefixes the cached SPS/PPS to a keyframe that arrived without
// them (§5.2: every IDR must carry its sequence header, and not every MFT
// repeats it inband). A keyframe that already has an SPS, or no cached headers
// to add, is returned untouched. The bool reports whether headers were added.
func withHeaders(data, headers []byte) ([]byte, bool) {
	if hasSPS(data) || len(headers) == 0 {
		return data, false
	}
	merged := make([]byte, 0, len(headers)+len(data))
	merged = append(merged, headers...)
	return append(merged, data...), true
}

// checkPlanes validates the CPU NV12 planes against the configured frame size.
// oos_enc_submit_cpu does memcpy(dst, y + r*YStride, Width) for every row with
// no bounds knowledge of its own, so a short slice or a stride smaller than the
// width is an out-of-bounds read inside C — silent corruption at best. Every
// such frame is rejected here instead.
func checkPlanes(f Frame, w, h int) error {
	// mft.c: chroma_rows = ((h+1) & ~1) / 2
	chromaRows := ((h + 1) &^ 1) / 2

	if f.YStride < w {
		return fmt.Errorf("encode: Y stride %d < width %d", f.YStride, w)
	}
	if f.UVStride < w {
		return fmt.Errorf("encode: UV stride %d < width %d", f.UVStride, w)
	}
	if need := f.YStride * h; len(f.Y) < need {
		return fmt.Errorf("encode: Y plane too small: %d bytes, need %d (stride %d x %d rows)",
			len(f.Y), need, f.YStride, h)
	}
	if need := f.UVStride * chromaRows; len(f.UV) < need {
		return fmt.Errorf("encode: UV plane too small: %d bytes, need %d (stride %d x %d rows)",
			len(f.UV), need, f.UVStride, chromaRows)
	}
	return nil
}
