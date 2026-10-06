package envelope

import (
	"bytes"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	f := &Frame{Flags: FlagKeyframe | FlagConfigured, ConfigEpoch: 7, FrameSeq: 123456789, PTS: 987654321, Payload: []byte{0, 0, 0, 1, 0x65, 1, 2, 3}}
	b, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// два кадри підряд в одному стрімі
	r := bytes.NewReader(append(append([]byte{}, b...), b...))
	for i := 0; i < 2; i++ {
		g, err := ReadFrame(r)
		if err != nil {
			t.Fatal(err)
		}
		if g.FrameSeq != f.FrameSeq || g.PTS != f.PTS || g.ConfigEpoch != 7 || !g.Keyframe() || !g.ConfigChanged() || !bytes.Equal(g.Payload, f.Payload) {
			t.Fatalf("mismatch: %+v", g)
		}
	}
	if _, err := ReadFrame(r); err != io.EOF {
		t.Fatalf("want clean EOF at frame boundary, got %v", err)
	}
}

func TestRejects(t *testing.T) {
	f := &Frame{Payload: make([]byte, MaxPayloadLen+1)}
	if _, err := f.Marshal(); err != ErrPayloadTooBig {
		t.Fatal("oversize marshal must fail")
	}
	b, _ := (&Frame{Payload: []byte{1}}).Marshal()
	bad := append([]byte{}, b...)
	bad[0] = 'X'
	if _, err := ReadFrame(bytes.NewReader(bad)); err != ErrBadMagic {
		t.Fatal("bad magic must fail")
	}
	bad = append([]byte{}, b...)
	bad[4] = 9
	if _, err := ReadFrame(bytes.NewReader(bad)); err == nil {
		t.Fatal("bad version must fail")
	}
	// оголошена довжина понад ліміт
	bad = append([]byte{}, b...)
	bad[24], bad[25], bad[26], bad[27] = 0xFF, 0xFF, 0xFF, 0x7F
	if _, err := ReadFrame(bytes.NewReader(bad)); err != ErrPayloadTooBig {
		t.Fatal("oversize declared len must fail")
	}
	// обрізаний заголовок
	if _, err := ReadFrame(bytes.NewReader(b[:10])); err != ErrShortHeader {
		t.Fatal("short header must fail")
	}
}

func FuzzReadFrame(f *testing.F) {
	seed, _ := (&Frame{Flags: 1, ConfigEpoch: 1, FrameSeq: 1, PTS: 1, Payload: []byte{0x65}}).Marshal()
	f.Add(seed)
	f.Add([]byte("OOSC"))
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := ReadFrame(bytes.NewReader(data))
		if err == nil {
			// що розпарсилось — мусить пере-серіалізуватись байт-у-байт
			out, merr := fr.Marshal()
			if merr != nil {
				t.Fatal(merr)
			}
			if !bytes.Equal(out, data[:len(out)]) {
				t.Fatal("re-marshal mismatch")
			}
		}
	})
}
