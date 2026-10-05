package main

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/organicoils/oo-screen/agent/audio"
)

type fakeCap struct {
	f      audio.Format
	frames [][]byte
}

func (f *fakeCap) Format() audio.Format { return f.f }
func (f *fakeCap) Close() error         { return nil }
func (f *fakeCap) NextFrame(context.Context) (*audio.Frame, error) {
	d := f.frames[0]
	f.frames = f.frames[1:]
	return &audio.Frame{Data: d, Format: f.f}, nil
}

// 44.1 кГц (Windows 7 без AUTOCONVERTPCM) -> 48 кГц: кількість кадрів і форма
// синуса мусять зберегтись, інакше Opus отримає прискорений/рваний звук.
func TestResample441To48(t *testing.T) {
	in := audio.Format{SampleRate: 44100, Channels: 2, SampleFormat: audio.SampleFormatPCM,
		BitsPerSample: 16, ValidBitsPerSample: 16, BytesPerFrame: 4}
	const tone = 440.0
	var pkts [][]byte
	k := 0
	for p := 0; p < 100; p++ { // 100 пакетів по 441 кадр = 1 с
		b := make([]byte, 0, 441*4)
		for i := 0; i < 441; i++ {
			v := int16(10000 * math.Sin(2*math.Pi*tone*float64(k)/44100))
			b = binary.LittleEndian.AppendUint16(b, uint16(v))
			b = binary.LittleEndian.AppendUint16(b, uint16(v))
			k++
		}
		pkts = append(pkts, b)
	}
	src := &fakeCap{f: in, frames: pkts}
	w := wrapResample(src)
	if w.Format().SampleRate != 48000 || w.Format().SampleFormat != audio.SampleFormatFloat {
		t.Fatalf("формат після обгортки %+v", w.Format())
	}
	if _, _, ok := audioLayout(w.Format(), 48000); !ok {
		t.Fatal("audioLayout не бере результат обгортки")
	}
	total, maxErr := 0, 0.0
	for p := 0; p < 100; p++ {
		fr, _ := w.NextFrame(context.Background())
		for i := 0; i < fr.Frames; i++ {
			got := float64(math.Float32frombits(binary.LittleEndian.Uint32(fr.Data[i*8:])))
			want := 10000.0 / 32768 * math.Sin(2*math.Pi*tone*float64(total)/48000)
			maxErr = math.Max(maxErr, math.Abs(got-want))
			total++
		}
	}
	if total < 47990 || total > 48010 {
		t.Fatalf("1 с на 44.1 кГц дала %d кадрів на 48 кГц, чекали ~48000", total)
	}
	if maxErr > 0.01 {
		t.Fatalf("синус спотворено: макс. похибка %.4f", maxErr)
	}
	// Частота, кратна 48 кГц, лишається без обгортки.
	if wrapResample(&fakeCap{f: audio.Format{SampleRate: 48000, Channels: 2}}).Format().SampleRate != 48000 {
		t.Fatal("48 кГц не мусить обгортатись")
	}
}
