package h264

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Тестові потоки — x264 (див. bench/quality/x264rc, aq=0): main CABAC ABR зі
// змінною ставкою, примусовими QP і IDR посередині; main з 3 слайсами,
// ref=3 і weightp=2 (pred_weight_table, ref_pic_list_modification);
// baseline CAVLC. Очікуваний QP — той, що повідомив сам x264 (і показує
// `ffmpeg -debug qp` для кожного MB: без AQ він рівний по кадру).
func readAUs(t *testing.T, name string) []AU {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return SplitAUs(b)
}

func TestQPReaderABRMain(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "qp-abr-main.txt"))
	if err != nil {
		t.Fatal(err)
	}
	exp := strings.Fields(string(want))
	aus := readAUs(t, "qp-abr-main.h264")
	if len(aus) != len(exp) {
		t.Fatalf("AU: %d, очікувань: %d", len(aus), len(exp))
	}
	r := NewQPReader()
	for i, au := range aus {
		qp, ok := r.Observe(au.Data)
		w, _ := strconv.Atoi(exp[i][:len(exp[i])-1])
		if !ok || qp != w {
			t.Fatalf("AU %d (%s): qp=%d ok=%v, хочемо %d", i, exp[i], qp, ok, w)
		}
		if key := exp[i][len(exp[i])-1] == 'I'; key != au.Keyframe {
			t.Fatalf("AU %d: keyframe=%v, x264 каже %s", i, au.Keyframe, exp[i])
		}
	}
}

func TestQPReaderConstQP(t *testing.T) {
	for _, c := range []struct {
		file string
		qp   int
	}{{"qp31-main-slices-weightp.h264", 31}, {"qp40-baseline.h264", 40}} {
		r := NewQPReader()
		aus := readAUs(t, c.file)
		if len(aus) < 20 {
			t.Fatalf("%s: лише %d AU", c.file, len(aus))
		}
		for i, au := range aus {
			qp, ok := r.Observe(au.Data)
			if !ok || qp != c.qp {
				t.Fatalf("%s AU %d: qp=%d ok=%v, хочемо %d", c.file, i, qp, ok, c.qp)
			}
		}
	}
}

// Без SPS/PPS QP невідомий: P-кадр із середини потоку — ok=false, а не
// вигадане число.
func TestQPReaderNeedsParameterSets(t *testing.T) {
	aus := readAUs(t, "qp-abr-main.h264")
	if qp, ok := NewQPReader().Observe(aus[3].Data); ok {
		t.Fatalf("без SPS/PPS: qp=%d ok=true", qp)
	}
	// Сміття й обрізані AU не панікують.
	r := NewQPReader()
	r.Observe(aus[0].Data)
	for _, b := range [][]byte{nil, {0, 0, 1}, {0, 0, 1, 0x65}, aus[1].Data[:8], {0, 0, 1, 0x68, 0xff, 0xff}} {
		r.Observe(b)
	}
	if _, ok := r.Observe(aus[1].Data); !ok {
		t.Fatal("після сміття читач загубив SPS/PPS")
	}
}

func FuzzQPReader(f *testing.F) {
	b, _ := os.ReadFile(filepath.Join("testdata", "qp-abr-main.h264"))
	if len(b) > 4000 {
		f.Add(b[:4000])
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewQPReader()
		r.Observe(data)
		r.Observe(data)
	})
}
