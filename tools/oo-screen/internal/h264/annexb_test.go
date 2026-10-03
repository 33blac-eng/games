package h264

import (
	"os"
	"testing"
)

// Гейт на РЕАЛЬНОМУ корпусі: 600 AU (10с × 60fps), 5 IDR (кожні 2с),
// SPS = High(100) 1920×1080 level 4.2.
func TestCorpusSplitAndSPS(t *testing.T) {
	data, err := os.ReadFile("../../bench/corpus/corpus-1080p60.h264")
	if err != nil {
		t.Skip("corpus not present:", err)
	}
	aus := SplitAUs(data)
	if len(aus) != 600 {
		t.Fatalf("AU count = %d, want 600", len(aus))
	}
	idr := 0
	var sps *SPS
	for _, au := range aus {
		if au.Keyframe {
			idr++
			if !au.HasSPS {
				t.Fatal("IDR AU without SPS — repeat-headers contract violated")
			}
		}
		if au.HasSPS && sps == nil {
			for _, nal := range SplitNALs(au.Data) {
				if nal[0]&0x1F == NALSPS {
					sps, err = ParseSPS(nal)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	if idr != 5 {
		t.Fatalf("IDR count = %d, want 5", idr)
	}
	if !aus[0].Keyframe {
		t.Fatal("first AU must be IDR")
	}
	if sps == nil {
		t.Fatal("no SPS parsed")
	}
	if sps.ProfileIDC != 100 || sps.LevelIDC != 42 || sps.Width != 1920 || sps.Height != 1080 {
		t.Fatalf("SPS = %+v, want High/4.2 1920x1080", sps)
	}
	if sps.CodecString() == "avc1." {
		t.Fatal("empty codec string")
	}
	t.Logf("codec=%s aus=%d idr=%d", sps.CodecString(), len(aus), idr)
}
