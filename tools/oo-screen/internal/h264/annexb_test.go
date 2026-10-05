package h264

import (
	"os"
	"testing"
	"time"
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

// bitW — мінімальний писар бітів для синтетичних SPS.
type bitW struct {
	b []byte
	n int
}

func (w *bitW) bit(v int) {
	if w.n%8 == 0 {
		w.b = append(w.b, 0)
	}
	if v != 0 {
		w.b[len(w.b)-1] |= 0x80 >> uint(w.n%8)
	}
	w.n++
}

func (w *bitW) ue(v uint32) {
	x := uint64(v) + 1
	l := 0
	for t := x; t > 1; t >>= 1 {
		l++
	}
	for i := 0; i < l; i++ {
		w.bit(0)
	}
	for i := l; i >= 0; i-- {
		w.bit(int(x>>uint(i)) & 1)
	}
}

// TestParseSPSHugePocCycleFailsFast — битий SPS з poc_type=1 і
// num_ref_frames_in_pic_order_cnt_cycle ~2^31 має відмовити одразу. Прибери
// перевірку n > 255 — цикл крутитиметься секунди.
func TestParseSPSHugePocCycleFailsFast(t *testing.T) {
	w := &bitW{}
	for _, v := range []byte{66, 0, 30} { // baseline, constraint, level
		for i := 7; i >= 0; i-- {
			w.bit(int(v>>uint(i)) & 1)
		}
	}
	w.ue(0)       // seq_parameter_set_id
	w.ue(0)       // log2_max_frame_num_minus4
	w.ue(1)       // pic_order_cnt_type
	w.bit(0)      // delta_pic_order_always_zero_flag
	w.ue(0)       // offset_for_non_ref_pic (se 0)
	w.ue(0)       // offset_for_top_to_bottom_field (se 0)
	w.ue(1 << 31) // num_ref_frames_in_pic_order_cnt_cycle — битий
	nal := append([]byte{0x67}, w.b...)

	start := time.Now()
	if _, err := ParseSPS(nal); err == nil {
		t.Fatal("битий SPS розібрано без помилки")
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("ParseSPS на битому SPS зайняв %v — цикл до 2^31", d)
	}
}
